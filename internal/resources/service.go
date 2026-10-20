package resources

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// Config describes one service instance. Host is the explicit application host
// whose Docker daemon this service may change. Runner is the only process
// boundary; tests inject a fake and production uses ExecRunner.
type Config struct {
	Host string
	// StateDir holds protected credential and restore-journal files.
	StateDir string
	// BackupDir is the dedicated directory destination for pg_dump archives.
	BackupDir string
	// Runner defaults to ExecRunner.
	Runner Runner
	// CommandTimeout bounds one external command. StartTimeout bounds waiting
	// for a newly started database to accept connections.
	CommandTimeout time.Duration
	StartTimeout   time.Duration
	PollInterval   time.Duration
	// Clock is optional and used for deterministic tests.
	Clock func() time.Time
	// Images maps an allowed PostgreSQL major version to a trusted image. The
	// caller cannot supply a free-form image.
	Images map[string]string
	// ControlPlaneResource and ControlPlaneDatabase identify the external
	// control plane even when a request forgets its protection marker.
	ControlPlaneResource string
	ControlPlaneDatabase string
}

// Service orchestrates PostgreSQL application resources on one explicit host.
type Service struct {
	config Config
	runner Runner
	images map[string]string
	clock  func() time.Time
}

var defaultImages = map[string]string{
	"14": "postgres:14",
	"15": "postgres:15",
	"16": "postgres:16",
	"17": "postgres:17",
}

func New(config Config) (*Service, error) {
	if strings.TrimSpace(config.Host) == "" {
		return nil, invalidResource("explicit application host is required", "Configure the service with the application host it owns.")
	}
	if !hostPattern.MatchString(config.Host) {
		return nil, invalidResource("invalid application host", "Use a bounded host identifier of letters, digits, dots, underscores, or hyphens.")
	}
	if strings.TrimSpace(config.StateDir) == "" {
		return nil, invalidResource("service state directory is required", "Configure a dedicated private directory for credentials and restore journals.")
	}
	if config.Runner == nil {
		config.Runner = ExecRunner{}
	}
	if config.CommandTimeout <= 0 {
		config.CommandTimeout = 2 * time.Minute
	}
	if config.StartTimeout <= 0 {
		config.StartTimeout = 90 * time.Second
	}
	if config.PollInterval <= 0 {
		config.PollInterval = time.Second
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	// Pin the service to the runner's verified node identity. Host is a
	// placement assertion, not a routing mechanism: the local ExecRunner cannot
	// claim to mutate a different machine merely because Host says so.
	identityCtx, identityCancel := context.WithTimeout(context.Background(), config.CommandTimeout)
	identity, identityErr := config.Runner.NodeIdentity(identityCtx)
	identityCancel()
	if identityErr != nil || strings.TrimSpace(identity) != config.Host {
		return nil, precondition(
			"configured host is not the runner's verified node identity",
			"Use the local Docker node name for this service. Remote hosts require a runner that verifies its own remote node; this local runner never routes by Host.",
		)
	}
	images := make(map[string]string, len(defaultImages))
	for version, image := range defaultImages {
		images[version] = image
	}
	for version, image := range config.Images {
		if !versionPattern.MatchString(version) || strings.TrimSpace(image) == "" {
			return nil, invalidResource("invalid image configuration", "Configure a supported two-digit PostgreSQL version with a nonempty trusted image.")
		}
		images[version] = image
	}
	if err := ensurePrivateDir(config.StateDir); err != nil {
		return nil, unavailable("could not prepare the service state directory", "Check permissions on the configured state directory.")
	}
	if config.BackupDir != "" {
		if err := ensurePrivateDir(config.BackupDir); err != nil {
			return nil, unavailable("could not prepare the backup directory", "Check permissions on the dedicated backup directory.")
		}
	}
	return &Service{config: config, runner: config.Runner, images: images, clock: config.Clock}, nil
}

func (s *Service) now() time.Time { return s.clock().UTC() }

func (s *Service) requireBackupDir() (string, error) {
	if strings.TrimSpace(s.config.BackupDir) == "" {
		return "", invalidResource("backup directory is not configured", "Configure a dedicated backup directory destination for backup, verify, and restore actions.")
	}
	if err := ensurePrivateDir(s.config.BackupDir); err != nil {
		return "", unavailable("backup directory is not usable", "Check permissions on the dedicated backup directory.")
	}
	return s.config.BackupDir, nil
}

// runCommand applies the per-command deadline and converts only start/deadline
// failures. A non-zero exit remains a CommandResult for the caller to classify.
func (s *Service) runCommand(ctx context.Context, step string, command Command) (CommandResult, error) {
	commandCtx, cancel := context.WithTimeout(ctx, s.config.CommandTimeout)
	defer cancel()
	result, err := s.runner.Run(commandCtx, command)
	if err != nil {
		return result, contextFailure(step, err)
	}
	if commandCtx.Err() != nil {
		return result, contextFailure(step, commandCtx.Err())
	}
	return result, nil
}

func (s *Service) docker(ctx context.Context, step string, args ...string) (CommandResult, error) {
	return s.runCommand(ctx, step, Command{Name: "docker", Args: args})
}

func (s *Service) containerExec(ctx context.Context, step string, spec Spec, args ...string) (CommandResult, error) {
	full := make([]string, 0, len(args)+2)
	full = append(full, "exec")
	full = append(full, containerName(spec))
	full = append(full, args...)
	return s.docker(ctx, step, full...)
}

func baseResult(action Action, spec Spec, state State) Result {
	return Result{
		Schema: Schema, Action: action, Resource: spec.Name, Host: spec.Host,
		Engine: spec.Engine, State: state, ObservedAt: time.Now().UTC(),
	}
}

// Plan validates the request and returns an ordered, non-mutating preview.
// Execute revalidates everything; the plan is not an authorization token.
func (s *Service) Plan(ctx context.Context, req Request) (Plan, error) {
	if err := validateRequest(req); err != nil {
		return Plan{}, err
	}
	spec, err := s.normalizeSpec(req.Spec)
	if err != nil {
		return Plan{}, err
	}
	if err := s.guardControlPlane(spec, req.Action); err != nil {
		return Plan{}, err
	}
	plan := Plan{
		Schema: Schema, Action: req.Action, Resource: spec.Name, Host: spec.Host,
		Engine: spec.Engine,
	}
	state, err := s.observeState(ctx, spec)
	if err != nil {
		return Plan{}, err
	}
	plan.CurrentState = state
	if _, err := os.Lstat(s.restoreJournalPath(spec.Name)); err == nil {
		plan.Warnings = append(plan.Warnings, "an interrupted restore journal exists; recover it before other mutations")
	}
	destructiveConfirmation := req.Action == ActionDestroy || req.Action == ActionRestore
	if destructiveConfirmation {
		plan.ConfirmationRequired = true
		plan.ConfirmationPhrase = spec.Name
	}
	switch req.Action {
	case ActionCreate:
		plan.Steps = []PlanStep{
			{ID: "validate", Description: "Validate the resource identity, version, explicit application host, endpoint, and credential references."},
			{ID: "volume", Description: "Create or verify the labelled named volume " + spec.Volume + "."},
			{ID: "container", Description: "Create or start the PostgreSQL container with the protected password file mounted read-only."},
			{ID: "health", Description: "Wait for pg_isready on the application host and publish the observed endpoint."},
		}
	case ActionHealth:
		plan.Steps = []PlanStep{{ID: "health", Description: "Inspect the owned container and run pg_isready. No mutation."}}
	case ActionDestroy:
		plan.Destructive = true
		plan.Steps = []PlanStep{
			{ID: "confirm", Description: "Require the exact resource name " + spec.Name + " and refuse the control-plane database.", Destructive: true},
			{ID: "container", Description: "Remove only the container whose ownership labels match this resource.", Destructive: true},
		}
		if req.Destroy.RemoveVolume {
			plan.Steps = append(plan.Steps, PlanStep{ID: "volume", Description: "Remove the labelled named volume and protected credential files (database data is deleted).", Destructive: true})
		} else {
			plan.Steps = append(plan.Steps, PlanStep{ID: "volume", Description: "Keep the named volume and credentials; data remains recoverable."})
		}
	case ActionBackup:
		dest, err := s.requireBackupDir()
		if err != nil {
			return Plan{}, err
		}
		plan.BackupDestination = dest
		plan.Steps = []PlanStep{
			{ID: "ready", Description: "Verify the owned container is accepting connections."},
			{ID: "dump", Description: "Run pg_dump --format=custom into a temporary file in the backup directory."},
			{ID: "checksum", Description: "Compute SHA-256, publish the archive atomically, and write its manifest."},
		}
	case ActionVerify:
		dest, err := s.requireBackupDir()
		if err != nil {
			return Plan{}, err
		}
		plan.BackupDestination = dest
		plan.Steps = []PlanStep{
			{ID: "checksum", Description: "Recompute and compare the archive SHA-256 with its manifest."},
			{ID: "list", Description: "Run pg_restore --list against the archive and require a nonzero table of contents."},
		}
	case ActionRestore:
		dest, err := s.requireBackupDir()
		if err != nil {
			return Plan{}, err
		}
		plan.Destructive = true
		plan.BackupDestination = dest
		plan.SafetyBackup = true
		plan.Steps = []PlanStep{
			{ID: "confirm", Description: "Require the exact resource name " + spec.Name + " and refuse the control-plane database.", Destructive: true},
			{ID: "verify", Description: "Verify the selected archive, its manifest, resource identity, checksum, and pg_restore listing."},
			{ID: "safety", Description: "Take and verify a safety backup before any target change."},
			{ID: "temporary", Description: "Restore into a temporary database next to the target."},
			{ID: "cutover", Description: "Terminate target connections, rename the old database aside, and rename the restored database into place.", Destructive: true},
			{ID: "health", Description: "Probe the restored database; on failure roll the previous database back.", Destructive: true},
		}
	case ActionRecover:
		plan.Steps = []PlanStep{{ID: "recover", Description: "Recover an interrupted restore from the durable journal, restoring the previous database when needed."}}
		plan.Warnings = append(plan.Warnings, "recovery changes database names to return to the pre-restore state")
	}
	if req.Action == ActionBackup && req.Backup != nil && req.Backup.Policy != nil {
		plan.Warnings = append(plan.Warnings, "backup policy is a declaration for the orchestrator; this action executes one backup only")
	}
	if req.Action == ActionRestore {
		plan.Warnings = append(plan.Warnings, "applications connected to the target database are terminated during cutover")
	}
	return plan, nil
}

// Execute performs one action. Confirmation is checked again here, before any
// mutation, even when the caller did not call Plan first. Mutating actions also
// require a stable operation/effect id and consult durable effect evidence
// before reapplying after a lost acknowledgement.
func (s *Service) Execute(ctx context.Context, req Request) (result Result, err error) {
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}
	spec, err := s.normalizeSpec(req.Spec)
	if err != nil {
		return Result{}, err
	}
	if err := s.guardControlPlane(spec, req.Action); err != nil {
		return Result{}, err
	}
	switch req.Action {
	case ActionDestroy:
		if !confirmed(req.Destroy.Confirmation, spec.Name) {
			return baseResult(req.Action, spec, StateUnknown), confirmationFault(spec.Name)
		}
	case ActionRestore:
		if !confirmed(req.Restore.Confirmation, spec.Name) {
			return baseResult(req.Action, spec, StateUnknown), confirmationFault(spec.Name)
		}
	}

	effect, completed, err := s.beginEffect(req, spec)
	if err != nil {
		return Result{}, err
	}
	if completed {
		return replayedResult(effect, req.OperationID)
	}
	if effect.EffectID != "" {
		req.EffectID = effect.EffectID
	}
	defer func() {
		if result.Schema != "" {
			result.OperationID = req.OperationID
			result.EffectID = effect.EffectID
			if result.EffectID == "" {
				result.EffectID = req.EffectID
			}
			if result.EffectID == "" {
				result.EffectID = req.OperationID
			}
		}
	}()

	if req.Action != ActionRestore && req.Action != ActionRecover && req.Action != ActionHealth && req.Action != ActionVerify {
		if err := s.requireNoRestoreJournal(spec); err != nil {
			return baseResult(req.Action, spec, StateUnknown), err
		}
	}
	switch req.Action {
	case ActionCreate:
		result, err = s.create(ctx, spec)
	case ActionHealth:
		result, err = s.health(ctx, spec)
	case ActionDestroy:
		result, err = s.destroy(ctx, spec, req.Destroy)
	case ActionBackup:
		result, err = s.backup(ctx, spec, req.Backup, effect)
	case ActionVerify:
		result, err = s.verify(ctx, spec, req.Verify)
	case ActionRestore:
		result, err = s.restore(ctx, spec, req.Restore, effect)
	case ActionRecover:
		result, err = s.recover(ctx, spec)
	default:
		return Result{}, invalidResource("unsupported resource action", "Use a supported action.")
	}
	if err != nil {
		return result, err
	}
	result.OperationID = req.OperationID
	result.EffectID = effect.EffectID
	if effect.EffectID != "" {
		if err := s.completeEffect(effect.EffectID, result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Service) requireNoRestoreJournal(spec Spec) error {
	if _, err := os.Lstat(s.restoreJournalPath(spec.Name)); err == nil {
		return precondition("an interrupted restore must be recovered first", "Run the recover action, or run restore again with its archive and exact confirmation.")
	} else if !os.IsNotExist(err) {
		return unavailable("restore journal cannot be inspected", "Check permissions on the service state directory.")
	}
	return nil
}

// observeState is read-only and returns an honest container observation.
func (s *Service) observeState(ctx context.Context, spec Spec) (State, error) {
	labels, exists, err := s.containerLabels(ctx, spec)
	if err != nil {
		return StateUnknown, err
	}
	if !exists {
		return StateAbsent, nil
	}
	if !s.labelsOwned(labels, spec) {
		return StateUnknown, conflict("container name belongs to a different resource", "Inspect the Docker container and use its real owner; this service will not adopt or mutate a foreign container.")
	}
	running, err := s.containerRunning(ctx, spec)
	if err != nil {
		return StateUnknown, err
	}
	if !running {
		return StateStopped, nil
	}
	return StateReady, nil
}

func (s *Service) publishedPort(ctx context.Context, spec Spec) (int, error) {
	result, err := s.docker(ctx, "inspect published port", "port", containerName(spec), "5432/tcp")
	if err != nil {
		return 0, err
	}
	if result.ExitCode != 0 {
		return 0, nil
	}
	for _, line := range strings.Split(strings.TrimSpace(string(result.Stdout)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		index := strings.LastIndex(line, ":")
		if index < 0 || index == len(line)-1 {
			continue
		}
		port, err := strconv.Atoi(line[index+1:])
		if err == nil && port > 0 && port <= 65535 {
			return port, nil
		}
	}
	return 0, nil
}

func (s *Service) endpoint(ctx context.Context, spec Spec) (*Endpoint, error) {
	port, err := s.publishedPort(ctx, spec)
	if err != nil {
		return nil, err
	}
	if port == 0 {
		return nil, nil
	}
	return &Endpoint{
		Node: spec.Host, Address: spec.Address, Port: port,
		Database: spec.Database, Username: spec.Username, SSLMode: "disable",
	}, nil
}

func resultStateForError(err error) State {
	if err == nil {
		return StateReady
	}
	switch contract.Classify(err).Code {
	case contract.Cancelled:
		return StateUnknown
	case contract.Unavailable:
		return StateDegraded
	default:
		return StateUnknown
	}
}

func joinWarnings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var _ = errors.Is
var _ = fmt.Sprintf
