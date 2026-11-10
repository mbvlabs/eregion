package cli

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/rpc"
	"golang.org/x/crypto/ssh"
)

type workspaceApplyTransport struct {
	params rpc.Request
	run    int
}

func (*workspaceApplyTransport) Dial(context.Context) (*ssh.Client, error) {
	return nil, io.EOF
}
func (*workspaceApplyTransport) Upload(context.Context, string, []host.RemoteFile) error {
	return nil
}
func (*workspaceApplyTransport) Remove(context.Context, string) error { return nil }

func (t *workspaceApplyTransport) Run(_ context.Context, _ string, stdin io.Reader, stdout, _ io.Writer) error {
	request, _, err := rpc.DecodeRequest(stdin)
	if err != nil {
		return err
	}
	t.run++
	t.params = request
	raw, err := json.Marshal(host.ApplyReport{Node: "main", Role: host.RoleMain, OperationID: "op_test"})
	if err != nil {
		return err
	}
	line, err := rpc.MarshalResult(rpc.Result{OK: true, Data: raw})
	if err != nil {
		return err
	}
	_, err = stdout.Write(line)
	return err
}

func TestHostApplySendsNoWorkspaceOverride(t *testing.T) {
	transport := &workspaceApplyTransport{}
	client := &nodeClient{remote: transport, checked: true}
	report, _, err := client.HostApply(context.Background(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	if report.Node != "main" || transport.run != 1 {
		t.Fatalf("apply report = %+v, runs = %d", report, transport.run)
	}
	if transport.params.Method != methodHostApply {
		t.Fatalf("method = %q", transport.params.Method)
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(transport.params.Params, &params); err != nil {
		t.Fatal(err)
	}
	if len(params) != 0 {
		t.Fatalf("unexpected apply params = %s", transport.params.Params)
	}
}

func TestHostApplyRejectsWorkspaceOverride(t *testing.T) {
	_, err := dispatchHostMethod(context.Background(), rpc.Request{Method: methodHostApply, Params: json.RawMessage(`{"workspace":"thelab"}`)}, io.Discard, nil)
	if contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("workspace override error = %v, want validation_failed", err)
	}
}

func workspaceRoute(address string) *host.WorkspaceSSH {
	return &host.WorkspaceSSH{
		Address: address, User: "admin", KnownHosts: filepath.Join("/tmp", "kh-"+address),
	}
}

func TestApplyRecordsVerifiedWorkspaceInTheCapturedWorkspace(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "workspaces.json")
	t.Setenv("EREGION_WORKSPACE_FILE", configPath)
	store, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	prodRoute := workspaceRoute("10.78.0.1:22")
	otherRoute := workspaceRoute("10.78.0.2:22")
	if _, err := store.PublishWorkspace("ws_0123456789abcdef01234567", "thelab", host.WorkspaceAccess{Name: "prod", SSH: prodRoute}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishWorkspace("ws_fedcba9876543210fedcba98", "other", host.WorkspaceAccess{Name: "other", SSH: otherRoute}, false); err != nil {
		t.Fatal(err)
	}
	// The RPC ran with prod selected; a concurrent workspace-use moves the
	// current selection to other before metadata persistence.
	if _, err := store.UseWorkspace("other"); err != nil {
		t.Fatal(err)
	}
	workspace := controlplane.WorkspaceIdentity{Configured: true, ID: "ws_0123456789abcdef01234567", Name: "thelab"}
	if err := recordWorkspaceIdentity(nil, "prod", prodRoute, workspace); err != nil {
		t.Fatal(err)
	}
	reloaded, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := reloaded.AccessByName("prod")
	if !ok {
		t.Fatal("prod access missing")
	}
	if saved.Workspace == nil || saved.Workspace.ID != workspace.ID || saved.Workspace.Name != workspace.Name {
		t.Fatalf("workspace access identity = %+v", saved.Workspace)
	}
	other, ok := reloaded.AccessByName("other")
	if !ok {
		t.Fatal("other access missing")
	}
	if other.Workspace == nil || other.Workspace.ID != "ws_fedcba9876543210fedcba98" {
		t.Fatalf("unrelated workspace was overwritten: %+v", other.Workspace)
	}
	if reloaded.Current != "other" {
		t.Fatalf("current workspace = %q, want other", reloaded.Current)
	}

	// An unconfigured node must leave the explicit state alone.
	if err := recordWorkspaceIdentity(nil, "prod", prodRoute, controlplane.WorkspaceIdentity{}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyWorkspaceRefreshRefusesAReplacedSameNameRoute(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "workspaces.json")
	t.Setenv("EREGION_WORKSPACE_FILE", configPath)
	store, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	original := workspaceRoute("10.78.0.1:22")
	if _, err := store.PublishWorkspace("ws_0123456789abcdef01234567", "thelab", host.WorkspaceAccess{Name: "prod", SSH: original}, false); err != nil {
		t.Fatal(err)
	}
	// A concurrent login replaces the same private access name with another
	// route while the node RPC still holds the original route snapshot.
	replacement := workspaceRoute("10.78.0.9:22")
	for index := range store.Access {
		if store.Access[index].Name == "prod" {
			store.Access[index].SSH = replacement
		}
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	workspace := controlplane.WorkspaceIdentity{Configured: true, ID: "ws_0123456789abcdef01234567", Name: "thelab"}
	if err := recordWorkspaceIdentity(nil, "prod", original, workspace); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("replaced-route workspace refresh = %v, want conflict", err)
	}
	reloaded, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := reloaded.AccessByName("prod")
	if !ok {
		t.Fatal("prod access missing")
	}
	if err != nil {
		t.Fatal(err)
	}
	if saved.Workspace == nil || saved.Workspace.ID != workspace.ID || saved.SSH.Address != replacement.Address {
		t.Fatalf("replacement was overwritten: %+v", saved)
	}
}

func TestApplyWorkspaceRefreshRefusesToReplaceAConfiguredWorkspaceID(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "workspaces.json")
	t.Setenv("EREGION_WORKSPACE_FILE", configPath)
	store, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	route := workspaceRoute("10.78.0.1:22")
	if _, err := store.PublishWorkspace("ws_0123456789abcdef01234567", "thelab", host.WorkspaceAccess{Name: "prod", SSH: route}, false); err != nil {
		t.Fatal(err)
	}
	first := controlplane.WorkspaceIdentity{Configured: true, ID: "ws_0123456789abcdef01234567", Name: "thelab"}
	if err := recordWorkspaceIdentity(nil, "prod", route, first); err != nil {
		t.Fatal(err)
	}
	second := controlplane.WorkspaceIdentity{Configured: true, ID: "ws_fedcba9876543210fedcba98", Name: "thelab"}
	if err := recordWorkspaceIdentity(nil, "prod", route, second); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("configured workspace-ID replacement = %v, want conflict", err)
	}
	reloaded, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := reloaded.AccessByName("prod")
	if !ok {
		t.Fatal("prod access missing")
	}
	if err != nil {
		t.Fatal(err)
	}
	if saved.Workspace == nil || saved.Workspace.ID != first.ID {
		t.Fatalf("configured workspace ID was overwritten: %+v", saved.Workspace)
	}
}

func TestApplyCommandExposesWorkspaceWithoutANewConfirmationFlag(t *testing.T) {
	a := newApp()
	cmd, _, err := a.root.Find([]string{"apply"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Flags().Lookup("workspace") == nil {
		t.Fatal("apply has no --workspace flag")
	}
	if cmd.Flags().Lookup("confirm") != nil {
		t.Fatal("apply gained an unrelated confirmation flag; the existing apply semantics must be retained")
	}
}

func TestApplyRejectsRemovedWorkspaceAssignmentFlag(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "workspaces.json"))
	code, stdout, _ := invoke(t, "apply", "--assign-workspace", "thelab", "--json")
	if code != 2 {
		t.Fatalf("exit = %d, want usage exit 2", code)
	}
	if envelope := decodeOne(t, stdout); envelope.Error == nil || envelope.Error.Code != contract.Usage {
		t.Fatalf("error = %+v, want usage", envelope.Error)
	}
}
