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

// Deployment step identities are shared by the worker, reconciliation, and the
// viewer. A restored switch is proven by the completed step record, never by a
// message.
const (
	deploymentPreflightStep   = "platform-preflight-01-v1"
	deploymentCandidateStep   = "platform-container-01-v1"
	deploymentHealthStep      = "platform-health-01-v1"
	deploymentSwitchStep      = "platform-container-02-v1"
	deploymentVerifyStep      = "platform-health-02-v1"
	deploymentRestoreStep     = "platform-container-03-v1"
	deploymentRestoreHealth   = "platform-health-03-v1"
	deploymentCleanupStep     = "platform-cleanup-01-v1"
	deploymentCommandStep     = "platform-command-01-v1"
	deploymentPackStep        = "platform-pack-01-v1"
	deploymentImageStep       = "platform-image-01-v1"
	deploymentPullStep        = "platform-pull-01-v1"
	deploymentBuildStageLabel = "build"
)

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
	case platform.KindNodeEnroll:
		return w.runNodeEnroll(ctx, op.ID, attempt, normalized)
	case platform.KindNodeReconcile:
		return w.runNodeReconcile(ctx, op.ID, attempt, normalized)
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
	if reason, ok := IsPlacementViolation(err); ok {
		// A refusal is also an event, so an agent reading the operation sees the
		// machine-readable reason without parsing the message.
		w.service.appendWorkerEvent(ctx, id, attempt, "placement_violation", fault.Message+" [reason="+reason+"]")
	}
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
	rec, err := loadNodeConn(ctx, w.service.pool, input.Node)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if rec.Enrollment == platform.EnrollmentEnrolled && rec.Transport == "ssh" {
		return w.runEnrolledCheck(ctx, id, attempt, rec)
	}
	// A registered node is observed through the worker's own transport, and
	// only when that transport authenticates as exactly this node.
	tr, err := w.service.resolver.TransportFor(ctx, input.Node)
	if err != nil {
		w.service.recordNodeCheck(ctx, input.Node, checkStateFor(err), contract.Classify(err).Message, false)
		return w.settle(ctx, id, attempt, err)
	}
	if _, err := w.runStepOn(ctx, tr, id, attempt, "platform-node-01-v1", platform.ArgvPayload{
		Step: "platform-node-01-v1", Argv: []string{"docker", "version"}, TimeoutSeconds: 30,
	}, nil); err != nil {
		w.service.recordNodeCheck(ctx, input.Node, checkStateFor(err), contract.Classify(err).Message, false)
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if _, err := w.runStepOn(ctx, tr, id, attempt, "platform-node-02-v1", platform.ArgvPayload{
		Step: "platform-node-02-v1", Argv: []string{"pack", "version"}, AllowFailure: true, TimeoutSeconds: 30,
	}, nil); err != nil {
		w.service.recordNodeCheck(ctx, input.Node, checkStateFor(err), contract.Classify(err).Message, false)
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		_, err := tx.Exec(ctx, `UPDATE eregion_platform_nodes SET status='ready',last_seen_at=clock_timestamp(),last_check_at=clock_timestamp(),
            last_check_state='ok',last_check_detail='docker reachable (declared facts, not enrolled)',updated_at=clock_timestamp() WHERE name=$1`, input.Node)
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
	// Image verification and builds both run on a node with an authenticated
	// transport. A build uses the environment's explicit builder placement.
	if err := crossNodeBuildUnsupported(env, input.Source.Type); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	nodeName := env.Node
	role, capability := platform.RoleApplication, platform.CapabilityDocker
	if input.Source.Type != "image" {
		nodeName = env.BuildNodeName()
		role, capability = platform.RoleBuilder, platform.CapabilityBuildpacks
	}
	n, err := w.service.node(ctx, nodeName)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.checkPlacement(n, role, capability); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	tr, err := w.service.resolver.TransportFor(ctx, n.Name)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	release, err := w.service.releaseForOperation(ctx, id)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	var result json.RawMessage
	if input.Source.Type == "image" {
		if input.Pull {
			if _, err := w.runStepOn(ctx, tr, id, attempt, deploymentPullStep, platform.ArgvPayload{
				Step: deploymentPullStep, Argv: []string{"docker", "pull", release.Image}, TimeoutSeconds: 600,
			}, nil); err != nil {
				return w.settle(ctx, id, attempt, err)
			}
		}
		result, err = w.runStepOn(ctx, tr, id, attempt, deploymentImageStep, platform.ArgvPayload{
			Step:           deploymentImageStep,
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
		limit := buildLimit(n)
		releaseSlot, err := w.service.acquireBuildSlot(ctx, n.Name, limit, func() {
			w.service.appendWorkerEvent(ctx, id, attempt, deploymentBuildStageLabel,
				fmt.Sprintf("build_queued on node %s; all %d build slot(s) are busy", n.Name, limit))
		})
		if err != nil {
			return w.settle(ctx, id, attempt, err)
		}
		w.service.appendWorkerEvent(ctx, id, attempt, deploymentBuildStageLabel,
			fmt.Sprintf("build_running on node %s; %d of %d build slot(s) in use", n.Name, 1, limit))
		_, buildErr := w.runStepOn(ctx, tr, id, attempt, deploymentPackStep, platform.PackPayload{
			Step: deploymentPackStep, Image: release.Image, Path: path,
			GitURL: input.Source.URL, GitRef: input.Source.Ref,
			Builder: build.Builder, Env: build.Env, TimeoutSeconds: 900,
		}, nil)
		releaseSlot()
		if buildErr != nil {
			return w.settle(ctx, id, attempt, buildErr)
		}
		result, err = w.runStepOn(ctx, tr, id, attempt, deploymentImageStep, platform.ArgvPayload{
			Step:           deploymentImageStep,
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
		if err := appendEvent(ctx, tx, op, contract.Event{
			Kind: "log", Stage: "release", ReleaseID: string(release.ID),
			Message: "Release " + string(release.ID) + " is ready with image " + imageID + ".",
		}); err != nil {
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
	tr, err := w.service.resolver.TransportFor(ctx, env.Node)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	secrets, err := w.service.resolveEnvironmentSecrets(ctx, env)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	candidateName := platform.CandidateContainerName(env.ID, id)
	servingName := env.ID.Container("serving")
	// A candidate step row may already exist from an earlier attempt. Cleanup is
	// a durable step, so a container created before a failure is removed instead
	// of leaking; but a retryable failure keeps it for the next attempt.
	candidateMayExist := w.service.stepExists(ctx, id, deploymentCandidateStep)
	finish := func(cause error) error {
		return w.finishDeploymentAttempt(ctx, tr, id, attempt, candidateName, candidateMayExist, cause)
	}

	if err := w.service.checkCancelled(ctx, id); err != nil {
		return finish(err)
	}
	// Preflight is durable and visible: it proves the immutable image is present
	// on the application node before the pipeline touches the serving container.
	if _, err := w.runStepOn(ctx, tr, id, attempt, deploymentPreflightStep, platform.ArgvPayload{
		Step:           deploymentPreflightStep,
		Argv:           []string{"docker", "image", "inspect", "--format", "{{.Id}}", release.ImageID},
		TimeoutSeconds: 30,
	}, nil); err != nil {
		return finish(err)
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
		if _, err := w.runStepOn(ctx, tr, id, attempt, deploymentCommandStep, platform.ArgvPayload{
			Step: deploymentCommandStep, Argv: commandArgs,
			Env: env.Env, SecretEnvKeys: secretKeys, TimeoutSeconds: release.Command.TimeoutSeconds,
		}, secrets); err != nil {
			return finish(err)
		}
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return finish(err)
	}

	candidatePayload := platform.ContainerPayload{
		Step: deploymentCandidateStep, Name: candidateName, Image: release.ImageID,
		ContainerPort: env.ContainerPort, HostPort: env.CandidatePort, Bind: env.Bind,
		Env: env.Env, SecretEnvKeys: sortedPlatformKeys(env.SecretRefs), Restart: "no",
		Labels: map[string]string{
			"io.eregion.app": env.App, "io.eregion.environment": env.Name,
			"io.eregion.release": string(release.ID), "io.eregion.role": "candidate",
		},
	}
	candidateMayExist = true // docker may have created a container even on error.
	if _, err := w.runStepOn(ctx, tr, id, attempt, deploymentCandidateStep, candidatePayload, secrets); err != nil {
		return finish(err)
	}

	candidateHealth, healthErr := w.runStepOn(ctx, tr, id, attempt, deploymentHealthStep, platform.HealthPayload{
		Step: deploymentHealthStep, Type: env.Health.Type, Target: platformHealthTarget(env, env.CandidatePort),
		TimeoutSeconds: env.Health.TimeoutSeconds, IntervalMS: 500,
	}, nil)
	w.observeHealth(ctx, id, attempt, deploymentHealthStep, platformHealthTarget(env, env.CandidatePort), env.Health, candidateHealth, healthErr)
	if healthErr != nil {
		return finish(healthErr)
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return finish(err)
	}

	servingPayload := platform.ContainerPayload{
		Step: deploymentSwitchStep, Name: servingName, Image: release.ImageID,
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
	if _, err := w.runStepOn(ctx, tr, id, attempt, deploymentSwitchStep, servingPayload, secrets); err != nil {
		return finish(err)
	}
	// An explicit acknowledgement makes the serving identity observable without
	// inferring it from a Docker log line.
	w.service.appendWorkerEventWith(ctx, id, attempt, contract.Event{
		Kind: "log", Stage: deploymentSwitchStep, Step: deploymentSwitchStep,
		ReleaseID: string(release.ID), ServingReleaseID: string(release.ID),
		Message: "Traffic switch acknowledged: release " + string(release.ID) + " now serves " + env.App + "/" + env.Name + ".",
	})

	verifyResult, verifyErr := w.runStepOn(ctx, tr, id, attempt, deploymentVerifyStep, platform.HealthPayload{
		Step: deploymentVerifyStep, Type: env.Health.Type, Target: platformHealthTarget(env, env.HostPort),
		TimeoutSeconds: env.Health.TimeoutSeconds, IntervalMS: 500,
	}, nil)
	w.observeHealth(ctx, id, attempt, deploymentVerifyStep, platformHealthTarget(env, env.HostPort), env.Health, verifyResult, verifyErr)
	if verifyErr != nil {
		// The candidate was healthy but the serving container is not. Restore the
		// previous release before reporting failure, so a failed verify never
		// leaves the environment stranded on an unverified release.
		restored := false
		restoredHealthy := false
		if previous.Image != "" {
			restore := platform.ContainerPayload{
				Step: deploymentRestoreStep, Name: servingName, Image: previous.ImageID,
				ContainerPort: env.ContainerPort, HostPort: env.HostPort, Bind: env.Bind,
				Env: env.Env, SecretEnvKeys: sortedPlatformKeys(env.SecretRefs), Restart: "unless-stopped",
				Labels: map[string]string{
					"io.eregion.app": env.App, "io.eregion.environment": env.Name,
					"io.eregion.release": string(previous.ID), "io.eregion.role": "serving",
				},
			}
			if _, restoreErr := w.runStepOn(ctx, tr, id, attempt, deploymentRestoreStep, restore, secrets); restoreErr == nil {
				restored = true
				restoreResult, restoreHealthErr := w.runStepOn(ctx, tr, id, attempt, deploymentRestoreHealth, platform.HealthPayload{
					Step: deploymentRestoreHealth, Type: env.Health.Type, Target: platformHealthTarget(env, env.HostPort),
					TimeoutSeconds: env.Health.TimeoutSeconds, IntervalMS: 500,
				}, nil)
				w.observeHealth(ctx, id, attempt, deploymentRestoreHealth, platformHealthTarget(env, env.HostPort), env.Health, restoreResult, restoreHealthErr)
				restoredHealthy = restoreHealthErr == nil
				if err := w.service.recordRestoredServing(ctx, id, attempt, env, previous.ID, restoredHealthy); err != nil {
					return finish(err)
				}
				w.service.appendWorkerEventWith(ctx, id, attempt, contract.Event{
					Kind: "log", Stage: deploymentRestoreStep, Step: deploymentRestoreStep,
					ReleaseID: string(previous.ID), ServingReleaseID: string(previous.ID),
					Message: "Previous release " + string(previous.ID) + " is serving again after failed verification.",
				})
				verifyErr = fmt.Errorf("%w; the previous release %s was restored", verifyErr, previous.ID)
			}
		}
		if !restored {
			// The new container may still be serving with unverified health.
			w.service.appendWorkerEventWith(ctx, id, attempt, contract.Event{
				Kind: "log", Stage: deploymentVerifyStep, Step: deploymentVerifyStep,
				ReleaseID: string(release.ID),
				Message:   "Serving release " + string(release.ID) + " was not verified and no previous release could be restored; health is unknown.",
			})
		}
		return finish(verifyErr)
	}
	if err := w.cleanupCandidate(ctx, tr, id, attempt, candidateName); err != nil {
		return finish(err)
	}
	candidateMayExist = false
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
		if err := appendEvent(ctx, tx, op, contract.Event{
			Kind: "log", Stage: "deploy", ReleaseID: string(release.ID), ServingReleaseID: string(release.ID),
			Message: "Deployment succeeded; serving release " + string(release.ID) + " with healthy verification.",
		}); err != nil {
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

// finishDeploymentAttempt cleans up the candidate when this attempt ends the
// operation (success, terminal failure, or cancellation). A retryable failure
// keeps the candidate and its durable step for the next attempt.
func (w *platformWorker) finishDeploymentAttempt(ctx context.Context, tr node.Transport, id contract.OperationID, attempt uint32, candidateName string, candidateMayExist bool, cause error) error {
	if cause == nil {
		return nil
	}
	if candidateMayExist && deploymentAttemptIsTerminal(cause) {
		if cleanupErr := w.cleanupCandidate(context.WithoutCancel(ctx), tr, id, attempt, candidateName); cleanupErr != nil {
			w.service.appendWorkerEvent(ctx, id, attempt, "cleanup", "Candidate cleanup did not complete: "+cleanupErr.Error())
			if contract.Classify(cleanupErr).Code == contract.Unavailable {
				// Do not terminalise with the candidate still present; retry the
				// cleanup before recording the outcome.
				return w.settle(ctx, id, attempt, cleanupErr)
			}
		}
	}
	return w.settle(ctx, id, attempt, cause)
}

// deploymentAttemptIsTerminal mirrors settle: only terminal faults and explicit
// cancellation end the operation. Unavailable faults (and unknown errors) retry.
func deploymentAttemptIsTerminal(err error) bool {
	if errors.Is(err, errPlatformCancelled) || errors.Is(err, errOperationTerminal) {
		return true
	}
	switch contract.Classify(err).Code {
	case contract.Unavailable:
		return false
	case contract.Cancelled:
		return true
	case contract.ValidationFailed, contract.PreconditionFailed, contract.NotFound, contract.Forbidden, contract.Conflict:
		return true
	default:
		return false
	}
}

// cleanupCandidate is a durable, idempotent node step. It is visible in operation
// history and reconciled after a crash instead of being an invisible defer.
func (w *platformWorker) cleanupCandidate(ctx context.Context, tr node.Transport, id contract.OperationID, attempt uint32, candidateName string) error {
	if tr == nil {
		return contract.NewFault(contract.Unavailable, "node transport is not configured", "Run this work in a worker configured with a node transport.")
	}
	// Cleanup must run even when the caller's context is being cancelled, but it
	// is bounded: a dead node cannot hold the operation open indefinitely.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	_, err := w.runStepOn(cleanupCtx, tr, id, attempt, deploymentCleanupStep, platform.ArgvPayload{
		Step: deploymentCleanupStep, Argv: []string{"docker", "rm", "-f", candidateName},
		AllowFailure: true, TimeoutSeconds: 30,
	}, nil)
	return err
}

// recordRestoredServing records the release that actually serves after a failed
// verification was rolled back. It is a domain write, not a guess from a log.
func (s *Service) recordRestoredServing(ctx context.Context, id contract.OperationID, attempt uint32, env platform.Environment, release platform.ReleaseID, healthy bool) error {
	state, detail := platform.HealthUnknown, "previous release restored after failed verification; health was not observed"
	if healthy {
		state, detail = platform.HealthHealthy, "previous release restored and passed its health check"
	}
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		_, err := tx.Exec(ctx, `UPDATE eregion_platform_environments
            SET serving_release_id=$2,applied_release_id=$2,health_state=$3,health_detail=$4,health_checked_at=clock_timestamp(),updated_at=clock_timestamp()
            WHERE id=$1`, env.ID, release, string(state), detail)
		return err
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

// observeHealth emits a structured health observation beside the node's log
// line, so clients never parse the message text.
func (w *platformWorker) observeHealth(ctx context.Context, id contract.OperationID, attempt uint32, step, target string, config platform.HealthConfig, result json.RawMessage, probeErr error) {
	var decoded struct {
		Healthy    bool `json:"healthy"`
		Checks     int  `json:"checks"`
		LastStatus int  `json:"last_status"`
	}
	_ = json.Unmarshal(result, &decoded)
	// A nil probe error is proof the step passed. The node may complete the step
	// from its read-only Check without running Apply, in which case no probe
	// count was reported (zero) and the viewer says so instead of inventing one.
	observation := &contract.HealthObservation{
		Target: target, Type: config.Type, Passed: probeErr == nil,
		Checks: decoded.Checks, TimeoutSeconds: config.TimeoutSeconds,
	}
	message := fmt.Sprintf("Health observation for %s: passed=%t checks=%d deadline=%ds", target, observation.Passed, observation.Checks, config.TimeoutSeconds)
	if probeErr != nil {
		observation.Detail = probeErr.Error()
		message += " detail=" + probeErr.Error()
	} else {
		observation.Detail = "probe succeeded"
		message += " detail=probe succeeded"
	}
	w.service.appendWorkerEventWith(ctx, id, attempt, contract.Event{
		Kind: "log", Stage: step, Step: step, Health: observation, Message: message,
	})
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
	if !localNodeNamesMatch(identity, n.Name) {
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
		var action, app, environment, releaseID, previousID, servingID string
		if err := tx.QueryRow(ctx, `SELECT d.action,a.name,e.name,d.release_id,coalesce(d.previous_release_id,''),coalesce(e.serving_release_id,'')
            FROM eregion_platform_deployments d
            JOIN eregion_platform_apps a ON a.id=d.app_id
            JOIN eregion_platform_environments e ON e.id=d.environment_id
            WHERE d.operation_id=$1`, id).Scan(&action, &app, &environment, &releaseID, &previousID, &servingID); err == nil {
			if err := appendEvent(ctx, tx, op, contract.Event{
				Kind: "log", Stage: "deploy", ReleaseID: releaseID, ServingReleaseID: servingID,
				Message: "Deployment failed; failed release " + releaseID + " is not recorded as serving" + servingSuffix(servingID) + ".",
			}); err != nil {
				return err
			}
			unchanged := servingID == "" || servingID == previousID
			if changeErr := insertPlatformChange(ctx, tx, op, action+".failed", app, environment,
				"Deployment failed: "+message,
				map[string]any{"release_id": releaseID},
				map[string]any{"error_code": code, "serving_release_id": servingID, "serving_unchanged": unchanged}); changeErr != nil {
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

func servingSuffix(releaseID string) string {
	if releaseID == "" {
		return ""
	}
	return "; release " + releaseID + " still serves"
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
		var releaseID, servingID string
		if err := tx.QueryRow(ctx, `SELECT coalesce(d.release_id,''),coalesce(e.serving_release_id,'')
            FROM eregion_platform_deployments d
            JOIN eregion_platform_environments e ON e.id=d.environment_id
            WHERE d.operation_id=$1`, id).Scan(&releaseID, &servingID); err == nil {
			if err := appendEvent(ctx, tx, op, contract.Event{
				Kind: "log", Stage: "deploy", ReleaseID: releaseID, ServingReleaseID: servingID,
				Message: "Deployment cancelled before a verified switch" + servingSuffix(servingID) + ".",
			}); err != nil {
				return err
			}
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
func (w *platformWorker) runStepOn(ctx context.Context, tr node.Transport, id contract.OperationID, attempt uint32, stepID string, payload any, secrets map[string]string) (json.RawMessage, error) {
	if tr == nil {
		return nil, contract.NewFault(contract.Unavailable, "node transport is not configured", "Run this work in a worker configured with a node transport.")
	}
	started := time.Now()
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
	status, err := tr.Call(ctx, node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec}, nil)
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
	response, err := tr.Call(ctx, node.Request{
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
		duration := time.Since(started).Milliseconds()
		w.service.appendWorkerEventWith(ctx, id, attempt, contract.Event{
			Kind: "log", Stage: stepID, Step: stepID, DurationMS: &duration,
			Message: fmt.Sprintf("Node step %s completed in %dms.", stepID, duration),
		})
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
	if op.Kind == platform.KindDeploy || op.Kind == platform.KindRollback {
		return s.reconcileDeployment(ctx, op)
	}
	if op.State == contract.Cancelling {
		return s.cancelPlatform(ctx, op.ID, op.Attempt, "Cancellation reconciled after the River job ended.")
	}
	switch op.Kind {
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
	case platform.KindNodeCheck, platform.KindNodeReconcile:
		return s.failReconciledPlatform(ctx, op, "the node action ended before it recorded a result; it is idempotent, so submit it again", contract.Unavailable)
	case platform.KindNodeEnroll:
		// Enrollment is durable only once the worker recorded it. A job that ended
		// first leaves the node pending (or failed) for platform nodes retry; the
		// operation never claims verification it did not see.
		var state string
		var enrollOp *string
		_ = s.pool.QueryRow(ctx, `SELECT enrollment_state,enroll_operation_id FROM eregion_platform_nodes WHERE name=(SELECT split_part($1,'/',2))`, op.Target).Scan(&state, &enrollOp)
		if state == string(platform.EnrollmentEnrolled) && enrollOp != nil && *enrollOp == string(op.ID) {
			return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil })
		}
		return s.failReconciledPlatform(ctx, op, "enrollment ended before the node was verified; retry with platform nodes retry", contract.Unavailable)
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

// reconcileDeployment never trusts the deployment row's intended release. It
// inspects completed step records and records the release that the node effects
// prove is serving, including a completed restore of the previous release.
func (s *Service) reconcileDeployment(ctx context.Context, op contract.Operation) error {
	dep, has, err := s.deploymentByOperation(ctx, op.ID)
	if err != nil {
		return err
	}
	if !has {
		return s.failReconciledPlatform(ctx, op, "deployment record is missing", contract.Unavailable)
	}
	env, envErr := s.environment(ctx, dep.App, dep.Environment)
	if envErr == nil {
		s.reconcileDeploymentCleanup(ctx, op, env, dep)
	}
	if dep.State == "succeeded" {
		return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(pgx.Tx, *contract.Operation) error { return nil })
	}
	restored := s.completedStep(ctx, op.ID, deploymentRestoreStep)
	switched := s.completedStep(ctx, op.ID, deploymentSwitchStep)
	verified := s.completedStep(ctx, op.ID, deploymentVerifyStep)
	cancelling := op.State == contract.Cancelling

	if restored {
		// The new release failed verification and the previous release was put
		// back. Recording the failed new release as serving would be a lie.
		healthy := s.completedStep(ctx, op.ID, deploymentRestoreHealth)
		if envErr != nil {
			return envErr
		}
		return s.failAfterRestore(ctx, op, dep, env, healthy)
	}
	if switched {
		if cancelling && verified {
			// Cancellation arrived after the switch and verification completed;
			// the worker would have finished successfully.
			return s.finishDeploymentAfterSwitch(ctx, op, dep, env)
		}
		if cancelling {
			return s.cancelAfterSwitch(ctx, op, dep, env, verified)
		}
		return s.failAfterSwitch(ctx, op, dep, env, verified)
	}
	if cancelling {
		return s.cancelPlatform(ctx, op.ID, op.Attempt, "Cancellation reconciled before the traffic switch; the previous serving release is unchanged.")
	}
	return s.failReconciledPlatform(ctx, op, "execution ended before the serving release was verified; the previous serving release is unchanged", contract.Unavailable)
}

func (s *Service) completedStep(ctx context.Context, id contract.OperationID, step string) bool {
	record, ok, err := s.stepRecord(ctx, id, step)
	return err == nil && ok && record.State == "completed"
}

// reconcileDeploymentCleanup removes a candidate left by a crashed attempt. The
// candidate step row proves a container may exist. Cleanup is best effort here:
// an unreachable node must not keep the operation running forever, but the
// failure is visible.
func (s *Service) reconcileDeploymentCleanup(ctx context.Context, op contract.Operation, env platform.Environment, dep platform.Deployment) {
	if !s.stepExists(ctx, op.ID, deploymentCandidateStep) {
		return
	}
	if s.resolver == nil {
		s.appendWorkerEvent(ctx, op.ID, op.Attempt, "cleanup", "Candidate cleanup was not attempted: the worker has no node resolver.")
		return
	}
	tr, err := s.resolver.TransportFor(ctx, env.Node)
	if err != nil {
		s.appendWorkerEvent(ctx, op.ID, op.Attempt, "cleanup", "Candidate cleanup could not reach the application node: "+contract.Classify(err).Message)
		return
	}
	worker := &platformWorker{service: s}
	if err := worker.cleanupCandidate(ctx, tr, op.ID, op.Attempt, platform.CandidateContainerName(env.ID, op.ID)); err != nil {
		s.appendWorkerEvent(ctx, op.ID, op.Attempt, "cleanup", "Candidate cleanup did not complete: "+contract.Classify(err).Message)
	}
}

func (s *Service) failAfterSwitch(ctx context.Context, op contract.Operation, dep platform.Deployment, env platform.Environment, verified bool) error {
	health, detail := platform.HealthUnknown, "deployment interrupted after the switch; health verification did not commit"
	if verified {
		health, detail = platform.HealthHealthy, "deployment interrupted after a verified switch; health check had passed"
	}
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
            SET state='failed',error_code=$2,message=$3,completed_at=clock_timestamp(),updated_at=clock_timestamp()
            WHERE operation_id=$1 AND state IN ('pending','running')`,
			op.ID, string(contract.Unavailable), "The serving container was switched, but the operation did not commit; serving release recorded with "+string(health)+" health."); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments
            SET serving_release_id=$2,applied_release_id=$2,health_state=$3,health_detail=$4,health_checked_at=clock_timestamp(),updated_at=clock_timestamp()
            WHERE id=$1`, env.ID, dep.ReleaseID, string(health), detail); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, current, contract.Event{
			Kind: "log", Stage: "deploy", ReleaseID: string(dep.ReleaseID), ServingReleaseID: string(dep.ReleaseID),
			Message: "Execution ended after the switched release " + string(dep.ReleaseID) + " began serving; recorded health is " + string(health) + ".",
		}); err != nil {
			return err
		}
		if err := markIncompleteSteps(ctx, tx, op.ID, contract.Unavailable, "operation reconciled after the serving switch"); err != nil {
			return err
		}
		return transition(ctx, tx, current, contract.Failed, contract.Unavailable, "Execution ended after the serving switch but before completion; release "+string(dep.ReleaseID)+" is recorded with "+string(health)+" health.")
	})
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	return nil
}

// failAfterRestore records the previous release as serving after a completed
// restore step. This is the honest outcome of a failed verification.
func (s *Service) failAfterRestore(ctx context.Context, op contract.Operation, dep platform.Deployment, env platform.Environment, healthy bool) error {
	health, detail, verification := platform.HealthUnknown, "previous release restored after a failed verification; restore health was not observed", "unknown"
	if healthy {
		health, detail, verification = platform.HealthHealthy, "previous release restored after a failed verification and passed its health check", "healthy"
	}
	serving := dep.PreviousReleaseID
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
            SET state='failed',error_code=$2,message=$3,completed_at=clock_timestamp(),updated_at=clock_timestamp()
            WHERE operation_id=$1 AND state IN ('pending','running')`,
			op.ID, string(contract.Unavailable), "The new release failed verification; the previous release "+string(serving)+" was restored."); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments
            SET serving_release_id=$2,applied_release_id=$2,health_state=$3,health_detail=$4,health_checked_at=clock_timestamp(),updated_at=clock_timestamp()
            WHERE id=$1`, env.ID, serving, string(health), detail); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, current, contract.Event{
			Kind: "log", Stage: "deploy", ReleaseID: string(dep.ReleaseID), ServingReleaseID: string(serving),
			Message: "Restore completed: release " + string(serving) + " serves again after release " + string(dep.ReleaseID) + " failed verification; restore health is " + verification + ".",
		}); err != nil {
			return err
		}
		if err := markIncompleteSteps(ctx, tx, op.ID, contract.Unavailable, "operation reconciled after restoring the previous release"); err != nil {
			return err
		}
		return transition(ctx, tx, current, contract.Failed, contract.Unavailable,
			"Verification failed and the previous release "+string(serving)+" was restored; recorded restore health is "+verification+".")
	})
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	return nil
}

// cancelAfterSwitch records a switch that completed before cancellation was
// reconciled. The operation is cancelled, but the served release and its health
// are recorded instead of leaving the environment claiming an older release.
func (s *Service) cancelAfterSwitch(ctx context.Context, op contract.Operation, dep platform.Deployment, env platform.Environment, verified bool) error {
	health, detail := platform.HealthUnknown, "deployment was cancelled after the switch; health verification did not commit"
	if verified {
		health, detail = platform.HealthHealthy, "deployment was cancelled after a verified switch; health check had passed"
	}
	_, owned, err := s.mutateOwned(ctx, op.ID, op.Attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
            SET state='cancelled',message=$2,completed_at=clock_timestamp(),updated_at=clock_timestamp()
            WHERE operation_id=$1 AND state IN ('pending','running')`,
			op.ID, "Cancellation reconciled after the switch; release "+string(dep.ReleaseID)+" is serving with "+string(health)+" health."); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments
            SET serving_release_id=$2,applied_release_id=$2,health_state=$3,health_detail=$4,health_checked_at=clock_timestamp(),updated_at=clock_timestamp()
            WHERE id=$1`, env.ID, dep.ReleaseID, string(health), detail); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, current, contract.Event{
			Kind: "log", Stage: "deploy", ReleaseID: string(dep.ReleaseID), ServingReleaseID: string(dep.ReleaseID),
			Message: "Cancellation reconciled after the switch; release " + string(dep.ReleaseID) + " serves with " + string(health) + " health.",
		}); err != nil {
			return err
		}
		if err := markIncompleteSteps(ctx, tx, op.ID, contract.Cancelled, "operation reconciled after the serving switch"); err != nil {
			return err
		}
		return transition(ctx, tx, current, contract.Canceled, "", "Cancellation reconciled after the switch; release "+string(dep.ReleaseID)+" is serving with "+string(health)+" health.")
	})
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	return nil
}

func (s *Service) finishDeploymentAfterSwitch(ctx context.Context, op contract.Operation, dep platform.Deployment, env platform.Environment) error {
	return s.finishPlatformOperation(ctx, op.ID, op.Attempt, func(tx pgx.Tx, current *contract.Operation) error {
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
            SET state='succeeded',completed_at=$2,updated_at=$2,message=$3,error_code=''
            WHERE operation_id=$1 AND state IN ('pending','running')`, op.ID, now,
			"Serving release "+string(dep.ReleaseID)+" passed health verification."); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments
            SET serving_release_id=$2,applied_release_id=$2,health_state='healthy',health_detail='serving release passed health verification',health_checked_at=$3,updated_at=$3
            WHERE id=$1`, env.ID, dep.ReleaseID, now); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, current, contract.Event{
			Kind: "log", Stage: "deploy", ReleaseID: string(dep.ReleaseID), ServingReleaseID: string(dep.ReleaseID),
			Message: "Deployment completed after the switch and verification; serving release " + string(dep.ReleaseID) + ".",
		}); err != nil {
			return err
		}
		return insertPlatformChange(ctx, tx, current, string(dep.Action)+".succeeded", dep.App, dep.Environment,
			"Serving release "+string(dep.ReleaseID)+" for "+dep.App+"/"+dep.Environment+".",
			map[string]any{"release_id": dep.ReleaseID},
			map[string]any{"serving_release_id": dep.ReleaseID, "health": "healthy"})
	})
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

// appendWorkerEvent records a best-effort observation under the attempt fence.
func (s *Service) appendWorkerEvent(ctx context.Context, id contract.OperationID, attempt uint32, stage, message string) {
	s.appendWorkerEventWith(ctx, id, attempt, contract.Event{Kind: "log", Stage: stage, Message: message})
}

// appendWorkerEventWith records one structured observation. It is best effort:
// a terminal operation, or one owned by a newer attempt, is never rewritten.
func (s *Service) appendWorkerEventWith(ctx context.Context, id contract.OperationID, attempt uint32, event contract.Event) {
	if event.Kind == "" {
		event.Kind = "log"
	}
	if len(event.Message) > 4096 {
		event.Message = event.Message[:4096]
	}
	if event.Health != nil && len(event.Health.Detail) > 4096 {
		event.Health.Detail = event.Health.Detail[:4096]
	}
	_, _, _ = s.mutateOwned(context.WithoutCancel(ctx), id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		return appendEvent(ctx, tx, op, event)
	})
}

// stepExists reports whether a durable step row exists, including a running or
// failed one. Reconciliation uses it to decide whether a container may exist.
func (s *Service) stepExists(ctx context.Context, id contract.OperationID, step string) bool {
	_, ok, err := s.stepRecord(ctx, id, step)
	return err == nil && ok
}

// crossNodeBuildUnsupported refuses a distinct build node honestly until the
// built image can be moved to the application node. The placement and
// capability checks still run for every build node; this gate exists because a
// local Docker image on one node is not visible on another without a registry.
func crossNodeBuildUnsupported(env platform.Environment, sourceType string) error {
	if sourceType == "image" || env.BuildNodeName() == env.Node {
		return nil
	}
	return contract.NewFault(contract.PreconditionFailed,
		"build_node is distinct from the application node; moving the built image requires a shared registry",
		"Use build_node equal to the environment node on a co-located builder, use an image source, or configure registry transfer before building across nodes.")
}

// buildLimit is the node's concurrent-build allowance.
func buildLimit(n platform.Node) int {
	if n.BuildConcurrency < 1 {
		return 1
	}
	return n.BuildConcurrency
}
