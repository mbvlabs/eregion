package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
)

// portAllocationLock serializes host/candidate port allocation across control
// plane processes. It is transaction-scoped and separate from River locks.
const portAllocationLock int64 = 0x657265706f727473 // "ereports"

func (s *Service) submitPlatformWithReceipt(
	ctx context.Context,
	sub operations.Submission,
) (operations.SubmissionResult, error) {
	var zero operations.SubmissionResult
	if sub.Confirmation != "" {
		return zero, contract.NewFault(
			contract.ValidationFailed,
			"platform actions do not accept confirmation text",
			"Omit confirmation; destructive actions are guarded by their inputs and operation state.",
		)
	}
	normalized, err := platform.Default().Normalize(sub.Kind, sub.Target, sub.Input)
	if err != nil {
		return zero, err
	}
	canonical := normalized.Canonical
	inputHash := hashInput(canonical)
	// A completed replay returns before any submission-side effect. Resolving a
	// git ref needs the remote, and an unreachable remote must not turn an
	// identical replay of an already-accepted operation into `unavailable`.
	if existing, found, err := s.replaySubmission(
		ctx,
		sub.Kind,
		normalized.Target.String(),
		sub.Actor,
		sub.IdempotencyKey,
		inputHash,
	); err != nil {
		return zero, err
	} else if found {
		if err := attachFirstDeployOperation(ctx, s.entities.Platform().Revisions(), &existing); err != nil {
			return zero, err
		}
		return operations.SubmissionResult{Operation: existing, Replay: true}, nil
	}
	if input, ok := normalized.Input.(*platform.AppCreateInput); ok {
		if err := platform.ValidateAppImageRepositoryForRegistry(
			input.ImageRepository,
			s.registryEndpoint,
		); err != nil {
			return zero, err
		}
	}
	resolved, err := s.resolveSubmissionGitRef(ctx, normalized)
	if err != nil {
		return zero, err
	}
	artifact, err := s.resolveSubmissionArtifact(ctx, normalized)
	if err != nil {
		return zero, err
	}
	id, err := contract.NewOperationID()
	if err != nil {
		return zero, err
	}
	if err := s.ensureLocalNode(ctx); err != nil {
		return zero, err
	}
	var result operations.SubmissionResult
	err = s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		result, err = s.recordPlatformSubmissionTx(
			ctx,
			tx,
			id,
			normalized,
			sub.Actor,
			sub.IdempotencyKey,
			resolved,
			artifact,
			sub.HumanFactor,
			sub.WebhookSecret,
		)
		return err
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return zero, unavailable(err)
		}
		return zero, err
	}
	return result, nil
}

// recordPlatformSubmissionTx records one ordinary platform submission in the
// caller's transaction: operation, idempotency replay, domain rows, the one
// River platform job and the queued event. It is shared by HTTP/CLI submission
// and by the webhook worker, so a webhook can commit its operation and its
// delivery link atomically without calling Service.Submit (which opens a
// transaction) or nesting one.
func (s *Service) recordPlatformSubmissionTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	normalized platform.Normalized,
	actor contract.Actor,
	key string,
	resolved *resolvedGitRef,
	artifact *resolvedArtifact,
	humanFactor string,
	webhookSecret []byte,
) (operations.SubmissionResult, error) {
	var zero operations.SubmissionResult
	inputHash := hashInput(normalized.Canonical)
	operationKey := entities.OperationKey{
		Kind: normalized.Kind, Target: normalized.Target.String(), Actor: actor, IdempotencyKey: key,
	}
	inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
		ID: id, Key: operationKey, InputHash: inputHash, Input: normalized.Canonical,
	})
	if err != nil {
		return zero, unavailable(err)
	}
	if !inserted {
		return s.replayPlatformSubmissionTx(ctx, tx, operationKey, inputHash)
	}
	if err := guardTopologyAdmissionTx(ctx, tx, normalized); err != nil {
		return zero, err
	}
	var activated bool
	if err := s.preparePlatform(
		ctx,
		tx,
		id,
		normalized,
		actor,
		resolved,
		artifact,
		humanFactor,
		webhookSecret,
		&activated,
	); err != nil {
		return zero, err
	}
	op := contract.Operation{
		ID:     id,
		Kind:   normalized.Kind,
		Target: normalized.Target.String(),
		Actor:  actor,
	}
	jobID, err := enqueuePlatformTx(ctx, tx, op)
	if err != nil {
		return zero, unavailable(err)
	}
	if err := tx.Operations().SetJob(ctx, id, jobID); err != nil {
		return zero, unavailable(err)
	}
	op, found, err := tx.Operations().Get(ctx, id)
	if err != nil || !found {
		return zero, unavailable(err)
	}
	if err := attachFirstDeployOperation(ctx, tx.Platform().Revisions(), &op); err != nil {
		return zero, err
	}
	if err := appendEvent(
		ctx,
		tx,
		&op,
		contract.Event{
			Kind:    "state",
			State:   contract.Queued,
			Message: platformAcceptedMessageFor(normalized),
		},
	); err != nil {
		return zero, unavailable()
	}
	if err := s.runBeforeSubmissionCommit(); err != nil {
		return zero, err
	}
	return operations.SubmissionResult{Operation: op, WebhookSecretActivated: activated}, nil
}

func (s *Service) replayPlatformSubmissionTx(
	ctx context.Context,
	tx *entities.StoreTx,
	key entities.OperationKey,
	inputHash string,
) (operations.SubmissionResult, error) {
	var zero operations.SubmissionResult
	identity, found, err := tx.Operations().Identity(ctx, key)
	if err != nil || !found {
		return zero, unavailable(err)
	}
	if identity.InputHash != inputHash {
		return zero, idempotencyInputConflict()
	}
	existing, found, err := tx.Operations().Get(ctx, identity.ID)
	if err != nil || !found {
		return zero, unavailable(err)
	}
	if err := attachFirstDeployOperation(ctx, tx.Platform().Revisions(), &existing); err != nil {
		return zero, err
	}
	return operations.SubmissionResult{Operation: existing, Replay: true}, nil
}

func (s *Service) runBeforeSubmissionCommit() error {
	if s.beforeSubmissionCommit == nil {
		return nil
	}
	return s.beforeSubmissionCommit()
}

// enqueuePlatformTx is the one admission-side routing decision for platform
// jobs: retention runs on the maintenance queue, every other platform action on
// the platform queue. Both go through the Store's one River client and the
// caller's transaction.
func enqueuePlatformTx(ctx context.Context, tx *entities.StoreTx, op contract.Operation) (int64, error) {
	if op.Kind == platform.KindRetentionPrune {
		return tx.Jobs().EnqueueRetention(ctx, op.ID)
	}
	return tx.Jobs().EnqueuePlatform(ctx, op)
}

func hashInput(canonical json.RawMessage) string {
	// hashInput keeps the canonical digest identical to the diagnostic path.
	// It is intentionally a helper, not a second persistence format.
	return sha256Hex(canonical)
}

func idempotencyInputConflict() error {
	return contract.NewFault(
		contract.Conflict,
		"idempotency key already binds different inputs",
		"Use the original inputs to recover the existing operation; use a new key only for deliberately new work.",
	)
}

// replaySubmission returns the operation an identical submission already
// created. It deliberately runs before git ref resolution so a replay of an
// accepted operation never depends on the remote being reachable. The
// transaction's ON CONFLICT remains the arbiter for concurrent first
// submissions; this read only short-circuits a completed replay.
func (s *Service) replaySubmission(
	ctx context.Context,
	kind, target string,
	actor contract.Actor,
	key, inputHash string,
) (contract.Operation, bool, error) {
	var existingID contract.OperationID
	var existingHash string
	identity, found, err := s.entities.Operations().Identity(ctx, entities.OperationKey{
		Kind: kind, Target: target, Actor: actor, IdempotencyKey: key,
	})
	if err != nil {
		return contract.Operation{}, false, unavailable(err)
	}
	if !found {
		return contract.Operation{}, false, nil
	}
	existingID, existingHash = identity.ID, identity.InputHash
	if existingHash != inputHash {
		return contract.Operation{}, true, idempotencyInputConflict()
	}
	op, found, err := s.entities.Operations().Get(ctx, existingID)
	if err != nil || !found {
		return contract.Operation{}, true, unavailable(err)
	}
	if err := s.populateActionResult(ctx, &op); err != nil {
		return contract.Operation{}, true, err
	}
	return op, true, nil
}

// attachFirstDeployOperation exposes the deploy operation an app or
// environment create queued in its own transaction. A create without deploy
// intent has no such row, so the field stays empty rather than being invented.
func attachFirstDeployOperation(ctx context.Context, revisions *entities.RevisionStore, op *contract.Operation) error {
	if op.Kind != platform.KindAppCreate && op.Kind != platform.KindEnvironmentCreate {
		return nil
	}
	id, found, err := revisions.FirstDeployOperation(ctx, op.ID, platform.KindDeploy, op.Actor)
	if err != nil {
		return unavailable(err)
	}
	if found {
		op.FirstDeployOperationID = id
	}
	return nil
}

func platformAcceptedMessage(kind string) string {
	switch kind {
	case platform.KindAppCreate:
		return "Application record accepted durably."
	case platform.KindEnvironmentCreate:
		return "Environment record accepted durably."
	case platform.KindEnvironmentUpdate, platform.KindProcessScale, platform.KindConfigSet,
		platform.KindConfigUnset, platform.KindEnvironmentSecretAttach,
		platform.KindEnvironmentResourceAttach, platform.KindEnvironmentResourceDetach:
		return "Environment desired state accepted durably as a new revision; no deploy was queued."
	case platform.KindEnvironmentProtect:
		return "Environment protection change accepted durably."
	case platform.KindBuildStop:
		return "Build stop accepted durably; the running pack process will be cancelled."
	case platform.KindBuildRetry:
		return "Build retry accepted durably; a new build of the same commit will run."
	case platform.KindDeploy:
		return "Deployment accepted durably; the serving release is unchanged until the candidate passes health and the public check."
	case platform.KindRollback:
		return "Rollback accepted durably; it redeploys the earlier release with its recorded revision and secret snapshot."
	case platform.KindRestart:
		return "Restart accepted durably; the serving release is reapplied with the current revision."
	case platform.KindPromote:
		return "Promotion accepted durably; the source serving artifact is pinned as a new production release and the serving release is unchanged until the candidate passes health and the public check."
	case platform.KindNodeRegister:
		return "Node placement facts accepted durably. This does not install or verify host state."
	case platform.KindNodeCheck:
		return "Node check accepted durably."
	case platform.KindNodeEnroll:
		return "Node enrollment accepted durably; the host key is pinned, but nothing is trusted until the node proves its identity."
	case platform.KindNodeReconcile:
		return "Node reconciliation accepted durably."
	case platform.KindDomainAdd, platform.KindDomainRemove:
		return "Domain change accepted durably; any explicit Cloudflare effect runs before the Caddy route write."
	case platform.KindEnvironmentDelete:
		return "Environment deletion accepted durably; containers and routes are removed before platform state."
	case platform.KindAppDelete:
		return "Application deletion accepted durably; every environment's containers and routes are removed before platform state."
	case platform.KindRetentionPrune:
		return "Disk retention accepted durably; it removes only artifacts outside the effective policy and records a retention run."
	case platform.KindResourceApply:
		return "Resource apply accepted durably; the container joins the environment's private network and is not ready until its health check passes."
	case platform.KindResourceDestroy:
		return "Resource destroy accepted durably; the container is removed, named volumes and the resource identity are retained."
	default:
		return "Platform action accepted durably."
	}
}

func platformAcceptedMessageFor(normalized platform.Normalized) string {
	if input, ok := normalized.Input.(*platform.EnvironmentUpdateInput); ok &&
		input.RotateWebhookSecret {
		return "Environment update and push-to-deploy secret rotation accepted durably; no deploy was queued."
	}
	return platformAcceptedMessage(normalized.Kind)
}

// lockDefinitionTargetTx is the shared order for app/environment admission.
// An empty environment denotes an application-wide operation.
func lockDefinitionTargetTx(ctx context.Context, tx *entities.StoreTx, app, environment string) error {
	if _, err := appByNameForUpdateTx(ctx, tx, app); err != nil {
		return err
	}
	if environment != "" {
		_, err := environmentByNameTx(ctx, tx, app, environment)
		return err
	}
	_, err := tx.Platform().Environments().LockTargetsForApp(ctx, app)
	return err
}

func fenceDefinitionAdmissionTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	n platform.Normalized,
	actor contract.Actor,
) error {
	app, environment := n.Target.App, n.Target.Environment
	if n.Target.Kind == platform.TargetBuild {
		placement, found, err := tx.Platform().Builds().Placement(ctx, n.Target.Build)
		if err != nil {
			return unavailable(err)
		}
		if found {
			app, environment = placement.App, placement.Environment
		}
	}
	if app == "" {
		return nil
	}
	if n.Kind == platform.KindPromote {
		environment = ""
	}
	// Environment creation names a not-yet-existing environment only in input;
	// its target is the application and is intentionally application-wide.
	if err := lockDefinitionTargetTx(ctx, tx, app, environment); err != nil {
		return err
	}
	blocked, err := tx.DefinitionRuns().BlocksAdmission(ctx, entities.DefinitionAdmissionGuard{
		App: app, Environment: environment, ChildOperation: id,
		Kind: n.Kind, Target: n.Target.String(), Input: n.Canonical,
		ParentKind: platform.KindDefinitionApply, Actor: actor,
	})
	if err != nil {
		return unavailable(err)
	}
	if blocked {
		return contract.NewFault(
			contract.Conflict,
			"target is reserved by an active definition run",
			"Wait for definition work to finish before submitting other changes.",
		)
	}
	return nil
}

func (s *Service) preparePlatform(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	normalized platform.Normalized,
	actor contract.Actor,
	resolved *resolvedGitRef,
	artifact *resolvedArtifact,
	humanFactor string,
	webhookSecret []byte,
	webhookActivated *bool,
) error {
	if err := fenceDefinitionAdmissionTx(ctx, tx, id, normalized, actor); err != nil {
		return err
	}
	if handled, err := s.preparePlatformEnvironmentTx(
		ctx, tx, id, normalized, actor, resolved, humanFactor, webhookSecret, webhookActivated,
	); handled {
		return err
	}
	if handled, err := s.preparePlatformDeliveryTx(
		ctx, tx, id, normalized, actor, resolved, artifact,
	); handled {
		return err
	}
	return nil
}

func (s *Service) preparePlatformEnvironmentTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	normalized platform.Normalized,
	actor contract.Actor,
	resolved *resolvedGitRef,
	humanFactor string,
	webhookSecret []byte,
	webhookActivated *bool,
) (bool, error) {
	switch input := normalized.Input.(type) {
	case *platform.AppCreateInput:
		return true, s.prepareAppCreateTx(ctx, tx, id, input, actor, resolved)
	case *platform.NodeRegisterInput:
		return true, s.prepareNodeRegisterTx(ctx, tx, input)
	case *platform.EnvironmentCreateInput:
		return true, s.prepareEnvironmentCreateTx(ctx, tx, id, input, actor, resolved)
	case *platform.EnvironmentUpdateInput:
		return true, s.prepareEnvironmentUpdateTx(ctx, tx, id, input, actor, webhookSecret, webhookActivated)
	case *platform.ProcessScaleInput:
		return true, s.prepareProcessScaleTx(ctx, tx, id, input, actor)
	case *platform.ConfigSetInput:
		return true, s.prepareConfigSetTx(ctx, tx, id, input, actor)
	case *platform.ConfigUnsetInput:
		return true, s.prepareConfigUnsetTx(ctx, tx, id, input, actor)
	case *platform.EnvironmentSecretAttachInput:
		return true, s.prepareEnvironmentSecretAttachTx(ctx, tx, id, input, actor)
	case *platform.EnvironmentResourceInput:
		return true, s.prepareEnvironmentResourceTx(ctx, tx, id, normalized, input, actor)
	case *platform.EnvironmentProtectInput:
		return true, s.prepareEnvironmentProtectTx(ctx, tx, input, humanFactor)
	case *platform.AppDeleteInput:
		return true, s.prepareAppDeleteTx(ctx, tx, id, input, actor, humanFactor)
	case *platform.EnvironmentDeleteInput:
		return true, s.prepareEnvironmentDeleteTx(ctx, tx, id, input, actor, humanFactor)
	}
	return false, nil
}

func (s *Service) preparePlatformDeliveryTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	normalized platform.Normalized,
	actor contract.Actor,
	resolved *resolvedGitRef,
	artifact *resolvedArtifact,
) (bool, error) {
	switch input := normalized.Input.(type) {
	case *platform.DomainInput:
		return true, s.prepareDomainTx(ctx, tx, id, normalized, input, actor)
	case *platform.BuildStopInput:
		return true, s.prepareBuildStopTx(ctx, tx, input)
	case *platform.BuildRetryInput:
		return true, s.prepareBuildRetryTx(ctx, tx, id, input)
	case *platform.DeployInput:
		return true, s.prepareDeployTx(ctx, tx, id, input, resolved, artifact)
	case *platform.PromoteInput:
		return true, s.preparePromotionTx(ctx, tx, id, input)
	case *platform.RollbackInput:
		return true, s.prepareRollbackTx(ctx, tx, id, input)
	case *platform.RestartInput:
		return true, s.prepareRestartTx(ctx, tx, id, input)
	case *platform.ResourceApplyInput:
		return true, s.prepareResourceApply(ctx, tx, id, input)
	case *platform.ResourceDestroyInput:
		return true, s.prepareResourceDestroy(ctx, tx, id, input)
	case *platform.NodeCheckInput:
		return true, prepareNodeExistsTx(ctx, tx, input.Node)
	case *platform.NodeEnrollInput:
		return true, s.prepareNodeEnroll(ctx, tx, id, input)
	case *platform.NodeReconcileInput:
		return true, prepareNodeExistsTx(ctx, tx, input.Node)
	}
	return false, nil
}

func (s *Service) prepareAppCreateTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	input *platform.AppCreateInput,
	actor contract.Actor,
	resolved *resolvedGitRef,
) error {
	appID, err := platform.NewAppID()
	if err != nil {
		return unavailable()
	}
	if _, err := tx.Platform().Apps().Insert(ctx, entities.AppInsert{
		ID: appID, Name: input.Name, ImageRepository: input.ImageRepository,
	}); err != nil {
		return platformWriteError(err, "application name or image repository already exists")
	}
	app := platform.App{ID: appID, Name: input.Name, ImageRepository: input.ImageRepository}
	for index := range input.Environments {
		spec := &input.Environments[index]
		deployIntent := input.Deploy && index == 0
		summary := "Created environment " + spec.Name + " with its first desired-state revision."
		if deployIntent {
			summary += " The first deploy was queued from that revision."
		}
		envID, err := s.insertEnvironmentTx(
			ctx,
			tx,
			app,
			spec,
			id,
			actor,
			"environment.created",
			summary,
			deployIntent,
		)
		if err != nil {
			return err
		}
		if deployIntent {
			if err := s.queueFirstDeployTx(ctx, tx, envID, actor, resolved); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) prepareNodeRegisterTx(ctx context.Context, tx *entities.StoreTx, input *platform.NodeRegisterInput) error {
	// Registration records declared facts. It can never overwrite a node that
	// is enrolling or enrolled: verified facts are not replaced by a claim.
	inserted, err := tx.Nodes().InsertRegistration(ctx, input.Name, input.Roles, input.Capabilities, input.Address)
	if err != nil {
		return platformWriteError(err, "node registration conflicts")
	}
	if !inserted {
		return contract.NewFault(
			contract.Conflict,
			"node is enrolling or enrolled; declared facts cannot overwrite verified ones",
			"An enrolled node's verified facts cannot be replaced by declared facts.",
		)
	}
	return nil
}

func (s *Service) prepareEnvironmentCreateTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	input *platform.EnvironmentCreateInput,
	actor contract.Actor,
	resolved *resolvedGitRef,
) error {
	app, err := appByNameForUpdateTx(ctx, tx, input.App)
	if err != nil {
		return err
	}
	summary := "Created environment " + input.Name + " with its first desired-state revision."
	if input.Deploy {
		summary += " The first deploy was queued from that revision."
	}
	envID, err := s.insertEnvironmentTx(
		ctx,
		tx,
		app,
		&input.EnvironmentSpec,
		id,
		actor,
		"environment.created",
		summary,
		input.Deploy,
	)
	if err != nil {
		return err
	}
	if input.Deploy {
		if err := s.queueFirstDeployTx(ctx, tx, envID, actor, resolved); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) prepareEnvironmentUpdateTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	input *platform.EnvironmentUpdateInput,
	actor contract.Actor,
	webhookSecret []byte,
	webhookActivated *bool,
) error {
	env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
	if err != nil {
		return err
	}
	if input.ExpectedRevisionID != "" && env.RevisionID != input.ExpectedRevisionID {
		return contract.NewFault(
			contract.Conflict,
			"the environment changed since the definition was read",
			"Re-read the environment and re-apply the definition; no revision was written.",
		)
	}
	base, err := currentRevisionStateTx(tx, env)
	if err != nil {
		return err
	}
	state := platform.ApplyEnvironmentPatch(base, *input)
	platform.ResolveWebhookSecretName(&state)
	if err := platform.ValidatePushToDeploy(state); err != nil {
		return err
	}
	if platform.EnvironmentUpdateRemovesState(base, state) {
		if err := platform.ValidateTargetConfirmation(
			input.Target(),
			input.Confirm,
		); err != nil {
			return err
		}
	}
	buildNode, buildNodeChanged, err := s.resolveUpdateBuildNodeTx(ctx, tx, input, env)
	if err != nil {
		return err
	}
	nodeName, err := s.resolveUpdateRuntimeNodeTx(ctx, tx, state, env)
	if err != nil {
		return err
	}
	state.Runtime.Node = nodeName
	activated, err := s.activateWebhookSecretTx(ctx, tx, actor, input, state, webhookSecret)
	if err != nil {
		return err
	}
	*webhookActivated = activated
	rotationOnly := input.RotateWebhookSecret && !buildNodeChanged &&
		reflect.DeepEqual(base, state)
	if !rotationOnly {
		if _, err := publishEnvironmentRevisionTx(
			ctx,
			tx,
			env,
			id,
			actor,
			"environment.updated",
			"Updated environment "+env.Name+" desired state.",
			state,
			buildNode,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) resolveUpdateBuildNodeTx(
	ctx context.Context,
	tx *entities.StoreTx,
	input *platform.EnvironmentUpdateInput,
	env platform.Environment,
) (*string, bool, error) {
	if input.BuildNode == nil {
		return nil, false, nil
	}
	value := *input.BuildNode
	if value != "" {
		resolved, err := s.resolveBuilderNameTx(ctx, tx, value)
		if err != nil {
			return nil, false, err
		}
		value = resolved
	}
	return &value, value != env.BuildNode, nil
}

func (s *Service) resolveBuilderNameTx(ctx context.Context, tx *entities.StoreTx, name string) (string, error) {
	builder, err := nodeByNameTx(ctx, tx, name)
	if err != nil {
		return "", err
	}
	if err := s.checkPlacement(
		builder,
		platform.RoleBuilder,
		platform.CapabilityBuildpacks,
	); err != nil {
		return "", err
	}
	return builder.Name, nil
}

func (s *Service) resolveUpdateRuntimeNodeTx(
	ctx context.Context,
	tx *entities.StoreTx,
	state platform.RevisionState,
	env platform.Environment,
) (string, error) {
	if state.Runtime.Node == env.Node {
		return env.Node, nil
	}
	node, err := nodeByNameTx(ctx, tx, state.Runtime.Node)
	if err != nil {
		return "", err
	}
	if err := s.checkPlacement(
		node,
		platform.RoleApplication,
		platform.CapabilityDocker,
	); err != nil {
		return "", err
	}
	return node.Name, nil
}

func (s *Service) prepareProcessScaleTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	input *platform.ProcessScaleInput,
	actor contract.Actor,
) error {
	// This transaction gate excludes legacy session locks and serializes the
	// durable claim check with acquisition. Environment claim acquisition must
	// take the same gate in its transaction.
	target := "env/" + input.App + "/" + input.Environment
	lockAcquired, err := tx.Platform().Locks().TryTargetXact(ctx, targetLockKey(target))
	if err != nil {
		return unavailable(err)
	}
	if !lockAcquired {
		return contract.NewFault(contract.Conflict,
			"a deployment lifecycle owns this environment",
			"Wait for the current deployment, rollback or restart to settle before scaling.")
	}
	claimed, err := tx.TargetClaimed(ctx, target)
	if err != nil {
		return unavailable(err)
	}
	if claimed {
		return contract.NewFault(contract.Conflict,
			"a deployment lifecycle owns this environment",
			"Wait for the current deployment, rollback or restart to settle before scaling.")
	}
	env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
	if err != nil {
		return err
	}
	ownerBusy, err := tx.Platform().Owners().ExistsForEnvironment(ctx, env.ID)
	if err != nil {
		return unavailable(err)
	}
	if ownerBusy {
		return contract.NewFault(contract.Conflict,
			"a deployment lifecycle owns this environment",
			"Wait for the current deployment, rollback or restart to settle before scaling.")
	}
	base, err := currentRevisionStateTx(tx, env)
	if err != nil {
		return err
	}
	state, err := platform.ScaleProcess(base, input.Process, input.Replicas)
	if err != nil {
		return err
	}
	revisionID, err := publishEnvironmentRevisionTx(
		ctx,
		tx,
		env,
		id,
		actor,
		"process.scaled",
		"Scaled process "+input.Process+" to "+fmt.Sprint(
			input.Replicas,
		)+" replica(s) in a new revision.",
		state,
		nil,
	)
	if err != nil {
		return err
	}
	// A serving environment applies the new formation immediately through
	// the same blue/green restart as deploycrate. The restart is queued in
	// this transaction, keyed by the revision it applies, so a retry never
	// starts two. Without a serving deployment the next deploy applies it.
	if env.ServingRelease != "" && env.ServingDeploymentID != "" {
		target := "env/" + env.App + "/" + env.Name
		restart := platform.RestartInput{App: env.App, Environment: env.Name}
		if err := s.submitFollowupOperationTx(
			ctx,
			tx,
			platform.KindRestart,
			target,
			restart,
			actor,
			"scale-restart:"+string(revisionID),
			nil,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) prepareConfigSetTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	input *platform.ConfigSetInput,
	actor contract.Actor,
) error {
	env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
	if err != nil {
		return err
	}
	base, err := currentRevisionStateTx(tx, env)
	if err != nil {
		return err
	}
	state := platform.SetConfig(base, input.Config)
	if _, err := publishEnvironmentRevisionTx(
		ctx,
		tx,
		env,
		id,
		actor,
		"config.set",
		"Set plain environment configuration in a new revision; no deploy was queued.",
		state,
		nil,
	); err != nil {
		return err
	}
	return nil
}

func (s *Service) prepareConfigUnsetTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	input *platform.ConfigUnsetInput,
	actor contract.Actor,
) error {
	env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
	if err != nil {
		return err
	}
	base, err := currentRevisionStateTx(tx, env)
	if err != nil {
		return err
	}
	state := platform.UnsetConfig(base, input.Keys)
	if _, err := publishEnvironmentRevisionTx(
		ctx,
		tx,
		env,
		id,
		actor,
		"config.unset",
		"Removed plain environment configuration in a new revision; no deploy was queued.",
		state,
		nil,
	); err != nil {
		return err
	}
	return nil
}

func (s *Service) prepareEnvironmentSecretAttachTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	input *platform.EnvironmentSecretAttachInput,
	actor contract.Actor,
) error {
	env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
	if err != nil {
		return err
	}
	base, err := currentRevisionStateTx(tx, env)
	if err != nil {
		return err
	}
	state := platform.AttachSecret(base, input.Name)
	if _, err := publishEnvironmentRevisionTx(
		ctx,
		tx,
		env,
		id,
		actor,
		"environment.secret.attached",
		"Attached vault secret name "+input.Name+" to the environment desired state; no value entered the revision.",
		state,
		nil,
	); err != nil {
		return err
	}
	return nil
}

func (s *Service) prepareEnvironmentResourceTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	normalized platform.Normalized,
	input *platform.EnvironmentResourceInput,
	actor contract.Actor,
) error {
	env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
	if err != nil {
		return err
	}
	base, err := currentRevisionStateTx(tx, env)
	if err != nil {
		return err
	}
	state := platform.AttachResource(base, input.Resource)
	operation, summary := "environment.resource.attached", "Attached managed resource "+input.Resource+" to the environment desired state; readiness still requires the resource to be healthy."
	if normalized.Kind == platform.KindEnvironmentResourceDetach {
		state = platform.DetachResource(base, input.Resource)
		operation, summary = "environment.resource.detached", "Detached managed resource "+input.Resource+" from the environment desired state; its container and volumes are retained."
	} else {
		stateName, found, err := tx.Resources().LockState(ctx, env.ID, input.Resource)
		if err != nil {
			return unavailable(err)
		}
		if !found {
			return contract.NewFault(
				contract.NotFound,
				"resource not found in this environment",
				"Apply the resource declaration first, then attach it.",
			)
		}
		if stateName == platform.ResourceDestroyed ||
			stateName == platform.ResourceDestroying {
			return contract.NewFault(
				contract.PreconditionFailed,
				"resource is being destroyed",
				"Apply the resource again before attaching it to an environment revision.",
			)
		}
	}
	if _, err := publishEnvironmentRevisionTx(
		ctx,
		tx,
		env,
		id,
		actor,
		operation,
		summary,
		state,
		nil,
	); err != nil {
		return err
	}
	return nil
}

func (s *Service) prepareEnvironmentProtectTx(
	ctx context.Context,
	tx *entities.StoreTx,
	input *platform.EnvironmentProtectInput,
	humanFactor string,
) error {
	env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
	if err != nil {
		return err
	}
	if env.Protected == input.Protected {
		return nil
	}
	if platform.ProtectionChangeRequiresHumanFactor(env.Protected, input.Protected) {
		if err := s.requireHumanFactorTx(humanFactor); err != nil {
			return err
		}
	}
	if _, err := tx.Platform().Environments().SetProtected(ctx, env.ID, input.Protected); err != nil {
		return unavailable(err)
	}
	return nil
}

func (s *Service) prepareAppDeleteTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	input *platform.AppDeleteInput,
	actor contract.Actor,
	humanFactor string,
) error {
	app, err := appByNameForUpdateTx(ctx, tx, input.App)
	if err != nil {
		return err
	}
	if err := platform.ValidateTargetConfirmation(input.Target(), input.Confirm); err != nil {
		return err
	}
	if err := s.requireHumanFactorTx(humanFactor); err != nil {
		return err
	}
	if err := s.ensureNoLiveResourcesTx(
		ctx,
		tx,
		string(app.ID),
		"",
	); err != nil {
		return err
	}
	if err := s.ensureNoActiveDeleteTargets(ctx, tx, id, input.App, ""); err != nil {
		return err
	}
	if err := s.ensureDomainDNSCleanupSafeTx(ctx, tx, actor, input.App, ""); err != nil {
		return err
	}
	if err := markDomainDNSCleanupForEnvironmentsTx(ctx, tx, input.App, "", id); err != nil {
		return err
	}
	return nil
}

func (s *Service) prepareEnvironmentDeleteTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	input *platform.EnvironmentDeleteInput,
	actor contract.Actor,
	humanFactor string,
) error {
	app, err := appByNameForUpdateTx(ctx, tx, input.App)
	if err != nil {
		return err
	}
	env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
	if err != nil {
		return err
	}
	if err := platform.ValidateTargetConfirmation(input.Target(), input.Confirm); err != nil {
		return err
	}
	last, err := lastEnvironmentTx(ctx, tx, app.ID, env.ID)
	if err != nil {
		return err
	}
	if platform.EnvironmentDeleteRequiresHumanFactor(env.Protected, last) {
		if err := s.requireHumanFactorTx(humanFactor); err != nil {
			return err
		}
	}
	if err := s.ensureNoLiveResourcesTx(
		ctx,
		tx,
		string(envAppID(ctx, tx, env.App)),
		string(env.ID),
	); err != nil {
		return err
	}
	if err := s.ensureNoActiveDeleteTargets(ctx, tx, id, env.App, env.Name); err != nil {
		return err
	}
	if err := s.ensureDomainDNSCleanupSafeTx(ctx, tx, actor, env.App, env.Name); err != nil {
		return err
	}
	if err := markDomainDNSCleanupForEnvironmentsTx(
		ctx,
		tx,
		env.App,
		env.Name,
		id,
	); err != nil {
		return err
	}
	return nil
}

func (s *Service) prepareBuildStopTx(ctx context.Context, tx *entities.StoreTx, input *platform.BuildStopInput) error {
	build, err := buildByIDTx(ctx, tx, input.BuildID)
	if err != nil {
		return err
	}
	if build.State == platform.BuildSucceeded {
		return contract.NewFault(
			contract.Conflict,
			"build already succeeded",
			"Retry with a new build after changing the source if another artifact is needed.",
		)
	}
	return nil
}

func (s *Service) prepareBuildRetryTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, input *platform.BuildRetryInput) error {
	old, err := buildByIDTx(ctx, tx, input.BuildID)
	if err != nil {
		return err
	}
	if old.State != platform.BuildFailed && old.State != platform.BuildCancelled {
		return contract.NewFault(
			contract.PreconditionFailed,
			"build is not in a retryable state",
			"Wait for the build to finish, or stop a running build first.",
		)
	}
	env, err := environmentByNameTx(ctx, tx, old.App, old.Environment)
	if err != nil {
		return err
	}
	return s.createRetryReleaseTx(ctx, tx, id, env, old)
}

func (s *Service) prepareDeployTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	input *platform.DeployInput,
	resolved *resolvedGitRef,
	artifact *resolvedArtifact,
) error {
	env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
	if err != nil {
		return err
	}
	explicitArtifact := input.ImageTag != "" || input.ImageDigest != "" || input.Image != ""
	if explicitArtifact && artifact == nil {
		return contract.NewFault(
			contract.Internal,
			"artifact selector was not resolved before admission",
			"Retry the deploy; no operation was recorded.",
		)
	}
	if err := s.checkDeployabilityTx(
		ctx,
		tx,
		env,
		input.ReleaseID != "" || explicitArtifact,
	); err != nil {
		return err
	}
	releaseID := input.ReleaseID
	if explicitArtifact {
		releaseID, err = s.createArtifactReleaseTx(ctx, tx, id, env, artifact)
		if err != nil {
			return err
		}
	} else if releaseID == "" {
		releaseID, err = s.createReleaseFromRevisionTx(ctx, tx, id, env, input.Ref, resolved)
		if err != nil {
			return err
		}
	}
	release, err := releaseByIDTx(ctx, tx, releaseID)
	if err != nil {
		return err
	}
	if release.App != env.App || release.Environment != env.Name {
		return contract.NewFault(
			contract.ValidationFailed,
			"release belongs to another environment",
			"Use a release created for this app and environment.",
		)
	}
	if err := refusePrunedRelease(release); err != nil {
		return err
	}
	if release.Status != platform.ReleaseReady && input.ReleaseID != "" {
		return contract.NewFault(
			contract.PreconditionFailed,
			"release is not ready",
			"Wait for the release operation to succeed, then deploy it.",
		)
	}
	if err := s.checkPlacementByTx(
		ctx,
		tx,
		env.Node,
	); err != nil {
		return err
	}
	return s.prepareDeploymentTx(
		ctx,
		tx,
		id,
		env,
		release.ID,
		platform.ActionDeploy,
		env.RevisionID,
		env.ServingDeploymentID,
		nil,
	)
}

func (s *Service) prepareRollbackTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	input *platform.RollbackInput,
) error {
	env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
	if err != nil {
		return err
	}
	targetID := input.ToReleaseID
	if targetID == "" {
		targetID, err = previousRollbackRelease(ctx, tx, env)
		if err != nil {
			return err
		}
	}
	if targetID == "" {
		return contract.NewFault(
			contract.PreconditionFailed,
			"environment has no previous release",
			"Deploy a release before rolling back, or provide to_release_id explicitly.",
		)
	}
	release, err := releaseByIDTx(ctx, tx, targetID)
	if err != nil {
		return err
	}
	if release.App != env.App || release.Environment != env.Name {
		return contract.NewFault(
			contract.ValidationFailed,
			"release belongs to another environment",
			"Use a release created for this app and environment.",
		)
	}
	if err := refusePrunedRelease(release); err != nil {
		return err
	}
	if release.Status != platform.ReleaseReady {
		return contract.NewFault(
			contract.PreconditionFailed,
			"release is not ready",
			"Roll back to a ready release.",
		)
	}
	if err := s.checkPlacementByTx(
		ctx,
		tx,
		env.Node,
	); err != nil {
		return err
	}
	revisionID, secrets, err := rollbackSnapshotTx(ctx, tx, env, release.ID)
	if err != nil {
		return err
	}
	return s.prepareDeploymentTx(
		ctx,
		tx,
		id,
		env,
		release.ID,
		platform.ActionRollback,
		revisionID,
		env.ServingDeploymentID,
		secrets,
	)
}

func (s *Service) prepareRestartTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	input *platform.RestartInput,
) error {
	env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
	if err != nil {
		return err
	}
	if env.ServingRelease == "" || env.ServingDeploymentID == "" {
		return contract.NewFault(
			contract.PreconditionFailed,
			"no serving deployment is available to restart",
			"Deploy a release before restarting the environment.",
		)
	}
	release, err := releaseByIDTx(ctx, tx, env.ServingRelease)
	if err != nil {
		return err
	}
	if release.Status != platform.ReleaseReady {
		return contract.NewFault(
			contract.PreconditionFailed,
			"serving release is not ready",
			"Deploy a ready release before restarting.",
		)
	}
	if input.ExpectedServingDeploymentID != "" {
		// A maintenance repair names the serving deployment it observed.
		// The environment row is locked FOR UPDATE by this transaction, so
		// a deploy accepted before this submission is visible here.
		active, err := activeEnvironmentOperations(ctx, tx.Operations(), env.App, env.Name, id)
		if err != nil {
			return err
		}
		if input.ExpectedServingDeploymentID != env.ServingDeploymentID {
			return contract.NewFault(
				contract.PreconditionFailed,
				"serving deployment changed before the repair was queued",
				"The next maintenance run will re-observe the serving workload.",
			)
		}
		if !repairSubmissionAllowed(
			input.ExpectedServingDeploymentID,
			env.ServingDeploymentID,
			active,
		) {
			return contract.NewFault(
				contract.Conflict,
				"environment has an active deployment, promotion, restart or deletion operation",
				"Let the current operation finish; the next maintenance run will re-observe.",
			)
		}
	}
	if err := s.checkPlacementByTx(
		ctx,
		tx,
		env.Node,
	); err != nil {
		return err
	}
	return s.prepareDeploymentTx(
		ctx,
		tx,
		id,
		env,
		release.ID,
		platform.ActionRestart,
		env.RevisionID,
		env.ServingDeploymentID,
		nil,
	)
}

func prepareNodeExistsTx(ctx context.Context, tx *entities.StoreTx, node string) error {
	_, err := nodeByNameTx(ctx, tx, node)
	return err
}

func (s *Service) prepareDomainTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	normalized platform.Normalized,
	input *platform.DomainInput,
	actor contract.Actor,
) error {
	env, step, err := s.prepareDomainCommonTx(ctx, tx, id, normalized, input, actor)
	if err != nil {
		return err
	}
	if normalized.Kind == platform.KindDomainRemove {
		return s.prepareDomainRemoveTx(ctx, tx, id, env, input, actor)
	}
	return s.prepareDomainAddTx(ctx, tx, id, env, input, actor, step)
}

func (s *Service) prepareDomainCommonTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	normalized platform.Normalized,
	input *platform.DomainInput,
	actor contract.Actor,
) (platform.Environment, *definitionDNSChild, error) {
	if _, err := appByNameForUpdateTx(ctx, tx, input.App); err != nil {
		return platform.Environment{}, nil, err
	}
	env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
	if err != nil {
		return platform.Environment{}, nil, err
	}
	step, err := loadDefinitionDNSChildTx(ctx, tx, id)
	if err != nil {
		return platform.Environment{}, nil, err
	}
	if step != nil {
		if err := validateDomainDefinitionChildTx(ctx, tx, normalized, input, actor, env, step); err != nil {
			return platform.Environment{}, nil, err
		}
	}
	if err := s.ensureNoActiveDomainChangeTx(
		ctx,
		tx,
		id,
		input.App,
		input.Environment,
	); err != nil {
		return platform.Environment{}, nil, err
	}
	return env, step, nil
}

func validateDomainDefinitionChildTx(
	ctx context.Context,
	tx *entities.StoreTx,
	normalized platform.Normalized,
	input *platform.DomainInput,
	actor contract.Actor,
	env platform.Environment,
	step *definitionDNSChild,
) error {
	if step.Actor != actor || step.Kind != normalized.Kind ||
		step.Target != normalized.Target.String() ||
		step.InputHash != hashInput(normalized.Canonical) ||
		step.EnvironmentID != env.ID {
		return definitionDNSConflict()
	}
	if normalized.Kind == platform.KindDomainRemove || step.AllocatedID == "" {
		existing, found, err := tx.Platform().Domains().LockByNameAndEnvironment(ctx, input.Domain, env.ID)
		if err != nil {
			return unavailable(err)
		}
		if !found || existing != step.DomainID {
			return definitionDNSConflict()
		}
	}
	return nil
}

func (s *Service) prepareDomainAddTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	env platform.Environment,
	input *platform.DomainInput,
	actor contract.Actor,
	step *definitionDNSChild,
) error {
	var managedZone integrations.DNSZone
	if input.DNSMode == integrations.DomainDNSModeCloudflare {
		if err := validateDomainDNSAddInput(input); err != nil {
			return err
		}
		zone, err := s.resolveDomainDNSZoneTx(ctx, tx, input.DNSZone)
		if err != nil {
			return err
		}
		if err := integrations.ValidateDomainDNSHost(zone, input.Domain); err != nil {
			return err
		}
		if err := s.authorizeDomainDNSWrite(ctx, actor); err != nil {
			return err
		}
		managedZone = zone
	}
	domainID, err := s.insertDomainBindingTx(ctx, tx, env, input, step)
	if err != nil {
		return err
	}
	if input.DNSMode == integrations.DomainDNSModeCloudflare {
		return upsertDomainDNSIntentTx(
			ctx,
			tx,
			domainID,
			managedZone,
			input.DNSOrigins,
			*input.DNSProxied,
			id,
		)
	}
	return s.preserveExistingDomainDNSTx(ctx, tx, id, domainID, actor)
}

func validateDomainDNSAddInput(input *platform.DomainInput) error {
	if input.DNSProxied == nil {
		return contract.NewFault(
			contract.ValidationFailed,
			"Cloudflare DNS needs an explicit proxy mode",
			"Use --dns-proxy dns-only or --dns-proxy proxied; the choice is never inferred.",
		)
	}
	if len(input.DNSOrigins) == 0 {
		return contract.NewFault(
			contract.ValidationFailed,
			"Cloudflare DNS needs explicit public origin addresses",
			"Pass --dns-origin for each public origin address; the WireGuard management endpoint is not used.",
		)
	}
	return nil
}

func (s *Service) insertDomainBindingTx(
	ctx context.Context,
	tx *entities.StoreTx,
	env platform.Environment,
	input *platform.DomainInput,
	step *definitionDNSChild,
) (platform.DomainID, error) {
	if step != nil {
		return s.insertDomainWithIDTx(ctx, tx, env, *input, step.DomainID)
	}
	return s.insertDomainTx(ctx, tx, env, *input)
}

func (s *Service) preserveExistingDomainDNSTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	domainID platform.DomainID,
	actor contract.Actor,
) error {
	ledger, err := loadDomainDNSLedgerByDomainIDTx(ctx, tx, domainID)
	if err != nil {
		return err
	}
	if ledger == nil {
		return nil
	}
	// A manual re-add or promotion of a managed binding still mutates the
	// ownership fencing row. Require DNS authority even when the provider
	// desired state is preserved.
	if err := s.authorizeDomainDNSWrite(ctx, actor); err != nil {
		return err
	}
	if ledger.State == domainDNSStateCleanupPending ||
		ledger.State == domainDNSStateCleanupFailed {
		return contract.NewFault(
			contract.Conflict,
			"managed DNS cleanup is pending for this binding",
			"Retry the deletion/cleanup, or re-add the binding explicitly with --dns cloudflare.",
		)
	}
	if ledger.State != domainDNSStateApplied {
		return contract.NewFault(
			contract.Conflict,
			"managed DNS desired state is not applied",
			"Retry domain add with the explicit Cloudflare flags, or clean up the binding first.",
		)
	}
	if len(ledger.DesiredOrigins) > 0 {
		if _, err := tx.Platform().Domains().SetExpectedAddressesByID(ctx, domainID, ledger.DesiredOrigins); err != nil {
			return unavailable(err)
		}
	}
	return fenceDomainDNSOperationTx(ctx, tx, domainID, id)
}

func (s *Service) prepareDomainRemoveTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	env platform.Environment,
	input *platform.DomainInput,
	actor contract.Actor,
) error {
	binding, found, err := tx.Platform().Domains().BindingByNameAndEnvironment(ctx, input.Domain, env.ID)
	if err != nil {
		return unavailable(err)
	}
	if !found {
		return contract.NewFault(
			contract.NotFound,
			input.Domain+" is not routed to "+input.App+"/"+input.Environment,
			"List routes with eregion domain list.",
		)
	}
	domainID, primary := binding.ID, binding.Primary
	if err := platform.ValidateDomainConfirmation(input.Domain, input.Confirm); err != nil {
		return err
	}
	if primary {
		return contract.NewFault(
			contract.PreconditionFailed,
			"the primary domain cannot be removed",
			"Add another domain with --primary first, then remove this one. Deleting the environment or app cleans its managed DNS.",
		)
	}
	ledger, err := loadDomainDNSLedgerByDomainIDTx(ctx, tx, domainID)
	if err != nil {
		return err
	}
	if ledger != nil {
		return s.prepareManagedDomainRemoveTx(ctx, tx, id, input, actor, domainID, ledger)
	}
	if input.DNSMode != integrations.DomainDNSModeCloudflare {
		return nil
	}
	return s.startDomainDNSCleanupTx(ctx, tx, id, input, actor, domainID)
}

func (s *Service) prepareManagedDomainRemoveTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	input *platform.DomainInput,
	actor contract.Actor,
	domainID platform.DomainID,
	ledger *domainDNSLedger,
) error {
	if err := s.validateManagedDomainDNSZoneTx(ctx, tx, input, ledger); err != nil {
		return err
	}
	if err := s.validateDomainDNSCleanupRecovery(*ledger); err != nil {
		return err
	}
	if err := s.authorizeDomainDNSWrite(ctx, actor); err != nil {
		return err
	}
	return markDomainDNSCleanupTx(ctx, tx, domainID, id)
}

func (s *Service) validateManagedDomainDNSZoneTx(
	ctx context.Context,
	tx *entities.StoreTx,
	input *platform.DomainInput,
	ledger *domainDNSLedger,
) error {
	if input.DNSMode != integrations.DomainDNSModeCloudflare || input.DNSZone == "" {
		return nil
	}
	zone, err := s.resolveDomainDNSZoneTx(ctx, tx, input.DNSZone)
	if err != nil {
		return err
	}
	if zone.ZoneID != ledger.ZoneID {
		return contract.NewFault(
			contract.Conflict,
			"managed DNS zone does not match the binding ledger",
			"Omit --dns-zone to use the persisted zone, or correct the flag.",
		)
	}
	return nil
}

func (s *Service) startDomainDNSCleanupTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id contract.OperationID,
	input *platform.DomainInput,
	actor contract.Actor,
	domainID platform.DomainID,
) error {
	zone, err := s.resolveDomainDNSZoneTx(ctx, tx, input.DNSZone)
	if err != nil {
		return err
	}
	if err := integrations.ValidateDomainDNSHost(zone, input.Domain); err != nil {
		return err
	}
	if err := s.authorizeDomainDNSWrite(ctx, actor); err != nil {
		return err
	}
	return insertDomainDNSCleanupIntentTx(ctx, tx, domainID, zone, id)
}

func (s *Service) previewDeployKind(
	ctx context.Context,
	normalized platform.Normalized,
	input *platform.DeployInput,
	preview *platform.Preview,
) error {
	explicitArtifact := input.ImageTag != "" || input.ImageDigest != "" || input.Image != ""
	if input.ReleaseID == "" {
		return s.previewDeployFromEnvironment(ctx, input, normalized, explicitArtifact, preview)
	}
	env, release, err := s.planDeployment(ctx, normalized.Target, input.ReleaseID)
	if err != nil {
		return err
	}
	preview.Steps = deploymentSteps()
	preview.Summary = fmt.Sprintf(
		"Deploy %s to %s/%s with the current revision after candidate health and public verification.",
		release.Image,
		env.App,
		env.Name,
	)
	addReleaseWarnings(preview, release, env)
	return nil
}

func (s *Service) previewBuildStopKind(ctx context.Context, input *platform.BuildStopInput, preview *platform.Preview) error {
	build, err := s.buildByID(ctx, input.BuildID)
	if err != nil {
		return err
	}
	preview.Steps = []string{
		"mark the build cancelled",
		"mark the build operation cancelling",
		"signal the running pack process",
	}
	preview.Summary = fmt.Sprintf(
		"Stop build %s for %s/%s.",
		build.ID,
		build.App,
		build.Environment,
	)
	return nil
}

func (s *Service) previewBuildRetryKind(ctx context.Context, input *platform.BuildRetryInput, preview *platform.Preview) error {
	build, err := s.buildByID(ctx, input.BuildID)
	if err != nil {
		return err
	}
	preview.Steps = []string{
		"record a new build of the same commit",
		"pack build",
		"verify image digest",
	}
	preview.Summary = fmt.Sprintf(
		"Retry build %s at commit %s as a new build; it does not deploy.",
		build.ID,
		build.CommitSHA,
	)
	return nil
}

func (s *Service) previewRestartKind(
	ctx context.Context,
	normalized platform.Normalized,
	preview *platform.Preview,
) error {
	env, err := s.environment(ctx, normalized.Target.App, normalized.Target.Environment)
	if err != nil {
		return err
	}
	if env.ServingRelease == "" {
		return contract.NewFault(
			contract.PreconditionFailed,
			"no serving deployment is available to restart",
			"Deploy a release before restarting the environment.",
		)
	}
	preview.Steps = deploymentSteps()
	preview.Summary = fmt.Sprintf(
		"Reapply the serving release %s to %s/%s with the current revision.",
		env.ServingRelease,
		env.App,
		env.Name,
	)
	return nil
}

func (s *Service) previewRollbackKind(
	ctx context.Context,
	normalized platform.Normalized,
	input *platform.RollbackInput,
	preview *platform.Preview,
) error {
	target := input.ToReleaseID
	env, err := s.environment(ctx, normalized.Target.App, normalized.Target.Environment)
	if err != nil {
		return err
	}
	if target == "" {
		target, err = s.previousRollbackRelease(ctx, env)
		if err != nil {
			return err
		}
	}
	if target == "" {
		return contract.NewFault(
			contract.PreconditionFailed,
			"environment has no previous release",
			"Deploy a release before rolling back, or provide to_release_id explicitly.",
		)
	}
	_, release, err := s.planDeployment(ctx, normalized.Target, target)
	if err != nil {
		return err
	}
	preview.Steps = deploymentSteps()
	preview.Summary = fmt.Sprintf(
		"Roll back %s/%s to %s using the same health-first path.",
		env.App,
		env.Name,
		release.Image,
	)
	addReleaseWarnings(preview, release, env)
	return nil
}

func (s *Service) previewResourceApplyKind(
	ctx context.Context,
	input *platform.ResourceApplyInput,
	preview *platform.Preview,
) error {
	env, err := s.environment(ctx, input.App, input.Environment)
	if err != nil {
		return err
	}
	if err := s.checkPreviewNode(
		ctx,
		env.Node,
		platform.RoleApplication,
		platform.CapabilityDocker,
	); err != nil {
		return err
	}
	preview.Steps = []string{
		"ensure the environment's private bridge network",
		"pull the declared image",
		"resolve and record the immutable image ID",
		"ensure resource-scoped named volumes",
		"start one container on the private network with no published host port",
		"run the declared exec health check",
	}
	existing, err := s.resourceByName(ctx, input.App, input.Environment, input.Name)
	if err != nil {
		if contract.Classify(err).Code != contract.NotFound {
			return err
		}
		preview.Summary = "Create resource " + input.Name + " for " + input.App + "/" + input.Environment + " from image " + input.Image + " on the environment's private network."
	} else {
		preview.Summary = "Apply resource " + input.Name + " for " + input.App + "/" + input.Environment + " from image " + input.Image + "; the same resource identity and named volumes are retained."
		if existing.Node != env.Node {
			preview.Warnings = append(
				preview.Warnings,
				"This resource is bound to node "+existing.Node+" and cannot be moved implicitly; apply would be refused until it is destroyed on that node.",
			)
		}
	}
	preview.Warnings = append(
		preview.Warnings,
		"A tag is resolved to an immutable image ID at apply time. The resource is not ready until its exec health check succeeds, and no host port is published.",
	)
	if len(input.SecretEnv) > 0 {
		preview.Warnings = append(
			preview.Warnings,
			"Secret values are resolved from the environment vault only inside the worker; this preview and the operation input contain secret names only.",
		)
	}
	return nil
}

func (s *Service) previewResourceDestroyKind(
	ctx context.Context,
	input *platform.ResourceDestroyInput,
	preview *platform.Preview,
) error {
	resource, err := s.resourceByName(ctx, input.App, input.Environment, input.Name)
	if err != nil {
		return err
	}
	if resource.State == platform.ResourceDestroyed && !input.DeleteData {
		preview.Summary = "Resource " + input.Name + " is already destroyed; applying it again reactivates the same identity and retained volumes, or --delete-data deletes the retained data explicitly."
		preview.Steps = []string{"observe that no container is running"}
		return nil
	}
	preview.Steps = []string{
		"verify resource ownership labels",
		"remove the resource container",
		"retain named volumes and the resource identity",
	}
	preview.Summary = "Destroy resource " + input.Name + " for " + input.App + "/" + input.Environment + "; its container is removed and its named volumes are retained."
	preview.Warnings = append(
		preview.Warnings,
		"Named volumes are not deleted by a default destroy; use resource destroy --delete-data to remove this resource's owned volumes explicitly.",
	)
	if input.DeleteData {
		preview.Steps = []string{
			"verify resource ownership labels",
			"remove the resource container",
			"verify and remove each resource-scoped named volume",
		}
		preview.Summary = "Destroy resource " + input.Name + " for " + input.App + "/" + input.Environment + " and permanently delete its named volumes."
		preview.Warnings = []string{
			"DATA DESTRUCTION: --delete-data permanently removes the resource's named volumes after the container is removed. This cannot be undone and is refused for volumes without this resource's ownership labels.",
		}
	}
	return nil
}

func (s *Service) previewRetentionKind(
	ctx context.Context,
	input *platform.RetentionPruneInput,
	preview *platform.Preview,
) error {
	plan, err := s.RetentionPlan(ctx, input.Node, "", "")
	if err != nil {
		return err
	}
	preview.Steps = retentionPlanSteps(plan)
	preview.Summary = fmt.Sprintf(
		"Prune %d planned item(s), reclaiming %d byte(s) on node %s.",
		len(retentionRemovalItems(plan.Items)),
		plan.PlannedBytes,
		input.Node,
	)
	preview.Warnings = append(preview.Warnings, plan.Warnings...)
	for _, item := range plan.Items {
		if item.Kind == retention.ItemRegistrySkipped {
			preview.Warnings = append(preview.Warnings, "registry skipped: "+item.Reason)
		}
	}
	return nil
}

func (s *Service) previewDomainKind(
	ctx context.Context,
	actor contract.Actor,
	normalized platform.Normalized,
	input *platform.DomainInput,
	preview *platform.Preview,
) error {
	if normalized.Kind == platform.KindDomainRemove {
		preview.Summary = "Remove the binding and route for " + input.Domain + " from " + input.App + "/" + input.Environment + "."
	} else {
		preview.Summary = "Bind " + input.Domain + " to " + input.App + "/" + input.Environment + " through Caddy."
	}
	ledger, err := s.loadDomainDNSLedger(ctx, input.App, input.Environment, input.Domain)
	if err != nil {
		return err
	}
	if normalized.Kind == platform.KindDomainAdd && input.DNSMode != "cloudflare" &&
		ledger != nil {
		previewPreserveManagedDomain(ledger, preview)
		return nil
	}
	if input.DNSMode == "cloudflare" || ledger != nil {
		return s.previewManagedDomain(ctx, actor, normalized, input, ledger, preview)
	}
	preview.Summary += " DNS stays manual; no provider change is planned."
	return nil
}

func previewPreserveManagedDomain(ledger *domainDNSLedger, preview *platform.Preview) {
	preview.Data = map[string]any{"dns_managed": managedDNSBinding(*ledger)}
	preview.Steps = append(
		preview.Steps,
		"preserve the persisted managed-DNS zone, origins and proxy desired state",
	)
	preview.Summary += " Managed DNS remains zone " + ledger.ZoneName + " with origins " + strings.Join(
		ledger.DesiredOrigins,
		", ",
	) + "; this operation does not change provider intent."
	preview.Warnings = append(
		preview.Warnings,
		"The binding is already provider-managed; use explicit --dns cloudflare flags to change or reconcile its DNS.",
	)
}

func (s *Service) previewManagedDomain(
	ctx context.Context,
	actor contract.Actor,
	normalized platform.Normalized,
	input *platform.DomainInput,
	ledger *domainDNSLedger,
	preview *platform.Preview,
) error {
	previewInput := *input
	previewInput.DNSMode = "cloudflare"
	if ledger != nil && previewInput.DNSZone == "" {
		previewInput.DNSZone = ledger.ZoneID
	}
	if normalized.Kind == platform.KindDomainAdd {
		if err := validateDomainDNSAddInput(&previewInput); err != nil {
			return err
		}
	}
	plan, err := s.previewDomainDNS(ctx, actor, &previewInput, normalized.Kind)
	if err != nil {
		return err
	}
	preview.Data = map[string]any{"dns_provider": plan}
	preview.Steps = append(
		preview.Steps,
		"read the Cloudflare provider snapshot for "+input.Domain,
	)
	if normalized.Kind == platform.KindDomainRemove {
		preview.Steps = append(
			preview.Steps,
			"remove only records whose comment is this binding's exact ownership marker",
		)
		preview.Summary += " Explicit Cloudflare removal targets only this binding's exact owned records."
	} else {
		preview.Steps = append(
			preview.Steps,
			"create or update only the exact A/AAAA records this binding owns",
		)
		preview.Steps = append(
			preview.Steps,
			"explicit public origins: "+strings.Join(previewInput.DNSOrigins, ", "),
		)
		preview.Summary += " Explicit Cloudflare DNS will ensure the exact desired record from " + strings.Join(
			previewInput.DNSOrigins,
			", ",
		) + "."
	}
	preview.Warnings = append(preview.Warnings, plan.Warnings...)
	preview.Warnings = append(
		preview.Warnings,
		"No provider change happens until this plan is submitted; manual DNS remains the default.",
	)
	return nil
}

func (s *Service) previewAppCreateKind(
	ctx context.Context,
	input *platform.AppCreateInput,
	preview *platform.Preview,
) error {
	if err := s.checkPreviewAppAvailable(ctx, input.Name); err != nil {
		return err
	}
	if strings.TrimSpace(s.registryEndpoint) != "" {
		if err := platform.ValidateAppImageRepositoryForRegistry(
			input.ImageRepository,
			s.registryEndpoint,
		); err != nil {
			return err
		}
	} else {
		return contract.NewFault(
			contract.PreconditionFailed,
			"the node has no configured private registry endpoint",
			"Start the worker with its managed loopback registry endpoint before creating an app image repository.",
		)
	}
	if len(input.Environments) == 0 {
		preview.Summary = "Create application " + input.Name + " without an environment."
		return nil
	}
	preview.Steps = []string{"record application"}
	preview.Summary = "Create application " + input.Name + " with environments"
	domains := map[string]string{}
	for index := range input.Environments {
		spec := &input.Environments[index]
		if err := s.previewAppEnvironmentSpec(ctx, spec, index, input.Deploy, domains, preview); err != nil {
			return err
		}
	}
	preview.Summary += "."
	if input.Deploy {
		preview.Steps = append(
			preview.Steps,
			"queue the first deploy from the first environment's new revision",
		)
		preview.Summary += " A first deploy is queued from " + input.Environments[0].Name + "'s new revision."
	}
	return nil
}

func (s *Service) previewAppEnvironmentSpec(
	ctx context.Context,
	spec *platform.EnvironmentSpec,
	index int,
	deploy bool,
	domains map[string]string,
	preview *platform.Preview,
) error {
	if err := s.checkPreviewPlacement(ctx, spec.Node, spec.BuildNode); err != nil {
		return err
	}
	if index == 0 {
		if err := checkPreviewDeploySource(spec, deploy); err != nil {
			return err
		}
	}
	for _, name := range environmentSpecDomains(spec) {
		if other, exists := domains[name]; exists {
			return contract.NewFault(
				contract.Conflict,
				name+" is declared for both "+other+" and "+spec.Name,
				"Each hostname can route to one environment; put distinct domains in each environment override.",
			)
		}
		domains[name] = spec.Name
		if err := s.checkPreviewDomainAvailable(ctx, name); err != nil {
			return err
		}
	}
	preview.Steps = append(
		preview.Steps,
		"record environment "+spec.Name+" and its first revision",
		"allocate ports for "+spec.Name,
	)
	preview.Summary += " " + spec.Name
	if spec.Domain != "" {
		preview.Steps = append(
			preview.Steps,
			"record the primary domain binding for "+spec.Name+" before any first deploy",
		)
	}
	if spec.BuildNode != "" && spec.BuildNode != spec.Node && spec.Source.URL != "" {
		preview.Warnings = append(
			preview.Warnings,
			"A distinct build node needs a shared registry to move the image to the application node; the build path refuses it until registry transfer is configured.",
		)
	}
	return nil
}

func (s *Service) previewEnvironmentProtectKind(
	ctx context.Context,
	normalized platform.Normalized,
	input *platform.EnvironmentProtectInput,
	preview *platform.Preview,
) error {
	env, err := s.environment(ctx, normalized.Target.App, normalized.Target.Environment)
	if err != nil {
		return err
	}
	state := "protected"
	if !input.Protected {
		state = "unprotected"
	}
	preview.Steps = []string{"record the environment protection state"}
	preview.Summary = "Set " + input.App + "/" + input.Environment + " to " + state + "."
	if platform.ProtectionChangeRequiresHumanFactor(env.Protected, input.Protected) {
		preview.Warnings = append(
			preview.Warnings,
			"Lowering protection requires the D7 TOTP human factor at a human terminal.",
		)
	}
	return nil
}

func (s *Service) previewEnvironmentCreateKind(
	ctx context.Context,
	input *platform.EnvironmentCreateInput,
	preview *platform.Preview,
) error {
	if err := s.checkPreviewApp(ctx, input.App); err != nil {
		return err
	}
	if err := s.checkPreviewEnvironmentAvailable(ctx, input.App, input.Name); err != nil {
		return err
	}
	if err := s.checkPreviewPlacement(ctx, input.Node, input.BuildNode); err != nil {
		return err
	}
	if err := checkPreviewDeploySource(&input.EnvironmentSpec, input.Deploy); err != nil {
		return err
	}
	if err := s.checkPreviewDomainAvailable(ctx, input.Domain); err != nil {
		return err
	}
	preview.Steps = []string{
		"record environment",
		"record first desired-state revision",
		"allocate stable and candidate ports",
	}
	preview.Summary = "Create environment " + input.Name + " on node " + input.Node + "."
	if input.Domain != "" {
		preview.Steps = append(
			preview.Steps,
			"record the primary domain binding before any first deploy",
		)
		preview.Summary += " Domain " + input.Domain + " is bound in the same submission."
	}
	if input.BuildNode != "" {
		preview.Summary += " Builds run on builder node " + input.BuildNode + "."
		if input.BuildNode != input.Node && input.Source.URL != "" {
			preview.Warnings = append(
				preview.Warnings,
				"A distinct build node needs a shared registry to move the image to the application node; the build path refuses it until registry transfer is configured.",
			)
		}
	}
	if input.Deploy {
		preview.Steps = append(preview.Steps, "queue the first deploy from the new revision")
		preview.Summary += " A first deploy is queued from the new revision."
	}
	return nil
}

func (s *Service) previewEnvironmentUpdateKind(
	ctx context.Context,
	normalized platform.Normalized,
	input *platform.EnvironmentUpdateInput,
	preview *platform.Preview,
) error {
	env, err := s.environment(ctx, normalized.Target.App, normalized.Target.Environment)
	if err != nil {
		return err
	}
	nodeName := env.Node
	if input.Node != nil {
		nodeName = *input.Node
	}
	if input.Runtime != nil && input.Runtime.Node != "" {
		nodeName = input.Runtime.Node
	}
	buildNode := env.BuildNode
	if input.BuildNode != nil {
		buildNode = *input.BuildNode
	}
	if nodeName != env.Node {
		if err := s.checkPreviewNode(
			ctx,
			nodeName,
			platform.RoleApplication,
			platform.CapabilityDocker,
		); err != nil {
			return err
		}
	}
	if input.BuildNode != nil && buildNode != "" {
		if err := s.checkPreviewNode(
			ctx,
			buildNode,
			platform.RoleBuilder,
			platform.CapabilityBuildpacks,
		); err != nil {
			return err
		}
	}
	preview.Steps = []string{
		"load current revision",
		"merge the patch",
		"append a new revision",
	}
	preview.Summary = "Update environment " + input.Environment + " desired state as a new immutable revision; no deploy is queued."
	return nil
}

func previewProcessScaleKind(input *platform.ProcessScaleInput, preview *platform.Preview) {
	preview.Steps = []string{
		"load current revision",
		"change replicas",
		"append a new revision",
	}
	preview.Summary = "Scale process " + input.Process + " to " + fmt.Sprint(
		input.Replicas,
	) + " replica(s) in a new revision; a serving environment also restarts its release through the blue/green path."
}

func previewConfigKind(preview *platform.Preview) {
	preview.Steps = []string{
		"load current revision",
		"change plain configuration",
		"append a new revision",
	}
	preview.Summary = "Change plain environment configuration as a new revision; no deploy is queued."
}

func (s *Service) previewEnvironmentSecretAttachKind(
	ctx context.Context,
	normalized platform.Normalized,
	input *platform.EnvironmentSecretAttachInput,
	preview *platform.Preview,
) error {
	if _, err := s.environment(
		ctx,
		normalized.Target.App,
		normalized.Target.Environment,
	); err != nil {
		return err
	}
	preview.Steps = []string{
		"load current revision",
		"attach the secret name",
		"append a new revision",
	}
	preview.Summary = "Attach vault secret name " + input.Name + " to the environment revision; no value enters the operation."
	return nil
}

func (s *Service) previewEnvironmentResourceKind(
	ctx context.Context,
	normalized platform.Normalized,
	input *platform.EnvironmentResourceInput,
	preview *platform.Preview,
) error {
	env, err := s.environment(ctx, normalized.Target.App, normalized.Target.Environment)
	if err != nil {
		return err
	}
	if _, err := s.resourceByName(
		ctx,
		input.App,
		input.Environment,
		input.Resource,
	); err != nil {
		return err
	}
	preview.Steps = []string{
		"load current revision",
		normalized.Kind + " " + input.Resource,
		"append a new revision",
	}
	if normalized.Kind == platform.KindEnvironmentResourceDetach {
		preview.Summary = "Detach managed resource " + input.Resource + " from " + env.App + "/" + env.Name + "; its container and named volumes are retained and no deploy is queued."
	} else {
		preview.Summary = "Attach managed resource " + input.Resource + " to " + env.App + "/" + env.Name + " as desired state; deployment stays blocked until the resource reports ready."
	}
	return nil
}

func previewNodeCheckKind(input *platform.NodeCheckInput, preview *platform.Preview) {
	preview.Steps = []string{"check docker", "check pack"}
	preview.Summary = "Observe runtime availability on node " + input.Node + "."
}

func previewNodeRegisterKind(input *platform.NodeRegisterInput, preview *platform.Preview) {
	preview.Summary = "Record placement facts for node " + input.Name + "; this does not install or verify host state."
}

func previewNodeEnrollKind(input *platform.NodeEnrollInput, preview *platform.Preview) {
	preview.Steps = []string{
		"pin SSH host key",
		"verify node identity",
		"probe host capabilities",
		"configure role directories",
		"record enrollment",
	}
	preview.Summary = "Enroll node " + input.Node + " over pinned SSH; it receives work only after it proves its identity and the probe satisfies its roles."
	if input.Retry {
		preview.Summary = "Re-run the recorded enrollment of node " + input.Node + "; completed steps are not repeated."
	}
	if input.PrivateNetwork != nil {
		preview.Warnings = append(
			preview.Warnings,
			"Private network keys are generated on the node and reconciled by platform.node.reconcile; enrollment itself does not change WireGuard.",
		)
	}
}

func previewNodeReconcileKind(input *platform.NodeReconcileInput, preview *platform.Preview) {
	preview.Steps = []string{
		"converge role directories",
		"generate node WireGuard key",
		"render peer configuration",
		"apply peers",
	}
	preview.Summary = "Converge host configuration and WireGuard peers on node " + input.Node + "; rerunning changes nothing when the node already matches."
}

// environmentSpecDomains lists every hostname a first-environment declaration
// would bind, including the legacy single-domain field.
func environmentSpecDomains(spec *platform.EnvironmentSpec) []string {
	names := make([]string, 0, len(spec.Domains)+1)
	if spec.Domain != "" {
		names = append(names, spec.Domain)
	}
	for _, domain := range spec.Domains {
		names = append(names, domain.Domain)
	}
	return names
}

// requireHumanFactorTx verifies the D7 TOTP code inside the submission
// transaction. A process with no verifier fails closed, so HTTP and direct
// service callers cannot perform a factor-gated action by omitting the code.
func (s *Service) requireHumanFactorTx(code string) error {
	if s.humanFactor == nil {
		return contract.NewFault(
			contract.PreconditionFailed,
			"this server cannot verify the human factor",
			"Run the deletion or unprotection through the node RPC transport, where the TOTP code is verified inside the request.",
		)
	}
	return s.humanFactor(code)
}

// lastEnvironmentTx reports whether envID is the application's only remaining
// environment. The application row is locked by the caller, so a concurrent
// environment create or delete cannot change the answer mid-submission.
func lastEnvironmentTx(ctx context.Context, tx *entities.StoreTx, appID platform.AppID, envID platform.EnvironmentID) (bool, error) {
	count, err := tx.Platform().Environments().CountForApp(ctx, appID)
	if err != nil {
		return false, unavailable(err)
	}
	if count != 1 {
		return false, nil
	}
	found, err := tx.Platform().Environments().ExistsForApp(ctx, appID, envID)
	if err != nil {
		return false, unavailable(err)
	}
	return found, nil
}

// insertDomainTx writes or promotes one domain binding. It is shared by
// platform.domain.add and by app/environment creation, which carry the first
// binding so it commits before any first deploy is queued.
func (s *Service) insertDomainTx(
	ctx context.Context,
	tx *entities.StoreTx,
	env platform.Environment,
	input platform.DomainInput,
) (platform.DomainID, error) {
	return s.insertDomainWithIDTx(ctx, tx, env, input, "")
}

// insertDomainWithIDTx accepts only an internally allocated binding identity.
// Callers must establish its authority from the durable definition journal.
func (s *Service) insertDomainWithIDTx(
	ctx context.Context,
	tx *entities.StoreTx,
	env platform.Environment,
	input platform.DomainInput,
	allocatedID platform.DomainID,
) (platform.DomainID, error) {
	var zero platform.DomainID
	addresses := input.Addresses
	if input.DNSMode == integrations.DomainDNSModeCloudflare && len(input.DNSOrigins) > 0 {
		addresses = input.DNSOrigins
	}
	if addresses == nil {
		addresses = []string{}
	}
	node, err := nodeByNameTx(ctx, tx, env.Node)
	if err != nil {
		return zero, err
	}
	if node.Transport != "local" {
		return zero, contract.NewFault(
			contract.PreconditionFailed,
			"domains route only to environments on the main node for now",
			"Create the environment on the main node.",
		)
	}
	existingID, existingEnvironment, found, err := tx.Platform().Domains().GetByName(ctx, input.Domain)
	if err != nil {
		return zero, unavailable(err)
	}
	switch {
	case found && existingEnvironment != env.ID:
		return zero, contract.NewFault(
			contract.Conflict,
			input.Domain+" is already routed to another environment",
			"Remove it from that environment first, or use another hostname.",
		)
	case found:
		return s.promoteDomainBindingTx(ctx, tx, env, input, addresses, existingID, allocatedID)
	default:
		return s.insertNewDomainBindingTx(ctx, tx, env, input, addresses, allocatedID)
	}
}

func (s *Service) promoteDomainBindingTx(
	ctx context.Context,
	tx *entities.StoreTx,
	env platform.Environment,
	input platform.DomainInput,
	addresses []string,
	existingID platform.DomainID,
	allocatedID platform.DomainID,
) (platform.DomainID, error) {
	var zero platform.DomainID
	if allocatedID != "" && existingID != allocatedID {
		return zero, contract.NewFault(
			contract.Conflict,
			"domain binding identity changed since approval",
			"Obtain a new definition plan before changing this binding.",
		)
	}
	// Re-adding the same binding is idempotent; --primary promotes it and
	// refreshes the recorded main-node addresses for DNS observation.
	if _, err := tx.Platform().Domains().SetCheckPath(ctx, input.Domain, input.CheckPath); err != nil {
		return zero, unavailable(err)
	}
	if len(addresses) > 0 {
		if _, err := tx.Platform().Domains().SetExpectedAddressesByName(ctx, input.Domain, addresses); err != nil {
			return zero, unavailable(err)
		}
	}
	if input.Primary {
		if _, err := tx.Platform().Domains().ClearPrimaryExcept(ctx, env.ID, input.Domain); err != nil {
			return zero, unavailable(err)
		}
		if _, err := tx.Platform().Domains().PromotePrimary(ctx, input.Domain); err != nil {
			return zero, unavailable(err)
		}
	}
	return existingID, nil
}

func (s *Service) insertNewDomainBindingTx(
	ctx context.Context,
	tx *entities.StoreTx,
	env platform.Environment,
	input platform.DomainInput,
	addresses []string,
	allocatedID platform.DomainID,
) (platform.DomainID, error) {
	var zero platform.DomainID
	id := allocatedID
	if id == "" {
		var idErr error
		id, idErr = platform.NewDomainID()
		if idErr != nil {
			return zero, unavailable()
		}
	}
	primary := input.Primary
	if !primary {
		hasPrimary, err := tx.Platform().Domains().HasPrimary(ctx, env.ID)
		if err != nil {
			return zero, unavailable(err)
		}
		primary = !hasPrimary
	}
	if primary {
		if _, err := tx.Platform().Domains().ClearPrimary(ctx, env.ID); err != nil {
			return zero, unavailable(err)
		}
	}
	if _, err := tx.Platform().Domains().Insert(ctx, entities.DomainInsert{
		ID: id, Domain: input.Domain, EnvironmentID: env.ID,
		Primary: primary, Addresses: addresses, CheckPath: input.CheckPath,
	}); err != nil {
		return zero, platformWriteError(err, input.Domain+" is already routed")
	}
	return id, nil
}

func envAppID(ctx context.Context, tx *entities.StoreTx, app string) platform.AppID {
	id, _, _ := tx.Platform().Apps().IDByName(ctx, app)
	return id
}

// checkDeployabilityTx refuses a deployment before any build or container work
// when the pure deployability report lists blockers. The gate blocks on
// artifact identity, runtime, a web process, a primary domain and a managed DNS
// binding that is not applied. A merely unobserved record does not block the
// first deploy, nor does a Cloudflare-proxied one; a wrong answer does. A Git
// deploy creates its artifact and an explicit artifact/release supplies one, so
// artifact readiness is not a blocker here. A Git-less revision is valid when an
// explicit artifact selector is present; a no-selector submission is refused
// with artifact guidance when the release is created.
func (s *Service) checkDeployabilityTx(
	ctx context.Context,
	tx *entities.StoreTx,
	env platform.Environment,
	explicitRelease bool,
) error {
	dns := ""
	if env.PrimaryDomain != "" {
		state, found, err := tx.Platform().Domains().PrimaryDNSState(ctx, env.ID)
		if err != nil {
			return unavailable(err)
		}
		if found {
			dns = state
		}
	}
	report := platform.EvaluateDeployability(env.Revision, platform.DeployabilityState{
		ArtifactReady:   true,
		ExplicitRelease: explicitRelease,
		BuilderRequired: !explicitRelease && env.Revision != nil && env.Revision.Source.URL != "",
		PrimaryDomain:   env.PrimaryDomain,
		DNS:             dns,
	})
	if env.Revision != nil && len(env.Revision.Resources) > 0 {
		missing, err := s.resourceDeployabilityTx(ctx, tx, env)
		if err != nil {
			return err
		}
		report.Missing = append(report.Missing, missing...)
		report.Deployable = len(report.Missing) == 0
	}
	if report.Deployable {
		return nil
	}
	fault := contract.NewFault(contract.PreconditionFailed,
		"environment is not deployable: "+strings.Join(report.Missing, ", "),
		"Fix the listed desired state before deploying; eregion env status shows the same report.")
	if strings.Contains(","+strings.Join(report.Missing, ",")+",", ",builder,") {
		fault.Hint = "Set source.builder or pass --builder with a builder image reference; there is no built-in default builder. Explicit --release/--image-tag/--image-digest/--image deploys do not need a builder."
	}
	fault.Details = map[string]any{"missing": report.Missing}
	return fault
}

func (s *Service) prepareDeploymentTx(
	ctx context.Context,
	tx *entities.StoreTx,
	opID contract.OperationID,
	env platform.Environment,
	releaseID platform.ReleaseID,
	action platform.DeploymentAction,
	revisionID platform.RevisionID,
	previousDeploymentID platform.DeploymentID,
	secretRevisions []byte,
) error {
	if err := validateDeploymentWrite(action, secretRevisions); err != nil {
		return err
	}
	app, err := lockPointerEnvironmentTx(ctx, tx, env.ID)
	if err != nil {
		return err
	}
	if err := lockReleasePointerTx(ctx, tx, app, env.ID, releaseID); err != nil {
		return err
	}
	if err := checkReleaseCommandForDeploymentTx(
		ctx,
		tx,
		releaseID,
		revisionID,
		action,
	); err != nil {
		return err
	}
	depID, err := platform.NewDeploymentID()
	if err != nil {
		return unavailable()
	}
	if len(secretRevisions) == 0 {
		secretRevisions = []byte(`{}`)
	}
	if _, err := tx.Platform().Deployments().Insert(ctx, entities.DeploymentInsert{
		ID: depID, OperationID: opID, AppID: envAppID(ctx, tx, env.App), EnvironmentID: env.ID,
		Action: action, ReleaseID: releaseID,
		PreviousReleaseID: env.ServingRelease, PreviousDeploymentID: previousDeploymentID,
		RevisionID: revisionID, SecretRevisions: secretRevisions,
	}); err != nil {
		return platformWriteError(err, "deployment could not be recorded")
	}
	if _, err := tx.Platform().Environments().SetDesiredRelease(ctx, env.ID, releaseID); err != nil {
		return unavailable(err)
	}
	return nil
}

func validateDeploymentWrite(action platform.DeploymentAction, secretRevisions []byte) error {
	switch action {
	case platform.ActionDeploy, platform.ActionRollback, platform.ActionRestart:
	default:
		return contract.NewFault(
			contract.ValidationFailed,
			"invalid deployment action",
			"Use deploy, rollback or restart.",
		)
	}
	if len(secretRevisions) > 0 {
		var revisions map[string]json.RawMessage
		if err := json.Unmarshal(secretRevisions, &revisions); err != nil || revisions == nil {
			return contract.NewFault(
				contract.ValidationFailed,
				"invalid deployment secret revisions",
				"Supply a JSON object of revision identities.",
			)
		}
	}
	return nil
}

func nullableDeploymentID(id platform.DeploymentID) any {
	if id == "" {
		return nil
	}
	return string(id)
}

// resolvedGitRef is the exact commit a submission resolved before its
// transaction opened. The transaction checks the URL and ref against the
// revision it reads, so a concurrent revision change cannot record a commit
// that belongs to another source. An empty SHA with a non-empty URL means the
// revision has a Git recipe but the caller did not request a build (for
// example an explicit artifact selector).
type resolvedGitRef struct {
	URL string
	Ref string
	SHA string
}

// resolveSubmissionGitRef resolves a git ref before the submission transaction
// opens for every path that creates a release from an environment revision:
// deploy, CI and the webhook (which already sends a commit), and the first
// deploy requested by app or env create. A failure here creates no operation.
// Explicit artifact selectors and existing releases do not read Git.
func (s *Service) resolveSubmissionGitRef(
	ctx context.Context,
	normalized platform.Normalized,
) (*resolvedGitRef, error) {
	switch input := normalized.Input.(type) {
	case *platform.DeployInput:
		return s.resolveDeployGitRef(ctx, input)
	case *platform.AppCreateInput:
		return s.resolveAppCreateGitRef(ctx, input)
	case *platform.EnvironmentCreateInput:
		return s.resolveEnvironmentCreateGitRef(ctx, input)
	}
	return nil, nil
}

func (s *Service) resolveDeployGitRef(ctx context.Context, input *platform.DeployInput) (*resolvedGitRef, error) {
	if input.ReleaseID != "" || input.ImageTag != "" || input.ImageDigest != "" ||
		input.Image != "" {
		return nil, nil
	}
	env, err := s.environment(ctx, input.App, input.Environment)
	if err != nil {
		return nil, err
	}
	if env.Revision == nil {
		return nil, nil
	}
	if env.Revision.Source.URL == "" {
		if input.Ref != "" {
			return nil, contract.NewFault(
				contract.ValidationFailed,
				"--ref requires a Git build recipe; deploy an explicit artifact with --image-tag, --image-digest, or --image instead",
				"Bind a Git source with env update --git-url, or deploy an explicit artifact with --image-tag, --image-digest, or --image.",
			)
		}
		return &resolvedGitRef{}, nil
	}
	ref := input.Ref
	if ref == "" {
		ref = env.Revision.Source.Ref
	}
	return s.resolveGitSourceRef(ctx, env.App, env.Name, env.Revision.Source, ref)
}

func (s *Service) resolveAppCreateGitRef(ctx context.Context, input *platform.AppCreateInput) (*resolvedGitRef, error) {
	if !input.Deploy || len(input.Environments) == 0 {
		return nil, nil
	}
	first := &input.Environments[0]
	if first.Source.URL == "" {
		return &resolvedGitRef{}, nil
	}
	return s.resolveGitSourceRef(ctx, input.Name, first.Name, first.Source, first.Source.Ref)
}

func (s *Service) resolveEnvironmentCreateGitRef(ctx context.Context, input *platform.EnvironmentCreateInput) (*resolvedGitRef, error) {
	if !input.Deploy {
		return nil, nil
	}
	if input.Source.URL == "" {
		return &resolvedGitRef{}, nil
	}
	return s.resolveGitSourceRef(ctx, input.App, input.Name, input.Source, input.Source.Ref)
}

func (s *Service) resolveGitSourceRef(
	ctx context.Context,
	app, environment string,
	source platform.SourceBinding,
	ref string,
) (*resolvedGitRef, error) {
	// A submission that already carries a commit (the CI and webhook paths) needs
	// no repository read and no credential. It is already exact.
	if platform.IsGitCommit(ref) {
		return &resolvedGitRef{URL: source.URL, Ref: ref, SHA: ref}, nil
	}
	var credential []byte
	if source.CredentialSecretName != "" {
		value, _, err := s.resolveCredentialSecret(
			ctx,
			platform.Environment{App: app, Name: environment},
			source.CredentialSecretName,
		)
		if err != nil {
			return nil, err
		}
		credential = []byte(value)
	}
	resolve := s.resolveGitRef
	if resolve == nil {
		resolve = platform.ResolveGitRef
	}
	sha, err := resolve(ctx, source.URL, ref, credential)
	if err != nil {
		return nil, err
	}
	return &resolvedGitRef{URL: source.URL, Ref: ref, SHA: sha}, nil
}

// sourceChangedDuringSubmission is the single conflict used when the revision
// read by the transaction differs from the one the ref was resolved against.
func sourceChangedDuringSubmission() error {
	return contract.NewFault(
		contract.Conflict,
		"environment source changed during submission; retry",
		"Retry the deploy with the environment's current revision.",
	)
}

// checkResolvedGitSource compares the submission-time resolution against the
// revision the transaction reads. A source-kind change can never fall through
// to another branch: a git source that became an image has no commit to record.
// An explicit --ref owns the effective ref, so only a URL change conflicts then.
func checkResolvedGitSource(
	resolved *resolvedGitRef,
	source platform.SourceBinding,
	refOverride string,
) error {
	if resolved == nil {
		return sourceChangedDuringSubmission()
	}
	if source.URL == "" {
		if resolved.URL != "" || resolved.Ref != "" || resolved.SHA != "" {
			return sourceChangedDuringSubmission()
		}
		return nil
	}
	if resolved.URL != source.URL {
		return sourceChangedDuringSubmission()
	}
	if refOverride == "" && resolved.Ref != source.Ref {
		return sourceChangedDuringSubmission()
	}
	if refOverride != "" && resolved.Ref != refOverride {
		return sourceChangedDuringSubmission()
	}
	return nil
}

// createReleaseFromRevisionTx records the pending release and build for a
// deploy from the environment's current Git revision. The ref was resolved to
// an exact commit before the transaction; the commit is recorded on both rows
// before any build starts, and the worker only builds that commit. The build
// publishes into the app's immutable private repository; an environment with no
// Git recipe must select an explicit artifact instead.
func (s *Service) createReleaseFromRevisionTx(
	ctx context.Context,
	tx *entities.StoreTx,
	opID contract.OperationID,
	env platform.Environment,
	refOverride string,
	resolved *resolvedGitRef,
) (platform.ReleaseID, error) {
	if _, err := lockPointerEnvironmentTx(ctx, tx, env.ID); err != nil {
		return "", err
	}
	if env.Revision == nil {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"environment has no current revision",
			"Save an environment revision before deploying.",
		)
	}
	state := env.Revision.RevisionState
	source := state.Source
	// Revalidate the submission-time source before the ordinary current-source
	// gate. A source cleared while an exact commit was being resolved is a
	// conflict, not a fresh Git-less deploy with no artifact selector.
	if err := checkResolvedGitSource(resolved, source, refOverride); err != nil {
		return "", err
	}
	if source.URL == "" {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"environment has no Git build source and no artifact was selected",
			"Deploy an explicit artifact with `eregion deploy "+env.App+" "+env.Name+" --image-tag TAG`, --image-digest, or --image. Use `eregion restart "+env.App+" "+env.Name+"` to reapply the serving artifact.",
		)
	}
	if strings.TrimSpace(source.Builder) == "" {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"Git build source has no explicit Buildpacks builder",
			"Set source.builder (or pass --builder with the builder image reference) before deploying; there is no built-in default builder. An explicit --release/--image-tag/--image-digest/--image deploy does not need a builder.",
		)
	}
	app, err := appByNameTx(ctx, tx, env.App)
	if err != nil {
		return "", err
	}
	if app.ImageRepository == "" {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"application has no image repository",
			"This app predates app-level image repositories; recreate it before building an artifact.",
		)
	}
	releaseID, err := platform.NewReleaseID()
	if err != nil {
		return "", unavailable()
	}
	if env.BuildNodeName() != env.Node {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"build_node is distinct from the application node; moving the built image requires a shared registry",
			"Use build_node equal to the environment node.",
		)
	}
	ref := refOverride
	if ref == "" {
		ref = source.Ref
	}
	if err := platform.ValidateGitSourceURL(source.URL); err != nil {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"the environment's git source is invalid",
			err.Error(),
		)
	}
	if resolved == nil || !platform.IsGitCommit(resolved.SHA) {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"the git ref did not resolve to an exact commit",
			"Retry the deploy so the ref resolves to a full commit SHA.",
		)
	}
	commit := resolved.SHA
	buildSource := platform.Source{
		Type: "git", URL: source.URL, Ref: ref, Path: source.ContextPath,
		CredentialSecretName: source.CredentialSecretName,
	}
	sourceJSON, _ := json.Marshal(buildSource)
	build := platform.Build{
		Builder:    source.Builder,
		Env:        source.BuildEnv,
		GoTargets:  source.GoTargets,
		Buildpacks: source.Buildpacks,
	}
	buildJSON, _ := json.Marshal(build)
	image := app.ImageRepository + ":" + strings.TrimPrefix(string(opID), "op_")[:12]
	if err := platform.ValidateImage(image); err != nil {
		return "", err
	}
	if _, err := tx.Platform().Releases().Insert(ctx, entities.ReleaseInsert{
		ID: releaseID, OperationID: opID, AppID: envAppID(ctx, tx, env.App), EnvironmentID: env.ID,
		Image: image, Source: sourceJSON, Build: buildJSON,
		Status: platform.ReleasePending, CommitSHA: commit,
	}); err != nil {
		return "", platformWriteError(err, "release could not be recorded")
	}
	buildID, err := insertBuildTx(
		ctx,
		tx,
		opID,
		env,
		releaseID,
		env.RevisionID,
		buildSource,
		commit,
	)
	if err != nil {
		return "", err
	}
	if err := setReleaseBuildPointerTx(ctx, tx, env.ID, releaseID, buildID); err != nil {
		return "", err
	}
	return releaseID, nil
}

func (s *Service) createRetryReleaseTx(
	ctx context.Context,
	tx *entities.StoreTx,
	opID contract.OperationID,
	env platform.Environment,
	old platform.BuildRecord,
) error {
	if _, err := lockPointerEnvironmentTx(ctx, tx, env.ID); err != nil {
		return err
	}
	oldRelease, err := releaseByIDTx(ctx, tx, old.ReleaseID)
	if err != nil {
		return err
	}
	app, err := appByNameTx(ctx, tx, env.App)
	if err != nil {
		return err
	}
	if app.ImageRepository == "" {
		return contract.NewFault(
			contract.PreconditionFailed,
			"application has no image repository",
			"This app predates app-level image repositories; recreate it before retrying a build.",
		)
	}
	image := app.ImageRepository + ":" + strings.TrimPrefix(string(opID), "op_")[:12]
	if err := platform.ValidateImage(image); err != nil {
		return err
	}
	source, commit, err := retryBuildSource(old.Source, old.CommitSHA)
	if err != nil {
		return err
	}
	sourceJSON, _ := json.Marshal(source)
	var buildJSON []byte
	if oldRelease.Build != nil {
		buildJSON, _ = json.Marshal(oldRelease.Build)
	}
	releaseID, err := platform.NewReleaseID()
	if err != nil {
		return unavailable()
	}
	if _, err := tx.Platform().Releases().Insert(ctx, entities.ReleaseInsert{
		ID: releaseID, OperationID: opID, AppID: envAppID(ctx, tx, env.App), EnvironmentID: env.ID,
		Image: image, Source: sourceJSON, Build: buildJSON,
		Status: platform.ReleasePending, CommitSHA: commit,
	}); err != nil {
		return platformWriteError(err, "retry release could not be recorded")
	}
	revisionID := old.RevisionID
	if revisionID == "" {
		revisionID = env.RevisionID
	}
	buildID, err := insertBuildTx(ctx, tx, opID, env, releaseID, revisionID, source, commit)
	if err != nil {
		return err
	}
	if err := setReleaseBuildPointerTx(ctx, tx, env.ID, releaseID, buildID); err != nil {
		return err
	}
	return nil
}

// retryBuildSource selects the exact source a retry rebuilds. A git retry uses
// only the original build's commit: it never falls back to a branch or tag name
// that may have moved since that build was accepted.
func retryBuildSource(source platform.Source, commit string) (platform.Source, string, error) {
	if source.Type != "git" {
		return source, commit, nil
	}
	if !platform.IsGitCommit(commit) {
		return source, "", contract.NewFault(
			contract.PreconditionFailed,
			"build has no exact commit to retry",
			"Start a new deploy so the ref resolves to a commit first.",
		)
	}
	source.Ref = commit
	return source, commit, nil
}

func insertBuildTx(
	ctx context.Context,
	tx *entities.StoreTx,
	opID contract.OperationID,
	env platform.Environment,
	releaseID platform.ReleaseID,
	revisionID platform.RevisionID,
	source platform.Source,
	commit string,
) (platform.BuildID, error) {
	if commit != "" && !platform.IsGitCommit(commit) {
		return "", contract.NewFault(
			contract.ValidationFailed,
			"invalid build commit",
			"Use an exact Git commit identity.",
		)
	}
	buildID, err := platform.NewBuildID()
	if err != nil {
		return "", unavailable()
	}
	sourceJSON, _ := json.Marshal(source)
	if _, err := tx.Platform().Builds().Insert(ctx, entities.BuildInsert{
		ID: buildID, OperationID: opID, AppID: envAppID(ctx, tx, env.App), EnvironmentID: env.ID,
		RevisionID: revisionID, ReleaseID: releaseID, Source: sourceJSON, CommitSHA: commit,
	}); err != nil {
		return "", platformWriteError(err, "build could not be recorded")
	}
	return buildID, nil
}

// rollbackSnapshotTx finds the revision and exact secret revisions the target
// release used when it last served (MASTER D18).
func rollbackSnapshotTx(
	ctx context.Context,
	tx *entities.StoreTx,
	env platform.Environment,
	releaseID platform.ReleaseID,
) (platform.RevisionID, []byte, error) {
	revisionID, secretRevisions, found, err := tx.Platform().Deployments().RollbackSnapshot(ctx, env.ID, releaseID)
	if err != nil {
		return "", nil, unavailable(err)
	}
	if !found {
		return env.RevisionID, []byte(`{}`), nil
	}
	if revisionID == "" {
		revisionID = env.RevisionID
	}
	if len(secretRevisions) == 0 {
		secretRevisions = []byte(`{}`)
	}
	return revisionID, secretRevisions, nil
}

func (s *Service) queueFirstDeployTx(
	ctx context.Context,
	tx *entities.StoreTx,
	envID platform.EnvironmentID,
	actor contract.Actor,
	resolved *resolvedGitRef,
) error {
	env, err := environmentByIDTx(ctx, tx, envID)
	if err != nil {
		return unavailable()
	}
	if env.Revision == nil || !env.Revision.DeployIntent {
		return nil
	}
	input := platform.DeployInput{App: env.App, Environment: env.Name}
	return s.submitFollowupOperationTx(
		ctx,
		tx,
		platform.KindDeploy,
		"env/"+env.App+"/"+env.Name,
		input,
		actor,
		"first-deploy:"+string(env.RevisionID),
		resolved,
	)
}

// submitFollowupOperationTx records one additional operation and River job in
// the caller's transaction. It is used for the first deploy requested by app
// create, so the intent is durable with the revision that requested it.
func (s *Service) submitFollowupOperationTx(
	ctx context.Context,
	tx *entities.StoreTx,
	kind, target string,
	input any,
	actor contract.Actor,
	key string,
	resolved *resolvedGitRef,
) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	normalized, err := platform.Default().Normalize(kind, target, raw)
	if err != nil {
		return err
	}
	id, err := contract.NewOperationID()
	if err != nil {
		return err
	}
	_, err = s.recordPlatformSubmissionTx(
		ctx,
		tx,
		id,
		normalized,
		actor,
		key,
		resolved,
		nil,
		"",
		nil,
	)
	return err
}

func nullableReleaseID(id platform.ReleaseID) any {
	if id == "" {
		return nil
	}
	return string(id)
}

func sha256Hex(raw []byte) string {
	// Kept in one place so platform and diagnostic submissions bind inputs the
	// same way.
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func platformWriteError(err error, message string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return contract.NewFault(
			contract.Conflict,
			message,
			"Choose a different name or recover the existing accepted record.",
		)
	}
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return contract.NewFault(
			contract.PreconditionFailed,
			"referenced platform record is missing",
			"Create the app, node, or environment first.",
		)
	}
	return unavailable(err)
}

func allocateHostPort(ctx context.Context, environments *entities.EnvironmentStore, explicitHost int) (int, error) {
	if explicitHost != 0 {
		used, err := environments.PortInUse(ctx, explicitHost)
		if err != nil {
			return 0, unavailable(err)
		}
		if used {
			return 0, contract.NewFault(
				contract.Conflict,
				"host port is already assigned",
				"Omit host_port to allocate one automatically.",
			)
		}
		return explicitHost, nil
	}
	next, found, err := environments.NextHostPort(ctx)
	if err != nil {
		return 0, unavailable(err)
	}
	if !found {
		return 0, contract.NewFault(
			contract.Unavailable,
			"no stable host port is available",
			"Remove an unused environment or choose a host_port explicitly.",
		)
	}
	return next, nil
}

func allocatePorts(ctx context.Context, tx *entities.StoreTx, explicitHost int) (int, int, error) {
	environments := tx.Platform().Environments()
	if err := environments.LockPortAllocation(ctx, portAllocationLock); err != nil {
		return 0, 0, unavailable(err)
	}
	host, err := allocateHostPort(ctx, environments, explicitHost)
	if err != nil {
		return 0, 0, err
	}
	candidate, found, err := environments.NextCandidatePort(ctx, host)
	if err != nil {
		return 0, 0, unavailable(err)
	}
	if !found {
		return 0, 0, contract.NewFault(
			contract.Unavailable,
			"no candidate host port is available",
			"Remove an unused environment.",
		)
	}
	return host, candidate, nil
}

func (s *Service) transportIdentity() (string, bool) {
	provider, ok := s.transport.(interface{ NodeIdentity() string })
	if !ok {
		return "", false
	}
	identity := strings.TrimSpace(provider.NodeIdentity())
	return identity, identity != ""
}

func (s *Service) checkPlacementByTx(
	ctx context.Context,
	tx *entities.StoreTx,
	nodeName string,
) error {
	node, err := nodeByNameTx(ctx, tx, nodeName)
	if err != nil {
		return err
	}
	return s.checkPlacement(node, platform.RoleApplication, platform.CapabilityDocker)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ensureLocalNode records the configured local node identity on every worker
// start without a manual registration command. It refreshes the roles,
// capabilities, status, and last-seen time of exactly that node row; it never
// touches another node and only records capabilities actually found on the
// host.
func (s *Service) ensureLocalNode(ctx context.Context) error {
	identity := strings.TrimSpace(s.localNode)
	if identity == "" {
		return nil
	}
	caps := []string{}
	if _, err := exec.LookPath("docker"); err == nil {
		caps = append(caps, platform.CapabilityDocker)
	}
	if _, err := exec.LookPath("pack"); err == nil {
		caps = append(caps, platform.CapabilityBuildpacks)
	}
	if len(caps) == 0 {
		caps = []string{"unverified"}
	}
	roles := []string{platform.RoleApplication, platform.RoleControlPlane}
	if contains(caps, platform.CapabilityBuildpacks) {
		roles = append(roles, platform.RoleBuilder)
	}
	if _, err := s.entities.Nodes().EnsureLocal(ctx, identity, roles, caps); err != nil {
		return unavailable(err)
	}
	return nil
}

// ---------------------------------------------------------------- inventory

// Inventory is the shared read model for GET /v1/platform.
func (s *Service) Inventory(
	ctx context.Context,
	actor contract.Actor,
	query platform.InventoryQuery,
) (platform.Inventory, error) {
	if err := actor.Validate(); err != nil {
		return platform.Inventory{}, err
	}
	if query.Limit == 0 {
		query.Limit = 50
	}
	if query.Limit < 1 || query.Limit > 200 {
		return platform.Inventory{}, contract.NewFault(
			contract.ValidationFailed,
			"inventory limit out of range",
			"Use a limit from 1 to 200.",
		)
	}
	for _, value := range []string{query.App, query.Environment, query.Node, query.State} {
		if value != "" && len(value) > 128 {
			return platform.Inventory{}, contract.NewFault(
				contract.ValidationFailed,
				"inventory filter is too long",
				"Use a documented app, environment, node, or state filter.",
			)
		}
	}
	inventory := platform.Inventory{Collection: query.Collection, Limit: query.Limit}
	switch query.Collection {
	case "summary":
		items, err := s.inventorySummary(ctx)
		return inventoryWithItems(inventory, items, err)
	case "nodes":
		items, err := s.inventoryNodes(ctx, query)
		return inventoryWithItems(inventory, items, err)
	case "apps":
		items, err := s.inventoryApps(ctx, query)
		return inventoryWithItems(inventory, items, err)
	case "environments":
		items, err := s.inventoryEnvironments(ctx, query)
		return inventoryWithItems(inventory, items, err)
	case "revisions":
		items, err := s.inventoryRevisions(ctx, query)
		return inventoryWithItems(inventory, items, err)
	case "deployability":
		items, err := s.inventoryDeployability(ctx, query)
		return inventoryWithItems(inventory, items, err)
	case "releases":
		items, err := s.inventoryReleases(ctx, query)
		return inventoryWithItems(inventory, items, err)
	case "builds":
		items, err := s.inventoryBuilds(ctx, query)
		return inventoryWithItems(inventory, items, err)
	case "deployments":
		items, err := s.inventoryDeployments(ctx, query)
		return inventoryWithItems(inventory, items, err)
	case "domains":
		items, err := s.inventoryDomains(ctx, query)
		return inventoryWithItems(inventory, items, err)
	case "changes":
		items, err := s.inventoryChanges(ctx, query)
		return inventoryWithItems(inventory, items, err)
	case "resources":
		items, err := s.inventoryResources(ctx, query)
		return inventoryWithItems(inventory, items, err)
	default:
		if provider, ok := platform.Default().InventoryProvider(query.Collection); ok {
			items, err := provider.Read(ctx, query)
			if err != nil {
				return inventory, err
			}
			if items == nil {
				items = []any{}
			}
			inventory.Items = items
			return inventory, nil
		}
		return inventory, contract.NewFault(
			contract.ValidationFailed,
			"unknown inventory collection",
			"Use summary, nodes, apps, environments, revisions, deployability, domains, releases, builds, deployments, changes, or resources.",
		)
	}
}

func inventoryWithItems[T any](inventory platform.Inventory, items T, err error) (platform.Inventory, error) {
	if err != nil {
		return inventory, err
	}
	inventory.Items = items
	return inventory, nil
}

func (s *Service) inventorySummary(ctx context.Context) (map[string]int64, error) {
	out, err := s.entities.Platform().Inventory().Summary(ctx)
	if err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

func (s *Service) inventoryNodes(
	ctx context.Context,
	query platform.InventoryQuery,
) ([]platform.Node, error) {
	nodes, err := s.entities.Nodes().List(ctx, entities.NodeFilter{
		Name: query.Node, State: query.State, Limit: query.Limit,
	})
	if err != nil {
		return nil, unavailable(err)
	}
	if nodes == nil {
		nodes = []platform.Node{}
	}
	return nodes, nil
}

func (s *Service) inventoryApps(
	ctx context.Context,
	query platform.InventoryQuery,
) ([]platform.App, error) {
	apps, err := s.entities.Platform().Apps().List(ctx, entities.AppFilter{
		Name: query.App, Limit: query.Limit,
	})
	if err != nil {
		return nil, unavailable(err)
	}
	if apps == nil {
		apps = []platform.App{}
	}
	return apps, nil
}

func (s *Service) inventoryEnvironments(
	ctx context.Context,
	query platform.InventoryQuery,
) ([]platform.Environment, error) {
	items := []platform.Environment{}
	observations, err := s.entities.Platform().Environments().List(ctx, entities.EnvironmentFilter{
		App: query.App, Environment: query.Environment, Node: query.Node,
		State: query.State, Limit: query.Limit,
	})
	if err != nil {
		return nil, unavailable(err)
	}
	for _, observation := range observations {
		env := observation.Environment
		env.Deployability = platform.EvaluateDeployability(env.Revision, platform.DeployabilityState{
			ArtifactReady: env.ArtifactReady, PrimaryDomain: env.PrimaryDomain,
			DNS: string(observation.PrimaryDNS),
		})
		missing, err := s.resourceDeployability(ctx, env)
		if err != nil {
			return nil, err
		}
		env.Deployability.Missing = append(env.Deployability.Missing, missing...)
		env.Deployability.Deployable = len(env.Deployability.Missing) == 0
		items = append(items, env)
	}
	return items, nil
}

func (s *Service) inventoryDomains(
	ctx context.Context,
	query platform.InventoryQuery,
) ([]platform.DomainBinding, error) {
	items, err := s.entities.Platform().Domains().List(ctx, entities.DomainFilter{
		App: query.App, Environment: query.Environment, Limit: query.Limit,
	})
	if err != nil {
		return nil, unavailable(err)
	}
	if items == nil {
		items = []platform.DomainBinding{}
	}
	return items, nil
}

func (s *Service) inventoryReleases(
	ctx context.Context,
	query platform.InventoryQuery,
) ([]platform.Release, error) {
	items, err := s.entities.Platform().Releases().List(ctx, entities.ReleaseFilter{
		App: query.App, Environment: query.Environment, State: query.State, Limit: query.Limit,
	})
	if err != nil {
		return nil, unavailable(err)
	}
	if items == nil {
		items = []platform.Release{}
	}
	return items, nil
}

func (s *Service) inventoryBuilds(
	ctx context.Context,
	query platform.InventoryQuery,
) ([]platform.BuildRecord, error) {
	items, err := s.entities.Platform().Builds().List(ctx, entities.BuildFilter{
		App: query.App, Environment: query.Environment, State: query.State,
		BuildID: platform.BuildID(query.Build), Limit: query.Limit,
	})
	if err != nil {
		return nil, unavailable(err)
	}
	if items == nil {
		items = []platform.BuildRecord{}
	}
	return items, nil
}

func (s *Service) buildByID(
	ctx context.Context,
	id platform.BuildID,
) (platform.BuildRecord, error) {
	build, found, err := s.entities.Platform().Builds().GetByID(ctx, id)
	if err != nil {
		return build, unavailable(err)
	}
	if !found {
		return build, contract.NewFault(
			contract.NotFound,
			"build not found",
			"Use a build ID from inventory.",
		)
	}
	return build, nil
}

// buildByIDTx reads the build through the admission transaction, so an open
// transaction never borrows a second pool connection.
func buildByIDTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id platform.BuildID,
) (platform.BuildRecord, error) {
	build, found, err := tx.Platform().Builds().GetByID(ctx, id)
	if err != nil {
		return build, unavailable(err)
	}
	if !found {
		return build, contract.NewFault(
			contract.NotFound,
			"build not found",
			"Use a build ID from inventory.",
		)
	}
	return build, nil
}

func (s *Service) buildByOperation(
	ctx context.Context,
	id contract.OperationID,
) (platform.BuildRecord, bool, error) {
	build, found, err := s.entities.Platform().Builds().GetByOperation(ctx, id)
	if err != nil {
		return build, false, unavailable(err)
	}
	return build, found, nil
}

func (s *Service) inventoryDeployments(
	ctx context.Context,
	query platform.InventoryQuery,
) ([]platform.Deployment, error) {
	if query.Deployment != "" {
		if err := platform.DeploymentID(query.Deployment).Validate(); err != nil {
			return nil, err
		}
	}
	items, err := s.entities.Platform().Deployments().List(ctx, entities.DeploymentFilter{
		App: query.App, Environment: query.Environment, State: query.State,
		DeploymentID: platform.DeploymentID(query.Deployment), Limit: query.Limit,
	})
	if err != nil {
		return nil, unavailable(err)
	}
	if items == nil {
		items = []platform.Deployment{}
	}
	return items, nil
}

func (s *Service) inventoryChanges(
	ctx context.Context,
	query platform.InventoryQuery,
) ([]platform.Change, error) {
	items, err := s.entities.Platform().Changes().List(ctx, entities.ChangeFilter{
		App: query.App, Environment: query.Environment, Kind: query.State, Limit: query.Limit,
	})
	if err != nil {
		return nil, unavailable(err)
	}
	if items == nil {
		items = []platform.Change{}
	}
	return items, nil
}

// previousRollbackRelease chooses the release that served before the current
// serving release became active. AppliedRelease is not sufficient: it normally
// equals the current serving release. A latest successful deployment's
// previous_release_id is the authoritative one-step history.
func previousRollbackRelease(
	ctx context.Context,
	tx *entities.StoreTx,
	env platform.Environment,
) (platform.ReleaseID, error) {
	if env.ServingRelease != "" {
		previous, found, err := tx.Platform().Deployments().PreviousReleaseOfServing(ctx, env.ID, env.ServingRelease)
		if err != nil {
			return "", unavailable(err)
		}
		if found && previous != "" {
			return previous, nil
		}
	}
	previous, found, err := tx.Platform().Deployments().LatestSucceededReleaseExcluding(ctx, env.ID, env.ServingRelease)
	if err != nil {
		return "", unavailable(err)
	}
	if !found {
		return "", nil
	}
	return previous, nil
}

func (s *Service) previousRollbackRelease(
	ctx context.Context,
	env platform.Environment,
) (platform.ReleaseID, error) {
	if env.ServingRelease != "" {
		previous, found, err := s.entities.Platform().Deployments().PreviousReleaseOfServing(ctx, env.ID, env.ServingRelease)
		if err != nil {
			return "", unavailable(err)
		}
		if found && previous != "" {
			return previous, nil
		}
	}
	previous, found, err := s.entities.Platform().Deployments().LatestSucceededReleaseExcluding(ctx, env.ID, env.ServingRelease)
	if err != nil {
		return "", unavailable(err)
	}
	if !found {
		return "", nil
	}
	return previous, nil
}

// --------------------------------------------------------------- tx lookups

func appByNameTx(ctx context.Context, tx *entities.StoreTx, name string) (platform.App, error) {
	return appByNameQueryTx(ctx, tx, name, false)
}

// appByNameForUpdateTx serializes environment creation and deletion against
// the application row, so "the last environment" is decided against a stable
// set and an application deletion cannot race a sibling environment creation.
func appByNameForUpdateTx(ctx context.Context, tx *entities.StoreTx, name string) (platform.App, error) {
	return appByNameQueryTx(ctx, tx, name, true)
}

func appByNameQueryTx(
	ctx context.Context,
	tx *entities.StoreTx,
	name string,
	forUpdate bool,
) (platform.App, error) {
	var (
		app   platform.App
		found bool
		err   error
	)
	if forUpdate {
		app, found, err = tx.Platform().Apps().GetByNameForUpdate(ctx, name)
	} else {
		app, found, err = tx.Platform().Apps().GetByName(ctx, name)
	}
	if err != nil {
		return app, unavailable(err)
	}
	if !found {
		return app, contract.NewFault(
			contract.NotFound,
			"application not found",
			"Create the application first.",
		)
	}
	return app, nil
}

func nodeByNameTx(ctx context.Context, tx *entities.StoreTx, name string) (platform.Node, error) {
	return loadNode(ctx, tx.Nodes(), name)
}

func environmentByNameTx(
	ctx context.Context,
	tx *entities.StoreTx,
	app, name string,
) (platform.Environment, error) {
	identity, found, err := tx.Platform().Environments().ResolveID(ctx, app, name)
	if err != nil {
		return platform.Environment{}, unavailable(err)
	}
	if !found {
		return platform.Environment{}, contract.NewFault(
			contract.NotFound,
			"environment not found",
			"Create the environment under this app first.",
		)
	}
	if err := lockAppKeyShareTx(ctx, tx, identity.AppID); err != nil {
		return platform.Environment{}, err
	}
	locked, err := tx.Platform().Environments().LockByIDAppName(ctx, identity.ID, identity.AppID, name)
	if err != nil {
		return platform.Environment{}, unavailable(err)
	}
	if !locked {
		return platform.Environment{}, pointerIntegrityFault()
	}
	return loadLockedEnvironmentTx(ctx, tx, identity.ID)
}

// environmentByIDTx locks and reads the exact environment row an admission
// observed. The opaque ID, not the app/name pair, proves the environment was not
// deleted and recreated while the push waited in the queue.
func environmentByIDTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id platform.EnvironmentID,
) (platform.Environment, error) {
	appID, found, err := tx.Platform().Environments().AppIDByID(ctx, id)
	if err != nil {
		return platform.Environment{}, unavailable(err)
	}
	if !found {
		return platform.Environment{}, contract.NewFault(
			contract.NotFound,
			"environment not found",
			"The environment was deleted after the push was admitted.",
		)
	}
	if err := lockAppKeyShareTx(ctx, tx, appID); err != nil {
		return platform.Environment{}, err
	}
	locked, err := tx.Platform().Environments().LockByIDApp(ctx, id, appID)
	if err != nil {
		return platform.Environment{}, unavailable(err)
	}
	if !locked {
		return platform.Environment{}, contract.NewFault(
			contract.NotFound,
			"environment not found",
			"The environment was deleted after the push was admitted.",
		)
	}
	return loadLockedEnvironmentTx(ctx, tx, id)
}

// A separate statement gets a fresh READ COMMITTED snapshot after the row lock.
func loadLockedEnvironmentTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id platform.EnvironmentID,
) (platform.Environment, error) {
	observation, found, err := tx.Platform().Environments().GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, entities.ErrPlatformPointerIntegrity) {
			return platform.Environment{}, pointerIntegrityFault()
		}
		return platform.Environment{}, unavailable(err)
	}
	if !found {
		return platform.Environment{}, pointerIntegrityFault()
	}
	env := observation.Environment
	if env.Revision == nil || env.RevisionID == "" || env.Revision.ID != env.RevisionID {
		return env, contract.NewFault(
			contract.PreconditionFailed,
			"environment current revision is missing or inconsistent",
			"Inspect the environment revision before retrying.",
		)
	}
	return env, nil
}

func releaseByIDTx(
	ctx context.Context,
	tx *entities.StoreTx,
	id platform.ReleaseID,
) (platform.Release, error) {
	release, found, err := tx.Platform().Releases().GetByID(ctx, id)
	if err != nil {
		return release, unavailable(err)
	}
	if !found {
		return release, contract.NewFault(
			contract.NotFound,
			"release not found",
			"Use a release ID from inventory.",
		)
	}
	return release, nil
}

// ---------------------------------------------------------------- dry run

// Preview implements the optional dry-run planner used by the API. When the
// actor already submitted this kind and target with idempotencyKey, the
// preview reports the original operation and skips every conflict check: a
// retry must reach the same submission without repeating its side effects.
// previewDeployFromEnvironment evaluates a deploy preview against one
// repeatable-read snapshot. It never resolves a remote ref and never writes.
func (s *Service) previewDeployFromEnvironment(
	ctx context.Context,
	input *platform.DeployInput,
	normalized platform.Normalized,
	explicitArtifact bool,
	preview *platform.Preview,
) error {
	return s.entities.WithinReadTx(ctx, func(tx *entities.StoreTx) error {
		observation, found, err := tx.Platform().Environments().GetByName(ctx, normalized.Target.App, normalized.Target.Environment)
		if err != nil {
			return unavailable(err)
		}
		if !found {
			return contract.NewFault(
				contract.NotFound,
				"environment not found",
				"Create the environment under this app first.",
			)
		}
		env := observation.Environment
		if env.Revision == nil {
			return contract.NewFault(
				contract.PreconditionFailed,
				"environment has no current revision",
				"Save an environment revision before deploying.",
			)
		}
		if explicitArtifact {
			return s.previewExplicitArtifactDeploy(ctx, tx, env, input, preview)
		}
		return s.previewGitDeploy(ctx, tx, env, input, preview)
	})
}

func (s *Service) previewExplicitArtifactDeploy(
	ctx context.Context,
	tx *entities.StoreTx,
	env platform.Environment,
	input *platform.DeployInput,
	preview *platform.Preview,
) error {
	selector := input.ImageTag
	kind := "image-tag"
	if input.ImageDigest != "" {
		selector, kind = input.ImageDigest, "image-digest"
	} else if input.Image != "" {
		selector, kind = input.Image, "image"
	}
	preview.Warnings = append(
		preview.Warnings,
		fmt.Sprintf(
			"Artifact availability is UNVERIFIED: this read-only preview does not contact the registry. The app repository is %s and the requested selector is %s %s.",
			env.ImageRepository,
			kind,
			selector,
		),
	)
	if err := s.checkDeployabilityTx(ctx, tx, env, true); err != nil {
		return err
	}
	if err := s.checkPlacementByTx(
		ctx,
		tx,
		env.Node,
	); err != nil {
		return err
	}
	preview.Steps = deploymentSteps()
	preview.Summary = fmt.Sprintf(
		"Resolve %s %s from the app repository %s at submission, then deploy it to %s/%s with a blue/green switch.",
		kind,
		selector,
		env.ImageRepository,
		env.App,
		env.Name,
	)
	if env.ServingRelease != "" {
		preview.Warnings = append(
			preview.Warnings,
			"The serving release stays until the candidate passes health and the public route check.",
		)
	}
	preview.Warnings = append(
		preview.Warnings,
		"This read-only preview is advisory; submission rechecks current state and resolves the artifact outside the admission transaction.",
	)
	return nil
}

func (s *Service) previewGitDeploy(
	ctx context.Context,
	tx *entities.StoreTx,
	env platform.Environment,
	input *platform.DeployInput,
	preview *platform.Preview,
) error {
	if env.Revision.Source.URL == "" {
		return contract.NewFault(
			contract.PreconditionFailed,
			"environment has no Git build source and no artifact was selected",
			"Deploy an explicit artifact with --image-tag, --image-digest, or --image; use `eregion restart "+env.App+" "+env.Name+"` to reapply the serving artifact.",
		)
	}
	if env.BuildNodeName() != env.Node {
		return contract.NewFault(
			contract.PreconditionFailed,
			"build_node is distinct from the application node; moving the built image requires a shared registry",
			"Use build_node equal to the environment node.",
		)
	}
	if err := platform.ValidateGitSourceURL(env.Revision.Source.URL); err != nil {
		return contract.NewFault(
			contract.PreconditionFailed,
			"the environment's git source is invalid",
			err.Error(),
		)
	}
	ref := input.Ref
	if ref == "" {
		ref = env.Revision.Source.Ref
	}
	if err := platform.ValidateGitRef(ref); err != nil {
		return err
	}
	if !platform.IsGitCommit(strings.TrimSpace(ref)) {
		preview.Warnings = append(
			preview.Warnings,
			"The Git ref was not resolved in this dry run; submission must verify it against the repository.",
		)
	}
	if err := s.checkDeployabilityTx(ctx, tx, env, false); err != nil {
		return err
	}
	if err := s.checkPlacementByTx(
		ctx,
		tx,
		env.Node,
	); err != nil {
		return err
	}
	preview.Warnings = append(
		preview.Warnings,
		"This read-only preview is advisory; submission rechecks current state.",
	)
	preview.Steps = deploymentSteps()
	preview.Summary = fmt.Sprintf(
		"Build %s from its Git source, then deploy it to %s/%s with a blue/green switch.",
		env.App,
		env.App,
		env.Name,
	)
	if env.ServingRelease != "" {
		preview.Warnings = append(
			preview.Warnings,
			"The serving release stays until the candidate passes health and the public route check.",
		)
	}
	return nil
}

func (s *Service) Preview(
	ctx context.Context,
	actor contract.Actor,
	normalized platform.Normalized,
	idempotencyKey string,
) (platform.Preview, error) {
	if err := actor.Validate(); err != nil {
		return platform.Preview{}, err
	}
	preview := platform.Preview{
		Kind:    normalized.Kind,
		Target:  normalized.Target.String(),
		DryRun:  true,
		Input:   normalized.Input.Preview(),
		Summary: normalized.Summary,
	}
	if idempotencyKey != "" {
		// Preview must use the same normalized-input hash fence as submit: an
		// existing key with identical input is a replay, while the same key
		// with changed input is a conflict and must not hide behind a preview.
		if op, found, err := s.replaySubmission(
			ctx,
			normalized.Kind,
			normalized.Target.String(),
			actor,
			idempotencyKey,
			hashInput(normalized.Canonical),
		); err != nil {
			return preview, err
		} else if found {
			preview.Replay, preview.Operation = true, &op
			preview.Summary = "Replay the accepted operation " + string(
				op.ID,
			) + "; the same submission already exists."
			return preview, nil
		}
	}
	if updated, handled, err := s.previewPlatformDeliveryKinds(ctx, actor, normalized, preview); handled {
		return updated, err
	}
	if updated, handled, err := s.previewPlatformEnvironmentKinds(ctx, normalized, preview); handled {
		return updated, err
	}
	if preview.Summary == "" {
		preview.Summary = "Execute registered platform action " + normalized.Kind + "."
	}
	return preview, nil
}

func (s *Service) previewPlatformDeliveryKinds(
	ctx context.Context,
	actor contract.Actor,
	normalized platform.Normalized,
	preview platform.Preview,
) (platform.Preview, bool, error) {
	switch input := normalized.Input.(type) {
	case *platform.DeployInput:
		err := s.previewDeployKind(ctx, normalized, input, &preview)
		return preview, true, err
	case *platform.PromoteInput:
		updated, err := s.previewPromotion(ctx, normalized, input)
		return updated, true, err
	case *platform.BuildStopInput:
		err := s.previewBuildStopKind(ctx, input, &preview)
		return preview, true, err
	case *platform.BuildRetryInput:
		err := s.previewBuildRetryKind(ctx, input, &preview)
		return preview, true, err
	case *platform.RestartInput:
		err := s.previewRestartKind(ctx, normalized, &preview)
		return preview, true, err
	case *platform.RollbackInput:
		err := s.previewRollbackKind(ctx, normalized, input, &preview)
		return preview, true, err
	case *platform.ResourceApplyInput:
		err := s.previewResourceApplyKind(ctx, input, &preview)
		return preview, true, err
	case *platform.ResourceDestroyInput:
		err := s.previewResourceDestroyKind(ctx, input, &preview)
		return preview, true, err
	case *platform.AppDeleteInput, *platform.EnvironmentDeleteInput:
		updated, err := s.deletionPreview(ctx, normalized)
		return updated, true, err
	case *platform.RetentionPruneInput:
		err := s.previewRetentionKind(ctx, input, &preview)
		return preview, true, err
	case *platform.DomainInput:
		err := s.previewDomainKind(ctx, actor, normalized, input, &preview)
		return preview, true, err
	}
	return preview, false, nil
}

func (s *Service) previewPlatformEnvironmentKinds(
	ctx context.Context,
	normalized platform.Normalized,
	preview platform.Preview,
) (platform.Preview, bool, error) {
	switch input := normalized.Input.(type) {
	case *platform.AppCreateInput:
		err := s.previewAppCreateKind(ctx, input, &preview)
		return preview, true, err
	case *platform.EnvironmentProtectInput:
		err := s.previewEnvironmentProtectKind(ctx, normalized, input, &preview)
		return preview, true, err
	case *platform.EnvironmentCreateInput:
		err := s.previewEnvironmentCreateKind(ctx, input, &preview)
		return preview, true, err
	case *platform.EnvironmentUpdateInput:
		err := s.previewEnvironmentUpdateKind(ctx, normalized, input, &preview)
		return preview, true, err
	case *platform.ProcessScaleInput:
		previewProcessScaleKind(input, &preview)
		return preview, true, nil
	case *platform.ConfigSetInput, *platform.ConfigUnsetInput:
		previewConfigKind(&preview)
		return preview, true, nil
	case *platform.EnvironmentSecretAttachInput:
		err := s.previewEnvironmentSecretAttachKind(ctx, normalized, input, &preview)
		return preview, true, err
	case *platform.EnvironmentResourceInput:
		err := s.previewEnvironmentResourceKind(ctx, normalized, input, &preview)
		return preview, true, err
	case *platform.NodeCheckInput:
		previewNodeCheckKind(input, &preview)
		return preview, true, nil
	case *platform.NodeRegisterInput:
		previewNodeRegisterKind(input, &preview)
		return preview, true, nil
	case *platform.NodeEnrollInput:
		previewNodeEnrollKind(input, &preview)
		return preview, true, nil
	case *platform.NodeReconcileInput:
		previewNodeReconcileKind(input, &preview)
		return preview, true, nil
	}
	return preview, false, nil
}

// checkPreviewPlacement applies the same node placement facts a create
// submission checks, so a dry run refuses before the CLI writes vault values.
// Transport identity is still rechecked by the worker at dispatch.
func (s *Service) checkPreviewPlacement(ctx context.Context, nodeName, buildNode string) error {
	if err := s.checkPreviewNode(
		ctx,
		nodeName,
		platform.RoleApplication,
		platform.CapabilityDocker,
	); err != nil {
		return err
	}
	if buildNode != "" {
		if err := s.checkPreviewNode(
			ctx,
			buildNode,
			platform.RoleBuilder,
			platform.CapabilityBuildpacks,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) checkPreviewNode(ctx context.Context, nodeName, role, capability string) error {
	node, err := s.node(ctx, nodeName)
	if err != nil {
		return err
	}
	return s.checkPlacement(node, role, capability)
}

func (s *Service) checkPreviewAppAvailable(ctx context.Context, name string) error {
	exists, err := s.entities.Platform().Apps().Exists(ctx, name)
	if err != nil {
		return unavailable(err)
	}
	if exists {
		return contract.NewFault(
			contract.Conflict,
			"app "+name+" already exists",
			"Choose another app name, or inspect the existing app.",
		)
	}
	return nil
}

func (s *Service) checkPreviewApp(ctx context.Context, name string) error {
	exists, err := s.entities.Platform().Apps().Exists(ctx, name)
	if err != nil {
		return unavailable(err)
	}
	if !exists {
		return contract.NewFault(
			contract.NotFound,
			"app "+name+" was not found",
			"Create the app first, or choose an existing app.",
		)
	}
	return nil
}

func (s *Service) checkPreviewDomainAvailable(ctx context.Context, domain string) error {
	if domain == "" {
		return nil
	}
	_, _, found, err := s.entities.Platform().Domains().GetByName(ctx, domain)
	if err != nil {
		return unavailable(err)
	}
	if !found {
		return nil
	}
	return contract.NewFault(
		contract.Conflict,
		domain+" is already routed to another environment",
		"Remove it from that environment first, or use another hostname.",
	)
}

func (s *Service) checkPreviewEnvironmentAvailable(
	ctx context.Context,
	app, environment string,
) error {
	exists, err := s.entities.Platform().Environments().Exists(ctx, app, environment)
	if err != nil {
		return unavailable(err)
	}
	if exists {
		return contract.NewFault(
			contract.Conflict,
			"environment "+app+"/"+environment+" already exists",
			"Change it with eregion env update, or choose another name.",
		)
	}
	return nil
}

// checkPreviewDeploySource mirrors the deterministic refusals of a first
// deploy from a new revision, before the CLI writes any vault value.
func checkPreviewDeploySource(spec *platform.EnvironmentSpec, deploy bool) error {
	if !deploy || spec.Source.URL == "" {
		return nil
	}
	if spec.BuildNode != "" && spec.BuildNode != spec.Node {
		return contract.NewFault(
			contract.PreconditionFailed,
			"build_node is distinct from the application node; moving the built image requires a shared registry",
			"Use build_node equal to the environment node, or create with --no-deploy and deploy an explicit artifact.",
		)
	}
	return nil
}

func deploymentSteps() []string {
	return []string{
		"preflight",
		"build from the revision when needed",
		"release command",
		"candidate containers per process replica on dynamic loopback ports",
		"candidate web health",
		"Caddy upstream switch",
		"public route verification",
		"remove the previous release containers",
	}
}

func addReleaseWarnings(
	preview *platform.Preview,
	release platform.Release,
	env platform.Environment,
) {
	if release.Status != platform.ReleaseReady {
		preview.Warnings = append(
			preview.Warnings,
			"Release is not ready; submission will fail until its release operation succeeds.",
		)
	}
	if env.ServingRelease == "" {
		preview.Warnings = append(
			preview.Warnings,
			"No release is serving yet; a candidate failure leaves the environment without a serving container.",
		)
	}
}

func (s *Service) planDeployment(
	ctx context.Context,
	target platform.Target,
	releaseID platform.ReleaseID,
) (platform.Environment, platform.Release, error) {
	env, err := s.environment(ctx, target.App, target.Environment)
	if err != nil {
		return env, platform.Release{}, err
	}
	release, err := s.releaseByID(ctx, releaseID)
	if err != nil {
		return env, release, err
	}
	if release.App != env.App || release.Environment != env.Name {
		return env, release, contract.NewFault(
			contract.ValidationFailed,
			"release belongs to another environment",
			"Use a release created for this app and environment.",
		)
	}
	if err := refusePrunedRelease(release); err != nil {
		return env, release, err
	}
	node, err := s.node(ctx, env.Node)
	if err != nil {
		return env, release, err
	}
	if err := s.checkPlacement(
		node,
		platform.RoleApplication,
		platform.CapabilityDocker,
	); err != nil {
		return env, release, err
	}
	if release.Status != platform.ReleaseReady {
		return env, release, contract.NewFault(
			contract.PreconditionFailed,
			"release is not ready",
			"Wait for the release operation to succeed, then deploy it.",
		)
	}
	return env, release, nil
}

func (s *Service) environment(ctx context.Context, app, name string) (platform.Environment, error) {
	observation, found, err := s.entities.Platform().Environments().GetByName(ctx, app, name)
	if err != nil {
		return platform.Environment{}, unavailable(err)
	}
	if !found {
		return platform.Environment{}, contract.NewFault(
			contract.NotFound,
			"environment not found",
			"Create the environment under this app first.",
		)
	}
	env := observation.Environment
	env.Deployability = platform.EvaluateDeployability(env.Revision, platform.DeployabilityState{
		ArtifactReady: env.ArtifactReady, PrimaryDomain: env.PrimaryDomain,
		DNS: string(observation.PrimaryDNS),
	})
	missing, err := s.resourceDeployability(ctx, env)
	if err != nil {
		return env, err
	}
	env.Deployability.Missing = append(env.Deployability.Missing, missing...)
	env.Deployability.Deployable = len(env.Deployability.Missing) == 0
	return env, nil
}

func (s *Service) releaseByID(
	ctx context.Context,
	id platform.ReleaseID,
) (platform.Release, error) {
	release, found, err := s.entities.Platform().Releases().GetByID(ctx, id)
	if err != nil {
		return release, unavailable(err)
	}
	if !found {
		return release, contract.NewFault(
			contract.NotFound,
			"release not found",
			"Use a release ID from inventory.",
		)
	}
	return release, nil
}

// appByName reads one application record, including its immutable private
// image repository, without opening a write transaction.
func (s *Service) appByName(ctx context.Context, name string) (platform.App, error) {
	app, found, err := s.entities.Platform().Apps().GetByName(ctx, name)
	if err != nil {
		return app, unavailable(err)
	}
	if !found {
		return app, contract.NewFault(
			contract.NotFound,
			"application not found",
			"Create the application first.",
		)
	}
	return app, nil
}

func (s *Service) node(ctx context.Context, name string) (platform.Node, error) {
	return loadNode(ctx, s.entities.Nodes(), name)
}
