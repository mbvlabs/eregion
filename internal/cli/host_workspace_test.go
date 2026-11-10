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

func TestHostApplySendsTheExplicitWorkspaceAssignment(t *testing.T) {
	transport := &workspaceApplyTransport{}
	client := &nodeClient{remote: transport, checked: true}
	report, _, err := client.HostApply(context.Background(), false, "", "thelab")
	if err != nil {
		t.Fatal(err)
	}
	if report.Node != "main" || transport.run != 1 {
		t.Fatalf("apply report = %+v, runs = %d", report, transport.run)
	}
	if transport.params.Method != methodHostApply {
		t.Fatalf("method = %q", transport.params.Method)
	}
	var params rpcHostApplyParams
	if err := json.Unmarshal(transport.params.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.Workspace != "thelab" {
		t.Fatalf("workspace param = %q", params.Workspace)
	}
}

func workspaceRoute(address string) *host.ContextSSH {
	return &host.ContextSSH{
		Address: address, User: "admin", KnownHosts: filepath.Join("/tmp", "kh-"+address),
	}
}

func TestApplyRecordsVerifiedWorkspaceInTheCapturedContext(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	store, err := host.LoadContextStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	prodRoute := workspaceRoute("10.78.0.1:22")
	otherRoute := workspaceRoute("10.78.0.2:22")
	for _, context := range []host.Context{{Name: "prod", SSH: prodRoute}, {Name: "other", SSH: otherRoute}} {
		if _, err := store.SaveContext(context); err != nil {
			t.Fatal(err)
		}
	}
	// The RPC ran with prod selected; a concurrent context-use moves the
	// current selection to other before metadata persistence.
	if _, err := store.Use("other"); err != nil {
		t.Fatal(err)
	}
	workspace := controlplane.WorkspaceIdentity{Configured: true, ID: "ws_0123456789abcdef01234567", Name: "thelab"}
	if err := recordContextWorkspace("prod", prodRoute, workspace); err != nil {
		t.Fatal(err)
	}
	reloaded, err := host.LoadContextStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reloaded.Get("prod")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Workspace == nil || saved.Workspace.ID != workspace.ID || saved.Workspace.Name != workspace.Name {
		t.Fatalf("context workspace = %+v", saved.Workspace)
	}
	other, err := reloaded.Get("other")
	if err != nil {
		t.Fatal(err)
	}
	if other.Workspace != nil {
		t.Fatalf("workspace metadata was written to the newly current context: %+v", other.Workspace)
	}
	if reloaded.Current != "other" {
		t.Fatalf("current context = %q, want other", reloaded.Current)
	}

	// An unconfigured node must leave the explicit state alone.
	if err := recordContextWorkspace("prod", prodRoute, controlplane.WorkspaceIdentity{}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyWorkspaceRefreshRefusesAReplacedSameNameRoute(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	store, err := host.LoadContextStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	original := workspaceRoute("10.78.0.1:22")
	if _, err := store.SaveContext(host.Context{Name: "prod", SSH: original}); err != nil {
		t.Fatal(err)
	}
	// A concurrent login replaces the same context name with another route
	// while the node RPC still holds the original route snapshot.
	replacement := workspaceRoute("10.78.0.9:22")
	if _, err := store.SaveContext(host.Context{Name: "prod", SSH: replacement}); err != nil {
		t.Fatal(err)
	}
	workspace := controlplane.WorkspaceIdentity{Configured: true, ID: "ws_0123456789abcdef01234567", Name: "thelab"}
	if err := recordContextWorkspace("prod", original, workspace); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("replaced-route workspace refresh = %v, want conflict", err)
	}
	reloaded, err := host.LoadContextStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reloaded.Get("prod")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Workspace != nil || saved.SSH.Address != replacement.Address {
		t.Fatalf("replacement was overwritten: %+v", saved)
	}
}

func TestApplyWorkspaceRefreshRefusesToReplaceAConfiguredWorkspaceID(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	store, err := host.LoadContextStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	route := workspaceRoute("10.78.0.1:22")
	if _, err := store.SaveContext(host.Context{Name: "prod", SSH: route}); err != nil {
		t.Fatal(err)
	}
	first := controlplane.WorkspaceIdentity{Configured: true, ID: "ws_0123456789abcdef01234567", Name: "thelab"}
	if err := recordContextWorkspace("prod", route, first); err != nil {
		t.Fatal(err)
	}
	second := controlplane.WorkspaceIdentity{Configured: true, ID: "ws_fedcba9876543210fedcba98", Name: "thelab"}
	if err := recordContextWorkspace("prod", route, second); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("configured workspace-ID replacement = %v, want conflict", err)
	}
	reloaded, err := host.LoadContextStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reloaded.Get("prod")
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
