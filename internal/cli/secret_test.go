package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
)

type cliFakeVault struct {
	mu    sync.Mutex
	items map[string]cliFakeItem
	sets  int
}

type cliFakeItem struct {
	metadata vault.Metadata
	value    string
}

func newCLIFakeVault() *cliFakeVault {
	return &cliFakeVault{items: make(map[string]cliFakeItem)}
}

func (f *cliFakeVault) Set(_ context.Context, namespace vault.Namespace, value []byte, expected uint64, actor contract.Actor) (vault.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	now := time.Now().UTC()
	item, exists := f.items[namespace.Key()]
	latest := uint64(0)
	if exists {
		latest = item.metadata.LatestRevision
	}
	if (!exists && expected != 0) || (exists && (expected == 0 || expected != latest)) {
		return vault.Metadata{}, contract.NewFault(contract.Conflict, "secret revision does not match", "Read metadata.")
	}
	meta := vault.Metadata{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
		LatestRevision: latest + 1, State: vault.StateActive,
		Revision:  vault.RevisionRef{Revision: latest + 1, Hash: strings.Repeat("a", 64), KeyID: "k_test", CreatedAt: now},
		CreatedBy: actor, UpdatedBy: actor, CreatedAt: now, UpdatedAt: now,
	}
	if exists {
		meta.CreatedAt, meta.CreatedBy = item.metadata.CreatedAt, item.metadata.CreatedBy
	}
	f.items[namespace.Key()] = cliFakeItem{metadata: meta, value: string(value)}
	return meta, nil
}

func (f *cliFakeVault) Archive(_ context.Context, namespace vault.Namespace, expected uint64, actor contract.Actor) (vault.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.items[namespace.Key()]
	if !ok {
		return vault.Metadata{}, contract.NewFault(contract.NotFound, "secret was not found", "List metadata.")
	}
	if expected != item.metadata.LatestRevision {
		return vault.Metadata{}, contract.NewFault(contract.Conflict, "secret revision does not match", "Read metadata.")
	}
	now := time.Now().UTC()
	item.metadata.State = vault.StateArchived
	item.metadata.ArchivedAt = &now
	item.metadata.UpdatedAt = now
	item.metadata.UpdatedBy = actor
	f.items[namespace.Key()] = item
	return item.metadata, nil
}

func (f *cliFakeVault) Get(_ context.Context, namespace vault.Namespace) (vault.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.items[namespace.Key()]
	if !ok {
		return vault.Metadata{}, contract.NewFault(contract.NotFound, "secret was not found", "List metadata.")
	}
	return item.metadata, nil
}

func (f *cliFakeVault) List(_ context.Context, app, environment string, limit int) ([]vault.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]vault.Metadata, 0)
	for _, item := range f.items {
		if item.metadata.App == app && item.metadata.Environment == environment {
			out = append(out, item.metadata)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *cliFakeVault) Export(_ context.Context, namespace vault.Namespace) (vault.Metadata, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.items[namespace.Key()]
	if !ok {
		return vault.Metadata{}, nil, contract.NewFault(contract.NotFound, "secret was not found", "List metadata.")
	}
	if item.metadata.State != vault.StateActive {
		return vault.Metadata{}, nil, contract.NewFault(contract.PreconditionFailed, "secret is archived", "Rotate the secret.")
	}
	return item.metadata, []byte(item.value), nil
}

func newSecretTestApp(t *testing.T, service *cliFakeVault) (*app, *cobra.Command, *httptest.Server) {
	t.Helper()
	handler := api.NewVaultHandler(service, func(_ *http.Request, _ api.VaultScope) (api.VaultAccess, error) {
		return api.NewVaultAccess(contract.Actor{Kind: "operator", Subject: "alice"}, func(string, string) error { return nil })
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("EREGION_API_URL", server.URL)
	t.Setenv("EREGION_API_TOKEN", strings.Repeat("a", 32))

	a := &app{meta: make(registry), secrets: &secrets.Registry{}}
	root := &cobra.Command{Use: "eregion"}
	root.SilenceErrors, root.SilenceUsage = true, true
	root.Short = "Secret command test root"
	root.RunE = func(*cobra.Command, []string) error { return nil }
	a.meta[root] = readMeta(root.Short, []string{"Test secret commands"}, []string{"eregion secret list --app api --environment production"})
	for _, s := range a.secretSpecs() {
		a.attach(root, s)
	}
	return a, root, server
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
	service := newCLIFakeVault()
	a, root, _ := newSecretTestApp(t, service)
	if err := a.meta.validate(root); err != nil {
		t.Fatalf("secret command metadata invalid: %+v", contract.Classify(err))
	}
}

func TestSecretSetRejectsInsecureInputFile(t *testing.T) {
	service := newCLIFakeVault()
	_, root, _ := newSecretTestApp(t, service)
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
	if service.sets != 0 {
		t.Fatal("insecure input reached the API")
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
