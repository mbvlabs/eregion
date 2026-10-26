package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/spf13/cobra"
)

func newSecretTestApp(t *testing.T) (*app, *cobra.Command) {
	t.Helper()
	a := &app{meta: make(registry), secrets: &secrets.Registry{}}
	root := &cobra.Command{Use: "eregion"}
	root.SilenceErrors, root.SilenceUsage = true, true
	root.Short = "Secret command test root"
	root.RunE = func(*cobra.Command, []string) error { return nil }
	a.meta[root] = readMeta(root.Short, []string{"Test secret commands"}, []string{"eregion secret list --app api --environment production"})
	for _, s := range a.secretSpecs() {
		a.attach(root, s)
	}
	return a, root
}

func writeSecretInput(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSecretCommandMetadataValidates(t *testing.T) {
	a, root := newSecretTestApp(t)
	if err := a.meta.validate(root); err != nil {
		t.Fatalf("secret command metadata invalid: %+v", contract.Classify(err))
	}
}

func TestSecretSetRejectsInsecureInputFile(t *testing.T) {
	_, root := newSecretTestApp(t)
	input := "value-should-not-leak"
	path := writeSecretInput(t, input)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	root.SetArgs([]string{"secret", "set", "--app", "api", "--environment", "production", "--name", "DATABASE_URL", "--file", path})
	err := root.Execute()
	if err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("insecure file error = %v, want precondition_failed", err)
	}
	if strings.Contains(err.Error(), input) {
		t.Fatalf("error leaked secret input: %v", err)
	}
}

func TestCLIProtectedFileHelpersRejectUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProtectedSecretFile(regular); err != nil {
		t.Fatalf("protected file rejected: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readProtectedSecretFile(link); err == nil {
		t.Fatal("symlink input accepted")
	}
	if _, err := readProtectedSecretFile("relative"); err == nil {
		t.Fatal("relative input accepted")
	}
	if err := writeProtectedSecretFile(regular, []byte("new")); err == nil {
		t.Fatal("existing destination overwritten")
	}
	dest := filepath.Join(dir, "dest")
	if err := writeProtectedSecretFile(dest, []byte("written")); err != nil {
		t.Fatalf("writeProtectedSecretFile: %v", err)
	}
	raw, err := os.ReadFile(dest)
	if err != nil || string(raw) != "written" {
		t.Fatalf("destination content = %q, %v", raw, err)
	}
}
