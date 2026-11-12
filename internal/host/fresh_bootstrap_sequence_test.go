package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/engine"
)

func TestFreshBootstrapWritesNodeConfigBeforeWorkerStart(t *testing.T) {
	cfg := fixture(t, RoleMain)
	cfg.Workspace = "thelab"
	if err := os.MkdirAll(cfg.Layout.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(cfg.Layout.ConfigDir, "node.json")
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("fresh fixture unexpectedly has node.json: %v", err)
	}
	hostIdentity := controlplane.RegistryHostIdentity{
		NodeName: cfg.Node, HostKeyType: "ssh-ed25519",
		HostKeyFingerprint: "SHA256:" + strings.Repeat("A", 43),
		StateDir:           cfg.Layout.NodeStateDir,
	}
	database := &fakeWorkspaceDatabase{}
	workspace := &workspaceStep{cfg: cfg, database: database, hostIdentity: &hostIdentity}
	if _, err := workspace.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	nodeConfig := &nodeConfigStep{cfg: cfg}
	if _, err := nodeConfig.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EREGION_NODE_NAME", cfg.Node)
	t.Setenv("EREGION_NODE_STATE_DIR", cfg.Layout.NodeStateDir)
	identity, err := localRegistryIdentityAtSummary(configPath, func() (string, error) {
		return "ssh-ed25519 SHA256:" + strings.Repeat("A", 43), nil
	})
	if err != nil {
		t.Fatalf("first simulated worker start could not read the actual node.json: %v", err)
	}
	if identity.NodeName != cfg.Node || identity.StateDir != cfg.Layout.NodeStateDir {
		t.Fatalf("worker identity = %+v, want node %q state %q", identity, cfg.Node, cfg.Layout.NodeStateDir)
	}
	if database.identity.RegistryHost == nil || !database.identity.RegistryHost.Equal(identity) {
		t.Fatalf("recorded binding %+v does not match actual node.json identity %+v", database.identity.RegistryHost, identity)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	if rawNodeString(values["node"]) != cfg.Node || rawNodeString(values["state_dir"]) != cfg.Layout.NodeStateDir {
		t.Fatalf("actual node.json lacks full node/state fields: %s", raw)
	}
	if rawNodeString(values["workspace_id"]) != testWorkspaceID || rawNodeString(values["workspace_name"]) != "thelab" {
		t.Fatalf("nodeConfigStep erased the workspace mirror: %s", raw)
	}
}

func TestNodeConfigStepRunsAfterBinaryAndBeforeServices(t *testing.T) {
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
	binary, hasBinary := positions["host-binary-v1"]
	nodeConfig, hasNodeConfig := positions["host-node-config-v1"]
	api, hasAPI := positions["host-api-service-v1"]
	worker, hasWorker := positions["host-worker-service-v1"]
	if !hasBinary || !hasNodeConfig || !hasAPI || !hasWorker {
		t.Fatalf("missing bootstrap steps: %v", positions)
	}
	if !(binary < nodeConfig && nodeConfig < api && nodeConfig < worker) {
		t.Fatalf("node configuration must be written after the binary and before API/worker services: binary=%d node_config=%d api=%d worker=%d", binary, nodeConfig, api, worker)
	}
}
