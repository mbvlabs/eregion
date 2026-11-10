package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/engine"
)

const testWorkspaceID = "ws_0123456789abcdef01234567"

type fakeWorkspaceDatabase struct {
	identity controlplane.WorkspaceIdentity
	reads    int
	assigns  int
}

func (f *fakeWorkspaceDatabase) Inspect(context.Context, string) (DatabaseState, error) {
	return DatabaseState{}, nil
}
func (f *fakeWorkspaceDatabase) Migrate(context.Context, string) (DatabaseState, error) {
	return DatabaseState{}, nil
}
func (f *fakeWorkspaceDatabase) ReadWorkspace(context.Context, string) (controlplane.WorkspaceIdentity, error) {
	f.reads++
	return f.identity, nil
}
func (f *fakeWorkspaceDatabase) AssignWorkspace(_ context.Context, _, name string) (controlplane.WorkspaceIdentity, error) {
	f.assigns++
	if f.identity.Configured {
		if f.identity.Name != name {
			return controlplane.WorkspaceIdentity{}, contract.NewFault(contract.Conflict, "already configured", "Use the existing name.")
		}
		return f.identity, nil
	}
	f.identity = controlplane.WorkspaceIdentity{Configured: true, ID: testWorkspaceID, Name: name}
	return f.identity, nil
}

func TestWorkspaceStepAssignsOnceMirrorsAndPreservesNode(t *testing.T) {
	cfg := fixture(t, RoleMain)
	cfg.Workspace = "thelab"
	configPath := filepath.Join(cfg.Layout.ConfigDir, "node.json")
	put(t, configPath, `{"node":"custom-node","state_dir":"/var/lib/custom","privileged_helper":{"keep":true}}`, 0o600)

	database := &fakeWorkspaceDatabase{}
	step := &workspaceStep{cfg: cfg, database: database}

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if check.Complete {
		t.Fatal("an unconfigured workspace was reported complete")
	}
	if database.reads != 1 || database.assigns != 0 {
		t.Fatalf("read-only check reads=%d assigns=%d", database.reads, database.assigns)
	}
	afterCheck, err := os.ReadFile(configPath)
	if err != nil || string(afterCheck) != string(before) {
		t.Fatalf("a read-only plan changed node.json: %v", err)
	}

	result, err := step.Apply(context.Background(), engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if database.assigns != 1 || result.Result.(map[string]any)["workspace_id"] != testWorkspaceID {
		t.Fatalf("apply result: %+v (assigns %d)", result.Result, database.assigns)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	if rawNodeString(values["node"]) != "custom-node" || rawNodeString(values["workspace_id"]) != testWorkspaceID ||
		rawNodeString(values["workspace_name"]) != "thelab" {
		t.Fatalf("node configuration mirror = %s", raw)
	}
	if !strings.Contains(string(raw), `"privileged_helper"`) || !strings.Contains(string(raw), `"keep"`) {
		t.Fatalf("workspace mirror erased an unknown node setting: %s", raw)
	}
	if info, err := os.Stat(configPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("node configuration mode = %v, %v", info, err)
	}

	check, err = step.Check(context.Background(), engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if !check.Complete {
		t.Fatalf("a matching workspace mirror was not complete: %+v", check)
	}

	// Same-name replay reads the existing identity and never generates a new
	// one; the fake returns the stored ID, matching the CAS contract.
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), testWorkspaceID) {
		t.Fatalf("same-name replay changed the mirror: %s", raw)
	}
}

func TestWorkspaceStepRefusesAConflictingName(t *testing.T) {
	cfg := fixture(t, RoleMain)
	cfg.Workspace = "requested"
	step := &workspaceStep{cfg: cfg, database: &fakeWorkspaceDatabase{
		identity: controlplane.WorkspaceIdentity{Configured: true, ID: testWorkspaceID, Name: "existing"},
	}}
	check, err := step.Check(context.Background(), engine.Runtime{})
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("check conflict = %v (result %+v), want conflict", err, check)
	}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("apply conflict = %v, want conflict", err)
	}
}

func TestWorkspaceStepReportsAMissingOrStaleMirror(t *testing.T) {
	cfg := fixture(t, RoleMain)
	cfg.Workspace = "thelab"
	database := &fakeWorkspaceDatabase{
		identity: controlplane.WorkspaceIdentity{Configured: true, ID: testWorkspaceID, Name: "thelab"},
	}
	step := &workspaceStep{cfg: cfg, database: database}
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || check.Complete {
		t.Fatalf("missing mirror = %+v, %v; want incomplete", check, err)
	}
	put(t, filepath.Join(cfg.Layout.ConfigDir, "node.json"), `{"workspace_id":"ws_aaaaaaaaaaaaaaaaaaaaaaaa","workspace_name":"thelab"}`, 0o600)
	check, err = step.Check(context.Background(), engine.Runtime{})
	if err != nil || check.Complete {
		t.Fatalf("stale mirror = %+v, %v; want incomplete", check, err)
	}
}

func TestWorkspaceStepRunsAfterMigrationsAndBeforeVaultAndServices(t *testing.T) {
	cfg := fixture(t, RoleMain)
	cfg.Workspace = "thelab"
	steps, err := (&Runner{Config: cfg}).steps(Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	positions := map[string]int{}
	for index, step := range steps {
		positions[step.ID()] = index
	}
	database, hasDatabase := positions["host-database-v1"]
	workspace, hasWorkspace := positions["host-workspace-v1"]
	vault, hasVault := positions["host-vault-keyring-v1"]
	if !hasDatabase || !hasWorkspace || !hasVault {
		t.Fatalf("missing steps: %v", positions)
	}
	if !(database < workspace && workspace < vault) {
		t.Fatalf("workspace step order database=%d workspace=%d vault=%d", database, workspace, vault)
	}
}

func TestRecordedIntentCarriesTheWorkspaceRequestButNoIdentityRow(t *testing.T) {
	cfg := fixture(t, RoleMain)
	cfg.Workspace = "thelab"
	if err := saveIntent(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadIntent(cfg.Layout)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace != "thelab" {
		t.Fatalf("recorded workspace = %q", loaded.Workspace)
	}
	loaded.DatabaseURL = cfg.DatabaseURL
	if err := loaded.Validate(); err != nil {
		t.Fatalf("recorded workspace intent does not validate: %v", err)
	}
}

func TestContextStorePersistsAndDisplaysWorkspaceMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.json")
	store, err := LoadContextStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveContext(Context{
		Name: "prod", SSH: &ContextSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: filepath.Join(t.TempDir(), "known_hosts")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveContext(Context{
		Name: "other", SSH: &ContextSSH{Address: "10.78.0.2:22", User: "admin", KnownHosts: filepath.Join(t.TempDir(), "known_hosts")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetWorkspace("prod", &ContextWorkspace{ID: testWorkspaceID, Name: "thelab"}); err != nil {
		t.Fatal(err)
	}
	if store.Current != "other" {
		t.Fatalf("SetWorkspace changed the current context to %q", store.Current)
	}
	loaded, err := LoadContextStore(path)
	if err != nil {
		t.Fatal(err)
	}
	view, err := loaded.Show("prod")
	if err != nil {
		t.Fatal(err)
	}
	if view.Workspace == nil || view.Workspace.ID != testWorkspaceID || view.Workspace.Name != "thelab" {
		t.Fatalf("context view workspace = %+v", view.Workspace)
	}
	// An invalid ID is refused on load rather than silently trusted.
	raw := `{"schema":2,"contexts":[{"name":"prod","workspace":{"id":"ws_short","name":"thelab"},"ssh":{"address":"10.78.0.1:22","user":"admin","known_hosts":"/tmp/kh"}}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadContextStore(path); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("invalid context workspace = %v", err)
	}
}

func TestContextStoreMutationReloadsLatestStateUnderTheSharedLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.json")
	first, err := LoadContextStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.SaveContext(Context{
		Name: "alpha", SSH: &ContextSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: filepath.Join(t.TempDir(), "alpha-known_hosts")},
	}); err != nil {
		t.Fatal(err)
	}
	// A second process loads before alpha's workspace update, adds another
	// context, and selects it.
	second, err := LoadContextStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.SaveContext(Context{
		Name: "beta", SSH: &ContextSSH{Address: "10.78.0.2:22", User: "admin", KnownHosts: filepath.Join(t.TempDir(), "beta-known_hosts")},
	}); err != nil {
		t.Fatal(err)
	}
	// The stale first store must reload beta and preserve beta as current while
	// updating only alpha's workspace metadata.
	if _, err := first.SetWorkspace("alpha", &ContextWorkspace{ID: testWorkspaceID, Name: "thelab"}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadContextStore(path)
	if err != nil {
		t.Fatal(err)
	}
	alpha, err := reloaded.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := reloaded.Get("beta")
	if err != nil {
		t.Fatalf("concurrent context was lost: %v", err)
	}
	if alpha.Workspace == nil || alpha.Workspace.ID != testWorkspaceID || beta.Name != "beta" || reloaded.Current != "beta" {
		t.Fatalf("locked mutation lost state: alpha=%+v beta=%+v current=%q", alpha.Workspace, beta.Name, reloaded.Current)
	}
}

func TestContextStoreAllowsAliasesButRefusesWorkspaceNameCollisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.json")
	store, err := LoadContextStore(path)
	if err != nil {
		t.Fatal(err)
	}
	save := func(name, address string, workspace *ContextWorkspace) error {
		_, err := store.SaveContext(Context{
			Name: name, SSH: &ContextSSH{Address: address, User: "admin", KnownHosts: filepath.Join(t.TempDir(), "known_hosts")},
			Workspace: workspace,
		})
		return err
	}
	alpha := &ContextWorkspace{ID: testWorkspaceID, Name: "thelab"}
	if err := save("alpha", "10.78.0.1:22", alpha); err != nil {
		t.Fatal(err)
	}
	// The same workspace ID under another local context name is a valid alias.
	if err := save("beta", "10.78.0.1:22", &ContextWorkspace{ID: testWorkspaceID, Name: "thelab"}); err != nil {
		t.Fatalf("same-ID alias: %v", err)
	}
	// The same human name with another generated ID is one workspace, not two.
	err = save("gamma", "10.78.0.1:22", &ContextWorkspace{ID: "ws_fedcba9876543210fedcba98", Name: "thelab"})
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("cross-context workspace-name collision = %v, want conflict", err)
	}
}

func TestWorkspaceConfigIsValidatedAndChangesTheConfigHash(t *testing.T) {
	cfg := fixture(t, RoleMain)
	without, err := cfg.ConfigHash()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspace = "thelab"
	with, err := cfg.ConfigHash()
	if err != nil {
		t.Fatal(err)
	}
	if without == with {
		t.Fatal("adding a workspace did not change the host config hash")
	}
	cfg.Workspace = "Thelab"
	if err := cfg.Validate(); err == nil {
		t.Fatal("an invalid workspace name was accepted")
	}
	application := fixture(t, RoleApplication)
	application.Workspace = "thelab"
	if err := application.Validate(); err == nil {
		t.Fatal("a non-control-plane role accepted a workspace identity")
	}
}
