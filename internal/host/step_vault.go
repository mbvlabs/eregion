package host

import (
	"context"
	"os"
	"path/filepath"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/vault"
)

// VaultKeyringName is the master-key file inside the host configuration
// directory. It never enters PostgreSQL.
const VaultKeyringName = "vault-keyring.json"

// VaultKeyringPath is the keyring the control plane loads through
// EREGION_VAULT_KEY_FILE.
func VaultKeyringPath(cfg Config) string {
	return filepath.Join(cfg.Layout.ConfigDir, VaultKeyringName)
}

// vaultKeyringStep generates the vault master key. Rotation is a deliberate
// operator action: an existing ring is never replaced, only verified and its
// ownership/mode converged.
type vaultKeyringStep struct {
	cfg Config
}

func (s *vaultKeyringStep) ID() string { return "host-vault-keyring-v1" }
func (s *vaultKeyringStep) Describe() string {
	return "Generate the vault keyring for environment secrets"
}
func (s *vaultKeyringStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *vaultKeyringStep) Inputs() ([]byte, error) {
	return canonicalInput(struct {
		Path string `json:"path"`
	}{VaultKeyringPath(s.cfg)})
}

func (s *vaultKeyringStep) Check(_ context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	path := VaultKeyringPath(s.cfg)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return incomplete("vault keyring is missing at " + path)
		}
		return engine.CheckResult{}, err
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 || mode&0o400 == 0 {
		return incomplete("vault keyring " + path + " is not owner-only")
	}
	ok, err := ownershipMatches(info, s.cfg.Layout.User, s.cfg.Layout.Group)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !ok {
		return incomplete("vault keyring " + path + " is not owned by the control-plane account")
	}
	ring, err := vault.LoadKeyRing(path)
	if err != nil {
		// A corrupt or unreadable ring is not rewritten: new key material would
		// make every existing ciphertext unreadable.
		return engine.CheckResult{}, err
	}
	return complete("vault keyring " + path + " is owner-only with current key " + ring.CurrentKeyID())
}

func (s *vaultKeyringStep) Apply(_ context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	path := VaultKeyringPath(s.cfg)
	_, _, exists, err := fileState(path)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if exists {
		// Verify before touching ownership: a damaged ring must fail closed.
		if _, err := vault.LoadKeyRing(path); err != nil {
			return engine.ApplyResult{}, err
		}
	} else {
		ring, err := vault.GenerateKeyRing()
		if err != nil {
			return engine.ApplyResult{}, err
		}
		if err := vault.WriteKeyRing(path, ring); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	// The API and worker run as the unprivileged control-plane account and
	// load the ring at startup; root creates it and hands it to that account
	// with mode 0600 so no group or other user can read it.
	if err := os.Chmod(path, 0o600); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := chownIfRoot(path, s.cfg.Layout.User, s.cfg.Layout.Group); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{
		Notes:  []string{"vault keyring is present at " + path + " (0600); new key material is never overwritten"},
		Result: map[string]any{"keyring": path},
	}, nil
}

func (s *vaultKeyringStep) Diff(context.Context, engine.Runtime) ([]string, error) {
	path := VaultKeyringPath(s.cfg)
	if _, _, exists, _ := fileState(path); exists {
		return []string{"= verify the existing vault keyring and its owner-only mode"}, nil
	}
	return []string{"+ generate vault keyring " + path + " (0600)"}, nil
}

func (s *vaultKeyringStep) Plan(ctx context.Context, runtime engine.Runtime) (PlanEntry, error) {
	return planEntry(ctx, s, runtime)
}
