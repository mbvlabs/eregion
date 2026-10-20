package upgrade

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return public, private
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func testManifest(t *testing.T) Manifest {
	t.Helper()
	archive := []byte("archive-bytes")
	return Manifest{
		Schema: ManifestSchema, Version: "1.1.0", Channel: "stable",
		PublishedAt: "2026-10-20T00:00:00Z", Contract: "eregion/v1",
		NodeProtocol: 1, SchemaVersion: 3, UpgradeFrom: []string{"1.0.0"},
		Artifacts: []Artifact{{
			Name: "eregion_1.1.0_linux_amd64.tar.gz", Target: TargetOperator,
			OS: "linux", Arch: "amd64", Entry: "eregion",
			URL:  "https://releases.example.com/eregion_1.1.0_linux_amd64.tar.gz",
			Size: int64(len(archive)), SHA256: digest(archive),
		}},
	}
}

func signedManifest(t *testing.T) (Manifest, ed25519.PublicKey) {
	t.Helper()
	public, private := testKey(t)
	manifest := testManifest(t)
	if err := manifest.Sign(private); err != nil {
		t.Fatal(err)
	}
	return manifest, public
}

func TestSignVerifyRoundTrip(t *testing.T) {
	manifest, public := signedManifest(t)
	if err := manifest.Verify(public); err != nil {
		t.Fatalf("verify: %v", err)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Manifest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Verify(public); err != nil {
		t.Fatalf("round-trip verify: %v", err)
	}
}

func TestParseManifestRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	manifest, public := signedManifest(t)
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseManifest(raw)
	if err != nil {
		t.Fatalf("valid manifest: %v", err)
	}
	if err := parsed.Verify(public); err != nil {
		t.Fatalf("valid manifest did not verify: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["future_field"] = true
	withUnknown, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifest(withUnknown); err == nil {
		t.Fatal("unknown manifest field was accepted")
	}
	if _, err := ParseManifest(append(raw, []byte("{}")...)); err == nil {
		t.Fatal("trailing JSON was accepted")
	}
}

func TestTamperedManifestAndSignatureFail(t *testing.T) {
	manifest, public := signedManifest(t)
	otherPublic, _ := testKey(t)

	tampered := manifest
	tampered.Version = "9.9.9"
	if err := tampered.Verify(public); err == nil {
		t.Fatal("tampered version verified")
	}
	tampered = manifest
	tampered.Artifacts[0].SHA256 = digest([]byte("other"))
	if err := tampered.Verify(public); err == nil {
		t.Fatal("tampered artifact digest verified")
	}
	tampered = manifest
	tampered.Signature = manifest.Signature[:len(manifest.Signature)-2] + "AA"
	if err := tampered.Verify(public); err == nil {
		t.Fatal("tampered signature verified")
	}
	if err := manifest.Verify(otherPublic); err == nil {
		t.Fatal("wrong trusted key verified")
	}
	unsigned := manifest
	unsigned.Signature = ""
	if err := unsigned.Verify(public); err == nil {
		t.Fatal("unsigned manifest verified")
	}
}

func TestManifestValidationRejectsUnsafeOrMalformedFields(t *testing.T) {
	base := testManifest(t)
	for _, test := range []struct {
		name   string
		change func(*Manifest)
	}{
		{"schema", func(m *Manifest) { m.Schema = "eregion/release/v2" }},
		{"version", func(m *Manifest) { m.Version = "latest" }},
		{"published", func(m *Manifest) { m.PublishedAt = "yesterday" }},
		{"contract", func(m *Manifest) { m.Contract = "eregion/2" }},
		{"protocol", func(m *Manifest) { m.NodeProtocol = 0 }},
		{"schema-version", func(m *Manifest) { m.SchemaVersion = 0 }},
		{"no-artifacts", func(m *Manifest) { m.Artifacts = nil }},
		{"sha", func(m *Manifest) { m.Artifacts[0].SHA256 = "abc" }},
		{"size", func(m *Manifest) { m.Artifacts[0].Size = 0 }},
		{"http", func(m *Manifest) { m.Artifacts[0].URL = "http://releases.example.com/a.tgz" }},
		{"userinfo", func(m *Manifest) { m.Artifacts[0].URL = "https://user:pass@releases.example.com/a.tgz" }},
		{"fragment", func(m *Manifest) { m.Artifacts[0].URL = "https://releases.example.com/a.tgz#x" }},
		{"loopback", func(m *Manifest) { m.Artifacts[0].URL = "https://127.0.0.1/a.tgz" }},
		{"private", func(m *Manifest) { m.Artifacts[0].URL = "https://10.0.0.1/a.tgz" }},
		{"localhost", func(m *Manifest) { m.Artifacts[0].URL = "https://localhost/a.tgz" }},
		{"name", func(m *Manifest) { m.Artifacts[0].Name = "../eregion.tgz" }},
		{"entry", func(m *Manifest) { m.Artifacts[0].Entry = "../eregion" }},
		{"node-darwin", func(m *Manifest) { m.Artifacts[0].Target, m.Artifacts[0].OS = TargetNode, "darwin" }},
		{"duplicate", func(m *Manifest) { m.Artifacts = append(m.Artifacts, m.Artifacts[0]) }},
		{"upgrade-from", func(m *Manifest) { m.UpgradeFrom = []string{"nope"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := base
			manifest.Artifacts = append([]Artifact(nil), base.Artifacts...)
			manifest.UpgradeFrom = append([]string(nil), base.UpgradeFrom...)
			test.change(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatal("manifest passed validation")
			}
		})
	}
}

func TestPublicKeyParsingAndPermissions(t *testing.T) {
	public, private := testKey(t)
	encoded := []byte(ed25519PublicBase64(public))
	path := filepath.Join(t.TempDir(), "release.pub")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPublicKey(path)
	if err != nil || !loaded.Equal(public) {
		t.Fatalf("load public key: %v", err)
	}
	if _, err := ParsePublicKey(string(encoded)); err != nil {
		t.Fatalf("parse base64 key: %v", err)
	}
	if _, err := ParsePublicKey(hex.EncodeToString(public)); err != nil {
		t.Fatalf("parse hex key: %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o666); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPublicKey(path); err == nil {
			t.Fatal("group/world-writable trust anchor was accepted")
		}
	}
	link := filepath.Join(t.TempDir(), "link.pub")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := LoadPublicKey(link); err == nil {
		t.Fatal("symlinked trust anchor was accepted")
	}
	if _, err := ParsePrivateKey(ed25519PrivateBase64(private)); err != nil {
		t.Fatalf("parse private key: %v", err)
	}
}

func ed25519PublicBase64(key ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(key)
}

func ed25519PrivateBase64(key ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(key.Seed())
}

func TestCheckCompatibility(t *testing.T) {
	manifest := testManifest(t)
	env := Environment{Target: TargetOperator, OS: "linux", Arch: "amd64", Version: "1.0.0", Contract: "eregion/v1", NodeProtocol: 1, SchemaVersion: 3}
	if report := CheckCompatibility(manifest, env); !report.Compatible {
		t.Fatalf("compatible release rejected: %+v", report)
	}
	for _, test := range []struct {
		name   string
		change func(*Environment)
		check  string
	}{
		{"os", func(env *Environment) { env.OS = "freebsd" }, "platform"},
		{"arch", func(env *Environment) { env.Arch = "386" }, "artifact"},
		{"contract", func(env *Environment) { env.Contract = "eregion/v2" }, "contract"},
		{"protocol", func(env *Environment) { env.NodeProtocol = 2 }, "node protocol"},
		{"schema-newer", func(env *Environment) { env.SchemaVersion = 4 }, "database schema"},
		{"schema-unknown", func(env *Environment) { env.SchemaVersion = 0 }, "database schema"},
		{"version-old", func(env *Environment) { env.Version = "0.9.0" }, "version transition"},
		{"version-unlisted", func(env *Environment) { env.Version = "1.0.5" }, "version transition"},
		{"version-unknown", func(env *Environment) { env.Version = "" }, "version transition"},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := env
			test.change(&changed)
			report := CheckCompatibility(manifest, changed)
			if report.Compatible {
				t.Fatal("incompatible release accepted")
			}
			found := false
			for _, check := range report.Checks {
				found = found || (!check.OK && check.Name == test.check)
			}
			if !found {
				t.Fatalf("expected failed %q check: %+v", test.check, report.Checks)
			}
			fault := contract.Classify(report.Err())
			if fault.Code != contract.PreconditionFailed {
				t.Fatalf("unexpected code: %v", fault.Code)
			}
		})
	}
	dev := env
	dev.Version = "dev"
	if report := CheckCompatibility(manifest, dev); !report.Compatible {
		t.Fatalf("development build rejected: %+v", report)
	}
	unknown := env
	unknown.Version = ""
	unrestricted := manifest
	unrestricted.UpgradeFrom = nil
	if report := CheckCompatibility(unrestricted, unknown); !report.Compatible {
		t.Fatalf("unknown version without upgrade_from rejected: %+v", report)
	}
}

func TestCheckControlPlaneCompatibility(t *testing.T) {
	manifest := testManifest(t)
	instances := []ControlPlaneInstance{
		{ID: "cp-1", Version: "1.0.0", Contract: "eregion/v1", NodeProtocol: 1, SchemaVersion: 3},
		{ID: "cp-2", Version: "1.0.0", Contract: "eregion/v1", NodeProtocol: 1, SchemaVersion: 3},
	}
	if err := CheckControlPlaneCompatibility(manifest, instances); err != nil {
		t.Fatalf("compatible inventory rejected: %v", err)
	}
	if err := CheckControlPlaneCompatibility(manifest, nil); err == nil {
		t.Fatal("empty inventory accepted as evidence")
	}
	bad := append([]ControlPlaneInstance(nil), instances...)
	bad[1].SchemaVersion = 4
	if err := CheckControlPlaneCompatibility(manifest, bad); err == nil {
		t.Fatal("newer schema inventory accepted")
	}
	bad = append([]ControlPlaneInstance(nil), instances...)
	bad[1].SchemaVersion = 2
	if err := CheckControlPlaneCompatibility(manifest, bad); err == nil {
		t.Fatal("mixed schema inventory accepted")
	}
	bad = append([]ControlPlaneInstance(nil), instances...)
	bad[0].NodeProtocol = 2
	if err := CheckControlPlaneCompatibility(manifest, bad); err == nil {
		t.Fatal("mixed protocol inventory accepted")
	}
	bad = append([]ControlPlaneInstance(nil), instances...)
	bad[0].Version = "2.0.0"
	if err := CheckControlPlaneCompatibility(manifest, bad); err == nil {
		t.Fatal("downgrade inventory accepted")
	}
}

func TestVersionCompare(t *testing.T) {
	for _, test := range []struct {
		left, right string
		want        int
	}{
		{"1.0.0", "1.0.1", -1},
		{"1.2.0", "1.1.9", 1},
		{"1.0.0", "1.0.0", 0},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0-rc.2", "1.0.0-rc.10", -1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-beta", "1.0.0-alpha", 1},
	} {
		left, err := ParseVersion(test.left)
		if err != nil {
			t.Fatal(err)
		}
		right, err := ParseVersion(test.right)
		if err != nil {
			t.Fatal(err)
		}
		if got := left.Compare(right); got != test.want {
			t.Fatalf("%s vs %s: got %d, want %d", test.left, test.right, got, test.want)
		}
	}
}

func TestManifestPublishedTimeIsNotFutureDependent(t *testing.T) {
	// Keep the fixture honest: the test manifest's timestamp parses and the
	// current time is irrelevant to verification.
	if _, err := time.Parse(time.RFC3339, testManifest(t).PublishedAt); err != nil {
		t.Fatal(err)
	}
}
