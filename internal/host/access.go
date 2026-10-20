package host

import (
	"os"
	"path/filepath"

	"github.com/mbvlabs/eregion/internal/contract"
)

// Access-changing convergence is deliberately unsupported until an independent,
// persisted rollback watchdog and post-change confirmation are implemented.
// A pre-apply plan token is NOT commit-confirm. No flag bypasses this gate.
func accessGate(cfg Config) error {
	unsupported := func() error {
		return contract.NewFault(contract.PreconditionFailed, "access-changing host apply is not implemented", "Timed rollback/commit-confirm is not implemented. Bootstrap only a fresh host with a loopback API and no SSH key changes. Manage live upgrades and enrollment separately; no acknowledgement flag bypasses this gate.")
	}
	if cfg.NodeAuthorizedKeysFile != "" {
		return unsupported()
	}
	role, err := cfg.RoleConfig()
	if err != nil {
		return err
	}
	if role.ControlPlane {
		if apiExternallyReachable(cfg) || cfg.APITLSCert != "" {
			return unsupported()
		}
		// Once a main host is live, even a loopback listener may sit behind an
		// operator's proxy/tunnel. Refuse updates to its binary, auth, or unit.
		for _, path := range []string{
			filepath.Join(cfg.Layout.SystemdDir, "eregion-api.service"),
			filepath.Join(cfg.Layout.SystemdDir, "eregion-worker.service"),
		} {
			if _, err := os.Lstat(path); err == nil {
				return unsupported()
			} else if !os.IsNotExist(err) {
				return err
			}
		}
	}
	if role.NodeAgent {
		step := &nodeConfigStep{cfg: cfg}
		want, err := step.config()
		if err != nil {
			return err
		}
		raw, _, exists, err := fileState(step.configPath())
		if err != nil {
			return err
		}
		if exists && string(raw) != string(want) {
			return unsupported()
		}
	}
	binaries := &binaryStep{cfg: cfg, role: role}
	specs, err := binaries.binaries()
	if err != nil {
		return err
	}
	for _, spec := range specs {
		got, _, err := fileSHA256Mode(spec.Dest)
		if err != nil {
			return err
		}
		if got != "" {
			want, err := fileSHA256(spec.Source)
			if err != nil {
				return err
			}
			if got != want {
				return unsupported()
			}
		}
	}
	return nil
}
