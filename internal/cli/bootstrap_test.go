package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/host"
)

func TestBootstrapAndContextAreAttached(t *testing.T) {
	for _, path := range [][]string{{"node"}, {"node", "bootstrap"}, {"node", "bootstrap", "plan"}, {"node", "bootstrap", "apply"}, {"node", "bootstrap", "confirm"}, {"node", "bootstrap", "remote"}, {"context"}} {
		code, stdout, stderr := invoke(t, append([]string{"commands", "--path", strings.Join(path, " ")}, "--json")...)
		if code != 0 {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", path, code, stdout, stderr)
		}
		envelope := decodeOne(t, stdout)
		if !envelope.OK {
			t.Fatalf("%v: %+v", path, envelope.Error)
		}
	}
}

func TestContextClientRequiresSavedContext(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	if code, _, _ := invoke(t, "operations", "get", string(followTestID), "--json"); code != 3 {
		t.Fatalf("missing context: code=%d", code)
	}
}

func TestRemoteBundleNeverPassesSecretsInArguments(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "eregion")
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := host.DefaultConfig(host.RoleMain, "main")
	cfg.BinaryPath = binary
	cfg.DatabaseURL = "postgres://u:private-password@127.0.0.1/test?sslmode=disable"
	files, args, err := remoteBundle(cfg, "/var/lib/eregion/bootstrap/remote/test")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range files {
		if file.Name == "database-url" {
			found = true
			if file.Mode.Perm() != 0o600 {
				t.Fatalf("database file mode %o", file.Mode.Perm())
			}
		}
	}
	if !found {
		t.Fatal("database URL file was not uploaded")
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "private-password") || strings.Contains(joined, cfg.DatabaseURL) {
		t.Fatalf("secret in remote arguments: %s", joined)
	}
	command := remoteCommand("/var/lib/eregion/bootstrap/remote/test", "plan", args, nil)
	if strings.Contains(command, "private-password") {
		t.Fatal("secret in remote command")
	}
	if !strings.Contains(command, "'--database-url-file'") {
		t.Fatalf("database flag missing: %s", command)
	}
}
