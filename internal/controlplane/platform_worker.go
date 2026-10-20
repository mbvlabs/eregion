package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

const platformMaxAttempts = 8

// platformStepTotal is the progress denominator. It is deliberately larger than
// any current pipeline and is informational only; completion depends on the
// actual step records, never this number.
const platformStepTotal = 16

type platformWorker struct {
	river.WorkerDefaults[platformArgs]
	service *Service
}

func (*platformWorker) Timeout(*river.Job[platformArgs]) time.Duration { return 30 * time.Minute }

func (*platformWorker) NextRetry(job *river.Job[platformArgs]) time.Time {
	delay := time.Duration(job.Attempt) * 2 * time.Second
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return time.Now().Add(delay)
}

func (w *platformWorker) Work(ctx context.Context, job *river.Job[platformArgs]) error {
	attempt := uint32(job.Attempt)
	op, raw, claimed, err := w.service.claimPlatform(ctx, job.Args.OperationID, attempt)
	if err != nil {
		return err
	}
	if !claimed {
		return w.service.waitForNewerAttempt(ctx, op.ID, attempt)
	}
	if op.State == contract.Cancelling {
		return w.service.cancelPlatform(ctx, op.ID, attempt, "Cancellation requested before execution started.")
	}
	normalized, err := platform.Default().Normalize(op.Kind, op.Target, raw)
	if err != nil {
		return w.settle(ctx, op.ID, attempt, err)
	}

	releaseLock, err := w.service.lockTarget(ctx, normalized.Target.String())
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	defer releaseLock()

	if err := w.service.checkCancelled(ctx, op.ID); err != nil {
		return w.settle(ctx, op.ID, attempt, err)
	}
	if executor := normalized.Executor(); executor != nil {
		return w.runExtension(ctx, op.ID, attempt, normalized, executor)
	}
	switch normalized.Kind {
	case platform.KindAppCreate, platform.KindNodeRegister, platform.KindEnvironmentCreate:
		return w.runRecordAction(ctx, op.ID, attempt, normalized)
	case platform.KindNodeCheck:
		return w.runNodeCheck(ctx, op.ID, attempt, normalized)
	case platform.KindReleaseCreate:
		return w.runRelease(ctx, op.ID, attempt, normalized)
	case platform.KindDeploy, platform.KindRollback:
		return w.runDeployment(ctx, op.ID, attempt, normalized)
	default:
		return w.settle(ctx, op.ID, attempt, contract.NewFault(contract.Internal, "platform action has no worker path", "This is a binary/registration mismatch; do not resubmit accepted work."))
	}
}

func (s *Service) claimPlatform(ctx context.Context, id contract.OperationID, attempt uint32) (contract.Operation, json.RawMessage, bool, error) {
	var raw json.RawMessage
	op, claimed, err := s.mutateOwnedOrClaim(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State == contract.Queued {
			if err := transition(ctx, tx, op, contract.Running, "", "Platform worker started durable execution."); err != nil {
				return err
			}
		}
		// The claim persists the attempt: every later fenced write compares
		// against it, so a claim that left it at 0 could never finish.
		if err := tx.QueryRow(ctx, `UPDATE eregion_operations SET attempt=$2 WHERE id=$1 RETURNING input`, id, op.Attempt).Scan(&raw); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
            SET state='running', updated_at=clock_timestamp()
            WHERE operation_id=$1 AND state IN ('pending','running')`, id)
		if err != nil {
			return err
		}
		return appendEvent(ctx, tx, op, contract.Event{Kind: "step", Stage: "platform", Message: "Platform execution attempt started under the target lock."})
	})
	return op, raw, claimed, err
}

var errPlatformCancelled = errors.New("platform operation cancelled at a safe boundary")

func (w *platformWorker) settle(ctx context.Context, id contract.OperationID, attempt uint32, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errOperationTerminal) {
		return nil
	}
	if errors.Is(err, errNotOwned) {
		return w.service.waitForNewerAttempt(ctx, id, attempt)
	}
	if errors.Is(err, errPlatformCancelled) {
		return w.service.cancelPlatform(ctx, id, attempt, "Cancellation acknowledged at a step boundary; completed effects were not rolled back.")
	}
	fault := contract.Classify(err)
	switch fault.Code {
	case contract.Unavailable:
		return err
	case contract.Cancelled:
		return w.service.cancelPlatform(ctx, id, attempt, fault.Message)
	case contract.ValidationFailed, contract.PreconditionFailed, contract.NotFound, contract.Forbidden, contract.Conflict:
		if failErr := w.service.failPlatformOperation(ctx, id, attempt, fault.Code, fault.Message); failErr != nil {
			return failErr
		}
		return nil
	default:
		return err
	}
}

// lockTarget serializes every operation for one canonical target across control
// plane processes. It includes extension actions and node targets, not just
// environment built-ins. A session lock is released automatically if the worker
// process dies, so it cannot wedge a target permanently.
func (s *Service) lockTarget(ctx context.Context, target string) (func(), error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, unavailable()
	}
	sum := sha256.Sum256([]byte(target))
	key := int64(binary.BigEndian.Uint64(sum[:8]))
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		conn.Release()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, unavailable()
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		conn.Release()
	}, nil
}

func (w *platformWorker) runRecordAction(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	return w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if normalized.Kind == platform.KindEnvironmentCreate {
			input := normalized.Input.(*platform.EnvironmentCreateInput)
			return insertPlatformChange(ctx, tx, op, "environment.created", input.App, input.Name,
				"Created environment "+input.Name+" on node "+input.Node,
				map[string]any{"node": input.Node, "bind": input.Bind, "container_port": input.ContainerPort},
				map[string]any{"host_port": nil, "serving_release_id": nil})
		}
		if normalized.Kind == platform.KindAppCreate {
			input := normalized.Input.(*platform.AppCreateInput)
			return insertPlatformChange(ctx, tx, op, "app.created", input.Name, "",
				"Created application "+input.Name, map[string]any{"app": input.Name}, nil)
		}
		// Node registration records declared facts only. It is not enrollment or
		// verification; node.check performs an authenticated transport check.
		return nil
	})
}

func (w *platformWorker) runNodeCheck(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	input := normalized.Input.(*platform.NodeCheckInput)
	n, err := w.service.node(ctx, input.Node)
	if err != nil {
		return err
	}
	if err := w.service.checkNodeIdentity(n); err != nil {
		return err
	}
	if _, err := w.runStep(ctx, id, attempt, "platform-node-01-v1", platform.ArgvPayload{
		Step: "platform-node-01-v1", Argv: []string{"docker", "version"}, TimeoutSeconds: 30,
	}, nil); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if _, err := w.runStep(ctx, id, attempt, "platform-node-02-v1", platform.ArgvPayload{
		Step: "platform-node-02-v1", Argv: []string{"pack", "version"}, AllowFailure: true, TimeoutSeconds: 30,
	}, nil); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		_, err := tx.Exec(ctx, `UPDATE eregion_platform_nodes SET status='ready',last_seen_at=clock_timestamp(),updated_at=clock_timestamp() WHERE name=$1`, input.Node)
		return err
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

func (w *platformWorker) runRelease(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	input := normalized.Input.(*platform.ReleaseCreateInput)
	env, err := w.service.environment(ctx, input.App, input.Environment)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	n, err := w.service.node(ctx, env.Node)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	role, capability := platform.RoleApplication, platform.CapabilityDocker
	if input.Source.Type != "image" {
		role, capability = platform.RoleBuilder, platform.CapabilityBuildpacks
	}
	if err := w.service.checkPlacement(n, role, capability); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	release, err := w.service.releaseForOperation(ctx, id)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	var result json.RawMessage
	if input.Source.Type == "image" {
		if input.Pull {
			if _, err := w.runStep(ctx, id, attempt, "platform-pull-01-v1", platform.ArgvPayload{
				Step: "platform-pull-01-v1", Argv: []string{"docker", "pull", release.Image}, TimeoutSeconds: 600,
			}, nil); err != nil {
				return w.settle(ctx, id, attempt, err)
			}
		}
		result, err = w.runStep(ctx, id, attempt, "platform-image-01-v1", platform.ArgvPayload{
			Step:           "platform-image-01-v1",
			Argv:           []string{"docker", "image", "inspect", "--format", "{{.Id}}", release.Image},
			TimeoutSeconds: 30,
		}, nil)
		if err != nil {
			return w.settle(ctx, id, attempt, err)
		}
	} else {
		path := input.Source.Path
		if input.Source.Type == "directory" {
			path, err = platform.ResolveBuildSource(path, w.service.buildSourceRoots())
			if err != nil {
				return w.settle(ctx, id, attempt, err)
			}
		}
		build := input.Build
		if build == nil {
			build = &platform.Build{Builder: platform.DefaultBuilder}
		}
		if _, err := w.runStep(ctx, id, attempt, "platform-pack-01-v1", platform.PackPayload{
			Step: "platform-pack-01-v1", Image: release.Image, Path: path,
			GitURL: input.Source.URL, GitRef: input.Source.Ref,
			Builder: build.Builder, Env: build.Env, TimeoutSeconds: 900,
		}, nil); err != nil {
			return w.settle(ctx, id, attempt, err)
		}
		result, err = w.runStep(ctx, id, attempt, "platform-image-01-v1", platform.ArgvPayload{
			Step:           "platform-image-01-v1",
			Argv:           []string{"docker", "image", "inspect", "--format", "{{.Id}}", release.Image},
			TimeoutSeconds: 30,
		}, nil)
		if err != nil {
			return w.settle(ctx, id, attempt, err)
		}
	}
	imageID := stepStdout(result)
	if !strings.HasPrefix(imageID, "sha256:") || len(imageID) != 71 {
		return w.settle(ctx, id, attempt, contract.NewFault(contract.PreconditionFailed, "image inspection did not return an immutable image ID", "Inspect the node image and release operation before retrying."))
	}
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_releases
            SET status='ready',image_id=$2,completed_at=clock_timestamp(),error='',error_code=''
            WHERE id=$1`, release.ID, imageID); err != nil {
			return err
		}
		return insertPlatformChange(ctx, tx, op, "release.ready", release.App, release.Environment,
			"Release "+string(release.ID)+" is ready from "+release.Source.Type+" source.",
			map[string]any{"release_id": release.ID, "image": release.Image},
			map[string]any{"release_id": release.ID, "image": release.Image, "image_id": imageID})
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

func stepStdout(raw json.RawMessage) string {
	var value struct {
		Stdout string `json:"stdout"`
	}
	_ = json.Unmarshal(raw, &value)
	return strings.TrimSpace(value.Stdout)
}

func (w *platformWorker) runDeployment(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	var dep platform.Deployment
	var env platform.Environment
	var release platform.Release
	var previous platform.Release
	var err error
	// The deployment row freezes the rollback target at acceptance. Recomputing
	// a default rollback on retry could silently select a different release.
	dep, env, release, previous, err = w.service.deploymentPlan(ctx, id)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	secrets, err := w.service.resolveEnvironmentSecrets(ctx, env)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	candidateName := platform.CandidateContainerName(env.ID, id)
	servingName := env.ID.Container("serving")
	candidateStarted := false
	defer func() {
		state, _, stateErr := w.service.operationState(context.WithoutCancel(ctx), id)
		if candidateStarted && stateErr == nil && state.Terminal() {
			// Cleanup is a bounded, idempotent node effect after the terminal
			// result. Never remove a candidate while a retry may still need it.
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			defer cancel()
			execution := engine.Execution{OperationID: id, Attempt: attempt, Step: "platform-cleanup-01-v1", RequestID: newRequestID()}
			raw, _ := json.Marshal(platform.ArgvPayload{Step: execution.Step, Argv: []string{"docker", "rm", "-f", candidateName}, AllowFailure: true, TimeoutSeconds: 30})
			_, _ = w.service.transport.Call(cleanupCtx, node.Request{Method: node.MethodRun, Execution: execution, RequestID: execution.RequestID, Payload: raw}, nil)
		}
	}()

	if err := w.service.checkCancelled(ctx, id); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if release.Command != nil {
		commandArgs := []string{"docker", "run", "--rm", "--name", candidateName + "-cmd", "--pull", "never"}
		envKeys := sortedPlatformKeys(env.Env)
		for _, key := range envKeys {
			commandArgs = append(commandArgs, "-e", key)
		}
		for _, key := range sortedPlatformKeys(env.SecretRefs) {
			commandArgs = append(commandArgs, "-e", key)
		}
		commandArgs = append(commandArgs, release.ImageID)
		commandArgs = append(commandArgs, release.Command.Argv...)
		secretKeys := sortedPlatformKeys(env.SecretRefs)
		if _, err := w.runStep(ctx, id, attempt, "platform-command-01-v1", platform.ArgvPayload{
			Step: "platform-command-01-v1", Argv: commandArgs,
			Env: env.Env, SecretEnvKeys: secretKeys, TimeoutSeconds: release.Command.TimeoutSeconds,
		}, secrets); err != nil {
			return w.settle(ctx, id, attempt, err)
		}
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return w.settle(ctx, id, attempt, err)
	}

	candidatePayload := platform.ContainerPayload{
		Step: "platform-container-01-v1", Name: candidateName, Image: release.ImageID,
		ContainerPort: env.ContainerPort, HostPort: env.CandidatePort, Bind: env.Bind,
		Env: env.Env, SecretEnvKeys: sortedPlatformKeys(env.SecretRefs), Restart: "no",
		Labels: map[string]string{
			"io.eregion.app": env.App, "io.eregion.environment": env.Name,
			"io.eregion.release": string(release.ID), "io.eregion.role": "candidate",
		},
	}
	candidateStarted = true // docker may have created a container even on error.
	if _, err := w.runStep(ctx, id, attempt, "platform-container-01-v1", candidatePayload, secrets); err != nil {
		return w.settle(ctx, id, attempt, err)
	}

	if _, err := w.runStep(ctx, id, attempt, "platform-health-01-v1", platform.HealthPayload{
		Step: "platform-health-01-v1", Type: env.Health.Type, Target: platformHealthTarget(env, env.CandidatePort),
		TimeoutSeconds: env.Health.TimeoutSeconds, IntervalMS: 500,
	}, nil); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return w.settle(ctx, id, attempt, err)
	}

	servingPayload := platform.ContainerPayload{
		Step: "platform-container-02-v1", Name: servingName, Image: release.ImageID,
		ContainerPort: env.ContainerPort, HostPort: env.HostPort, Bind: env.Bind,
		Env: env.Env, SecretEnvKeys: sortedPlatformKeys(env.SecretRefs), Restart: "unless-stopped",
		Labels: map[string]string{
			"io.eregion.app": env.App, "io.eregion.environment": env.Name,
			"io.eregion.release": string(release.ID), "io.eregion.role": "serving",
		},
	}
	if previous.Image != "" {
		servingPayload.FallbackImage = previous.ImageID
	}
	if _, err := w.runStep(ctx, id, attempt, "platform-container-02-v1", servingPayload, secrets); err != nil {
		return w.settle(ctx, id, attempt, err)
	}

	_, verifyErr := w.runStep(ctx, id, attempt, "platform-health-02-v1", platform.HealthPayload{
		Step: "platform-health-02-v1", Type: env.Health.Type, Target: platformHealthTarget(env, env.HostPort),
		TimeoutSeconds: env.Health.TimeoutSeconds, IntervalMS: 500,
	}, nil)
	if verifyErr != nil {
		// The candidate was healthy but the serving container is not. Restore the
		// previous release before reporting failure, so a failed verify never
		// leaves the environment stranded on an unverified release.
		if previous.Image != "" {
			restore := platform.ContainerPayload{
				Step: "platform-container-03-v1", Name: servingName, Image: previous.ImageID,
				ContainerPort: env.ContainerPort, HostPort: env.HostPort, Bind: env.Bind,
				Env: env.Env, SecretEnvKeys: sortedPlatformKeys(env.SecretRefs), Restart: "unless-stopped",
				Labels: map[string]string{
					"io.eregion.app": env.App, "io.eregion.environment": env.Name,
					"io.eregion.release": string(previous.ID), "io.eregion.role": "serving",
				},
			}
			if _, restoreErr := w.runStep(ctx, id, attempt, "platform-container-03-v1", restore, secrets); restoreErr == nil {
				_, _ = w.runStep(ctx, id, attempt, "platform-health-03-v1", platform.HealthPayload{
					Step: "platform-health-03-v1", Type: env.Health.Type, Target: platformHealthTarget(env, env.HostPort),
					TimeoutSeconds: env.Health.TimeoutSeconds, IntervalMS: 500,
				}, nil)
				verifyErr = fmt.Errorf("%w; the previous release %s was restored", verifyErr, previous.ID)
			}
		}
		return w.settle(ctx, id, attempt, verifyErr)
	}
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
            SET state='succeeded',completed_at=$2,updated_at=$2,message=$3,error_code=''
            WHERE operation_id=$1 AND state IN ('pending','running')`, id, now, "Serving release "+string(release.ID)+" passed health verification."); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments
            SET serving_release_id=$2,applied_release_id=$2,health_state='healthy',health_detail='serving release passed health verification',health_checked_at=$3,updated_at=$3
            WHERE id=$1`, env.ID, release.ID, now); err != nil {
			return err
		}
		return insertPlatformChange(ctx, tx, op, string(dep.Action)+".succeeded", env.App, env.Name,
			"Serving release "+string(release.ID)+" for "+env.App+"/"+env.Name+".",
			map[string]any{"release_id": release.ID, "image": release.Image},
			map[string]any{"serving_release_id": release.ID, "image": release.Image, "health": "healthy"})
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

// platformHealthTarget always probes loopback. A container bound to 0.0.0.0 is
// reachable there; a bind to one specific external address is not, which is an
// honest unsupported placement rather than an unbounded network probe.
func platformHealthTarget(env platform.Environment, port int) string {
	if env.Health.Type == "tcp" {
		return fmt.Sprintf("127.0.0.1:%d", port)
	}
	path := env.Health.Path
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
}

func sortedPlatformKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	// Local deterministic sort avoids importing another package's helper.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func (s *Service) resolveEnvironmentSecrets(ctx context.Context, env platform.Environment) (map[string]string, error) {
	if len(env.SecretRefs) == 0 {
		return nil, nil
	}
	if s.secrets == nil {
		return nil, contract.NewFault(contract.PreconditionFailed, "secret injection is not configured", "Set EREGION_VAULT_KEY_FILE on every control-plane worker and pin the secret references before deploying.")
	}
	var values map[string]string
	var err error
	if scoped, ok := s.secrets.(EnvironmentSecretResolver); ok {
		values, err = scoped.ResolveEnvironment(ctx, env.App, env.Name, env.SecretRefs)
	} else {
		values, err = s.secrets.Resolve(ctx, env.SecretRefs)
	}
	if err != nil {
		return nil, contract.NewFault(contract.Unavailable, "environment secrets could not be resolved", "Check the vault agent and reference revisions, then retry the deployment.")
	}
	out := make(map[string]string, len(env.SecretRefs))
	for key := range env.SecretRefs {
		value := values[key]
		if value == "" {
			return nil, contract.NewFault(contract.PreconditionFailed, "a referenced environment secret is missing", "Resolve every configured secret reference before deploying.")
		}
		out[key] = value
	}
	return out, nil
}

func (s *Service) deploymentPlan(ctx context.Context, operationID contract.OperationID) (platform.Deployment, platform.Environment, platform.Release, platform.Release, error) {
	var dep platform.Deployment
	err := s.pool.QueryRow(ctx, `SELECT `+deploymentColumns+` FROM eregion_platform_deployments d
        JOIN eregion_platform_apps a ON a.id=d.app_id
        JOIN eregion_platform_environments e ON e.id=d.environment_id
        WHERE d.operation_id=$1`, operationID).Scan(&dep.ID, &dep.OperationID, &dep.App, &dep.Environment, &dep.Action, &dep.ReleaseID, &dep.PreviousReleaseID, &dep.State, &dep.ErrorCode, &dep.Message, &dep.CreatedAt, &dep.UpdatedAt, &dep.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return dep, platform.Environment{}, platform.Release{}, platform.Release{}, contract.NewFault(contract.NotFound, "deployment record is missing", "Do not retry an operation that has no durable deployment row.")
	}
	if err != nil {
		return dep, platform.Environment{}, platform.Release{}, platform.Release{}, unavailable()
	}
	env, err := s.environment(ctx, dep.App, dep.Environment)
	if err != nil {
		return dep, env, platform.Release{}, platform.Release{}, err
	}
	// Capture the actual predecessor after acquiring the distributed target
	// lock, not the potentially stale serving value seen at submission. Once
	// steps exist, keep the plan stable for journal recovery.
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_deployments SET previous_release_id=$2
        WHERE operation_id=$1 AND NOT EXISTS (SELECT 1 FROM eregion_operation_steps WHERE operation_id=$1)`, operationID, nullableReleaseID(env.ServingRelease)); err != nil {
		return dep, env, platform.Release{}, platform.Release{}, unavailable(err)
	}
	var previousID string
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(previous_release_id,'') FROM eregion_platform_deployments WHERE operation_id=$1`, operationID).Scan(&previousID); err != nil {
		return dep, env, platform.Release{}, platform.Release{}, unavailable(err)
	}
	dep.PreviousReleaseID = platform.ReleaseID(previousID)
	release, err := s.releaseByID(ctx, dep.ReleaseID)
	if err != nil {
		return dep, env, release, platform.Release{}, err
	}
	if release.App != env.App || release.Environment != env.Name || release.Status != platform.ReleaseReady {
		return dep, env, release, platform.Release{}, contract.NewFault(contract.PreconditionFailed, "release is not ready for this environment", "Wait for the release operation to succeed, then retry the deployment.")
	}
	n, err := s.node(ctx, env.Node)
	if err != nil {
		return dep, env, release, platform.Release{}, err
	}
	if err := s.checkPlacement(n, platform.RoleApplication, platform.CapabilityDocker); err != nil {
		return dep, env, release, platform.Release{}, err
	}
	var previous platform.Release
	if dep.PreviousReleaseID != "" {
		previous, err = s.releaseByID(ctx, dep.PreviousReleaseID)
		if err != nil {
			return dep, env, release, platform.Release{}, err
		}
	}
	return dep, env, release, previous, nil
}

const deploymentColumns = `d.id,d.operation_id,a.name,e.name,d.action,d.release_id,coalesce(d.previous_release_id,''),d.state,d.error_code,d.message,d.created_at,d.updated_at,d.completed_at`

func (s *Service) releaseForOperation(ctx context.Context, operationID contract.OperationID) (platform.Release, error) {
	var releaseID platform.ReleaseID
	err := s.pool.QueryRow(ctx, `SELECT id FROM eregion_platform_releases WHERE operation_id=$1`, operationID).Scan(&releaseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.Release{}, contract.NewFault(contract.NotFound, "release record is missing", "Do not retry an operation that has no durable release row.")
	}
	if err != nil {
		return platform.Release{}, unavailable()
	}
	return s.releaseByID(ctx, releaseID)
}

func (w *platformWorker) runExtension(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized, executor platform.Executor) error {
	op, _, err := w.service.operationSnapshot(ctx, id)
	if err != nil {
		return err
	}
	result, err := executor.Execute(ctx, platform.Execution{
		OperationID: id, Attempt: attempt, Kind: normalized.Kind, Target: normalized.Target.String(),
		Actor: op.Actor, Input: normalized.Canonical,
		Report: func(event platform.Event) {
			message := strings.TrimSpace(event.Message)
			if message == "" {
				return
			}
			if len(message) > 4096 {
				message = message[:4096]
			}
			kind := "log"
			if event.Level == "warning" {
				kind = "warning"
			}
			_, _, _ = w.service.mutateOwned(context.WithoutCancel(ctx), id, attempt, func(tx pgx.Tx, current *contract.Operation) error {
				return appendEvent(ctx, tx, current, contract.Event{Kind: kind, Stage: event.Stage, Message: message})
			})
		},
	})
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	message := strings.TrimSpace(result.Message)
	if message == "" {
		message = normalized.Summary + " completed."
	}
	if len(message) > 4096 {
		message = message[:4096]
	}
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		return nil
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	// finishPlatformOperation appends a generic success event; record the
	// executor's redacted summary as a separate observation.
	_, _, _ = w.service.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		return appendEvent(ctx, tx, current, contract.Event{Kind: "log", Message: message})
	})
	return nil
}

func (s *Service) operationSnapshot(ctx context.Context, id contract.OperationID) (contract.Operation, json.RawMessage, error) {
	op, err := scanOperation(s.pool.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", id))
	if err != nil {
		return op, nil, unavailable()
	}
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT input FROM eregion_operations WHERE id=$1`, id).Scan(&raw); err != nil {
		return op, nil, unavailable()
	}
	return op, raw, nil
}

// checkNodeIdentity requires the authenticated transport identity to match the
// node record. Registration alone is never proof of identity.
func (s *Service) checkNodeIdentity(n platform.Node) error {
	identity, known := s.transportIdentity()
	if !known {
		if s.transport != nil {
			return contract.NewFault(contract.PreconditionFailed, "node transport identity is not pinned", "Configure the transport's expected node name before checking a node.")
		}
		return nil
	}
	if identity != n.Name {
		return contract.NewFault(contract.PreconditionFailed, "node identity does not match the authenticated transport", fmt.Sprintf("The transport identifies %q, not %q. Hostnames are not authorization.", identity, n.Name))
	}
	return nil
}

func (s *Service) checkCancelled(ctx context.Context, id contract.OperationID) error {
	state, _, err := s.operationState(ctx, id)
	if err != nil {
		return err
	}
	if state == contract.Cancelling {
		return errPlatformCancelled
	}
	if state.Terminal() {
		return errOperationTerminal
	}
	return nil
}

func (s *Service) finishPlatformOperation(ctx context.Context, id contract.OperationID, attempt uint32, apply func(pgx.Tx, *contract.Operation) error) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		if err := apply(tx, op); err != nil {
			return err
		}
		return transition(ctx, tx, op, contract.Succeeded, "", "Platform action succeeded; domain state and history are durable.")
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func (s *Service) failPlatformOperation(ctx context.Context, id contract.OperationID, attempt uint32, code contract.Code, message string) error {
	if len(message) > 4096 {
		message = message[:4096]
	}
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_releases SET status='failed',error_code=$2,error=$3,completed_at=clock_timestamp()
            WHERE operation_id=$1 AND status='pending'`, id, string(code), message); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
            SET state='failed',error_code=$2,message=$3,completed_at=clock_timestamp(),updated_at=clock_timestamp()
            WHERE operation_id=$1 AND state IN ('pending','running')`, id, string(code), message); err != nil {
			return err
		}
		var action, app, environment, releaseID string
		if err := tx.QueryRow(ctx, `SELECT d.action,a.name,e.name,d.release_id
            FROM eregion_platform_deployments d
            JOIN eregion_platform_apps a ON a.id=d.app_id
            JOIN eregion_platform_environments e ON e.id=d.environment_id
            WHERE d.operation_id=$1`, id).Scan(&action, &app, &environment, &releaseID); err == nil {
			if changeErr := insertPlatformChange(ctx, tx, op, action+".failed", app, environment,
				"Deployment failed: "+message,
				map[string]any{"release_id": releaseID},
				map[string]any{"error_code": code, "serving_unchanged": true}); changeErr != nil {
				return changeErr
			}
		}
		if err := markIncompleteSteps(ctx, tx, id, code, message); err != nil {
			return err
		}
		return transition(ctx, tx, op, contract.Failed, code, message)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func (s *Service) cancelPlatform(ctx context.Context, id contract.OperationID, attempt uint32, message string) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
            SET state='cancelled',message=$2,completed_at=clock_timestamp(),updated_at=clock_timestamp()
            WHERE operation_id=$1 AND state IN ('pending','running')`, id, message); err != nil {
			return err
		}
		if err := markIncompleteSteps(ctx, tx, id, contract.Cancelled, message); err != nil {
			return err
		}
		return transition(ctx, tx, op, contract.Canceled, "", message)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

// runStep mirrors the diagnostic worker's durable effect protocol for a generic
// platform payload. Database acknowledgement is attempt-fenced; node status is
// always consulted before a retry dispatches another effect.
func (w *platformWorker) runStep(ctx context.Context, id contract.OperationID, attempt uint32, stepID string, payload any, secrets map[string]string) (json.RawMessage, error) {
	if w.service.transport == nil {
		return nil, contract.NewFault(contract.Unavailable, "node transport is not configured", "Run this work in a worker configured with a node transport.")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	exec := engine.Execution{OperationID: id, Attempt: attempt, Step: stepID, RequestID: newRequestID()}
	step, err := node.DefaultRegistry().Build(exec, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid platform step: %v", engine.ErrPermanent, err)
	}
	inputs, err := step.Inputs()
	if err != nil {
		return nil, err
	}
	inputsHash := engine.InputsHash(step.ID(), inputs)
	record, hasRecord, err := w.service.stepRecord(ctx, id, stepID)
	if err != nil {
		return nil, err
	}
	if hasRecord {
		if record.InputsHash != inputsHash {
			return nil, contract.NewFault(contract.PreconditionFailed, "step inputs changed for an existing effect", "Never reuse an operation ID with different inputs; inspect the node journal.")
		}
		if record.State == "completed" {
			return record.Result, nil
		}
	}
	status, err := w.service.transport.Call(ctx, node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec}, nil)
	if err != nil {
		if errors.Is(err, engine.ErrJournalCorrupt) {
			return nil, contract.NewFault(contract.PreconditionFailed, "node journal is corrupt", "Inspect the node journal before retrying.")
		}
		if node.IsPermanentTransport(err) {
			return nil, contract.NewFault(contract.PreconditionFailed, "the node transport refused the request", "Check the pinned node identity and host configuration.")
		}
		return nil, fmt.Errorf("node status %s: %w", stepID, err)
	}
	if status.InputsHash != "" && status.InputsHash != inputsHash {
		return nil, contract.NewFault(contract.PreconditionFailed, "node effect has different inputs", "Do not acknowledge an effect recorded under other inputs.")
	}
	if status.Status == engine.StatusCompleted {
		if !hasRecord {
			if _, err := w.service.beginStep(ctx, id, attempt, stepID, inputsHash, status.ExecutionID); err != nil {
				return nil, err
			}
		}
		if err := w.service.recordRecoveredStep(ctx, id, attempt, stepID, inputsHash, status); err != nil {
			return nil, err
		}
		if _, err := w.service.ackStep(ctx, id, attempt, stepID, inputsHash, status.ExecutionID, status.Result, platformStepTotal); err != nil {
			return nil, err
		}
		return status.Result, nil
	}
	already, err := w.service.beginStep(ctx, id, attempt, stepID, inputsHash, exec.ExecutionID())
	if err != nil {
		return nil, err
	}
	if already {
		return record.Result, nil
	}
	var events []node.Event
	response, err := w.service.transport.Call(ctx, node.Request{
		Method: node.MethodRun, RequestID: exec.RequestID, Execution: exec, Payload: raw, Secrets: secrets,
	}, func(event node.Event) { events = append(events, event) })
	if appendErr := w.service.appendNodeEvents(ctx, id, attempt, events); appendErr != nil && err == nil {
		err = appendErr
	}
	if err != nil {
		if node.IsPermanentTransport(err) {
			return nil, contract.NewFault(contract.PreconditionFailed, "the node transport refused the request", "Check the pinned node identity and host configuration.")
		}
		// Unknown outcome: the next attempt asks status first and never starts a
		// second effect under the same key.
		return nil, fmt.Errorf("node run %s: %w", stepID, err)
	}
	if response.InputsHash != "" && response.InputsHash != inputsHash {
		return nil, contract.NewFault(contract.PreconditionFailed, "node effect has different inputs", "Do not acknowledge an effect recorded under other inputs.")
	}
	switch response.Status {
	case engine.StatusCompleted:
		if _, err := w.service.ackStep(ctx, id, attempt, stepID, inputsHash, response.ExecutionID, response.Result, platformStepTotal); err != nil {
			return nil, err
		}
		return response.Result, nil
	case engine.StatusRunning:
		return nil, contract.NewFault(contract.Unavailable, "node task is still running", "The operation remains running; retrying will attach to the existing effect, not start another.")
	case engine.StatusFailed:
		code := contract.Code(response.ErrorCode)
		if code == "" {
			code = contract.Internal
		}
		detail := response.Detail
		if detail == "" {
			detail = "node step failed"
		}
		if failErr := w.service.failStep(ctx, id, attempt, stepID, code, detail); failErr != nil {
			return nil, failErr
		}
		if response.Retryable {
			return nil, fmt.Errorf("node step %s failed: %s", stepID, detail)
		}
		return nil, contract.NewFault(contract.PreconditionFailed, "node step failed: "+detail, "Inspect operation events and node state before resubmitting.")
	default:
		return nil, fmt.Errorf("node returned unexpected status %q for %s", response.Status, stepID)
	}
}

// reconcilePlatformOperation closes an operation whose River job disappeared.
// It never invents success: it accepts only durable domain state that proves the
// action committed, and otherwise records failure with the serving release that
// the environment actually had (or a switched release with unknown health).
func (s *Service) reconcilePlatformOperation(ctx context.Context, op contract.Operation) error {
	if op.State == contract.Cancelling {
		return s.cancelPlatform(ctx, op.ID, op.Attempt, "Cancellation reconciled after the River job ended.")
	}
	switch op.Kind {
	case platform.KindDeploy, platform.KindRollback:
		dep, has, err := s.deploymentByOperation(ctx, op.ID)
		if err != nil {
			return err
		}
		if !has {
			return s.failReconciledPlatform(ctx, op, "deployment record is missing", contract.Unavailable)
		}
		if dep.State == "succeeded" {
			return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil })
		}
		// A completed switch effect means the new container is actually serving
		// even though the process died before the verification/domain commit.
		if record, ok, err := s.stepRecord(ctx, op.ID, "platform-container-02-v1"); err == nil && ok && record.State == "completed" {
			return s.failAfterSwitch(ctx, op, dep)
		}
		return s.failReconciledPlatform(ctx, op, "execution ended before the serving release was verified; the previous serving release is unchanged", contract.Unavailable)
	case platform.KindReleaseCreate:
		release, err := s.releaseForOperation(ctx, op.ID)
		if err != nil {
			if fault := contract.Classify(err); fault.Code == contract.NotFound {
				return s.failReconciledPlatform(ctx, op, "release execution ended before a durable release was ready", contract.Unavailable)
			}
			return err
		}
		if release.Status == platform.ReleaseReady {
			return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil })
		}
		return s.failReconciledPlatform(ctx, op, "release execution ended before the image was verified or built", contract.Unavailable)
	case platform.KindAppCreate, platform.KindEnvironmentCreate, platform.KindNodeRegister:
		exists, err := s.recordActionExists(ctx, op)
		if err != nil {
			return err
		}
		if exists {
			return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil })
		}
		return s.failReconciledPlatform(ctx, op, "execution ended before the record was durable", contract.Unavailable)
	default:
		return s.failReconciledPlatform(ctx, op, "extension execution ended without a durable operation result; extension side effects are not assumed successful", contract.Unavailable)
	}
}

func (s *Service) deploymentByOperation(ctx context.Context, id contract.OperationID) (platform.Deployment, bool, error) {
	dep, err := scanDeploymentRow(s.pool.QueryRow(ctx, deploymentSelect+" WHERE d.operation_id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return dep, false, nil
	}
	if err != nil {
		return dep, false, unavailable()
	}
	return dep, true, nil
}

func (s *Service) recordActionExists(ctx context.Context, op contract.Operation) (bool, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx, "SELECT input FROM eregion_operations WHERE id=$1", op.ID).Scan(&raw); err != nil {
		return false, unavailable()
	}
	normalized, err := platform.Default().Normalize(op.Kind, op.Target, raw)
	if err != nil {
		return false, err
	}
	var exists bool
	switch input := normalized.Input.(type) {
	case *platform.AppCreateInput:
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_apps WHERE name=$1)`, input.Name).Scan(&exists)
	case *platform.EnvironmentCreateInput:
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1 AND e.name=$2)`, input.App, input.Name).Scan(&exists)
	case *platform.NodeRegisterInput:
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_nodes WHERE name=$1)`, input.Name).Scan(&exists)
	default:
		return false, nil
	}
	if err != nil {
		return false, unavailable()
	}
	return exists, nil
}

func (s *Service) failReconciledPlatform(ctx context.Context, op contract.Operation, message string, code contract.Code) error {
	if err := s.failPlatformOperation(ctx, op.ID, op.Attempt, code, message); err != nil {
		if errors.Is(err, errNotOwned) {
			return nil
		}
		return err
	}
	return nil
}

func (s *Service) failAfterSwitch(ctx context.Context, op contract.Operation, dep platform.Deployment) error {
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
            SET state='failed',error_code=$2,message=$3,completed_at=clock_timestamp(),updated_at=clock_timestamp()
            WHERE operation_id=$1 AND state IN ('pending','running')`,
			op.ID, string(contract.Unavailable), "The serving container was switched, but health verification did not commit; serving release recorded with unknown health."); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments
            SET serving_release_id=$2,applied_release_id=$2,health_state='unknown',health_detail=$3,health_checked_at=NULL,updated_at=clock_timestamp()
            WHERE id=$1`, depEnvironmentID(ctx, tx, dep), dep.ReleaseID, "deployment interrupted after switch; run a deployment or health check"); err != nil {
			return err
		}
		if err := markIncompleteSteps(ctx, tx, op.ID, contract.Unavailable, "operation reconciled after the serving switch"); err != nil {
			return err
		}
		return transition(ctx, tx, current, contract.Failed, contract.Unavailable, "Execution ended after the serving switch but before verification; the switched release is recorded with unknown health.")
	})
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	return nil
}

func depEnvironmentID(ctx context.Context, tx pgx.Tx, dep platform.Deployment) string {
	var id string
	_ = tx.QueryRow(ctx, `SELECT e.id FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1 AND e.name=$2`, dep.App, dep.Environment).Scan(&id)
	return id
}

// insertPlatformChange records desired/applied history without secret values.
func insertPlatformChange(ctx context.Context, tx pgx.Tx, op *contract.Operation, kind, app, environment, summary string, desired, applied any) error {
	var appID string
	if err := tx.QueryRow(ctx, `SELECT id FROM eregion_platform_apps WHERE name=$1`, app).Scan(&appID); err != nil {
		return err
	}
	var envID any
	if environment != "" {
		var id string
		if err := tx.QueryRow(ctx, `SELECT id FROM eregion_platform_environments WHERE app_id=$1 AND name=$2`, appID, environment).Scan(&id); err != nil {
			return err
		}
		envID = id
	}
	var sequence int64
	if envID == nil {
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(sequence),0)+1 FROM eregion_platform_changes WHERE app_id=$1 AND environment_id IS NULL`, appID).Scan(&sequence); err != nil {
			return err
		}
	} else {
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(sequence),0)+1 FROM eregion_platform_changes WHERE environment_id=$1`, envID).Scan(&sequence); err != nil {
			return err
		}
	}
	changeID, err := platform.NewChangeID()
	if err != nil {
		return err
	}
	desiredJSON, _ := json.Marshal(desired)
	appliedJSON, _ := json.Marshal(applied)
	_, err = tx.Exec(ctx, `INSERT INTO eregion_platform_changes
        (id,app_id,environment_id,sequence,kind,operation_id,actor_kind,actor_subject,summary,desired,applied)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
        ON CONFLICT (operation_id,kind) DO NOTHING`,
		changeID, appID, envID, sequence, kind, op.ID, op.Actor.Kind, op.Actor.Subject, summary,
		nullableJSON(desiredJSON), nullableJSON(appliedJSON))
	return err
}
