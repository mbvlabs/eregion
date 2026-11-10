package host

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestWorkspaceStorePersistsAndDisplaysWorkspaceMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.json")
	store, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishUnconfiguredAccess(WorkspaceAccess{
		Name: "prod", SSH: &WorkspaceSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: filepath.Join(t.TempDir(), "known_hosts")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishUnconfiguredAccess(WorkspaceAccess{
		Name: "other", SSH: &WorkspaceSSH{Address: "10.78.0.2:22", User: "admin", KnownHosts: filepath.Join(t.TempDir(), "known_hosts")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetWorkspaceIdentity("prod", &WorkspaceIdentity{ID: testWorkspaceID, Name: "thelab"}, nil); err != nil {
		t.Fatal(err)
	}
	if store.Current != "other" {
		t.Fatalf("SetWorkspace changed the current context to %q", store.Current)
	}
	loaded, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	view, err := loaded.ShowWorkspace(testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != testWorkspaceID || view.Name != "thelab" {
		t.Fatalf("workspace view = %+v", view)
	}
	// An invalid ID is refused on load rather than silently trusted.
	raw := `{"schema":2,"contexts":[{"name":"prod","workspace":{"id":"ws_short","name":"thelab"},"ssh":{"address":"10.78.0.1:22","user":"admin","known_hosts":"/tmp/kh"}}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkspaceStore(path); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("invalid context workspace = %v", err)
	}
}

func TestWorkspaceStoreMutationReloadsLatestStateUnderTheSharedLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.json")
	first, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.PublishUnconfiguredAccess(WorkspaceAccess{
		Name: "alpha", SSH: &WorkspaceSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: filepath.Join(t.TempDir(), "alpha-known_hosts")},
	}); err != nil {
		t.Fatal(err)
	}
	// A second process loads before alpha's workspace update, adds another
	// context, and selects it.
	second, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.PublishUnconfiguredAccess(WorkspaceAccess{
		Name: "beta", SSH: &WorkspaceSSH{Address: "10.78.0.2:22", User: "admin", KnownHosts: filepath.Join(t.TempDir(), "beta-known_hosts")},
	}); err != nil {
		t.Fatal(err)
	}
	// The stale first store must reload beta and preserve beta as current while
	// updating only alpha's workspace metadata.
	if _, err := first.SetWorkspaceIdentity("alpha", &WorkspaceIdentity{ID: testWorkspaceID, Name: "thelab"}, nil); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	alpha, ok := reloaded.AccessByName("alpha")
	if !ok {
		t.Fatal("alpha access missing")
	}
	beta, ok := reloaded.AccessByName("beta")
	if !ok {
		t.Fatal("concurrent access was lost")
	}
	if alpha.Workspace == nil || alpha.Workspace.ID != testWorkspaceID || beta.Name != "beta" || reloaded.Current != "beta" {
		t.Fatalf("locked mutation lost state: alpha=%+v beta=%+v current=%q", alpha.Workspace, beta.Name, reloaded.Current)
	}
}

func TestWorkspaceStoreGroupsDuplicateAccessAndRefusesNameConflicts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.json")
	store, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	route := &WorkspaceSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: filepath.Join(t.TempDir(), "known_hosts")}
	if _, err := store.PublishWorkspace(testWorkspaceID, "thelab", WorkspaceAccess{Name: "alpha", SSH: route}, false); err != nil {
		t.Fatal(err)
	}
	// A historical second access record for the same ID is one workspace, not
	// another target. It makes the group ambiguous unless its record is current.
	store.Access = append(store.Access, WorkspaceAccess{
		Name: "beta", SSH: route, Workspace: &WorkspaceIdentity{ID: testWorkspaceID, Name: "thelab"},
	})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	views := reloaded.ListWorkspaces()
	if len(views) != 1 || views[0].ID != testWorkspaceID || !views[0].Current {
		t.Fatalf("one workspace per ID = %+v", views)
	}
	selected, group, err := reloaded.ResolveWorkspace("thelab")
	if err != nil {
		t.Fatal(err)
	}
	if selected.Name != "alpha" || len(group.Access) != 2 {
		t.Fatalf("selected record = %+v group = %+v", selected, group)
	}
	// Same name with another ID is a hard conflict.
	otherID := "ws_fedcba9876543210fedcba98"
	other := &WorkspaceStore{Schema: workspaceStoreSchema, Path: path, Now: time.Now}
	other.Access = append(append([]WorkspaceAccess(nil), reloaded.Access...), WorkspaceAccess{
		Name: "gamma", SSH: route, Workspace: &WorkspaceIdentity{ID: otherID, Name: "thelab"},
	})
	other.Current = "gamma"
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := other.ResolveWorkspace("thelab"); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("workspace-name collision = %v, want conflict", err)
	}
	if _, err := other.ShowWorkspace(testWorkspaceID); err != nil {
		t.Fatalf("exact ID must address a name conflict: %v", err)
	}
}

func TestWorkspaceGroupWithDifferentNamesIsConflictButIDIsAddressable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.json")
	store, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	route := &WorkspaceSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: filepath.Join(t.TempDir(), "known_hosts")}
	store.Access = []WorkspaceAccess{
		{Name: "alpha", SSH: route, Workspace: &WorkspaceIdentity{ID: testWorkspaceID, Name: "thelab"}},
		{Name: "beta", SSH: route, Workspace: &WorkspaceIdentity{ID: testWorkspaceID, Name: "other-name"}},
	}
	store.Current = "alpha"
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	views := reloaded.ListWorkspaces()
	if len(views) != 1 || views[0].ID != testWorkspaceID || views[0].Conflict == "" || views[0].Name != "" {
		t.Fatalf("same-ID/different-name view = %+v", views)
	}
	if _, _, err := reloaded.ResolveWorkspace(testWorkspaceID); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("conflicting workspace resolved: %v", err)
	}
	if _, err := reloaded.ShowWorkspace(testWorkspaceID); err != nil {
		t.Fatalf("exact ID did not address name conflict: %v", err)
	}
	if _, removed, err := reloaded.RemoveWorkspace(testWorkspaceID); err != nil || len(removed) != 2 {
		t.Fatalf("remove by exact ID = %v, removed %d", err, len(removed))
	}
}

func TestCredentialDirNamePropagatesNonENOENTErrors(t *testing.T) {
	root := t.TempDir()
	parentFile := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &WorkspaceStore{Path: filepath.Join(parentFile, "contexts.json")}
	if _, err := store.CredentialDirName(testWorkspaceID); err == nil {
		t.Fatal("a non-directory parent was accepted")
	}

	if os.Geteuid() != 0 {
		locked := filepath.Join(root, "locked")
		if err := os.Mkdir(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		store := &WorkspaceStore{Path: filepath.Join(locked, "contexts.json")}
		if _, err := store.CredentialDirName(testWorkspaceID); err == nil {
			t.Fatal("a permission error was treated as a free directory")
		}
	}
}

func TestInitializationAccessSelectionIsPrivateAndRouteChecked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contexts.json")
	store, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	route := &WorkspaceSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: "/tmp/kh"}
	if _, err := store.PublishUnconfiguredAccess(WorkspaceAccess{Name: "historical-private", SSH: route}); err != nil {
		t.Fatal(err)
	}
	store.Current = ""
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ResolveWorkspace("historical-private"); err == nil {
		t.Fatal("an unconfigured private name resolved as a public workspace")
	}
	if err := store.SelectInitializationAccess("historical-private", route); err != nil {
		t.Fatalf("initialization selection: %v", err)
	}
	if store.Current != "historical-private" {
		t.Fatalf("initialization current = %q", store.Current)
	}
	if _, _, err := store.ResolveWorkspace(""); err == nil {
		t.Fatal("unconfigured access became a normal workspace selector")
	}
	if err := store.SelectInitializationAccess("historical-private", &WorkspaceSSH{Address: "10.78.0.9:22", User: "admin", KnownHosts: "/tmp/kh"}); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("changed initialization route = %v, want conflict", err)
	}
}

func TestWorkspaceRemovalCleanupRetainsSharedNestedAndSymlinkedDirs(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "contexts.json")
	store, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(root, "servers", "shared")
	unique := filepath.Join(root, "servers", "unique")
	for _, dir := range []string{shared, unique, filepath.Join(root, "servers", "nested")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(shared, "known_hosts"), []byte("pin"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unique, "known_hosts"), []byte("pin"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.Access = []WorkspaceAccess{
		{Name: "remove-shared", CredentialDir: filepath.Join("servers", "shared"), Workspace: &WorkspaceIdentity{ID: testWorkspaceID, Name: "remove-shared"}},
		{Name: "keep", CredentialDir: filepath.Join("servers", "other"), SSH: &WorkspaceSSH{KnownHosts: filepath.Join(shared, "known_hosts"), Identity: "/dev/null", Address: "10.78.0.1:22", User: "admin"}, Workspace: &WorkspaceIdentity{ID: "ws_fedcba9876543210fedcba98", Name: "keep"}},
	}
	store.Current = "remove-shared"
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, retained, err := reloaded.RemoveWorkspaceWithCleanup("remove-shared", nil); err != nil || len(retained) != 1 {
		t.Fatalf("shared cleanup = retained %v err %v", retained, err)
	}
	if _, err := os.Stat(shared); err != nil {
		t.Fatalf("shared credential directory was deleted: %v", err)
	}
	if _, ok := reloaded.AccessByName("keep"); !ok {
		t.Fatal("unrelated access was deleted")
	}

	// A unique unshared directory is removable.
	store.Access = []WorkspaceAccess{{Name: "unique", CredentialDir: filepath.Join("servers", "unique"), Workspace: &WorkspaceIdentity{ID: testWorkspaceID, Name: "unique"}}}
	store.Current = "unique"
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err = LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, retained, err := reloaded.RemoveWorkspaceWithCleanup("unique", nil); err != nil || len(retained) != 0 {
		t.Fatalf("unique cleanup = retained %v err %v", retained, err)
	}
	if _, err := os.Stat(unique); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unique credential directory still exists: %v", err)
	}
}

func TestWorkspaceRemovalCleanupRechecksUnrelatedConcurrentPublication(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "contexts.json")
	first, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "servers", "candidate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	first.Access = []WorkspaceAccess{{Name: "candidate", CredentialDir: filepath.Join("servers", "candidate"), Workspace: &WorkspaceIdentity{ID: testWorkspaceID, Name: "candidate"}}}
	first.Current = "candidate"
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	second, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	second.Access = append(second.Access, WorkspaceAccess{
		Name: "concurrent", CredentialDir: filepath.Join("servers", "other"),
		SSH:       &WorkspaceSSH{KnownHosts: filepath.Join(dir, "known_hosts"), Identity: "/dev/null", Address: "10.78.0.1:22", User: "admin"},
		Workspace: &WorkspaceIdentity{ID: "ws_fedcba9876543210fedcba98", Name: "concurrent"},
	})
	if err := second.Save(); err != nil {
		t.Fatal(err)
	}
	// first still has a stale pre-publication snapshot; removal must reload and
	// retain the directory referenced by second's new record.
	if _, _, retained, err := first.RemoveWorkspaceWithCleanup("candidate", nil); err != nil || len(retained) != 1 {
		t.Fatalf("concurrent cleanup = retained %v err %v", retained, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("concurrently published credential directory was deleted")
	}
}

func TestWorkspaceRemovalCleanupRetainsNestedAndSymlinkAncestors(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "contexts.json")
	store, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "servers", "parent")
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	store.Access = []WorkspaceAccess{
		{Name: "parent", CredentialDir: filepath.Join("servers", "parent"), Workspace: &WorkspaceIdentity{ID: testWorkspaceID, Name: "parent"}},
		{Name: "child", CredentialDir: filepath.Join("servers", "parent", "child"), Workspace: &WorkspaceIdentity{ID: "ws_fedcba9876543210fedcba98", Name: "child"}},
	}
	store.Current = "parent"
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, retained, err := reloaded.RemoveWorkspaceWithCleanup("parent", nil); err != nil || len(retained) != 1 {
		t.Fatalf("nested cleanup = retained %v err %v", retained, err)
	}
	if _, err := os.Stat(parent); err != nil {
		t.Fatal("nested credential parent was deleted")
	}

	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "servers", "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	store.Access = []WorkspaceAccess{{Name: "link", CredentialDir: filepath.Join("servers", "link"), Workspace: &WorkspaceIdentity{ID: testWorkspaceID, Name: "link"}}}
	store.Current = "link"
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err = LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, retained, err := reloaded.RemoveWorkspaceWithCleanup("link", nil); err != nil || len(retained) != 1 {
		t.Fatalf("symlink cleanup = retained %v err %v", retained, err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("symlink target was deleted")
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
