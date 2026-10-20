package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/upgrade"
)

func attachUpgrade(t *testing.T) *app {
	t.Helper()
	a := newApp()
	for _, spec := range a.upgradeSpecs() {
		a.attach(a.root, spec)
	}
	if err := a.meta.validate(a.root); err != nil {
		t.Fatalf("upgrade metadata does not validate: %v", err)
	}
	return a
}

func TestUpgradeSpecsMetadata(t *testing.T) {
	a := attachUpgrade(t)
	cmd, _, err := a.root.Find([]string{"upgrade", "apply"})
	if err != nil {
		t.Fatal(err)
	}
	meta := a.meta[cmd]
	if !meta.Mutating || !meta.Destructive || !meta.DryRun || meta.Confirmation != "plan-bound" {
		t.Fatalf("apply metadata is not a plan-bound destructive mutation: %+v", meta)
	}
	if meta.NeedsRestart != "required" {
		t.Fatalf("apply restart requirement = %q", meta.NeedsRestart)
	}
	guide := a.currentUpgradeGuide()
	if guide.Schema != upgrade.GuideSchema || guide.GuideVersion != upgrade.GuideVersion {
		t.Fatalf("guide identity = %+v", guide)
	}
	found := false
	for _, command := range guide.Commands {
		if command.Path == "eregion upgrade apply" {
			found = true
			if !command.Destructive || !command.DryRun || !command.Mutating {
				t.Fatalf("guide lost apply safety metadata: %+v", command)
			}
		}
	}
	if !found {
		t.Fatal("generated guide does not contain upgrade apply")
	}
	markdown := upgrade.RenderMarkdown(guide)
	if !strings.Contains(markdown, "Upgrade workflow") || !strings.Contains(markdown, "upgrade rollback") {
		t.Fatalf("markdown guide is incomplete:\n%s", markdown)
	}
	if strings.Contains(markdown, "eregion deploy") {
		t.Fatal("guide advertises an unimplemented command")
	}
}

func TestUpgradeManifestRejectsPlaintextAndUnsigned(t *testing.T) {
	a := attachUpgrade(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("EREGION_UPGRADE_PUBLIC_KEY", base64.StdEncoding.EncodeToString(public))
	t.Setenv("EREGION_UPGRADE_PUBLIC_KEY_FILE", "")
	cmd, _, err := a.root.Find([]string{"upgrade", "check"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("manifest", "http://releases.example.com/manifest.json"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.upgradeManifest(cmd); err == nil {
		t.Fatal("plaintext manifest URL was accepted")
	}

	manifest := upgrade.Manifest{
		Schema: upgrade.ManifestSchema, Version: "1.1.0", Channel: "stable",
		PublishedAt: "2026-10-20T00:00:00Z", Contract: "eregion/v1",
		NodeProtocol: 1, SchemaVersion: 3,
		Artifacts: []upgrade.Artifact{{
			Name: "eregion_1.1.0_linux_amd64.tar.gz", Target: upgrade.TargetOperator,
			OS: "linux", Arch: "amd64", Entry: "eregion",
			URL:  "https://releases.example.com/eregion_1.1.0_linux_amd64.tar.gz",
			Size: 10, SHA256: strings.Repeat("a", 64),
		}},
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("manifest", path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.upgradeManifest(cmd); err == nil {
		t.Fatal("unsigned manifest was accepted")
	}
	if err := manifest.Sign(private); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(manifest)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.upgradeManifest(cmd); err != nil {
		t.Fatalf("signed manifest was refused: %v", err)
	}
}

func TestUpgradeCheckBuildsVerifiedPlanFromLocalManifest(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "bin", "eregion")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	artifacts := filepath.Join(root, "artifacts")
	if err := os.MkdirAll(artifacts, 0o700); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	content := []byte("new-binary")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "eregion", Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	name := "eregion_1.1.0_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	if err := os.WriteFile(filepath.Join(artifacts, name), archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive.Bytes())
	manifest := upgrade.Manifest{
		Schema: upgrade.ManifestSchema, Version: "1.1.0", Channel: "stable",
		PublishedAt: "2026-10-20T00:00:00Z", Contract: "eregion/v1",
		NodeProtocol: 1, SchemaVersion: 3,
		Artifacts: []upgrade.Artifact{{
			Name: name, Target: upgrade.TargetOperator, OS: runtime.GOOS, Arch: runtime.GOARCH, Entry: "eregion",
			URL: "https://releases.example.com/" + name, Size: int64(archive.Len()),
			SHA256: hex.EncodeToString(sum[:]),
		}},
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Sign(private); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(manifest)
	manifestPath := filepath.Join(root, "manifest.json")
	if err := os.WriteFile(manifestPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "release.pub")
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(public)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := attachUpgrade(t)
	cmd, _, err := a.root.Find([]string{"upgrade", "check"})
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"manifest":        manifestPath,
		"artifact-dir":    artifacts,
		"public-key-file": keyPath,
		"target":          "operator",
		"binary":          binary,
		"state-dir":       filepath.Join(root, "state"),
		"schema-version":  "3",
	} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set --%s: %v", name, err)
		}
	}
	result, err := a.upgradeCheck(cmd, nil)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	plan, ok := result.data.(upgrade.Plan)
	if !ok || plan.Confirmation == "" || plan.NextVersion != "1.1.0" {
		t.Fatalf("unexpected check result: %#v", result.data)
	}
	if _, err := os.Stat(filepath.Join(root, "state")); !os.IsNotExist(err) {
		t.Fatalf("check created upgrade state: %v", err)
	}
	if got, _ := os.ReadFile(binary); string(got) != "old-binary" {
		t.Fatal("check replaced the binary")
	}
}

func TestUpgradeStatusReadsEmptyState(t *testing.T) {
	a := attachUpgrade(t)
	root := t.TempDir()
	binary := filepath.Join(root, "eregion")
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd, _, err := a.root.Find([]string{"upgrade", "status"})
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"target": "operator", "binary": binary, "state-dir": filepath.Join(root, "state"),
	} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	result, err := a.upgradeStatus(cmd, nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	status, ok := result.data.(upgrade.Status)
	if !ok || status.Interrupted || status.SHA256 == "" {
		t.Fatalf("unexpected status: %#v", result.data)
	}
}

func TestUpgradeGuideFormatValidation(t *testing.T) {
	a := attachUpgrade(t)
	cmd, _, err := a.root.Find([]string{"upgrade", "guide"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("format", "yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.upgradeGuide(cmd, nil); err == nil {
		t.Fatal("unsupported guide format accepted")
	}
	if err := cmd.Flags().Set("format", "json"); err != nil {
		t.Fatal(err)
	}
	result, err := a.upgradeGuide(cmd, nil)
	if err != nil || result.plain == "" {
		t.Fatalf("json guide: %v %q", err, result.plain)
	}
}
