// Command upgrade_drill_sign is a tiny drill-only signer. It builds one
// Ed25519-signed release manifest for a single artifact so scripts/upgrade_drill.py
// can exercise the real eregion CLI without the eight-archive release matrix.
//
// It is not part of the release workflow and never reads a production key.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/upgrade"
)

func main() {
	artifact := flag.String("artifact", "", "artifact archive path")
	name := flag.String("name", "", "artifact file name (defaults to the base name)")
	version := flag.String("version", "", "release version")
	baseURL := flag.String("base-url", "https://objects.invalid/eregion", "artifact base URL")
	entry := flag.String("entry", "eregion", "binary entry inside the archive")
	schemaVersion := flag.Int("schema-version", 0, "required database schema version")
	upgradeFrom := flag.String("upgrade-from", "", "comma-separated supported source versions")
	out := flag.String("out", "", "manifest output path")
	publicOut := flag.String("public-key-out", "", "public key output path")
	flag.Parse()

	if *artifact == "" || *version == "" || *out == "" || *publicOut == "" || *schemaVersion < 1 {
		fail("artifact, version, out, public-key-out, and a positive schema-version are required")
	}
	info, err := os.Lstat(*artifact)
	if err != nil || !info.Mode().IsRegular() {
		fail("artifact is missing or not a regular file")
	}
	raw, err := os.ReadFile(*artifact) //nolint:gosec // drill-selected temporary artifact.
	if err != nil {
		fail("artifact could not be read")
	}
	if *name == "" {
		*name = filepath.Base(*artifact)
	}
	sum := sha256.Sum256(raw)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fail("key generation failed")
	}
	manifest := upgrade.Manifest{
		Schema: upgrade.ManifestSchema, Version: strings.TrimPrefix(*version, "v"),
		Channel: "drill", PublishedAt: time.Now().UTC().Format(time.RFC3339),
		Contract: contract.Schema, NodeProtocol: node.ProtocolVersion,
		SchemaVersion: *schemaVersion, UpgradeFrom: splitList(*upgradeFrom),
		Artifacts: []upgrade.Artifact{{
			Name: *name, Target: upgrade.TargetOperator, OS: "linux", Arch: "amd64",
			Entry: *entry, URL: strings.TrimRight(*baseURL, "/") + "/" + *name,
			Size: info.Size(), SHA256: hex.EncodeToString(sum[:]),
		}},
	}
	if err := manifest.Sign(private); err != nil {
		fail(err.Error())
	}
	if err := manifest.Verify(public); err != nil {
		fail("self-verification failed: " + err.Error())
	}
	rawManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		fail("manifest could not be encoded")
	}
	if err := os.WriteFile(*out, append(rawManifest, '\n'), 0o644); err != nil {
		fail("manifest could not be written")
	}
	if err := os.WriteFile(*publicOut, []byte(base64.StdEncoding.EncodeToString(public)+"\n"), 0o644); err != nil {
		fail("public key could not be written")
	}
	manifestSum := sha256.Sum256(rawManifest)
	fmt.Printf("manifest: %s\nmanifest_sha256: %s\n", *out, hex.EncodeToString(manifestSum[:]))
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

func fail(message string) {
	fmt.Fprintln(os.Stderr, "upgrade drill signer: "+message)
	os.Exit(1)
}
