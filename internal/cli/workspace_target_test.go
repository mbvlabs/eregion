package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/spf13/cobra"
)

type workspaceTargetBackend struct {
	Backend
	info rpcNodeInfoResult
	err  error
}

func (b workspaceTargetBackend) NodeInfo(context.Context) (rpcNodeInfoResult, error) {
	return b.info, b.err
}

func workspaceTargetFixture(t *testing.T) *cobra.Command {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "contexts.json")
	t.Setenv("EREGION_CONTEXT_FILE", configPath)
	store, err := host.LoadContextStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveContext(host.Context{
		Name: "prod", SSH: &host.ContextSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: filepath.Join(t.TempDir(), "known_hosts")},
	}); err != nil {
		t.Fatal(err)
	}
	return &cobra.Command{}
}

func TestWorkspaceTargetUsesNodeIdentitySeparateFromRoute(t *testing.T) {
	cmd := workspaceTargetFixture(t)
	node, workspace, known, configured, contextName := workspaceTarget(context.Background(), cmd, workspaceTargetBackend{info: rpcNodeInfoResult{
		Node: "main", Hostname: "main.example.com", Platform: "linux/amd64",
		Workspace: controlplane.WorkspaceIdentity{Configured: true, ID: "ws_0123456789abcdef01234567", Name: "thelab"},
	}})
	if node != "main" || node == "admin@10.78.0.1" {
		t.Fatalf("node label = %q; want the actual node name, not the route", node)
	}
	if workspace != "thelab" || !known || !configured {
		t.Fatalf("workspace = %q known=%t configured=%t", workspace, known, configured)
	}
	if contextName != "prod" {
		t.Fatalf("context = %q", contextName)
	}
}

func TestWorkspaceTargetDistinguishesUnknownFromUnconfigured(t *testing.T) {
	cmd := workspaceTargetFixture(t)
	_, _, known, configured, _ := workspaceTarget(context.Background(), cmd, workspaceTargetBackend{
		info: rpcNodeInfoResult{Node: "main", WorkspaceError: "control database unavailable"},
	})
	if known || configured {
		t.Fatalf("failed read = known %t configured %t; want unknown", known, configured)
	}
	node, workspace, known, configured, _ := workspaceTarget(context.Background(), cmd, workspaceTargetBackend{
		info: rpcNodeInfoResult{Node: "main"},
	})
	if node != "main" || workspace != "" || !known || configured {
		t.Fatalf("unconfigured read = node %q workspace %q known %t configured %t", node, workspace, known, configured)
	}
}

func TestWorkspaceTargetFallsBackToUnknownOnNodeInfoFailure(t *testing.T) {
	cmd := workspaceTargetFixture(t)
	node, _, known, configured, contextName := workspaceTarget(context.Background(), cmd, workspaceTargetBackend{err: context.DeadlineExceeded})
	if node != "unknown" || known || configured || contextName != "prod" {
		t.Fatalf("NodeInfo failure = node %q known %t configured %t context %q", node, known, configured, contextName)
	}
}
