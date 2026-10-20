package host

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
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

	// SkipRootCheck exists only for unit tests. Real apply is root-only.
	SkipRootCheck bool
}

// Plan is a read-only preview. It is also the source of the confirmation
// token required by Apply.
type Plan struct {
	Node         string       `json:"node"`
	Role         Role         `json:"role"`
	Platform     Platform     `json:"platform"`
	Supported    bool         `json:"supported"`
	ConfigHash   string       `json:"config_hash"`
	Generation   uint32       `json:"generation"`
	OperationID  string       `json:"operation_id"`
	Resumed      bool         `json:"resumed"`
	Ready        bool         `json:"ready"`
	AccessRisks  []string     `json:"access_risks,omitempty"`
	Steps        []PlanEntry  `json:"steps"`
	Confirmation Confirmation `json:"confirmation"`
	Journal      string       `json:"journal"`
	StateFile    string       `json:"state_file"`
	ApplyBlocked string       `json:"apply_blocked,omitempty"`
}

// ApplyReport is returned after a successful convergence. A failed apply is a
// Fault with the same identifying details in its Details map.
type ApplyReport struct {
	Node        string       `json:"node"`
	Role        Role         `json:"role"`
	OperationID string       `json:"operation_id"`
	Generation  uint32       `json:"generation"`
	Steps       []ApplyEntry `json:"steps"`
	Resumed     bool         `json:"resumed"`
	CompletedAt time.Time    `json:"completed_at"`
}

// ApplyEntry reports one step's engine outcome. Executed is false when a
// durable effect record from the same generation was recovered.
type ApplyEntry struct {
	ID       string `json:"id"`
	Complete bool   `json:"complete"`
	Executed bool   `json:"executed"`
	Detail   string `json:"detail,omitempty"`
}

// Status is the bootstrap state plus current drift observations.
type Status struct {
	Bootstrapped bool   `json:"bootstrapped"`
	Ready        bool   `json:"ready"`
	State        *State `json:"state,omitempty"`
	Plan         Plan   `json:"plan"`
}

// NewRunner validates the intent and fills production defaults.
func NewRunner(cfg Config, registry *secrets.Registry) (*Runner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
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
	return runner, nil
}

func (r *Runner) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func (r *Runner) steps(credentials Credentials) ([]Step, error) {
	role, err := r.Config.RoleConfig()
	if err != nil {
		return nil, err
	}
	steps := []Step{&platformStep{cfg: r.Config}, &packagesStep{cfg: r.Config, exec: r.Exec, role: role}, &usersStep{cfg: r.Config, exec: r.Exec, role: role}, &directoriesStep{cfg: r.Config, role: role}}
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
		steps = append(steps, &credentialsStep{cfg: r.Config, credentials: credentials})
	}
	steps = append(steps, &binaryStep{cfg: r.Config, role: role})
	if role.ControlPlane {
		steps = append(steps, newAPIService(r.Config, r.Exec), newWorkerService(r.Config, r.Exec))
		steps = append(steps, &readinessStep{cfg: r.Config, exec: r.Exec, probe: r.Probe, token: credentials.Operator})
	}
	if role.NodeAgent {
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
			entry.Error = fmt.Sprintf("%s is not in the verified matrix (Debian 12/13, Ubuntu 22.04/24.04; amd64/arm64)", platform)
		}
		plan.Steps = []PlanEntry{entry}
		return plan, nil
	}
	plan.Supported = true
	if err := accessGate(r.Config); err != nil {
		plan.ApplyBlocked = safeError(err)
	}
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
		}
		plan.Steps = append(plan.Steps, entry)
	}
	risks := r.Config.AccessRisks()
	plan.AccessRisks = risks
	plan.ConfigHash = configHash
	plan.Generation = projected.Generation
	plan.OperationID = projected.OperationID
	plan.Resumed = exists && state.Active && state.OperationID == projected.OperationID
	plan.Ready = ready
	confirmation, err := issueConfirmation("host.apply", r.Config.Node+"/"+string(r.Config.Role), configHash, planInputs(configHash, projected.Generation, risks), r.now())
	if err != nil {
		return Plan{}, err
	}
	plan.Confirmation = confirmation
	return plan, nil
}

// Apply converges the host after validating the exact plan token. The engine
// journal is written before the database step, so an interrupted first contact
// resumes without the platform store.
func (r *Runner) Apply(ctx context.Context, confirmation string, acceptAccessRisk bool) (ApplyReport, error) {
	if err := r.Config.Validate(); err != nil {
		return ApplyReport{}, err
	}
	if err := accessGate(r.Config); err != nil {
		return ApplyReport{}, err
	}
	if !r.SkipRootCheck {
		if err := requireRoot(); err != nil {
			return ApplyReport{}, err
		}
	}
	risks := r.Config.AccessRisks()
	// Kept for adapter compatibility, never a bypass for the access gate.
	_ = acceptAccessRisk
	configHash, err := r.Config.ConfigHash()
	if err != nil {
		return ApplyReport{}, err
	}
	projected, err := projectState(r.Config, configHash, r.now())
	if err != nil {
		return ApplyReport{}, err
	}
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
	if err := accessGate(r.Config); err != nil {
		return ApplyReport{}, err
	}
	projected, err = projectState(r.Config, configHash, r.now())
	if err != nil {
		return ApplyReport{}, err
	}
	if err := verifyConfirmation(confirmation, "host.apply", r.Config.Node+"/"+string(r.Config.Role), configHash, planInputs(configHash, projected.Generation, risks), r.now()); err != nil {
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
	report := ApplyReport{Node: r.Config.Node, Role: r.Config.Role, OperationID: state.OperationID, Generation: state.Generation}
	for _, step := range steps {
		execution := engine.Execution{OperationID: contract.OperationID(state.OperationID), Attempt: state.Generation, Step: step.ID(), RequestID: fmt.Sprintf("bootstrap-%d", state.Generation)}
		status, runErr := hostEngine.Run(ctx, step, execution, nil)
		if runErr != nil {
			return report, r.applyFault(step, execution, status, report, runErr)
		}
		entry := ApplyEntry{ID: step.ID(), Complete: status.Status == engine.StatusCompleted, Executed: status.Executed, Detail: status.Detail}
		report.Steps = append(report.Steps, entry)
		if !entry.Complete {
			return report, contract.NewFault(contract.Internal, "host step did not complete", "Inspect the bootstrap journal and re-run apply with a fresh plan.")
		}
	}
	if err := finishState(r.Config, state.OperationID, r.now()); err != nil {
		return report, err
	}
	report.CompletedAt = r.now().UTC()
	return report, nil
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

// Status reads the bootstrap state and observes drift. It never mutates.
func (r *Runner) Status(ctx context.Context) (Status, error) {
	state, exists, err := loadState(r.Config)
	if err != nil {
		return Status{}, err
	}
	plan, err := r.Plan(ctx)
	if err != nil {
		return Status{}, err
	}
	status := Status{Bootstrapped: exists, Ready: exists && plan.Ready, Plan: plan}
	if exists {
		status.State = &state
	}
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
