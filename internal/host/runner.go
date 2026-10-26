package host

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// Runner owns one host intent. All external effects go through its Exec,
// Downloader, Database, and Probe seams, which tests replace with temporary
// roots and fakes. Production defaults are real host commands.
type Runner struct {
	Config     Config
	Exec       Exec
	Downloader Downloader
	Database   Database
	Probe      Probe
	Secrets    *secrets.Registry
	Now        func() time.Time

	// Reporter receives redacted step progress. The CLI writes it to stderr so
	// a remote apply can be followed while the final envelope stays on stdout.
	Reporter engine.Reporter
	// SpawnWatchdog exists for tests; production starts a detached shell.
	SpawnWatchdog func(string) error

	// SkipRootCheck exists only for unit tests. Real apply is root-only.
	SkipRootCheck bool
}

// Plan is a read-only preview. It is also the source of the confirmation
// token required by Apply.
type Plan struct {
	Node          string         `json:"node"`
	Role          Role           `json:"role"`
	Platform      Platform       `json:"platform"`
	Supported     bool           `json:"supported"`
	ConfigHash    string         `json:"config_hash"`
	Generation    uint32         `json:"generation"`
	OperationID   string         `json:"operation_id"`
	Resumed       bool           `json:"resumed"`
	Ready         bool           `json:"ready"`
	AccessRisks   []string       `json:"access_risks,omitempty"`
	AccessChanges []AccessChange `json:"access_changes,omitempty"`
	Steps         []PlanEntry    `json:"steps"`
	// Total is the number of selected steps; WouldChange counts the steps that
	// are incomplete or report an error, so a second apply can be a no-op.
	Total        int          `json:"total"`
	WouldChange  int          `json:"would_change"`
	Confirmation Confirmation `json:"confirmation"`
	Journal      string       `json:"journal"`
	StateFile    string       `json:"state_file"`
	ApplyBlocked string       `json:"apply_blocked,omitempty"`
}

// Pending returns the steps an apply would change.
func (p Plan) Pending() []PlanEntry {
	var pending []PlanEntry
	for _, entry := range p.Steps {
		if !entry.Complete || entry.Error != "" {
			pending = append(pending, entry)
		}
	}
	return pending
}

// ApplyReport is returned after a successful convergence. A failed apply is a
// Fault with the same identifying details in its Details map.
type ApplyReport struct {
	Node        string       `json:"node"`
	Role        Role         `json:"role"`
	OperationID string       `json:"operation_id"`
	Generation  uint32       `json:"generation"`
	Steps       []ApplyEntry `json:"steps"`
	Applied     int          `json:"applied"`
	Skipped     int          `json:"skipped"`
	Total       int          `json:"total"`
	Resumed     bool         `json:"resumed"`
	CompletedAt time.Time    `json:"completed_at"`
	// Pending is true when an access-changing apply is live but not yet
	// committed by node bootstrap confirm.
	Pending        bool           `json:"pending,omitempty"`
	CommitDeadline time.Time      `json:"commit_deadline,omitempty"`
	AccessChanges  []AccessChange `json:"access_changes,omitempty"`
}

// ApplyEntry reports one step's engine outcome. Executed is false when a
// durable effect record from the same generation was recovered.
type ApplyEntry struct {
	ID       string `json:"id"`
	Complete bool   `json:"complete"`
	Executed bool   `json:"executed"`
	Detail   string `json:"detail,omitempty"`
}

// Status is the node-local bootstrap state plus the persisted step journal. It
// deliberately does not evaluate drift: status works without the database URL,
// binary, or other apply-only inputs. Run `node bootstrap plan` when those
// inputs are available to observe drift.
type Status struct {
	Bootstrapped bool `json:"bootstrapped"`
	Complete     bool `json:"complete"`
	Active       bool `json:"active"`
	Pending      bool `json:"pending_commit,omitempty"`
	// RollbackRequired reports an expired commit window that no live watchdog
	// has rolled back (for example after a reboot). Run apply or confirm to
	// reconcile it before planning a new generation.
	RollbackRequired bool         `json:"rollback_required,omitempty"`
	CommitDeadline   *time.Time   `json:"commit_deadline,omitempty"`
	State            *State       `json:"state,omitempty"`
	Journal          string       `json:"journal"`
	Steps            []StatusStep `json:"steps"`
}

// StatusStep is one persisted effect in the current bootstrap generation.
type StatusStep struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Attempt   uint32 `json:"attempt"`
	Applied   uint32 `json:"applied_attempt,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"completed_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

// NewRunner validates the intent and fills production defaults.
func NewRunner(cfg Config, registry *secrets.Registry) (*Runner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return newRunner(cfg, registry), nil
}

// NewConfirmRunner is the narrow constructor for `node bootstrap confirm`. It needs
// only the host layout, not the database URL or credential outputs that the
// original apply used; those values are in the journal.
func NewConfirmRunner(cfg Config, registry *secrets.Registry) (*Runner, error) {
	if err := cfg.Layout.Validate(); err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, err.Error(), "Use the default bootstrap layout or the layout used by the original apply.")
	}
	return newRunner(cfg, registry), nil
}

// NewStatusRunner reads the node-local bootstrap state. It needs only a valid
// layout: status must work on a fresh shell without the database URL, operator
// token output, or binaries that apply required.
func NewStatusRunner(cfg Config, registry *secrets.Registry) (*Runner, error) {
	if err := cfg.Layout.Validate(); err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, err.Error(), "Use the default bootstrap layout or the layout used by the original apply.")
	}
	return newRunner(cfg, registry), nil
}

func newRunner(cfg Config, registry *secrets.Registry) *Runner {
	if registry == nil {
		registry = &secrets.Registry{}
	}
	runner := &Runner{
		Config:   cfg,
		Exec:     ShellExec{Secrets: registry},
		Database: PostgresDatabase{},
		Probe:    httpProbe{},
		Secrets:  registry,
		Now:      time.Now,
	}
	runner.Downloader = HTTPDownloader{}
	return runner
}

func (r *Runner) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func (r *Runner) spawnWatchdog(path string) error {
	if r.SpawnWatchdog != nil {
		return r.SpawnWatchdog(path)
	}
	return spawnDetached(path)
}

func (r *Runner) steps(credentials Credentials) ([]Step, error) {
	role, err := r.Config.RoleConfig()
	if err != nil {
		return nil, err
	}
	// An undetectable platform fails the platform step first; distro is then empty.
	platform, _ := DetectPlatform(r.Config.PlatformPath, r.Config.Arch)
	steps := []Step{&platformStep{cfg: r.Config}, &packagesStep{cfg: r.Config, exec: r.Exec, role: role, distro: platform.ID}, &usersStep{cfg: r.Config, exec: r.Exec, role: role}, &directoriesStep{cfg: r.Config, role: role}}
	if role.Runtime {
		steps = append(steps, &dockerStep{exec: r.Exec})
	}
	if role.Builder {
		steps = append(steps, &buildpacksStep{cfg: r.Config, exec: r.Exec, downloader: r.Downloader})
	}
	if role.Registry {
		steps = append(steps, &registryStep{cfg: r.Config, exec: r.Exec})
	}
	if role.ControlPlane {
		if r.Config.APITLSCert != "" {
			steps = append(steps, &tlsStep{cfg: r.Config})
		}
		steps = append(steps, &databaseStep{cfg: r.Config, database: r.Database})
		// Before the credentials file that names EREGION_VAULT_KEY_FILE and
		// before the services that load it.
		steps = append(steps, &vaultKeyringStep{cfg: r.Config})
		steps = append(steps, &credentialsStep{cfg: r.Config, credentials: credentials})
		// Before the services: the worker's unit makes the routes dir writable,
		// which must exist when it starts.
		if r.Config.Hostname != "" {
			steps = append(steps, &ingressStep{cfg: r.Config, exec: r.Exec})
		}
	}
	steps = append(steps, &binaryStep{cfg: r.Config, role: role})
	if role.ControlPlane {
		steps = append(steps, newAPIService(r.Config, r.Exec), newWorkerService(r.Config, r.Exec))
		steps = append(steps, &readinessStep{cfg: r.Config, exec: r.Exec, probe: r.Probe})
		// node_exporter listens on the node's WireGuard address only. It is
		// installed here and started by the access setup once wg0 exists.
		steps = append(steps, &nodeExporterStep{cfg: r.Config, exec: r.Exec})
	}
	// The main node also needs /etc/eregion/node.json: node RPC, the CI URLs,
	// push-to-deploy and secrets export all read the node identity and main
	// hostname from it. Only a role with a node execution account installs
	// forced-command operator keys.
	if role.ControlPlane || role.NodeAgent {
		steps = append(steps, &nodeConfigStep{cfg: r.Config})
	}
	return steps, nil
}

// Plan observes every step without writing a file or starting a service.
func (r *Runner) Plan(ctx context.Context) (Plan, error) {
	platform, platformErr := DetectPlatform(r.Config.PlatformPath, r.Config.Arch)
	plan := Plan{
		Node:      r.Config.Node,
		Role:      r.Config.Role,
		Platform:  platform,
		Journal:   JournalPath(r.Config),
		StateFile: statePath(r.Config),
	}
	if platformErr != nil || !platform.Supported() {
		plan.Supported = false
		entry := PlanEntry{ID: "host-platform-v1", Description: "Verify a supported Debian/Ubuntu release and architecture", Complete: false}
		if platformErr != nil {
			entry.Error = safeError(platformErr)
		} else {
			entry.Error = fmt.Sprintf("%s is not in the verified matrix (Debian 13, Ubuntu 26.04; amd64/arm64)", platform)
		}
		plan.Steps = []PlanEntry{entry}
		plan.Total, plan.WouldChange = 1, 1
		return plan, nil
	}
	plan.Supported = true
	configHash, err := r.Config.ConfigHash()
	if err != nil {
		return Plan{}, err
	}
	state, exists, err := loadState(r.Config)
	if err != nil {
		return Plan{}, err
	}
	projected, err := projectState(r.Config, configHash, r.now())
	if err != nil {
		return Plan{}, err
	}
	credentials, err := LoadCredentials(r.Config, false)
	if err != nil {
		return Plan{}, err
	}
	changes, err := AccessChanges(r.Config, credentials)
	if err != nil {
		return Plan{}, err
	}
	plan.AccessChanges = changes
	r.registerSecrets(credentials)
	steps, err := r.steps(credentials)
	if err != nil {
		return Plan{}, err
	}
	runtimeView := engine.Runtime{DryRun: true, Secrets: r.Secrets, Root: r.Config.Layout.StateDir}
	ready := true
	for _, step := range steps {
		entry, _ := step.Plan(ctx, runtimeView)
		entry.Error = r.Secrets.Redact(entry.Error)
		entry.Detail = r.Secrets.Redact(entry.Detail)
		for i := range entry.Diff {
			entry.Diff[i] = r.Secrets.Redact(entry.Diff[i])
		}
		if !entry.Complete || entry.Error != "" {
			ready = false
			plan.WouldChange++
		}
		plan.Steps = append(plan.Steps, entry)
	}
	plan.Total = len(plan.Steps)
	risks := r.Config.AccessRisks()
	plan.AccessRisks = risks
	bindingRisks := append(append([]string(nil), risks...), changeStrings(changes)...)
	plan.ConfigHash = configHash
	plan.Generation = projected.Generation
	plan.OperationID = projected.OperationID
	plan.Resumed = exists && state.Active && state.OperationID == projected.OperationID
	plan.Ready = ready
	confirmation, err := issueConfirmation("host.apply", r.Config.Node+"/"+string(r.Config.Role), configHash, planInputs(configHash, projected.Generation, bindingRisks), r.now())
	if err != nil {
		return Plan{}, err
	}
	plan.Confirmation = confirmation
	return plan, nil
}

// Apply converges the host after validating the exact plan token. The engine
// journal is written before the database step, so an interrupted first contact
// resumes without the platform store. An access-changing apply also arms a
// detached rollback watchdog and returns confirmation_required; it is not
// complete until Confirm arrives before the deadline.
func (r *Runner) Apply(ctx context.Context, confirmation string, acceptAccessRisk bool) (ApplyReport, error) {
	if err := r.Config.Validate(); err != nil {
		return ApplyReport{}, err
	}
	if !r.SkipRootCheck {
		if err := requireRoot(); err != nil {
			return ApplyReport{}, err
		}
	}
	// Kept for adapter compatibility; the plan/confirmation binding is the
	// acknowledgement. No flag bypasses the commit-confirm window.
	_ = acceptAccessRisk
	configHash, err := r.Config.ConfigHash()
	if err != nil {
		return ApplyReport{}, err
	}
	projected, err := projectState(r.Config, configHash, r.now())
	if err != nil {
		return ApplyReport{}, err
	}
	preview, err := LoadCredentials(r.Config, false)
	if err != nil {
		return ApplyReport{}, err
	}
	changes, err := AccessChanges(r.Config, preview)
	if err != nil {
		return ApplyReport{}, err
	}
	risks := append(append([]string(nil), r.Config.AccessRisks()...), changeStrings(changes)...)
	if err := verifyConfirmation(confirmation, "host.apply", r.Config.Node+"/"+string(r.Config.Role), configHash, planInputs(configHash, projected.Generation, risks), r.now()); err != nil {
		return ApplyReport{}, err
	}
	if err := ensureState(r.Config); err != nil {
		return ApplyReport{}, err
	}
	releaseApply, err := engine.Acquire(ctx, filepath.Join(r.Config.Layout.StateDir, "apply.lock"), engine.LockExclusive, 30*time.Second)
	if err != nil {
		return ApplyReport{}, err
	}
	defer releaseApply()
	// Revalidate under the whole-apply lock, not just per-effect locks.
	projected, err = projectState(r.Config, configHash, r.now())
	if err != nil {
		return ApplyReport{}, err
	}
	if err := verifyConfirmation(confirmation, "host.apply", r.Config.Node+"/"+string(r.Config.Role), configHash, planInputs(configHash, projected.Generation, risks), r.now()); err != nil {
		return ApplyReport{}, err
	}
	if err := r.reconcileCommits(projected.OperationID); err != nil {
		return ApplyReport{}, err
	}
	credentials, err := LoadCredentials(r.Config, true)
	if err != nil {
		return ApplyReport{}, err
	}
	r.registerSecrets(credentials)
	state, err := beginState(r.Config, configHash, r.now())
	if err != nil {
		return ApplyReport{}, err
	}
	if state.OperationID != projected.OperationID {
		if err := verifyConfirmation(confirmation, "host.apply", r.Config.Node+"/"+string(r.Config.Role), configHash, planInputs(configHash, state.Generation, risks), r.now()); err != nil {
			return ApplyReport{}, err
		}
	}
	if state.CommitID != "" {
		report := ApplyReport{Node: r.Config.Node, Role: r.Config.Role, OperationID: state.OperationID, Generation: state.Generation, Pending: true, CommitDeadline: state.CommitDeadline}
		if r.now().Before(state.CommitDeadline) {
			return report, pendingCommitFault(report, "this access-changing apply is already waiting for confirmation")
		}
		return report, contract.NewFault(contract.PreconditionFailed, "the previous rollback window has expired", "Inspect the host, then run node bootstrap plan again. Do not assume the pending access change is live.")
	}
	if err := ensureState(r.Config); err != nil {
		return ApplyReport{}, err
	}
	// Share the node execution host lock when bootstrap and node state are
	// separate. A bootstrap drop never races a live node effect on the host.
	releaseHost := func() {}
	if filepath.Clean(r.Config.Layout.StateDir) != filepath.Clean(r.Config.Layout.NodeStateDir) {
		release, lockErr := engine.Acquire(ctx, filepath.Join(r.Config.Layout.NodeStateDir, "host.lock"), engine.LockExclusive, 30*time.Second)
		if lockErr == nil {
			releaseHost = release
		} else if errors.Is(lockErr, engine.ErrLockHeld) {
			return ApplyReport{}, contract.NewFault(contract.Unavailable, "another local host mutation is running", "Wait for the other node/bootstrap operation to finish; bootstrap holds the same host lock as node effects.")
		} else if !r.SkipRootCheck {
			return ApplyReport{}, lockErr
		}
	}
	defer releaseHost()

	hostEngine, err := engine.New(r.Config.Layout.StateDir, r.Config.Node, r.Secrets)
	if err != nil {
		return ApplyReport{}, err
	}
	steps, err := r.steps(credentials)
	if err != nil {
		return ApplyReport{}, err
	}
	report := ApplyReport{Node: r.Config.Node, Role: r.Config.Role, OperationID: state.OperationID, Generation: state.Generation, AccessChanges: changes}
	commit := CommitState{}
	if len(changes) > 0 {
		commit, err = r.armCommit(state.OperationID, changes)
		if err != nil {
			return report, contract.NewFault(contract.PreconditionFailed, "could not arm the rollback watchdog", "No host mutation was started. Check that the bootstrap state directory is writable and the host can start a detached process; then re-run apply.")
		}
		report.CommitDeadline = commit.Deadline
	}
	for _, step := range steps {
		execution := engine.Execution{OperationID: contract.OperationID(state.OperationID), Attempt: state.Generation, Step: step.ID(), RequestID: fmt.Sprintf("bootstrap-%d", state.Generation)}
		status, runErr := hostEngine.Run(ctx, step, execution, r.Reporter)
		if runErr != nil {
			if len(changes) > 0 {
				_ = r.rollbackCommit(state.OperationID)
			}
			return report, r.applyFault(step, execution, status, report, runErr)
		}
		entry := ApplyEntry{ID: step.ID(), Complete: status.Status == engine.StatusCompleted, Executed: status.Executed, Detail: status.Detail}
		report.Steps = append(report.Steps, entry)
		report.Total++
		if entry.Executed {
			report.Applied++
		} else {
			report.Skipped++
		}
		if !entry.Complete {
			if len(changes) > 0 {
				_ = r.rollbackCommit(state.OperationID)
			}
			return report, contract.NewFault(contract.Internal, "host step did not complete", "Inspect the bootstrap journal and re-run apply with a fresh plan.")
		}
	}
	// Record the non-secret host intent on the node so later plan/apply runs
	// reconstruct the same desired state without the operator's original
	// flags or uploaded files.
	if err := saveIntent(r.Config); err != nil {
		return report, contract.NewFault(contract.PreconditionFailed, "the host converged but its intent could not be recorded", err.Error())
	}
	if len(changes) > 0 {
		if err := markCommitPending(r.Config, state.OperationID, commit); err != nil {
			return report, err
		}
		report.Pending = true
		return report, pendingCommitFault(report, "access-changing apply is live and waiting for commit confirmation")
	}
	if err := finishState(r.Config, state.OperationID, r.now()); err != nil {
		return report, err
	}
	report.CompletedAt = r.now().UTC()
	return report, nil
}

func changeStrings(changes []AccessChange) []string {
	out := make([]string, 0, len(changes))
	for _, change := range changes {
		out = append(out, change.String())
	}
	return out
}

func pendingCommitFault(report ApplyReport, message string) error {
	return &contract.Fault{
		Code:    contract.ConfirmationRequired,
		Message: message,
		Hint:    "Verify the host is reachable over the new access path, then run: eregion node bootstrap confirm " + report.OperationID,
		Details: map[string]any{
			"operation_id":   report.OperationID,
			"deadline":       report.CommitDeadline.UTC().Format(time.RFC3339),
			"pending":        true,
			"access_changes": report.AccessChanges,
			"completed":      report.Steps,
		},
	}
}

func (r *Runner) applyFault(step Step, execution engine.Execution, status engine.TaskStatus, report ApplyReport, cause error) error {
	fault := &contract.Fault{Code: contract.Unavailable, Message: "host step " + step.ID() + " failed", Hint: "Fix the reported precondition, run host plan again, and resume with its confirmation token. Completed steps are not re-applied.", Cause: cause}
	var existing *contract.Fault
	if errors.As(cause, &existing) && contract.KnownCode(existing.Code) {
		copy := *existing
		copy.Message = "host step " + step.ID() + ": " + existing.Message
		fault = &copy
	}
	if r.Secrets != nil {
		fault.Message = r.Secrets.Redact(fault.Message)
		fault.Hint = r.Secrets.Redact(fault.Hint)
	}
	fault.Details = map[string]any{
		"step":         step.ID(),
		"operation_id": string(execution.OperationID),
		"generation":   execution.Attempt,
		"journal":      JournalPath(r.Config),
		"state_file":   statePath(r.Config),
		"resumable":    true,
		"completed":    report.Steps,
	}
	if status.Detail != "" && r.Secrets != nil {
		fault.Details["step_detail"] = r.Secrets.Redact(status.Detail)
	}
	return fault
}

// Status reads the node-local state and journal only. It never evaluates a
// plan: status must work without apply-only inputs and must not mutate.
func (r *Runner) Status(_ context.Context) (Status, error) {
	state, exists, err := loadState(r.Config)
	if err != nil {
		return Status{}, err
	}
	status := Status{Bootstrapped: exists, Journal: JournalPath(r.Config), Steps: []StatusStep{}}
	var operationID string
	if exists {
		status.State = &state
		status.Active = state.Active
		status.Complete = !state.Active && state.CommitID == "" && !state.CompletedAt.IsZero()
		operationID = state.OperationID
		if state.CommitID != "" {
			status.Pending = true
			deadline := state.CommitDeadline
			status.CommitDeadline = &deadline
			status.RollbackRequired = !r.now().Before(deadline)
		}
	}
	journal, err := engine.LoadJournal(JournalPath(r.Config))
	if err != nil {
		return Status{}, err
	}
	for _, task := range journal.Tasks {
		if operationID != "" && task.OperationID != operationID {
			continue
		}
		detail := task.Error
		if r.Secrets != nil {
			detail = r.Secrets.Redact(detail)
		}
		status.Steps = append(status.Steps, StatusStep{
			ID: task.Step, Status: task.Status, Attempt: task.Attempt, Applied: task.AppliedAttempt,
			StartedAt: task.StartedAt, EndedAt: task.CompletedAt, Error: detail,
		})
	}
	sort.Slice(status.Steps, func(i, j int) bool { return status.Steps[i].ID < status.Steps[j].ID })
	return status, nil
}

func (r *Runner) registerSecrets(credentials Credentials) {
	if r.Secrets == nil {
		r.Secrets = &secrets.Registry{}
	}
	r.Secrets.Add(secretsForRedaction(r.Config, credentials)...)
}

// requireRoot is implemented per platform.
func requireRoot() error {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		return rootCheck()
	}
	return contract.NewFault(contract.ValidationFailed, "host bootstrap requires a Unix host", "Run bootstrap on a supported Debian or Ubuntu host.")
}
