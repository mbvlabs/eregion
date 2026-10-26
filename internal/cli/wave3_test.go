package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/host"
)

func TestBootstrapStatusNeedsNoApplyInputs(t *testing.T) {
	t.Setenv("EREGION_DATABASE_URL", "")
	code, stdout, stderr := invoke(t, "bootstrap", "status", "--role", "main", "--node", "main", "--json")
	if code != 0 {
		t.Fatalf("status: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	envelope := decodeOne(t, stdout)
	if !envelope.OK {
		t.Fatalf("status: %+v", envelope.Error)
	}
	data, _ := json.Marshal(envelope.Data)
	var status struct {
		Bootstrapped bool   `json:"bootstrapped"`
		Journal      string `json:"journal"`
	}
	if err := json.Unmarshal(data, &status); err != nil || status.Journal == "" {
		t.Fatalf("status output: %s %v", data, err)
	}
}

func TestRemoteBundleDirIsStablePerHostAndContent(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "eregion")
	if err := os.WriteFile(binary, []byte("cli-v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := host.DefaultConfig(host.RoleMain, "main")
	cfg.BinaryPath = binary

	first, err := remoteBundleDir(cfg, "203.0.113.10:22")
	if err != nil {
		t.Fatal(err)
	}
	second, err := remoteBundleDir(cfg, "203.0.113.10:22")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("remote bundle path is not stable: %q != %q", first, second)
	}
	if !strings.Contains(first, filepath.Join("bootstrap", "remote")) {
		t.Fatalf("bundle is not under the bootstrap state directory: %s", first)
	}
	if other, _ := remoteBundleDir(cfg, "203.0.113.11:22"); other == first {
		t.Fatal("different hosts share one bundle directory")
	}
	if err := os.WriteFile(binary, []byte("cli-v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if changed, _ := remoteBundleDir(cfg, "203.0.113.10:22"); changed == first {
		t.Fatal("a changed binary reused the old bundle directory")
	}

	// The uploaded argument paths must be identical across invocations: the
	// remote ConfigHash includes them.
	stable := t.TempDir()
	cfg.BinaryPath = binary
	files, args, err := remoteBundle(cfg, stable)
	if err != nil {
		t.Fatal(err)
	}
	files2, args2, err := remoteBundle(cfg, stable)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(args, "\x00") != strings.Join(args2, "\x00") || len(files) != len(files2) {
		t.Fatalf("remote bundle arguments changed:\n%v\n%v", args, args2)
	}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(stable, file.Name), file.Data, file.Mode); err != nil {
			t.Fatal(err)
		}
	}
	binaryArg := ""
	for i, arg := range args {
		if arg == "--binary" && i+1 < len(args) {
			binaryArg = args[i+1]
		}
	}
	if binaryArg == "" {
		t.Fatalf("remote bundle has no binary argument: %v", args)
	}
	remoteConfig := cfg
	remoteConfig.BinaryPath = binaryArg
	firstHash, err := remoteConfig.ConfigHash()
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := remoteConfig.ConfigHash()
	if err != nil {
		t.Fatal(err)
	}
	if firstHash != secondHash {
		t.Fatalf("remote config hash is not stable: %q != %q", firstHash, secondHash)
	}
}
