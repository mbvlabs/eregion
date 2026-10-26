package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

// DefaultCommitConfirmWindow is how long an access-changing apply waits for a
// commit confirmation before a detached watchdog restores the previous files
// and restarts the affected services.
const DefaultCommitConfirmWindow = 90 * time.Second

// AccessChange is one reason an apply may break operator access. It is safe to
// show: it names a path and the kind of change, never a credential value.
type AccessChange struct {
	Kind   string `json:"kind"`
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail"`
}

func (c AccessChange) String() string {
	if c.Path == "" {
		return c.Kind + ": " + c.Detail
	}
	return c.Kind + " " + c.Path + ": " + c.Detail
}

// CommitState is the persisted view of one armed rollback window. It is part of
// the bootstrap state file, so a later `node bootstrap confirm` can find it
// without depending on the platform database.
type CommitState struct {
	Path     string    `json:"path"`
	Deadline time.Time `json:"deadline"`
	// Services lists units that must be restarted after a rollback.
	Services []string `json:"services,omitempty"`
}

type commitFile struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Backup string `json:"backup,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	UID    int    `json:"uid,omitempty"`
	GID    int    `json:"gid,omitempty"`
}

type commitManifest struct {
	Schema      int            `json:"schema"`
	OperationID string         `json:"operation_id"`
	Node        string         `json:"node"`
	Role        Role           `json:"role"`
	CreatedAt   time.Time      `json:"created_at"`
	Deadline    time.Time      `json:"deadline"`
	Files       []commitFile   `json:"files"`
	Services    []commitUnit   `json:"services"`
	Changes     []AccessChange `json:"changes"`
}

type commitUnit struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
}

const commitSchema = 1

func commitRoot(cfg Config) string {
	return filepath.Join(cfg.Layout.StateDir, "commit")
}

func commitPath(cfg Config, operationID string) string {
	return filepath.Join(commitRoot(cfg), operationID)
}

func commitManifestPath(cfg Config, operationID string) string {
	return filepath.Join(commitPath(cfg, operationID), "manifest.json")
}

func commitConfirmedPath(cfg Config, operationID string) string {
	return filepath.Join(commitPath(cfg, operationID), "confirmed")
}

func commitRolledBackPath(cfg Config, operationID string) string {
	return filepath.Join(commitPath(cfg, operationID), "rolled-back")
}

// AccessChanges reports file or policy changes that could remove the
// operator's current path to the host. A fresh host with only a loopback API
// and no SSH-key replacement has no access changes and applies without a
// commit window.
func AccessChanges(cfg Config, credentials Credentials) ([]AccessChange, error) {
	var changes []AccessChange
	for _, risk := range cfg.AccessRisks() {
		changes = append(changes, AccessChange{Kind: "policy", Detail: risk})
	}
	role, err := cfg.RoleConfig()
	if err != nil {
		return nil, err
	}
	// A configured key file always changes SSH access, even when the same keys
	// are already installed: the step owns the whole file.
	if cfg.NodeAuthorizedKeysFile != "" {
		changes = append(changes, AccessChange{Kind: "ssh", Path: cfg.NodeAuthorizedKeysFile, Detail: "node authorized_keys may be replaced"})
	}
	compare := func(kind, path string, desired []byte, hasDesired bool) error {
		raw, mode, exists, err := fileState(path)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		if !hasDesired {
			changes = append(changes, AccessChange{Kind: kind, Path: path, Detail: "existing access material would be replaced by credentials that are not available yet"})
			return nil
		}
		if !bytesEqual(raw, desired) || (kind == "credential" && mode != 0o600) {
			changes = append(changes, AccessChange{Kind: kind, Path: path, Detail: "existing content differs from the requested intent"})
		}
		return nil
	}
	if role.ControlPlane {
		if err := compare("service", filepath.Join(cfg.Layout.SystemdDir, "eregion-api.service"), apiServiceUnit(cfg), true); err != nil {
			return nil, err
		}
		if err := compare("service", filepath.Join(cfg.Layout.SystemdDir, "eregion-worker.service"), workerServiceUnit(cfg), true); err != nil {
			return nil, err
		}
		auth, authErr := credentials.AuthFile(cfg.Node)
		if err := compare("credential", filepath.Join(cfg.Layout.ConfigDir, "auth.json"), auth, authErr == nil); err != nil {
			return nil, err
		}
		env := (&credentialsStep{cfg: cfg, credentials: credentials}).env()
		if err := compare("credential", filepath.Join(cfg.Layout.ConfigDir, "control-plane.env"), env, true); err != nil {
			return nil, err
		}
		if cfg.APITLSCert != "" {
			cert, certErr := os.ReadFile(cfg.APITLSCert) //nolint:gosec // G304: operator-provided TLS material.
			if err := compare("tls", filepath.Join(cfg.Layout.ConfigDir, "tls", "server.crt"), cert, certErr == nil); err != nil {
				return nil, err
			}
			key, keyErr := os.ReadFile(cfg.APITLSKey) //nolint:gosec // G304: operator-provided TLS material.
			if err := compare("tls", filepath.Join(cfg.Layout.ConfigDir, "tls", "server.key"), key, keyErr == nil); err != nil {
				return nil, err
			}
		}
		source, sourceErr := cfg.EffectiveBinaryPath()
		if sourceErr != nil {
			return nil, sourceErr
		}
		if differs, err := binaryDiffers(filepath.Join(cfg.Layout.BinDir, "eregion"), source); err != nil {
			return nil, err
		} else if differs {
			changes = append(changes, AccessChange{Kind: "binary", Path: filepath.Join(cfg.Layout.BinDir, "eregion"), Detail: "installed executable differs from the supplied source"})
		}
	}
	if role.Registry {
		registry := &registryStep{cfg: cfg}
		if err := compare("registry", filepath.Join(cfg.Layout.SystemdDir, "eregion-registry.service"), registry.unit(), true); err != nil {
			return nil, err
		}
		if err := compare("registry", filepath.Join(cfg.Layout.ConfigDir, "registry", "config.yml"), registry.config(), true); err != nil {
			return nil, err
		}
	}
	if role.NodeAgent || role.ControlPlane {
		node := &nodeConfigStep{cfg: cfg}
		desired, err := node.config()
		if err != nil {
			return nil, err
		}
		if err := compare("node-identity", node.configPath(), desired, true); err != nil {
			return nil, err
		}
		source, sourceErr := cfg.EffectiveBinaryPath()
		if sourceErr != nil {
			return nil, sourceErr
		}
		if differs, err := binaryDiffers(filepath.Join(cfg.Layout.BinDir, "eregion"), source); err != nil {
			return nil, err
		} else if differs {
			changes = append(changes, AccessChange{Kind: "binary", Path: filepath.Join(cfg.Layout.BinDir, "eregion"), Detail: "installed executable differs from the supplied source"})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Path != changes[j].Path {
			return changes[i].Path < changes[j].Path
		}
		return changes[i].Kind < changes[j].Kind
	})
	return changes, nil
}

func binaryDiffers(dest, source string) (bool, error) {
	raw, mode, exists, err := fileState(dest)
	if err != nil || !exists {
		return false, err
	}
	want, err := fileSHA256(source)
	if err != nil {
		return false, err
	}
	return contentSHA256(raw) != want || mode&0o111 == 0, nil
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// accessFiles lists every path a commit snapshot must protect.
func accessFiles(cfg Config) []string {
	role, err := cfg.RoleConfig()
	if err != nil {
		return nil
	}
	paths := []string{
		filepath.Join(cfg.Layout.ConfigDir, "auth.json"),
		filepath.Join(cfg.Layout.ConfigDir, "control-plane.env"),
		filepath.Join(cfg.Layout.ConfigDir, "node.json"),
		filepath.Join(cfg.Layout.ConfigDir, "tls", "server.crt"),
		filepath.Join(cfg.Layout.ConfigDir, "tls", "server.key"),
		filepath.Join(cfg.Layout.SystemdDir, "eregion-api.service"),
		filepath.Join(cfg.Layout.SystemdDir, "eregion-worker.service"),
		filepath.Join(cfg.Layout.SystemdDir, "eregion-registry.service"),
		filepath.Join(cfg.Layout.ConfigDir, "registry", "config.yml"),
		filepath.Join(cfg.Layout.BinDir, "eregion"),
	}
	if role.NodeAgent {
		paths = append(paths, filepath.Join(filepath.Dir(cfg.Layout.DataDir), cfg.Layout.NodeUser, ".ssh", "authorized_keys"))
	}
	return paths
}

func commitServices(cfg Config) []commitUnit {
	role, err := cfg.RoleConfig()
	if err != nil {
		return nil
	}
	var units []commitUnit
	check := func(name string) {
		_, _, exists, err := fileState(filepath.Join(cfg.Layout.SystemdDir, name))
		if err == nil {
			units = append(units, commitUnit{Name: name, Exists: exists})
		}
	}
	if role.ControlPlane {
		check("eregion-api.service")
		check("eregion-worker.service")
	}
	if role.Registry {
		check("eregion-registry.service")
	}
	return units
}

// armCommit writes the rollback snapshot and starts the detached watchdog
// before any access-changing step runs.
func (r *Runner) armCommit(operationID string, changes []AccessChange) (CommitState, error) {
	dir := commitPath(r.Config, operationID)
	if err := ensureDir(dir, 0o700); err != nil {
		return CommitState{}, err
	}
	backupDir := filepath.Join(dir, "backup")
	if err := ensureDir(backupDir, 0o700); err != nil {
		return CommitState{}, err
	}
	manifest := commitManifest{
		Schema: commitSchema, OperationID: operationID, Node: r.Config.Node, Role: r.Config.Role,
		CreatedAt: r.now().UTC(), Deadline: r.now().Add(r.commitWindow()).UTC(), Changes: changes,
		Services: commitServices(r.Config),
	}
	for _, path := range accessFiles(r.Config) {
		raw, mode, exists, err := fileState(path)
		if err != nil {
			return CommitState{}, err
		}
		entry := commitFile{Path: path, Exists: exists, Mode: uint32(mode)}
		if exists {
			entry.UID, entry.GID, err = fileOwner(path)
			if err != nil {
				return CommitState{}, err
			}
			sum := sha256.Sum256([]byte(path))
			entry.Backup = filepath.Join("backup", hex.EncodeToString(sum[:]))
			if err := os.WriteFile(filepath.Join(dir, entry.Backup), raw, os.FileMode(mode)); err != nil {
				return CommitState{}, err
			}
			if err := os.Chmod(filepath.Join(dir, entry.Backup), os.FileMode(mode)&0o777); err != nil {
				return CommitState{}, err
			}
		}
		manifest.Files = append(manifest.Files, entry)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return CommitState{}, err
	}
	if err := writeReplacingSecret(commitManifestPath(r.Config, operationID), append(raw, '\n'), "", ""); err != nil {
		return CommitState{}, err
	}
	script := commitScript(dir, manifest)
	if err := os.WriteFile(filepath.Join(dir, "watchdog.sh"), []byte(script), 0o700); err != nil {
		return CommitState{}, err
	}
	if err := r.spawnWatchdog(filepath.Join(dir, "watchdog.sh")); err != nil {
		return CommitState{}, err
	}
	return CommitState{Path: dir, Deadline: manifest.Deadline, Services: unitNames(manifest.Services)}, nil
}

func unitNames(units []commitUnit) []string {
	var names []string
	for _, unit := range units {
		names = append(names, unit.Name)
	}
	return names
}

func (r *Runner) commitWindow() time.Duration {
	if r.Config.CommitConfirmSeconds <= 0 {
		return DefaultCommitConfirmWindow
	}
	return time.Duration(r.Config.CommitConfirmSeconds) * time.Second
}

func commitScript(dir string, manifest commitManifest) string {
	confirmed := shellQuote(filepath.Join(dir, "confirmed"))
	rolledBack := shellQuote(filepath.Join(dir, "rolled-back"))
	rollingBack := shellQuote(filepath.Join(dir, "rollback.lock"))
	failed := shellQuote(filepath.Join(dir, "rollback-failed"))
	var out strings.Builder
	out.WriteString("#!/bin/sh\nset -u\n")
	out.WriteString("if [ -f " + confirmed + " ] || [ -f " + rolledBack + " ]; then exit 0; fi\n")
	out.WriteString("while :; do\n")
	out.WriteString("  if [ -f " + confirmed + " ] || [ -f " + rolledBack + " ]; then exit 0; fi\n")
	out.WriteString("  now=$(date +%s)\n")
	out.WriteString(fmt.Sprintf("  if [ \"$now\" -ge %d ]; then\n", manifest.Deadline.Unix()))
	// Re-check the markers after observing the deadline: a confirmation or an
	// explicit rollback may have raced the last poll iteration.
	out.WriteString("    if [ -f " + confirmed + " ] || [ -f " + rolledBack + " ]; then exit 0; fi\n")
	// Only one restore may run. An old watchdog from operation A must never
	// overwrite the configuration confirmed by a newer operation B.
	out.WriteString("    if mkdir " + rollingBack + " 2>/dev/null; then :; else exit 0; fi\n")
	out.WriteString("    if [ -f " + confirmed + " ] || [ -f " + rolledBack + " ]; then rmdir " + rollingBack + " 2>/dev/null || true; exit 0; fi\n")
	for _, file := range manifest.Files {
		target := shellQuote(file.Path)
		if file.Exists {
			backup := shellQuote(filepath.Join(dir, file.Backup))
			out.WriteString("    mkdir -p " + shellQuote(filepath.Dir(file.Path)) + "\n")
			out.WriteString("    if ! cp -a " + backup + " " + target + "; then printf 'restore failed at %s\\n' \"$now\" > " + failed + "; rmdir " + rollingBack + " 2>/dev/null || true; exit 1; fi\n")
			out.WriteString(fmt.Sprintf("    chmod %04o %s 2>/dev/null || true\n", file.Mode&0o777, target))
			if file.UID >= 0 && file.GID >= 0 {
				out.WriteString(fmt.Sprintf("    chown %d:%d %s 2>/dev/null || true\n", file.UID, file.GID, target))
			}
		} else {
			out.WriteString("    rm -f " + target + "\n")
		}
	}
	out.WriteString("    systemctl daemon-reload 2>/dev/null || true\n")
	for _, unit := range manifest.Services {
		if unit.Exists {
			out.WriteString("    systemctl try-restart " + shellQuote(unit.Name) + " 2>/dev/null || true\n")
		} else {
			out.WriteString("    systemctl disable --now " + shellQuote(unit.Name) + " 2>/dev/null || true\n")
		}
	}
	out.WriteString("    printf 'rolled back at %s\\n' \"$now\" > " + rolledBack + "\n")
	out.WriteString("    rmdir " + rollingBack + " 2>/dev/null || true\n")
	out.WriteString("    exit 0\n")
	out.WriteString("  fi\n")
	out.WriteString("  sleep 1\n")
	out.WriteString("done\n")
	return out.String()
}

// reconcileCommits runs before a new apply. A detached watchdog dies with a
// reboot, so an expired, unconfirmed snapshot must be restored here rather than
// left to overwrite a newer configuration later. A future window from another
// operation blocks mutation: two rollback windows must never overlap.
func (r *Runner) reconcileCommits(projectedOperation string) error {
	entries, err := os.ReadDir(commitRoot(r.Config))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	now := r.now()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		operationID := entry.Name()
		raw, _, exists, err := fileState(commitManifestPath(r.Config, operationID))
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		var manifest commitManifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return contract.NewFault(contract.PreconditionFailed, "a rollback snapshot is invalid", "Inspect the bootstrap commit directory before applying again.")
		}
		_, _, hasConfirmed, err := fileState(commitConfirmedPath(r.Config, operationID))
		if err != nil {
			return err
		}
		_, _, hasRolledBack, err := fileState(commitRolledBackPath(r.Config, operationID))
		if err != nil {
			return err
		}
		if hasConfirmed || hasRolledBack {
			continue
		}
		if !now.Before(manifest.Deadline) {
			if err := r.rollbackCommit(operationID); err != nil {
				return contract.NewFault(contract.PreconditionFailed, "an expired access change could not be rolled back", "Inspect the commit snapshot and restore it manually before applying again.")
			}
			continue
		}
		if operationID != projectedOperation {
			return contract.NewFault(contract.ConfirmationRequired, "another access-changing apply still has an open rollback window", "Confirm or wait for operation "+operationID+" before starting a new convergence; a stale watchdog must not overwrite newer configuration.")
		}
	}
	return nil
}

// Confirm closes the rollback window for operationID. It must be called from a
// new process/connection after the operator verified the host is still
// reachable; the watchdog only cares that the marker arrived before the
// deadline.
func (r *Runner) Confirm(ctx context.Context, operationID string) (ConfirmReport, error) {
	if !r.SkipRootCheck {
		if err := requireRoot(); err != nil {
			return ConfirmReport{}, err
		}
	}
	state, exists, err := loadState(r.Config)
	if err != nil {
		return ConfirmReport{}, err
	}
	if !exists {
		return ConfirmReport{}, contract.NewFault(contract.NotFound, "no bootstrap state is present", "Run node bootstrap plan and apply first.")
	}
	if operationID == "" {
		operationID = state.OperationID
	}
	if state.OperationID != operationID {
		return ConfirmReport{}, contract.NewFault(contract.NotFound, "the bootstrap operation was not found", "Use the operation_id returned by an access-changing apply.")
	}
	if state.CommitID == "" {
		if !state.Active && !state.CompletedAt.IsZero() {
			return ConfirmReport{OperationID: operationID, AlreadyConfirmed: true}, nil
		}
		return ConfirmReport{}, contract.NewFault(contract.Conflict, "this bootstrap operation has no pending rollback window", "Access-changing apply returns the operation ID and deadline; a normal apply is already committed.")
	}
	dir := commitPath(r.Config, operationID)
	raw, _, exists, err := fileState(commitManifestPath(r.Config, operationID))
	if err != nil {
		return ConfirmReport{}, err
	}
	if !exists {
		return ConfirmReport{}, contract.NewFault(contract.PreconditionFailed, "the rollback snapshot is missing", "Do not delete the bootstrap commit directory while a confirmation is pending; inspect the host and re-apply if needed.")
	}
	var manifest commitManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return ConfirmReport{}, contract.NewFault(contract.PreconditionFailed, "the rollback snapshot is invalid", "Inspect the commit directory; the detached watchdog may still be running.")
	}
	if _, _, rolledBack, err := fileState(commitRolledBackPath(r.Config, operationID)); err != nil {
		return ConfirmReport{}, err
	} else if rolledBack {
		return ConfirmReport{}, contract.NewFault(contract.PreconditionFailed, "the access change was already rolled back", "Inspect the host and run node bootstrap plan again; do not confirm a state that is no longer live.")
	}
	now := r.now()
	if now.After(manifest.Deadline) {
		// The detached watchdog may not have survived a reboot or a PID-namespace
		// tear-down. Reconcile the snapshot now rather than confirming a state
		// that is no longer live.
		if err := r.rollbackCommit(operationID); err != nil {
			return ConfirmReport{}, contract.NewFault(contract.PreconditionFailed, "the rollback deadline has passed and the snapshot could not be restored", "Inspect the host and the commit snapshot manually; do not assume the new state is live.")
		}
		return ConfirmReport{}, contract.NewFault(contract.PreconditionFailed, "the rollback deadline has passed", "The previous access state was restored. Inspect the host and run node bootstrap plan again; do not assume the new state is live.")
	}
	if err := os.WriteFile(commitConfirmedPath(r.Config, operationID), []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return ConfirmReport{}, err
	}
	if err := markConfirmed(r.Config, operationID, now); err != nil {
		return ConfirmReport{}, err
	}
	return ConfirmReport{
		OperationID: operationID, ConfirmedAt: now.UTC(), Deadline: manifest.Deadline,
		Path: dir, Services: unitNames(manifest.Services),
	}, nil
}

// ConfirmReport never contains a credential.
type ConfirmReport struct {
	OperationID      string    `json:"operation_id"`
	ConfirmedAt      time.Time `json:"confirmed_at,omitempty"`
	Deadline         time.Time `json:"deadline,omitempty"`
	Path             string    `json:"path,omitempty"`
	Services         []string  `json:"services,omitempty"`
	AlreadyConfirmed bool      `json:"already_confirmed,omitempty"`
}

// rollbackCommit restores the snapshot. The detached watchdog runs the same
// logic through its generated shell script; tests call this directly to prove
// the revert path without waiting for a wall-clock deadline.
func (r *Runner) rollbackCommit(operationID string) error {
	dir := commitPath(r.Config, operationID)
	confirmedPath := commitConfirmedPath(r.Config, operationID)
	rolledBackPath := commitRolledBackPath(r.Config, operationID)
	for _, marker := range []string{confirmedPath, rolledBackPath} {
		if _, _, exists, err := fileState(marker); err != nil {
			return err
		} else if exists {
			// A confirmed operation must never be reverted by a late caller; a
			// second rollback is already done. This is the disarm path for an
			// old watchdog after a newer configuration was confirmed.
			return nil
		}
	}
	raw, _, exists, err := fileState(commitManifestPath(r.Config, operationID))
	if err != nil {
		return err
	}
	if !exists {
		return contract.NewFault(contract.NotFound, "the rollback snapshot is missing", "The watchdog cannot restore a snapshot that was deleted.")
	}
	var manifest commitManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return err
	}
	// Serialize with a detached watchdog using the same atomic directory claim.
	lock := filepath.Join(dir, "rollback.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		if os.IsExist(err) {
			return nil // Another rollback owns this snapshot.
		}
		return err
	}
	removeLock := func() { _ = os.Remove(lock) }
	// Re-check after taking the claim: a confirmation may have arrived while
	// this process was reading the manifest.
	for _, marker := range []string{confirmedPath, rolledBackPath} {
		if _, _, exists, err := fileState(marker); err != nil {
			removeLock()
			return err
		} else if exists {
			removeLock()
			return nil
		}
	}
	for _, file := range manifest.Files {
		if file.Exists {
			source := filepath.Join(dir, file.Backup)
			if err := os.MkdirAll(filepath.Dir(file.Path), 0o750); err != nil {
				removeLock()
				return err
			}
			raw, _, _, err := fileState(source)
			if err != nil {
				removeLock()
				return err
			}
			if err := writeFile(file.Path, raw, os.FileMode(file.Mode)&0o777, "root", "root"); err != nil {
				removeLock()
				return err
			}
			if os.Geteuid() == 0 && file.UID >= 0 && file.GID >= 0 {
				if err := os.Chown(file.Path, file.UID, file.GID); err != nil {
					removeLock()
					return err
				}
			}
		} else if err := os.Remove(file.Path); err != nil && !os.IsNotExist(err) {
			removeLock()
			return err
		}
	}
	if r.Exec != nil {
		if err := r.Exec.Run(context.Background(), "host-commit-rollback", "systemctl daemon-reload 2>/dev/null || true", nil, nil); err != nil {
			removeLock()
			return err
		}
		for _, unit := range manifest.Services {
			script := "systemctl try-restart " + shellQuote(unit.Name) + " 2>/dev/null || true"
			if !unit.Exists {
				script = "systemctl disable --now " + shellQuote(unit.Name) + " 2>/dev/null || true"
			}
			_ = r.Exec.Run(context.Background(), "host-commit-rollback-"+unit.Name, script, nil, nil)
		}
	}
	if err := os.WriteFile(rolledBackPath, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		removeLock()
		return err
	}
	removeLock()
	// Clear the pending window so a subsequent plan starts cleanly. This is
	// also the boot reconciliation path when the detached watchdog did not
	// survive a reboot.
	_ = engineWithStateLock(r.Config, func(state *State) error {
		if state.OperationID == operationID && state.CommitID == operationID {
			state.CommitID = ""
			state.CommitDeadline = time.Time{}
		}
		return nil
	})
	return nil
}

func markConfirmed(cfg Config, operationID string, now time.Time) error {
	return engineWithStateLock(cfg, func(state *State) error {
		if state.OperationID != operationID {
			return contract.NewFault(contract.PreconditionFailed, "the bootstrap state changed during confirmation", "Inspect the state file before retrying.")
		}
		state.Active = false
		state.CompletedAt = now.UTC()
		state.CommitID = ""
		state.CommitDeadline = time.Time{}
		return nil
	})
}

func markCommitPending(cfg Config, operationID string, commit CommitState) error {
	return engineWithStateLock(cfg, func(state *State) error {
		if state.OperationID != operationID {
			return contract.NewFault(contract.PreconditionFailed, "the bootstrap state changed during apply", "Inspect the state file before retrying.")
		}
		state.CommitID = operationID
		state.CommitDeadline = commit.Deadline
		return nil
	})
}

func engineWithStateLock(cfg Config, mutate func(*State) error) error {
	return engine.WithFileLock(statePath(cfg)+".lock", func() error {
		state, exists, err := loadState(cfg)
		if err != nil {
			return err
		}
		if !exists {
			return contract.NewFault(contract.PreconditionFailed, "the bootstrap state is missing", "Run node bootstrap plan and apply again.")
		}
		if err := mutate(&state); err != nil {
			return err
		}
		raw, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return err
		}
		return writeReplacingSecret(statePath(cfg), append(raw, '\n'), "", "")
	})
}
