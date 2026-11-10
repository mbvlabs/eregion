package cli

import (
	"context"
	"testing"

	"github.com/mbvlabs/eregion/internal/controlplane"
)

type workspaceTargetBackend struct {
	Backend
	info rpcNodeInfoResult
	err  error
}

func (b workspaceTargetBackend) NodeInfo(context.Context) (rpcNodeInfoResult, error) {
	return b.info, b.err
}

func TestWorkspaceTargetUsesNodeIdentitySeparateFromRoute(t *testing.T) {
	node, workspace, known, configured := workspaceTarget(context.Background(), workspaceTargetBackend{info: rpcNodeInfoResult{
		Node: "main", Hostname: "main.example.com", Platform: "linux/amd64",
		Workspace: controlplane.WorkspaceIdentity{Configured: true, ID: "ws_0123456789abcdef01234567", Name: "thelab"},
	}})
	if node != "main" || node == "admin@10.78.0.1" {
		t.Fatalf("node label = %q; want the actual node name, not the route", node)
	}
	if workspace != "thelab" || !known || !configured {
		t.Fatalf("workspace = %q known=%t configured=%t", workspace, known, configured)
	}
}

func TestWorkspaceTargetDistinguishesUnknownFromUnconfigured(t *testing.T) {
	_, _, known, configured := workspaceTarget(context.Background(), workspaceTargetBackend{
		info: rpcNodeInfoResult{Node: "main", WorkspaceError: "control database unavailable"},
	})
	if known || configured {
		t.Fatalf("failed read = known %t configured %t; want unknown", known, configured)
	}
	node, workspace, known, configured := workspaceTarget(context.Background(), workspaceTargetBackend{
		info: rpcNodeInfoResult{Node: "main"},
	})
	if node != "main" || workspace != "" || !known || configured {
		t.Fatalf("unconfigured read = node %q workspace %q known %t configured %t", node, workspace, known, configured)
	}
}

func TestWorkspaceTargetFallsBackToUnknownOnNodeInfoFailure(t *testing.T) {
	node, _, known, configured := workspaceTarget(context.Background(), workspaceTargetBackend{err: context.DeadlineExceeded})
	if node != "unknown" || known || configured {
		t.Fatalf("NodeInfo failure = node %q known %t configured %t", node, known, configured)
	}
}
