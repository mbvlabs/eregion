// Command releasegen builds and signs an eregion release manifest from the
// archives produced by the release workflow. It reads the Ed25519 signing key
// only from an environment variable or a strict-permission file, and it never
// prints the private key.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/upgrade"
)

type artifactSpec struct {
	target upgrade.Target
	goos   string
	arch   string
}

var expected = []artifactSpec{
	{upgrade.TargetOperator, "linux", "amd64"},
	{upgrade.TargetOperator, "linux", "arm64"},
	{upgrade.TargetOperator, "darwin", "amd64"},
	{upgrade.TargetOperator, "darwin", "arm64"},
	{upgrade.TargetOperator, "windows", "amd64"},
	{upgrade.TargetOperator, "windows", "arm64"},
	{upgrade.TargetNode, "linux", "amd64"},
	{upgrade.TargetNode, "linux", "arm64"},
}

func main() {
	dir := flag.String("dir", "", "directory containing release archives")
	version := flag.String("version", "", "release version without the v prefix")
	channel := flag.String("channel", "stable", "release channel")
	published := flag.String("published", "", "RFC3339 publication time (defaults to now)")
	baseURL := flag.String("base-url", "", "direct HTTPS base URL for archives (no redirects)")
	out := flag.String("out", "", "manifest output path (defaults to DIR/manifest.json)")
	checksums := flag.String("checksums", "", "optional checksum listing output path")
	publicOut := flag.String("public-key-out", "", "optional path to write the release public key")
	contractVersion := flag.String("contract", contract.Schema, "result contract version")
	nodeProtocol := flag.Int("node-protocol", node.ProtocolVersion, "node wire protocol version")
	schemaVersion := flag.Int("schema-version", 0, "required database schema version")
	upgradeFrom := flag.String("upgrade-from", "", "comma-separated supported upgrade source versions")
	keyEnv := flag.String("key-env", "EREGION_RELEASE_SIGNING_KEY", "environment variable holding the base64 Ed25519 signing key")
	keyFile := flag.String("key-file", "", "optional private key file (0600, no group/other access)")
	flag.Parse()

	if strings.TrimSpace(*dir) == "" || strings.TrimSpace(*version) == "" || strings.TrimSpace(*baseURL) == "" {
		fail(fmt.Errorf("-dir, -version, and -base-url are required"))
	}
	if *schemaVersion < 1 {
		fail(fmt.Errorf("-schema-version must be positive"))
	}
	if *published == "" {
		*published = time.Now().UTC().Format(time.RFC3339)
	}
	private, err := signingKey(*keyEnv, *keyFile)
	if err != nil {
		fail(err)
	}
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		fail(fmt.Errorf("signing key has no Ed25519 public key"))
	}

	manifest := upgrade.Manifest{
		Schema: upgrade.ManifestSchema, Version: strings.TrimPrefix(*version, "v"), Channel: *channel,
		PublishedAt: *published, Contract: *contractVersion, NodeProtocol: *nodeProtocol,
		SchemaVersion: *schemaVersion, UpgradeFrom: splitList(*upgradeFrom),
	}
	checksumLines := make([]string, 0, len(expected))
	for _, spec := range expected {
		name := archiveName(manifest.Version, spec)
		path := filepath.Join(*dir, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			fail(fmt.Errorf("required archive %s is missing or not a regular file", name))
		}
		if info.Size() <= 0 || info.Size() > upgrade.MaxArtifactBytes {
			fail(fmt.Errorf("archive %s has an invalid size", name))
		}
		sha, err := fileDigest(path)
		if err != nil {
			fail(fmt.Errorf("archive %s could not be hashed: %w", name, err))
		}
		entry := "eregion"
		if spec.target == upgrade.TargetNode {
			entry = "eregion-node"
		}
		if spec.goos == "windows" {
			entry += ".exe"
		}
		manifest.Artifacts = append(manifest.Artifacts, upgrade.Artifact{
			Name: name, Target: spec.target, OS: spec.goos, Arch: spec.arch, Entry: entry,
			URL: strings.TrimRight(*baseURL, "/") + "/" + name, Size: info.Size(), SHA256: sha,
		})
		checksumLines = append(checksumLines, sha+"  "+name)
	}
	if err := manifest.Sign(private); err != nil {
		fail(err)
	}
	if err := manifest.Verify(public); err != nil {
		fail(fmt.Errorf("generated manifest failed self-verification: %w", err))
	}
	raw, err := marshalManifest(manifest)
	if err != nil {
		fail(err)
	}
	output := *out
	if output == "" {
		output = filepath.Join(*dir, "manifest.json")
	}
	if err := writeFile(output, raw, 0o644); err != nil {
		fail(fmt.Errorf("manifest could not be written: %w", err))
	}
	if *checksums != "" {
		sort.Strings(checksumLines)
		if err := writeFile(*checksums, []byte(strings.Join(checksumLines, "\n")+"\n"), 0o644); err != nil {
			fail(fmt.Errorf("checksum listing could not be written: %w", err))
		}
	}
	if *publicOut != "" {
		if err := writeFile(*publicOut, []byte(base64.StdEncoding.EncodeToString(public)+"\n"), 0o644); err != nil {
			fail(fmt.Errorf("public key could not be written: %w", err))
		}
	}
	sum := sha256.Sum256(raw)
	fmt.Printf("manifest: %s\nmanifest_sha256: %s\n", output, hex.EncodeToString(sum[:]))
}

func signingKey(envName, path string) (ed25519.PrivateKey, error) {
	if path != "" {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("signing key file is missing or not a regular file")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("signing key file must not be readable or writable by group or others")
		}
		if info.Size() > 4096 {
			return nil, fmt.Errorf("signing key file is too large")
		}
		raw, err := os.ReadFile(path) //nolint:gosec // G304: operator-selected release key.
		if err != nil {
			return nil, fmt.Errorf("signing key file could not be read")
		}
		return upgrade.ParsePrivateKey(string(raw))
	}
	if strings.TrimSpace(os.Getenv(envName)) == "" {
		return nil, fmt.Errorf("release signing key is not configured; set the repository secret exposed as %s", envName)
	}
	return upgrade.ParsePrivateKey(os.Getenv(envName))
}

func archiveName(version string, spec artifactSpec) string {
	base := "eregion"
	if spec.target == upgrade.TargetNode {
		base = "eregion-node"
	}
	return fmt.Sprintf("%s_%s_%s_%s.tar.gz", base, version, spec.goos, spec.arch)
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // G304: release archive selected by this tool.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func splitList(value string) []string {
	var out []string
	for _, field := range strings.Split(value, ",") {
		if field = strings.TrimSpace(field); field != "" {
			out = append(out, field)
		}
	}
	return out
}

func writeFile(path string, raw []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := temp.Write(raw); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func marshalManifest(manifest upgrade.Manifest) ([]byte, error) {
	// The manifest has no maps; encoding/json preserves struct field order and
	// therefore the canonical bytes are stable across Go versions.
	return json.MarshalIndent(manifest, "", "  ")
}

func fail(err error) {
	fault := contract.Classify(err)
	fmt.Fprintf(os.Stderr, "error [%s]: %s\n", fault.Code, fault.Message)
	if fault.Hint != "" {
		fmt.Fprintf(os.Stderr, "hint: %s\n", fault.Hint)
	}
	os.Exit(1)
}
