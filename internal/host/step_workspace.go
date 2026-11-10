package host

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/engine"
)

// workspaceStep records the one-time portable installation workspace identity
// after migrations exist. It only runs when an explicit workspace name was
// supplied (new setup or `apply --assign-workspace`); a legacy apply without the flag
// leaves the installation explicitly unconfigured and never guesses.
type workspaceStep struct {
	cfg      Config
	database Database
}

func (s *workspaceStep) ID() string { return "host-workspace-v1" }
func (s *workspaceStep) Describe() string {
	return "Record the portable workspace identity in PostgreSQL"
}
func (s *workspaceStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *workspaceStep) configPath() string {
	return filepath.Join(s.cfg.Layout.ConfigDir, "node.json")
}

func (s *workspaceStep) Inputs() ([]byte, error) {
	return canonicalInput(struct {
		Node      string `json:"node"`
		Workspace string `json:"workspace"`
		Role      Role   `json:"role"`
	}{s.cfg.Node, s.cfg.Workspace, s.cfg.Role})
}

func (s *workspaceStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	if s.database == nil {
		return engine.CheckResult{}, contractError("no database adapter is configured for the workspace identity")
	}
	identity, err := s.database.ReadWorkspace(ctx, s.cfg.DatabaseURL)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !identity.Configured {
		return incomplete("workspace identity is not configured; apply " + s.cfg.Workspace)
	}
	if identity.Name != s.cfg.Workspace {
		return engine.CheckResult{}, workspaceNameConflict(identity.Name, s.cfg.Workspace)
	}
	mirrorOK, err := s.mirrorMatches(identity)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !mirrorOK {
		return incomplete("the node configuration does not mirror workspace " + identity.Name)
	}
	return complete("workspace " + identity.Name + " (" + identity.ID + ") is configured")
}

func (s *workspaceStep) mirrorMatches(identity controlplane.WorkspaceIdentity) (bool, error) {
	values, exists, err := loadNodeConfigMap(s.configPath())
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	return rawNodeString(values["workspace_id"]) == identity.ID &&
		rawNodeString(values["workspace_name"]) == identity.Name, nil
}

func (s *workspaceStep) Apply(ctx context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	if s.database == nil {
		return engine.ApplyResult{}, contractError("no database adapter is configured for the workspace identity")
	}
	identity, err := s.database.AssignWorkspace(ctx, s.cfg.DatabaseURL, s.cfg.Workspace)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if err := s.writeMirror(identity); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{
		Notes: []string{"workspace " + identity.Name + " is configured; the node configuration mirrors its portable ID"},
		Result: map[string]any{
			"workspace": identity.Name, "workspace_id": identity.ID, "configured": true,
		},
	}, nil
}

// writeMirror updates only the two workspace keys in node.json. It preserves
// operator-owned keys, including ones this binary does not know, so apply
// --workspace cannot erase a privileged helper or future node setting.
func (s *workspaceStep) writeMirror(identity controlplane.WorkspaceIdentity) error {
	values, _, err := loadNodeConfigMap(s.configPath())
	if err != nil {
		return err
	}
	id, err := json.Marshal(identity.ID)
	if err != nil {
		return err
	}
	name, err := json.Marshal(identity.Name)
	if err != nil {
		return err
	}
	values["workspace_id"], values["workspace_name"] = id, name
	merged, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return err
	}
	user, group := nodeConfigOwnership(s.cfg)
	if err := writeReplacingSecret(s.configPath(), append(merged, '\n'), user, group); err != nil {
		return fmt.Errorf("write workspace mirror: %w", err)
	}
	return nil
}

// workspaceNameConflict is shared by the pre-apply check and the step itself.
func workspaceNameConflict(configured, requested string) error {
	fault := contract.NewFault(contract.Conflict,
		"the workspace identity is already configured as "+configured,
		"The workspace name and ID are immutable. Use "+configured+", or restore the intended database before assigning another workspace.")
	fault.Details = map[string]any{
		"configured_workspace": configured,
		"requested_workspace":  requested,
	}
	return fault
}

func (s *workspaceStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	return []string{
		"+ assign the immutable portable workspace identity " + s.cfg.Workspace,
		"+ mirror its generated ID and name in " + s.configPath(),
	}, nil
}

func (s *workspaceStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}

// loadNodeConfigMap reads node.json as a generic object so writers preserve
// unknown keys. A missing file is an empty object, not a fabricated identity.
func loadNodeConfigMap(path string) (map[string]json.RawMessage, bool, error) {
	raw, _, exists, err := fileState(path)
	if err != nil {
		return nil, false, err
	}
	values := map[string]json.RawMessage{}
	if !exists {
		return values, false, nil
	}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, true, fmt.Errorf("the existing node configuration %s is not valid JSON: %w", path, err)
	}
	return values, true, nil
}

func rawNodeString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}
