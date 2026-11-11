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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
)

// portAllocationLock serializes host/candidate port allocation across control
// plane processes. It is transaction-scoped and separate from River locks.
const portAllocationLock int64 = 0x657265706f727473 // "ereports"

const platformColumns = `id, kind, target, actor_kind, actor_subject, state, attempt, last_sequence, error_code, created_at, updated_at`

func (s *Service) submitPlatformWithReceipt(ctx context.Context, sub operations.Submission) (operations.SubmissionResult, error) {
	var zero operations.SubmissionResult
	if sub.Confirmation != "" {
		return zero, contract.NewFault(contract.ValidationFailed, "platform actions do not accept confirmation text", "Omit confirmation; destructive actions are guarded by their inputs and operation state.")
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
	if existing, found, err := s.replaySubmission(ctx, sub.Kind, normalized.Target.String(), sub.Actor, sub.IdempotencyKey, inputHash); err != nil {
		return zero, err
	} else if found {
		if err := attachFirstDeployOperation(ctx, s.pool, &existing); err != nil {
			return zero, err
		}
		return operations.SubmissionResult{Operation: existing, Replay: true}, nil
	}
	resolved, err := s.resolveSubmissionGitRef(ctx, normalized)
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return zero, unavailable()
	}
	defer rollback(tx)
	result, err := s.recordPlatformSubmissionTx(ctx, tx, id, normalized, sub.Actor, sub.IdempotencyKey, resolved, sub.HumanFactor, sub.WebhookSecret)
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, unavailable()
	}
	return result, nil
}

// recordPlatformSubmissionTx records one ordinary platform submission in the
// caller's transaction: operation, idempotency replay, domain rows, the one
// River platform job and the queued event. It is shared by HTTP/CLI submission
// and by the webhook worker, so a webhook can commit its operation and its
// delivery link atomically without calling Service.Submit (which opens a
// transaction) or nesting one.
func (s *Service) recordPlatformSubmissionTx(ctx context.Context, tx pgx.Tx, id contract.OperationID, normalized platform.Normalized, actor contract.Actor, key string, resolved *resolvedGitRef, humanFactor string, webhookSecret []byte) (operations.SubmissionResult, error) {
	var zero operations.SubmissionResult
	inputHash := hashInput(normalized.Canonical)
	inserted, err := tx.Exec(ctx, `INSERT INTO eregion_operations
        (id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'queued')
        ON CONFLICT (actor_kind,actor_subject,kind,target,idempotency_key) DO NOTHING`,
		id, normalized.Kind, normalized.Target.String(), actor.Kind, actor.Subject, key, inputHash, normalized.Canonical)
	if err != nil {
		return zero, unavailable()
	}
	if inserted.RowsAffected() == 0 {
		var existingID contract.OperationID
		var existingHash string
		if err := tx.QueryRow(ctx, `SELECT id,input_hash FROM eregion_operations WHERE actor_kind=$1 AND actor_subject=$2 AND kind=$3 AND target=$4 AND idempotency_key=$5`,
			actor.Kind, actor.Subject, normalized.Kind, normalized.Target.String(), key).Scan(&existingID, &existingHash); err != nil {
			return zero, unavailable()
		}
		if existingHash != inputHash {
			return zero, idempotencyInputConflict()
		}
		existing, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", existingID))
		if err != nil {
			return zero, err
		}
		if err := attachFirstDeployOperation(ctx, tx, &existing); err != nil {
			return zero, err
		}
		return operations.SubmissionResult{Operation: existing, Replay: true}, nil
	}
	var activated bool
	if err := s.preparePlatform(ctx, tx, id, normalized, actor, resolved, humanFactor, webhookSecret, &activated); err != nil {
		return zero, err
	}
	if s.enqueuePlatform == nil {
		return zero, unavailable()
	}
	op := contract.Operation{ID: id, Kind: normalized.Kind, Target: normalized.Target.String(), Actor: actor}
	jobID, err := s.enqueuePlatform(ctx, tx, op)
	if err != nil {
		return zero, unavailable()
	}
	if _, err := tx.Exec(ctx, "UPDATE eregion_operations SET job_id=$2 WHERE id=$1", id, jobID); err != nil {
		return zero, unavailable()
	}
	op, err = scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", id))
	if err != nil {
		return zero, unavailable()
	}
	if err := attachFirstDeployOperation(ctx, tx, &op); err != nil {
		return zero, err
	}
	if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "state", State: contract.Queued, Message: platformAcceptedMessageFor(normalized)}); err != nil {
		return zero, unavailable()
	}
	return operations.SubmissionResult{Operation: op, WebhookSecretActivated: activated}, nil
}

func hashInput(canonical json.RawMessage) string {
	// hashInput keeps the canonical digest identical to the diagnostic path.
	// It is intentionally a helper, not a second persistence format.
	return sha256Hex(canonical)
}

func idempotencyInputConflict() error {
	return contract.NewFault(contract.Conflict, "idempotency key already binds different inputs", "Use the original inputs to recover the existing operation; use a new key only for deliberately new work.")
}

// replaySubmission returns the operation an identical submission already
// created. It deliberately runs before git ref resolution so a replay of an
// accepted operation never depends on the remote being reachable. The
// transaction's ON CONFLICT remains the arbiter for concurrent first
// submissions; this read only short-circuits a completed replay.
func (s *Service) replaySubmission(ctx context.Context, kind, target string, actor contract.Actor, key, inputHash string) (contract.Operation, bool, error) {
	var existingID contract.OperationID
	var existingHash string
	err := s.pool.QueryRow(ctx, `SELECT id,input_hash FROM eregion_operations
        WHERE actor_kind=$1 AND actor_subject=$2 AND kind=$3 AND target=$4 AND idempotency_key=$5`,
		actor.Kind, actor.Subject, kind, target, key).Scan(&existingID, &existingHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.Operation{}, false, nil
	}
	if err != nil {
		return contract.Operation{}, false, unavailable()
	}
	if existingHash != inputHash {
		return contract.Operation{}, true, idempotencyInputConflict()
	}
	op, err := scanOperation(s.pool.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", existingID))
	if err != nil {
		return contract.Operation{}, true, unavailable()
	}
	return op, true, nil
}

// rowQueryer is the read surface shared by pgxpool.Pool and pgx.Tx. The
// first-deploy lookup must run inside the create transaction so the returned
// ID is part of the same accepted submission.
type rowQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// attachFirstDeployOperation exposes the deploy operation an app or
// environment create queued in its own transaction. A create without deploy
// intent has no such row, so the field stays empty rather than being invented.
func attachFirstDeployOperation(ctx context.Context, q rowQueryer, op *contract.Operation) error {
	if op.Kind != platform.KindAppCreate && op.Kind != platform.KindEnvironmentCreate {
		return nil
	}
	var id contract.OperationID
	err := q.QueryRow(ctx, `SELECT o.id
        FROM eregion_platform_environment_revisions r
        JOIN eregion_operations o ON o.idempotency_key = 'first-deploy:' || r.id
            AND o.kind=$2 AND o.actor_kind=$3 AND o.actor_subject=$4
        WHERE r.operation_id=$1
        ORDER BY r.created_at, o.created_at LIMIT 1`, op.ID, platform.KindDeploy, op.Actor.Kind, op.Actor.Subject).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return unavailable()
	}
	op.FirstDeployOperationID = id
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
	case platform.KindReleaseCreate:
		return "Release accepted durably; the image is verified or built before it becomes ready."
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
	if input, ok := normalized.Input.(*platform.EnvironmentUpdateInput); ok && input.RotateWebhookSecret {
		return "Environment update and push-to-deploy secret rotation accepted durably; no deploy was queued."
	}
	return platformAcceptedMessage(normalized.Kind)
}

func (s *Service) preparePlatform(ctx context.Context, tx pgx.Tx, id contract.OperationID, normalized platform.Normalized, actor contract.Actor, resolved *resolvedGitRef, humanFactor string, webhookSecret []byte, webhookActivated *bool) error {
	switch input := normalized.Input.(type) {
	case *platform.AppCreateInput:
		appID, err := platform.NewAppID()
		if err != nil {
			return unavailable()
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_apps(id,name) VALUES($1,$2)`, appID, input.Name); err != nil {
			return platformWriteError(err, "application name already exists")
		}
		app := platform.App{ID: appID, Name: input.Name}
		for index := range input.Environments {
			spec := &input.Environments[index]
			deployIntent := input.Deploy && index == 0
			summary := "Created environment " + spec.Name + " with its first desired-state revision."
			if deployIntent {
				summary += " The first deploy was queued from that revision."
			}
			envID, err := s.insertEnvironmentTx(ctx, tx, app, spec, id, actor, "environment.created", summary, deployIntent)
			if err != nil {
				return err
			}
			if deployIntent {
				if err := s.queueFirstDeployTx(ctx, tx, envID, actor, resolved); err != nil {
					return err
				}
			}
		}
	case *platform.NodeRegisterInput:
		// Registration records declared facts. It can never overwrite a node that
		// is enrolling or enrolled: verified facts are not replaced by a claim.
		tag, err := tx.Exec(ctx, `INSERT INTO eregion_platform_nodes(name,roles,capabilities,address,status)
            VALUES($1,$2,$3,$4,'registered')
            ON CONFLICT (name) DO UPDATE SET roles=EXCLUDED.roles, capabilities=EXCLUDED.capabilities,
                address=EXCLUDED.address, updated_at=clock_timestamp()
                WHERE eregion_platform_nodes.enrollment_state='registered'`,
			input.Name, input.Roles, input.Capabilities, input.Address)
		if err != nil {
			return platformWriteError(err, "node registration conflicts")
		}
		if tag.RowsAffected() == 0 {
			return contract.NewFault(contract.Conflict, "node is enrolling or enrolled; declared facts cannot overwrite verified ones", "An enrolled node's verified facts cannot be replaced by declared facts.")
		}
	case *platform.EnvironmentCreateInput:
		app, err := appByNameForUpdateTx(ctx, tx, input.App)
		if err != nil {
			return err
		}
		summary := "Created environment " + input.Name + " with its first desired-state revision."
		if input.Deploy {
			summary += " The first deploy was queued from that revision."
		}
		envID, err := s.insertEnvironmentTx(ctx, tx, app, &input.EnvironmentSpec, id, actor, "environment.created", summary, input.Deploy)
		if err != nil {
			return err
		}
		if input.Deploy {
			if err := s.queueFirstDeployTx(ctx, tx, envID, actor, resolved); err != nil {
				return err
			}
		}
	case *platform.EnvironmentUpdateInput:
		env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
		if err != nil {
			return err
		}
		if input.ExpectedRevisionID != "" && env.RevisionID != input.ExpectedRevisionID {
			return contract.NewFault(contract.Conflict, "the environment changed since the definition was read", "Re-read the environment and re-apply the definition; no revision was written.")
		}
		base, err := currentRevisionStateTx(ctx, tx, env)
		if err != nil {
			return err
		}
		state := platform.ApplyEnvironmentPatch(base, *input)
		platform.ResolveWebhookSecretName(&state)
		if err := platform.ValidatePushToDeploy(state); err != nil {
			return err
		}
		if platform.EnvironmentUpdateRemovesState(base, state) {
			if err := platform.ValidateTargetConfirmation(input.Target(), input.Confirm); err != nil {
				return err
			}
		}
		var buildNode *string
		buildNodeChanged := false
		if input.BuildNode != nil {
			value := *input.BuildNode
			buildNode = &value
			if value != "" {
				builder, err := nodeByNameTx(ctx, tx, value)
				if err != nil {
					return err
				}
				if err := s.checkPlacement(builder, platform.RoleBuilder, platform.CapabilityBuildpacks); err != nil {
					return err
				}
				value = builder.Name
				buildNode = &value
			}
			buildNodeChanged = *buildNode != env.BuildNode
		}
		if state.Runtime.Node != env.Node {
			node, err := nodeByNameTx(ctx, tx, state.Runtime.Node)
			if err != nil {
				return err
			}
			if err := s.checkPlacement(node, platform.RoleApplication, platform.CapabilityDocker); err != nil {
				return err
			}
			state.Runtime.Node = node.Name
		}
		activated, err := s.activateWebhookSecretTx(ctx, tx, actor, input, state, webhookSecret)
		if err != nil {
			return err
		}
		*webhookActivated = activated
		rotationOnly := input.RotateWebhookSecret && !buildNodeChanged && reflect.DeepEqual(base, state)
		if !rotationOnly {
			if _, err := publishEnvironmentRevisionTx(ctx, tx, env, id, actor, "environment.updated",
				"Updated environment "+env.Name+" desired state.", state, buildNode); err != nil {
				return err
			}
		}
	case *platform.ProcessScaleInput:
		// Scale publishes a revision and queues a restart during submission.
		// Try the same advisory target lock as deployment execution before the
		// environment row lock. A conflict is returned immediately instead of
		// holding a pool connection in a blocking lock wait; the order remains
		// advisory -> row.
		var lockAcquired bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`,
			targetLockKey("env/"+input.App+"/"+input.Environment)).Scan(&lockAcquired); err != nil {
			return unavailable(err)
		}
		if !lockAcquired {
			return contract.NewFault(contract.Conflict,
				"a deployment lifecycle owns this environment",
				"Wait for the current deployment, rollback or restart to settle before scaling.")
		}
		env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
		if err != nil {
			return err
		}
		var ownerBusy bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(
            SELECT 1 FROM eregion_platform_deployment_owners WHERE environment_id=$1)`, env.ID).Scan(&ownerBusy); err != nil {
			return unavailable()
		}
		if ownerBusy {
			return contract.NewFault(contract.Conflict,
				"a deployment lifecycle owns this environment",
				"Wait for the current deployment, rollback or restart to settle before scaling.")
		}
		base, err := currentRevisionStateTx(ctx, tx, env)
		if err != nil {
			return err
		}
		state, err := platform.ScaleProcess(base, input.Process, input.Replicas)
		if err != nil {
			return err
		}
		revisionID, err := publishEnvironmentRevisionTx(ctx, tx, env, id, actor, "process.scaled",
			"Scaled process "+input.Process+" to "+fmt.Sprint(input.Replicas)+" replica(s) in a new revision.", state, nil)
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
			if err := s.submitFollowupOperationTx(ctx, tx, platform.KindRestart, target, restart, actor, "scale-restart:"+string(revisionID), nil); err != nil {
				return err
			}
		}
	case *platform.ConfigSetInput:
		env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
		if err != nil {
			return err
		}
		base, err := currentRevisionStateTx(ctx, tx, env)
		if err != nil {
			return err
		}
		state := platform.SetConfig(base, input.Config)
		if _, err := publishEnvironmentRevisionTx(ctx, tx, env, id, actor, "config.set",
			"Set plain environment configuration in a new revision; no deploy was queued.", state, nil); err != nil {
			return err
		}
	case *platform.ConfigUnsetInput:
		env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
		if err != nil {
			return err
		}
		base, err := currentRevisionStateTx(ctx, tx, env)
		if err != nil {
			return err
		}
		state := platform.UnsetConfig(base, input.Keys)
		if _, err := publishEnvironmentRevisionTx(ctx, tx, env, id, actor, "config.unset",
			"Removed plain environment configuration in a new revision; no deploy was queued.", state, nil); err != nil {
			return err
		}
	case *platform.EnvironmentSecretAttachInput:
		env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
		if err != nil {
			return err
		}
		base, err := currentRevisionStateTx(ctx, tx, env)
		if err != nil {
			return err
		}
		state := platform.AttachSecret(base, input.Name)
		if _, err := publishEnvironmentRevisionTx(ctx, tx, env, id, actor, "environment.secret.attached",
			"Attached vault secret name "+input.Name+" to the environment desired state; no value entered the revision.", state, nil); err != nil {
			return err
		}
	case *platform.EnvironmentResourceInput:
		env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
		if err != nil {
			return err
		}
		base, err := currentRevisionStateTx(ctx, tx, env)
		if err != nil {
			return err
		}
		state := platform.AttachResource(base, input.Resource)
		operation, summary := "environment.resource.attached", "Attached managed resource "+input.Resource+" to the environment desired state; readiness still requires the resource to be healthy."
		if normalized.Kind == platform.KindEnvironmentResourceDetach {
			state = platform.DetachResource(base, input.Resource)
			operation, summary = "environment.resource.detached", "Detached managed resource "+input.Resource+" from the environment desired state; its container and volumes are retained."
		} else {
			var stateName string
			err := tx.QueryRow(ctx, `SELECT state FROM eregion_platform_resources WHERE environment_id=$1 AND name=$2 FOR UPDATE`, env.ID, input.Resource).Scan(&stateName)
			if errors.Is(err, pgx.ErrNoRows) {
				return contract.NewFault(contract.NotFound, "resource not found in this environment", "Apply the resource declaration first, then attach it.")
			}
			if err != nil {
				return unavailable()
			}
			if stateName == string(platform.ResourceDestroyed) || stateName == string(platform.ResourceDestroying) {
				return contract.NewFault(contract.PreconditionFailed, "resource is being destroyed", "Apply the resource again before attaching it to an environment revision.")
			}
		}
		if _, err := publishEnvironmentRevisionTx(ctx, tx, env, id, actor, operation, summary, state, nil); err != nil {
			return err
		}
	case *platform.DomainInput:
		if _, err := appByNameForUpdateTx(ctx, tx, input.App); err != nil {
			return err
		}
		env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
		if err != nil {
			return err
		}
		if err := s.ensureNoActiveDomainChangeTx(ctx, tx, id, input.App, input.Environment); err != nil {
			return err
		}
		if normalized.Kind == platform.KindDomainAdd && input.DNSMode == integrations.DomainDNSModeCloudflare {
			if input.DNSProxied == nil {
				return contract.NewFault(contract.ValidationFailed, "Cloudflare DNS needs an explicit proxy mode", "Use --dns-proxy dns-only or --dns-proxy proxied; the choice is never inferred.")
			}
			if len(input.DNSOrigins) == 0 {
				return contract.NewFault(contract.ValidationFailed, "Cloudflare DNS needs explicit public origin addresses", "Pass --dns-origin for each public origin address; the WireGuard management endpoint is not used.")
			}
		}
		var managedZone integrations.DNSZone
		if normalized.Kind == platform.KindDomainAdd && input.DNSMode == integrations.DomainDNSModeCloudflare {
			managedZone, err = s.resolveDomainDNSZone(ctx, input.DNSZone)
			if err != nil {
				return err
			}
			if err := integrations.ValidateDomainDNSHost(managedZone, input.Domain); err != nil {
				return err
			}
			if err := s.authorizeDomainDNSWrite(ctx, actor); err != nil {
				return err
			}
		}
		if normalized.Kind == platform.KindDomainRemove {
			var domainID platform.DomainID
			var primary bool
			err := tx.QueryRow(ctx, `SELECT id,is_primary FROM eregion_platform_domains WHERE name=$1 AND environment_id=$2`, input.Domain, env.ID).Scan(&domainID, &primary)
			if errors.Is(err, pgx.ErrNoRows) {
				return contract.NewFault(contract.NotFound, input.Domain+" is not routed to "+input.App+"/"+input.Environment, "List routes with eregion domain list.")
			}
			if err != nil {
				return unavailable()
			}
			if err := platform.ValidateDomainConfirmation(input.Domain, input.Confirm); err != nil {
				return err
			}
			if primary {
				return contract.NewFault(contract.PreconditionFailed, "the primary domain cannot be removed",
					"Add another domain with --primary first, then remove this one. Deleting the environment or app cleans its managed DNS.")
			}
			ledger, err := loadDomainDNSLedgerByDomainIDTx(ctx, tx, domainID)
			if err != nil {
				return err
			}
			if ledger != nil {
				if input.DNSMode == integrations.DomainDNSModeCloudflare && input.DNSZone != "" {
					zone, err := s.resolveDomainDNSZone(ctx, input.DNSZone)
					if err != nil {
						return err
					}
					if zone.ZoneID != ledger.ZoneID {
						return contract.NewFault(contract.Conflict, "managed DNS zone does not match the binding ledger", "Omit --dns-zone to use the persisted zone, or correct the flag.")
					}
				}
				if err := s.validateDomainDNSCleanupRecovery(*ledger); err != nil {
					return err
				}
				if err := s.authorizeDomainDNSWrite(ctx, actor); err != nil {
					return err
				}
				return markDomainDNSCleanupTx(ctx, tx, domainID, id)
			}
			if input.DNSMode != integrations.DomainDNSModeCloudflare {
				return nil
			}
			zone, err := s.resolveDomainDNSZone(ctx, input.DNSZone)
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
		domainID, err := s.insertDomainTx(ctx, tx, env, *input)
		if err != nil {
			return err
		}
		if input.DNSMode == integrations.DomainDNSModeCloudflare {
			return upsertDomainDNSIntentTx(ctx, tx, domainID, managedZone, input.DNSOrigins, *input.DNSProxied, id)
		}
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
		if ledger.State == domainDNSStateCleanupPending || ledger.State == domainDNSStateCleanupFailed {
			return contract.NewFault(contract.Conflict, "managed DNS cleanup is pending for this binding", "Retry the deletion/cleanup, or re-add the binding explicitly with --dns cloudflare.")
		}
		if ledger.State != domainDNSStateApplied {
			return contract.NewFault(contract.Conflict, "managed DNS desired state is not applied", "Retry domain add with the explicit Cloudflare flags, or clean up the binding first.")
		}
		if len(ledger.DesiredOrigins) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domains SET expected_addresses=$2 WHERE id=$1`, domainID, ledger.DesiredOrigins); err != nil {
				return unavailable()
			}
		}
		return fenceDomainDNSOperationTx(ctx, tx, domainID, id)
	case *platform.ReleaseCreateInput:
		env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
		if err != nil {
			return err
		}
		if input.Source.Type != "image" {
			if err := crossNodeBuildUnsupported(env, input.Source.Type); err != nil {
				return err
			}
			node, err := nodeByNameTx(ctx, tx, env.BuildNodeName())
			if err != nil {
				return err
			}
			if err := s.checkPlacement(node, platform.RoleBuilder, platform.CapabilityBuildpacks); err != nil {
				return err
			}
		}
		if input.Source.Type == "directory" {
			if _, err := platform.ResolveBuildSource(input.Source.Path, s.buildSourceRoots()); err != nil {
				return err
			}
		}
		image := input.Source.Image
		if image == "" {
			if input.Build == nil || input.Build.Image == "" {
				image = "eregion/" + input.App + "-" + input.Environment + ":" + strings.TrimPrefix(string(id), "op_")[:12]
			} else {
				image = input.Build.Image
			}
		}
		if err := platform.ValidateImage(image); err != nil {
			return err
		}
		releaseID, err := platform.NewReleaseID()
		if err != nil {
			return unavailable()
		}
		source, _ := json.Marshal(input.Source)
		var build, command []byte
		if input.Build != nil {
			build, _ = json.Marshal(input.Build)
		}
		if input.Command != nil {
			command, _ = json.Marshal(input.Command)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_releases
            (id,operation_id,app_id,environment_id,image,source,build,command,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'pending')`,
			releaseID, id, envAppID(ctx, tx, env.App), env.ID, image, source, nullableJSON(build), nullableJSON(command)); err != nil {
			return platformWriteError(err, "release could not be recorded")
		}
		if input.Source.Type != "image" {
			buildID, err := insertBuildTx(ctx, tx, id, env, releaseID, env.RevisionID, input.Source, "")
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE eregion_platform_releases SET build_id=$2 WHERE id=$1`, releaseID, buildID); err != nil {
				return unavailable()
			}
		}
	case *platform.BuildStopInput:
		build, err := s.buildByID(ctx, input.BuildID)
		if err != nil {
			return err
		}
		if build.State == platform.BuildSucceeded {
			return contract.NewFault(contract.Conflict, "build already succeeded", "Retry with a new build after changing the source if another artifact is needed.")
		}
		return nil
	case *platform.BuildRetryInput:
		old, err := s.buildByID(ctx, input.BuildID)
		if err != nil {
			return err
		}
		if old.State != platform.BuildFailed && old.State != platform.BuildCancelled {
			return contract.NewFault(contract.PreconditionFailed, "build is not in a retryable state", "Wait for the build to finish, or stop a running build first.")
		}
		env, err := environmentByNameTx(ctx, tx, old.App, old.Environment)
		if err != nil {
			return err
		}
		if err := s.createRetryReleaseTx(ctx, tx, id, env, old); err != nil {
			return err
		}
	case *platform.DeployInput:
		env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
		if err != nil {
			return err
		}
		if err := s.checkDeployabilityTx(ctx, tx, env, input.ReleaseID != ""); err != nil {
			return err
		}
		releaseID := input.ReleaseID
		if releaseID == "" {
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
			return contract.NewFault(contract.ValidationFailed, "release belongs to another environment", "Use a release created for this app and environment.")
		}
		if err := refusePrunedRelease(release); err != nil {
			return err
		}
		if release.Status != platform.ReleaseReady && input.ReleaseID != "" {
			return contract.NewFault(contract.PreconditionFailed, "release is not ready", "Wait for the release operation to succeed, then deploy it.")
		}
		if err := s.checkPlacementByTx(ctx, tx, env.Node, platform.RoleApplication, platform.CapabilityDocker); err != nil {
			return err
		}
		if err := s.prepareDeploymentTx(ctx, tx, id, env, release.ID, platform.ActionDeploy, env.RevisionID, env.ServingDeploymentID, nil); err != nil {
			return err
		}
	case *platform.PromoteInput:
		return s.preparePromotionTx(ctx, tx, id, input)
	case *platform.RollbackInput:
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
			return contract.NewFault(contract.PreconditionFailed, "environment has no previous release", "Deploy a release before rolling back, or provide to_release_id explicitly.")
		}
		release, err := releaseByIDTx(ctx, tx, targetID)
		if err != nil {
			return err
		}
		if release.App != env.App || release.Environment != env.Name {
			return contract.NewFault(contract.ValidationFailed, "release belongs to another environment", "Use a release created for this app and environment.")
		}
		if err := refusePrunedRelease(release); err != nil {
			return err
		}
		if release.Status != platform.ReleaseReady {
			return contract.NewFault(contract.PreconditionFailed, "release is not ready", "Roll back to a ready release.")
		}
		if err := s.checkPlacementByTx(ctx, tx, env.Node, platform.RoleApplication, platform.CapabilityDocker); err != nil {
			return err
		}
		revisionID, secrets, err := rollbackSnapshotTx(ctx, tx, env, release.ID)
		if err != nil {
			return err
		}
		if err := s.prepareDeploymentTx(ctx, tx, id, env, release.ID, platform.ActionRollback, revisionID, env.ServingDeploymentID, secrets); err != nil {
			return err
		}
	case *platform.RestartInput:
		env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
		if err != nil {
			return err
		}
		if env.ServingRelease == "" || env.ServingDeploymentID == "" {
			return contract.NewFault(contract.PreconditionFailed, "no serving deployment is available to restart", "Deploy a release before restarting the environment.")
		}
		release, err := releaseByIDTx(ctx, tx, env.ServingRelease)
		if err != nil {
			return err
		}
		if release.Status != platform.ReleaseReady {
			return contract.NewFault(contract.PreconditionFailed, "serving release is not ready", "Deploy a ready release before restarting.")
		}
		if input.ExpectedServingDeploymentID != "" {
			// A maintenance repair names the serving deployment it observed.
			// The environment row is locked FOR UPDATE by this transaction, so
			// a deploy accepted before this submission is visible here.
			active, err := activeEnvironmentOperations(ctx, tx, env.App, env.Name, id)
			if err != nil {
				return err
			}
			if input.ExpectedServingDeploymentID != env.ServingDeploymentID {
				return contract.NewFault(contract.PreconditionFailed, "serving deployment changed before the repair was queued", "The next maintenance run will re-observe the serving workload.")
			}
			if !repairSubmissionAllowed(input.ExpectedServingDeploymentID, env.ServingDeploymentID, active) {
				return contract.NewFault(contract.Conflict, "environment has an active deployment, promotion, restart or deletion operation", "Let the current operation finish; the next maintenance run will re-observe.")
			}
		}
		if err := s.checkPlacementByTx(ctx, tx, env.Node, platform.RoleApplication, platform.CapabilityDocker); err != nil {
			return err
		}
		if err := s.prepareDeploymentTx(ctx, tx, id, env, release.ID, platform.ActionRestart, env.RevisionID, env.ServingDeploymentID, nil); err != nil {
			return err
		}
	case *platform.ResourceApplyInput:
		return s.prepareResourceApply(ctx, tx, id, input)
	case *platform.ResourceDestroyInput:
		return s.prepareResourceDestroy(ctx, tx, id, input)
	case *platform.EnvironmentProtectInput:
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
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments SET protected=$2,updated_at=clock_timestamp() WHERE id=$1`, env.ID, input.Protected); err != nil {
			return unavailable()
		}
	case *platform.AppDeleteInput:
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
		if err := s.ensureNoLiveResourcesTx(ctx, tx, input.App, "", string(app.ID), ""); err != nil {
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
	case *platform.EnvironmentDeleteInput:
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
		last, err := lastEnvironmentTx(ctx, tx, string(app.ID), string(env.ID))
		if err != nil {
			return err
		}
		if platform.EnvironmentDeleteRequiresHumanFactor(env.Protected, last) {
			if err := s.requireHumanFactorTx(humanFactor); err != nil {
				return err
			}
		}
		if err := s.ensureNoLiveResourcesTx(ctx, tx, env.App, env.Name, string(envAppID(ctx, tx, env.App)), string(env.ID)); err != nil {
			return err
		}
		if err := s.ensureNoActiveDeleteTargets(ctx, tx, id, env.App, env.Name); err != nil {
			return err
		}
		if err := s.ensureDomainDNSCleanupSafeTx(ctx, tx, actor, env.App, env.Name); err != nil {
			return err
		}
		if err := markDomainDNSCleanupForEnvironmentsTx(ctx, tx, env.App, env.Name, id); err != nil {
			return err
		}
	case *platform.NodeCheckInput:
		if _, err := nodeByNameTx(ctx, tx, input.Node); err != nil {
			return err
		}
	case *platform.NodeEnrollInput:
		return s.prepareNodeEnroll(ctx, tx, id, input)
	case *platform.NodeReconcileInput:
		if _, err := nodeByNameTx(ctx, tx, input.Node); err != nil {
			return err
		}
	default:
		// Extension actions own their domain rows. The operation row, idempotency,
		// River job, event history, and target lock remain shared.
	}
	return nil
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
		return contract.NewFault(contract.PreconditionFailed,
			"this server cannot verify the human factor",
			"Run the deletion or unprotection through the node RPC transport, where the TOTP code is verified inside the request.")
	}
	return s.humanFactor(code)
}

// lastEnvironmentTx reports whether envID is the application's only remaining
// environment. The application row is locked by the caller, so a concurrent
// environment create or delete cannot change the answer mid-submission.
func lastEnvironmentTx(ctx context.Context, tx pgx.Tx, appID, envID string) (bool, error) {
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_environments WHERE app_id=$1`, appID).Scan(&count); err != nil {
		return false, unavailable()
	}
	if count != 1 {
		return false, nil
	}
	var found string
	if err := tx.QueryRow(ctx, `SELECT id FROM eregion_platform_environments WHERE app_id=$1 AND id=$2`, appID, envID).Scan(&found); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, unavailable()
	}
	return true, nil
}

// insertDomainTx writes or promotes one domain binding. It is shared by
// platform.domain.add and by app/environment creation, which carry the first
// binding so it commits before any first deploy is queued.
func (s *Service) insertDomainTx(ctx context.Context, tx pgx.Tx, env platform.Environment, input platform.DomainInput) (platform.DomainID, error) {
	var zero platform.DomainID
	addresses := input.Addresses
	if input.DNSMode == integrations.DomainDNSModeCloudflare && len(input.DNSOrigins) > 0 {
		addresses = input.DNSOrigins
	}
	node, err := nodeByNameTx(ctx, tx, env.Node)
	if err != nil {
		return zero, err
	}
	if node.Transport != "local" {
		return zero, contract.NewFault(contract.PreconditionFailed, "domains route only to environments on the main node for now", "Create the environment on the main node.")
	}
	var existingID platform.DomainID
	var existingEnvironment string
	err = tx.QueryRow(ctx, `SELECT id,environment_id FROM eregion_platform_domains WHERE name=$1`, input.Domain).Scan(&existingID, &existingEnvironment)
	switch {
	case err == nil && existingEnvironment != string(env.ID):
		return zero, contract.NewFault(contract.Conflict, input.Domain+" is already routed to another environment", "Remove it from that environment first, or use another hostname.")
	case err == nil:
		// Re-adding the same binding is idempotent; --primary promotes it and
		// refreshes the recorded main-node addresses for DNS observation.
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domains SET check_path=$2 WHERE name=$1`, input.Domain, input.CheckPath); err != nil {
			return zero, unavailable()
		}
		if len(addresses) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domains SET expected_addresses=$2 WHERE name=$1`, input.Domain, addresses); err != nil {
				return zero, unavailable()
			}
		}
		if input.Primary {
			if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domains SET is_primary=false WHERE environment_id=$1 AND is_primary AND name<>$2`, env.ID, input.Domain); err != nil {
				return zero, unavailable()
			}
			if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domains SET is_primary=true, desired_revision=desired_revision+1 WHERE name=$1`, input.Domain); err != nil {
				return zero, unavailable()
			}
		}
		return existingID, nil
	case errors.Is(err, pgx.ErrNoRows):
		id, idErr := platform.NewDomainID()
		if idErr != nil {
			return zero, unavailable()
		}
		primary := input.Primary
		if !primary {
			var hasPrimary bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_domains WHERE environment_id=$1 AND is_primary)`, env.ID).Scan(&hasPrimary); err != nil {
				return zero, unavailable()
			}
			primary = !hasPrimary
		}
		if primary {
			if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domains SET is_primary=false WHERE environment_id=$1 AND is_primary`, env.ID); err != nil {
				return zero, unavailable()
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_domains(id,name,environment_id,is_primary,expected_addresses,check_path)
            VALUES($1,$2,$3,$4,$5,$6)`, id, input.Domain, env.ID, primary, addresses, input.CheckPath); err != nil {
			return zero, platformWriteError(err, input.Domain+" is already routed")
		}
		return id, nil
	default:
		return zero, unavailable()
	}
}

func envAppID(ctx context.Context, tx pgx.Tx, app string) string {
	var id string
	_ = tx.QueryRow(ctx, `SELECT id FROM eregion_platform_apps WHERE name=$1`, app).Scan(&id)
	return id
}

// checkDeployabilityTx refuses a deployment before any build or container work
// when the pure deployability report lists blockers. Deploycrate's gate blocks
// on source, registry/artifact, runtime, a web process, a primary domain and a
// managed DNS binding that is not applied. A merely unobserved record does not
// block the first deploy, nor does a Cloudflare-proxied one; a wrong answer does. A source
// deploy creates its artifact, and an explicit release is checked separately,
// so artifact readiness is not a blocker here. An explicit release also carries
// its own artifact, so it does not need the revision's source binding.
func (s *Service) checkDeployabilityTx(ctx context.Context, tx pgx.Tx, env platform.Environment, explicitRelease bool) error {
	dns := ""
	if env.PrimaryDomain != "" {
		var state string
		err := tx.QueryRow(ctx, `SELECT dns_state FROM eregion_platform_domains
            WHERE environment_id=$1 AND is_primary LIMIT 1`, env.ID).Scan(&state)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return unavailable()
		}
		dns = state
	}
	report := platform.EvaluateDeployability(env.Revision, platform.DeployabilityState{
		ArtifactReady: true, ExplicitRelease: explicitRelease, PrimaryDomain: env.PrimaryDomain, DNS: dns,
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
	fault.Details = map[string]any{"missing": report.Missing}
	return fault
}

func (s *Service) prepareDeploymentTx(ctx context.Context, tx pgx.Tx, opID contract.OperationID, env platform.Environment, releaseID platform.ReleaseID, action platform.DeploymentAction, revisionID platform.RevisionID, previousDeploymentID platform.DeploymentID, secretRevisions []byte) error {
	if err := checkReleaseCommandForDeploymentTx(ctx, tx, releaseID, revisionID, action); err != nil {
		return err
	}
	depID, err := platform.NewDeploymentID()
	if err != nil {
		return unavailable()
	}
	if len(secretRevisions) == 0 {
		secretRevisions = []byte(`{}`)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_deployments
        (id,operation_id,app_id,environment_id,action,release_id,previous_release_id,previous_deployment_id,revision_id,secret_revisions)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		depID, opID, envAppID(ctx, tx, env.App), env.ID, string(action), releaseID,
		nullableReleaseID(env.ServingRelease), nullableDeploymentID(previousDeploymentID),
		nullableRevisionID(revisionID), secretRevisions); err != nil {
		return platformWriteError(err, "deployment could not be recorded")
	}
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments SET desired_release_id=$2, updated_at=clock_timestamp() WHERE id=$1`, env.ID, releaseID); err != nil {
		return unavailable()
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
// that belongs to another source.
type resolvedGitRef struct {
	Kind string
	URL  string
	Ref  string
	SHA  string
}

// resolveSubmissionGitRef resolves a git ref before the submission transaction
// opens for every path that creates a release from an environment revision:
// deploy, CI and the webhook (which already sends a commit), and the first
// deploy requested by app or env create. A failure here creates no operation.
func (s *Service) resolveSubmissionGitRef(ctx context.Context, normalized platform.Normalized) (*resolvedGitRef, error) {
	switch input := normalized.Input.(type) {
	case *platform.DeployInput:
		if input.ReleaseID != "" {
			return nil, nil
		}
		env, err := s.environment(ctx, input.App, input.Environment)
		if err != nil {
			return nil, err
		}
		if env.Revision == nil {
			return nil, nil
		}
		if env.Revision.Source.Kind != "git" {
			// Record the kind so a revision that becomes git before the
			// transaction opens conflicts instead of taking the git branch
			// without an exact commit.
			return &resolvedGitRef{Kind: env.Revision.Source.Kind, URL: env.Revision.Source.URL}, nil
		}
		ref := input.Ref
		if ref == "" {
			ref = env.Revision.Source.Ref
		}
		return s.resolveGitSourceRef(ctx, env.App, env.Name, env.Revision.Source, ref)
	case *platform.AppCreateInput:
		if !input.Deploy || len(input.Environments) == 0 {
			return nil, nil
		}
		first := &input.Environments[0]
		if first.Source.Kind != "git" {
			return &resolvedGitRef{Kind: first.Source.Kind, URL: first.Source.URL}, nil
		}
		return s.resolveGitSourceRef(ctx, input.Name, first.Name, first.Source, first.Source.Ref)
	case *platform.EnvironmentCreateInput:
		if !input.Deploy {
			return nil, nil
		}
		if input.Source.Kind != "git" {
			return &resolvedGitRef{Kind: input.Source.Kind, URL: input.Source.URL}, nil
		}
		return s.resolveGitSourceRef(ctx, input.App, input.Name, input.Source, input.Source.Ref)
	}
	return nil, nil
}

func (s *Service) resolveGitSourceRef(ctx context.Context, app, environment string, source platform.SourceBinding, ref string) (*resolvedGitRef, error) {
	// A submission that already carries a commit (the CI and webhook paths) needs
	// no repository read and no credential. It is already exact.
	if platform.IsGitCommit(ref) {
		return &resolvedGitRef{Kind: "git", URL: source.URL, Ref: ref, SHA: ref}, nil
	}
	var credential []byte
	if source.CredentialSecretName != "" {
		value, _, err := s.resolveCredentialSecret(ctx, platform.Environment{App: app, Name: environment}, source.CredentialSecretName)
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
	return &resolvedGitRef{Kind: "git", URL: source.URL, Ref: ref, SHA: sha}, nil
}

// sourceChangedDuringSubmission is the single conflict used when the revision
// read by the transaction differs from the one the ref was resolved against.
func sourceChangedDuringSubmission() error {
	return contract.NewFault(contract.Conflict, "environment source changed during submission; retry", "Retry the deploy with the environment's current revision.")
}

// checkResolvedGitSource compares the submission-time resolution against the
// revision the transaction reads. A source-kind change can never fall through
// to another branch: a git source that became an image has no commit to record.
// An explicit --ref owns the effective ref, so only a URL change conflicts then.
func checkResolvedGitSource(resolved *resolvedGitRef, source platform.SourceBinding, refOverride string) error {
	if resolved == nil || resolved.Kind != source.Kind {
		return sourceChangedDuringSubmission()
	}
	if source.Kind != "git" {
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
// deploy from the environment's current revision. The ref was resolved to an
// exact commit before the transaction; the commit is recorded on both rows
// before any build starts, and the worker only builds that commit.
func (s *Service) createReleaseFromRevisionTx(ctx context.Context, tx pgx.Tx, opID contract.OperationID, env platform.Environment, refOverride string, resolved *resolvedGitRef) (platform.ReleaseID, error) {
	if env.Revision == nil {
		return "", contract.NewFault(contract.PreconditionFailed, "environment has no current revision", "Save an environment revision before deploying.")
	}
	state := env.Revision.RevisionState
	source := state.Source
	if err := checkResolvedGitSource(resolved, source, refOverride); err != nil {
		return "", err
	}
	if refOverride != "" && source.Kind != "git" {
		return "", contract.NewFault(contract.ValidationFailed, "ref is only valid for a git source", "Omit ref when the environment revision binds an image.")
	}
	releaseID, err := platform.NewReleaseID()
	if err != nil {
		return "", unavailable()
	}
	var sourceJSON []byte
	var buildJSON []byte
	var buildSource platform.Source
	commit := ""
	switch source.Kind {
	case "image":
		buildSource = platform.Source{Type: "image", Image: source.Image}
		sourceJSON, _ = json.Marshal(buildSource)
	case "git":
		if env.BuildNodeName() != env.Node {
			return "", contract.NewFault(contract.PreconditionFailed, "build_node is distinct from the application node; moving the built image requires a shared registry", "Use build_node equal to the environment node, or deploy an image release.")
		}
		ref := refOverride
		if ref == "" {
			ref = source.Ref
		}
		if err := platform.ValidateGitSourceURL(source.URL); err != nil {
			return "", contract.NewFault(contract.PreconditionFailed, "the environment's git source is invalid", err.Error())
		}
		if !platform.IsGitCommit(resolved.SHA) {
			return "", contract.NewFault(contract.PreconditionFailed, "the git ref did not resolve to an exact commit", "Retry the deploy so the ref resolves to a full commit SHA.")
		}
		commit = resolved.SHA
		buildSource = platform.Source{
			Type: "git", URL: source.URL, Ref: ref, Path: source.ContextPath,
			CredentialSecretName: source.CredentialSecretName,
		}
		sourceJSON, _ = json.Marshal(buildSource)
		build := platform.Build{Builder: source.Builder, Env: source.BuildEnv, GoTargets: source.GoTargets, Buildpacks: source.Buildpacks}
		buildJSON, _ = json.Marshal(build)
	default:
		return "", contract.NewFault(contract.PreconditionFailed, "environment source is not deployable", "Bind an image or HTTPS git source in the environment revision.")
	}
	image := "eregion/" + env.App + "-" + env.Name + ":" + strings.TrimPrefix(string(opID), "op_")[:12]
	if source.Kind == "image" {
		image = source.Image
	}
	if err := platform.ValidateImage(image); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_releases
        (id,operation_id,app_id,environment_id,image,source,build,status,commit_sha)
        VALUES($1,$2,$3,$4,$5,$6,$7,'pending',$8)`,
		releaseID, opID, envAppID(ctx, tx, env.App), env.ID, image, sourceJSON, nullableJSON(buildJSON), commit); err != nil {
		return "", platformWriteError(err, "release could not be recorded")
	}
	if source.Kind == "git" {
		buildID, err := insertBuildTx(ctx, tx, opID, env, releaseID, env.RevisionID, buildSource, commit)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_releases SET build_id=$2 WHERE id=$1`, releaseID, buildID); err != nil {
			return "", unavailable()
		}
	}
	return releaseID, nil
}

func (s *Service) createRetryReleaseTx(ctx context.Context, tx pgx.Tx, opID contract.OperationID, env platform.Environment, old platform.BuildRecord) error {
	oldRelease, err := releaseByIDTx(ctx, tx, old.ReleaseID)
	if err != nil {
		return err
	}
	image := "eregion/" + env.App + "-" + env.Name + ":" + strings.TrimPrefix(string(opID), "op_")[:12]
	if old.Source.Type == "image" {
		image = old.Source.Image
	}
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
	if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_releases
        (id,operation_id,app_id,environment_id,image,source,build,status,commit_sha)
        VALUES($1,$2,$3,$4,$5,$6,$7,'pending',$8)`,
		releaseID, opID, envAppID(ctx, tx, env.App), env.ID, image, sourceJSON, nullableJSON(buildJSON), commit); err != nil {
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
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_releases SET build_id=$2 WHERE id=$1`, releaseID, buildID); err != nil {
		return unavailable()
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
		return source, "", contract.NewFault(contract.PreconditionFailed, "build has no exact commit to retry", "Start a new deploy so the ref resolves to a commit first.")
	}
	source.Ref = commit
	return source, commit, nil
}

func insertBuildTx(ctx context.Context, tx pgx.Tx, opID contract.OperationID, env platform.Environment, releaseID platform.ReleaseID, revisionID platform.RevisionID, source platform.Source, commit string) (platform.BuildID, error) {
	buildID, err := platform.NewBuildID()
	if err != nil {
		return "", unavailable()
	}
	sourceJSON, _ := json.Marshal(source)
	if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_builds
        (id,operation_id,app_id,environment_id,revision_id,release_id,source,commit_sha,state)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,'pending')`,
		buildID, opID, envAppID(ctx, tx, env.App), env.ID, nullableRevisionID(revisionID),
		nullableReleaseID(releaseID), sourceJSON, commit); err != nil {
		return "", platformWriteError(err, "build could not be recorded")
	}
	return buildID, nil
}

// rollbackSnapshotTx finds the revision and exact secret revisions the target
// release used when it last served (MASTER D18).
func rollbackSnapshotTx(ctx context.Context, tx pgx.Tx, env platform.Environment, releaseID platform.ReleaseID) (platform.RevisionID, []byte, error) {
	var revisionID string
	var secretRevisions []byte
	err := tx.QueryRow(ctx, `SELECT coalesce(revision_id,''),secret_revisions
        FROM eregion_platform_deployments
        WHERE environment_id=$1 AND release_id=$2 AND state='succeeded'
        ORDER BY completed_at DESC NULLS LAST, created_at DESC LIMIT 1`, env.ID, releaseID).
		Scan(&revisionID, &secretRevisions)
	if errors.Is(err, pgx.ErrNoRows) {
		return env.RevisionID, []byte(`{}`), nil
	}
	if err != nil {
		return "", nil, unavailable()
	}
	if revisionID == "" {
		revisionID = string(env.RevisionID)
	}
	if len(secretRevisions) == 0 {
		secretRevisions = []byte(`{}`)
	}
	return platform.RevisionID(revisionID), secretRevisions, nil
}

func (s *Service) queueFirstDeployTx(ctx context.Context, tx pgx.Tx, envID platform.EnvironmentID, actor contract.Actor, resolved *resolvedGitRef) error {
	env, err := environmentByIDTx(ctx, tx, envID)
	if err != nil {
		return unavailable()
	}
	if env.Revision == nil || !env.Revision.DeployIntent {
		return nil
	}
	input := platform.DeployInput{App: env.App, Environment: env.Name}
	return s.submitFollowupOperationTx(ctx, tx, platform.KindDeploy, "env/"+env.App+"/"+env.Name, input, actor, "first-deploy:"+string(env.RevisionID), resolved)
}

// submitFollowupOperationTx records one additional operation and River job in
// the caller's transaction. It is used for the first deploy requested by app
// create, so the intent is durable with the revision that requested it.
func (s *Service) submitFollowupOperationTx(ctx context.Context, tx pgx.Tx, kind, target string, input any, actor contract.Actor, key string, resolved *resolvedGitRef) error {
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
	_, err = s.recordPlatformSubmissionTx(ctx, tx, id, normalized, actor, key, resolved, "", nil)
	return err
}

func nullableReleaseID(id platform.ReleaseID) any {
	if id == "" {
		return nil
	}
	return string(id)
}

func nullableRevisionID(id platform.RevisionID) any {
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
		return contract.NewFault(contract.Conflict, message, "Choose a different name or recover the existing accepted record.")
	}
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return contract.NewFault(contract.PreconditionFailed, "referenced platform record is missing", "Create the app, node, or environment first.")
	}
	return unavailable(err)
}

func allocatePorts(ctx context.Context, tx pgx.Tx, explicitHost int) (int, int, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, portAllocationLock); err != nil {
		return 0, 0, unavailable()
	}
	host := explicitHost
	if host != 0 {
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_environments WHERE host_port=$1 OR candidate_port=$1)`, host).Scan(&used); err != nil {
			return 0, 0, unavailable()
		}
		if used {
			return 0, 0, contract.NewFault(contract.Conflict, "host port is already assigned", "Omit host_port to allocate one automatically.")
		}
	} else if err := tx.QueryRow(ctx, `SELECT p FROM generate_series(20000,29999) p
        WHERE NOT EXISTS (SELECT 1 FROM eregion_platform_environments e WHERE e.host_port=p OR e.candidate_port=p) ORDER BY p LIMIT 1`).Scan(&host); err != nil {
		return 0, 0, contract.NewFault(contract.Unavailable, "no stable host port is available", "Remove an unused environment or choose a host_port explicitly.")
	}
	var candidate int
	if err := tx.QueryRow(ctx, `SELECT p FROM generate_series(30000,39999) p
        WHERE p<>$1 AND NOT EXISTS (SELECT 1 FROM eregion_platform_environments e WHERE e.candidate_port=p OR e.host_port=p) ORDER BY p LIMIT 1`, host).Scan(&candidate); err != nil {
		return 0, 0, contract.NewFault(contract.Unavailable, "no candidate host port is available", "Remove an unused environment.")
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

func (s *Service) checkPlacementByTx(ctx context.Context, tx pgx.Tx, nodeName, role, capability string) error {
	node, err := nodeByNameTx(ctx, tx, nodeName)
	if err != nil {
		return err
	}
	return s.checkPlacement(node, role, capability)
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
	_, err := s.pool.Exec(ctx, `INSERT INTO eregion_platform_nodes(name,roles,capabilities,address,status,last_seen_at)
        VALUES($1,$2,$3,'','ready',clock_timestamp())
        ON CONFLICT (name) DO UPDATE SET roles=EXCLUDED.roles,capabilities=EXCLUDED.capabilities,
            status='ready',last_seen_at=clock_timestamp(),updated_at=clock_timestamp()`, identity, roles, caps)
	if err != nil {
		return unavailable(err)
	}
	return nil
}

// ---------------------------------------------------------------- inventory

// Inventory is the shared read model for GET /v1/platform.
func (s *Service) Inventory(ctx context.Context, actor contract.Actor, query platform.InventoryQuery) (platform.Inventory, error) {
	if err := actor.Validate(); err != nil {
		return platform.Inventory{}, err
	}
	if query.Limit == 0 {
		query.Limit = 50
	}
	if query.Limit < 1 || query.Limit > 200 {
		return platform.Inventory{}, contract.NewFault(contract.ValidationFailed, "inventory limit out of range", "Use a limit from 1 to 200.")
	}
	for _, value := range []string{query.App, query.Environment, query.Node, query.State} {
		if value != "" && len(value) > 128 {
			return platform.Inventory{}, contract.NewFault(contract.ValidationFailed, "inventory filter is too long", "Use a documented app, environment, node, or state filter.")
		}
	}
	inventory := platform.Inventory{Collection: query.Collection, Limit: query.Limit}
	switch query.Collection {
	case "summary":
		items, err := s.inventorySummary(ctx)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "nodes":
		items, err := s.inventoryNodes(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "apps":
		items, err := s.inventoryApps(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "environments":
		items, err := s.inventoryEnvironments(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "revisions":
		items, err := s.inventoryRevisions(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "deployability":
		items, err := s.inventoryDeployability(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "releases":
		items, err := s.inventoryReleases(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "builds":
		items, err := s.inventoryBuilds(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "deployments":
		items, err := s.inventoryDeployments(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "domains":
		items, err := s.inventoryDomains(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "changes":
		items, err := s.inventoryChanges(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "resources":
		items, err := s.inventoryResources(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
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
		return inventory, contract.NewFault(contract.ValidationFailed, "unknown inventory collection", "Use summary, nodes, apps, environments, revisions, deployability, domains, releases, builds, deployments, changes, or resources.")
	}
	return inventory, nil
}

func (s *Service) inventorySummary(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	rows, err := s.pool.Query(ctx, `SELECT 'apps', count(*) FROM eregion_platform_apps
        UNION ALL SELECT 'environments', count(*) FROM eregion_platform_environments
        UNION ALL SELECT 'releases', count(*) FROM eregion_platform_releases
        UNION ALL SELECT 'builds', count(*) FROM eregion_platform_builds
        UNION ALL SELECT 'deployments', count(*) FROM eregion_platform_deployments
        UNION ALL SELECT 'nodes', count(*) FROM eregion_platform_nodes
        UNION ALL SELECT 'resources', count(*) FROM eregion_platform_resources
        UNION ALL SELECT 'serving', count(*) FROM eregion_platform_environments WHERE serving_release_id IS NOT NULL`)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var count int64
		if err := rows.Scan(&name, &count); err != nil {
			return nil, unavailable()
		}
		out[name] = count
	}
	return out, rows.Err()
}

func (s *Service) inventoryNodes(ctx context.Context, query platform.InventoryQuery) ([]platform.Node, error) {
	items := []platform.Node{}
	rows, err := s.pool.Query(ctx, nodeSelect+` WHERE ($2='' OR name=$2) AND ($3='' OR status=$3 OR enrollment_state=$3) ORDER BY name LIMIT $1`, query.Limit, query.Node, query.State)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, unavailable()
		}
		items = append(items, node)
	}
	return items, rows.Err()
}

func (s *Service) inventoryApps(ctx context.Context, query platform.InventoryQuery) ([]platform.App, error) {
	items := []platform.App{}
	rows, err := s.pool.Query(ctx, `SELECT id,name,created_at,updated_at FROM eregion_platform_apps WHERE ($2='' OR name=$2) ORDER BY name LIMIT $1`, query.Limit, query.App)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		var app platform.App
		if err := rows.Scan(&app.ID, &app.Name, &app.CreatedAt, &app.UpdatedAt); err != nil {
			return nil, unavailable()
		}
		items = append(items, app)
	}
	return items, rows.Err()
}

const environmentSelect = `SELECT e.id,a.name,e.name,e.node_name,e.protected,coalesce(e.build_node,''),e.host_port,e.candidate_port,e.container_port,
    e.bind_address,e.health_type,e.health_path,e.health_timeout_seconds,e.env,e.secret_refs,e.secret_revision,
    coalesce(e.desired_release_id,''),coalesce(e.applied_release_id,''),coalesce(e.serving_release_id,''),coalesce(e.serving_deployment_id,''),
    e.health_state,e.health_detail,e.health_checked_at,
    coalesce((SELECT array_agg(d.name ORDER BY d.name) FROM eregion_platform_domains d WHERE d.environment_id=e.id),'{}'),
    coalesce((SELECT d.name FROM eregion_platform_domains d WHERE d.environment_id=e.id AND d.is_primary ORDER BY d.created_at,d.name LIMIT 1),''),
    coalesce((SELECT d.dns_state FROM eregion_platform_domains d WHERE d.environment_id=e.id AND d.is_primary ORDER BY d.created_at,d.name LIMIT 1),''),
    coalesce((SELECT array_agg(c.container_name ORDER BY c.container_name)
              FROM eregion_platform_container_cleanup c WHERE c.environment_id=e.id),'{}'),
    EXISTS(SELECT 1 FROM eregion_platform_releases rr WHERE rr.environment_id=e.id AND rr.status='ready'),
    coalesce(e.current_revision_id,''),coalesce(r.id,''),coalesce(r.number,0),coalesce(r.operation_id,''),
    coalesce(r.actor_kind,'system'),coalesce(r.actor_subject,''),coalesce(r.operation,''),coalesce(r.summary,''),
    coalesce(r.source,'{}'::jsonb),coalesce(r.auto_deploy,false),coalesce(r.webhook_secret_name,''),
    coalesce(r.runtime,'{}'::jsonb),coalesce(r.processes,'[]'::jsonb),coalesce(r.config,'{}'::jsonb),
    coalesce(r.secret_names,'{}'),coalesce(r.resources,'{}'),coalesce(r.retention,'{}'::jsonb),coalesce(r.deploy_intent,false),r.created_at,
    e.created_at,e.updated_at
    FROM eregion_platform_environments e
    JOIN eregion_platform_apps a ON a.id=e.app_id
    LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id AND r.environment_id=e.id`

func scanEnvironmentRow(row pgx.Row) (platform.Environment, error) {
	var env platform.Environment
	var rawEnv, rawRefs, rawSource, rawRuntime, rawProcesses, rawConfig, rawRetention []byte
	var desired, applied, serving, servingDeployment, revisionID, loadedRevisionID string
	var revisionNumber int
	var operationID, actorKind, actorSubject, operation, summary, webhookSecretName, primaryDNS string
	var autoDeploy, deployIntent bool
	var secretNames, resourceNames []string
	var revisionCreated *time.Time
	err := row.Scan(&env.ID, &env.App, &env.Name, &env.Node, &env.Protected, &env.BuildNode, &env.HostPort, &env.CandidatePort, &env.ContainerPort,
		&env.Bind, &env.Health.Type, &env.Health.Path, &env.Health.TimeoutSeconds, &rawEnv, &rawRefs, &env.SecretRevision,
		&desired, &applied, &serving, &servingDeployment, &env.HealthState, &env.HealthDetail, &env.HealthCheckedAt, &env.Domains,
		&env.PrimaryDomain, &primaryDNS, &env.PendingCleanup, &env.ArtifactReady, &revisionID, &loadedRevisionID, &revisionNumber, &operationID,
		&actorKind, &actorSubject, &operation, &summary, &rawSource, &autoDeploy, &webhookSecretName,
		&rawRuntime, &rawProcesses, &rawConfig, &secretNames, &resourceNames, &rawRetention, &deployIntent, &revisionCreated,
		&env.CreatedAt, &env.UpdatedAt)
	if err != nil {
		return env, err
	}
	if revisionID != loadedRevisionID {
		return env, contract.NewFault(contract.PreconditionFailed, "environment current revision is missing or inconsistent", "Inspect the environment revision before retrying.")
	}
	env.ServingDeploymentID = platform.DeploymentID(servingDeployment)
	env.Env = map[string]string{}
	env.SecretRefs = map[string]string{}
	if len(rawEnv) > 0 {
		if err := json.Unmarshal(rawEnv, &env.Env); err != nil {
			return env, err
		}
	}
	if len(rawRefs) > 0 {
		if err := json.Unmarshal(rawRefs, &env.SecretRefs); err != nil {
			return env, err
		}
	}
	env.DesiredRelease, env.AppliedRelease, env.ServingRelease = platform.ReleaseID(desired), platform.ReleaseID(applied), platform.ReleaseID(serving)
	if revisionID != "" {
		revision := &platform.EnvironmentRevision{
			ID: platform.RevisionID(revisionID), App: env.App, Environment: env.Name, Number: revisionNumber,
			OperationID: contract.OperationID(operationID),
			Actor:       contract.Actor{Kind: actorKind, Subject: actorSubject},
			Operation:   operation, Summary: summary,
		}
		if len(rawSource) > 0 {
			if err := json.Unmarshal(rawSource, &revision.Source); err != nil {
				return env, err
			}
		}
		revision.AutoDeploy = autoDeploy
		revision.WebhookSecretName = webhookSecretName
		if len(rawRuntime) > 0 {
			if err := json.Unmarshal(rawRuntime, &revision.Runtime); err != nil {
				return env, err
			}
		}
		if len(rawProcesses) > 0 {
			if err := json.Unmarshal(rawProcesses, &revision.Processes); err != nil {
				return env, err
			}
		}
		revision.Config = map[string]string{}
		if len(rawConfig) > 0 {
			if err := json.Unmarshal(rawConfig, &revision.Config); err != nil {
				return env, err
			}
		}
		revision.SecretNames = secretNames
		revision.Resources = resourceNames
		if len(rawRetention) > 0 {
			if err := json.Unmarshal(rawRetention, &revision.Retention); err != nil {
				return env, err
			}
		}
		revision.DeployIntent = deployIntent
		if revisionCreated != nil {
			revision.CreatedAt = *revisionCreated
		}
		if revision.Processes == nil {
			revision.Processes = []platform.ProcessSpec{}
		}
		if revision.SecretNames == nil {
			revision.SecretNames = []string{}
		}
		if revision.Resources == nil {
			revision.Resources = []string{}
		}
		env.RevisionID = revision.ID
		env.Revision = revision
		env.Processes = revision.Processes
	}
	if env.Processes == nil {
		env.Processes = []platform.ProcessSpec{}
	}
	env.Deployability = platform.EvaluateDeployability(env.Revision, platform.DeployabilityState{
		ArtifactReady: env.ArtifactReady, PrimaryDomain: env.PrimaryDomain, DNS: primaryDNS,
	})
	return env, nil
}

func (s *Service) inventoryEnvironments(ctx context.Context, query platform.InventoryQuery) ([]platform.Environment, error) {
	items := []platform.Environment{}
	statement := environmentSelect + " WHERE 1=1"
	args := []any{}
	if query.App != "" {
		args = append(args, query.App)
		statement += fmt.Sprintf(" AND a.name=$%d", len(args))
	}
	if query.Environment != "" {
		args = append(args, query.Environment)
		statement += fmt.Sprintf(" AND e.name=$%d", len(args))
	}
	if query.Node != "" {
		args = append(args, query.Node)
		statement += fmt.Sprintf(" AND e.node_name=$%d", len(args))
	}
	if query.State != "" {
		args = append(args, query.State)
		statement += fmt.Sprintf(" AND e.health_state=$%d", len(args))
	}
	args = append(args, query.Limit)
	statement += fmt.Sprintf(" ORDER BY a.name,e.name LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		env, err := scanEnvironmentRow(rows)
		if err != nil {
			return nil, unavailable()
		}
		missing, err := s.resourceDeployability(ctx, env)
		if err != nil {
			return nil, err
		}
		env.Deployability.Missing = append(env.Deployability.Missing, missing...)
		env.Deployability.Deployable = len(env.Deployability.Missing) == 0
		items = append(items, env)
	}
	return items, rows.Err()
}

func (s *Service) inventoryDomains(ctx context.Context, query platform.InventoryQuery) ([]platform.DomainBinding, error) {
	items := []platform.DomainBinding{}
	statement := `SELECT d.id,d.name,a.name,e.name,d.is_primary,coalesce(d.check_path,''),d.desired_revision,d.applied_revision,
        d.route_state,d.route_error,d.route_applied_at,d.route_observed_at,
        d.expected_addresses,d.dns_state,d.dns_addresses,d.dns_error,d.dns_observed_at,
        d.certificate_state,d.certificate_not_after,d.certificate_error,d.certificate_observed_at,
        d.public_state,coalesce(d.public_status,0),d.public_error,d.public_observed_at,d.observed_at,
        coalesce(dep.instances,'[]'::jsonb),
        coalesce(x.provider,''),coalesce(x.zone_id,''),coalesce(x.zone_name,''),coalesce(x.desired_origins,'{}'),
        coalesce(x.desired_proxied,false),coalesce(x.state,''),coalesce(x.generation,0),coalesce(x.applied_generation,0)
        FROM eregion_platform_domains d
        JOIN eregion_platform_environments e ON e.id=d.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        LEFT JOIN eregion_platform_deployments dep ON dep.id=e.serving_deployment_id
        LEFT JOIN eregion_platform_domain_dns x ON x.domain_id=d.id
        WHERE ($1='' OR a.name=$1) AND ($2='' OR e.name=$2)
        ORDER BY a.name,e.name,d.is_primary DESC,d.name LIMIT $3`
	rows, err := s.pool.Query(ctx, statement, query.App, query.Environment, query.Limit)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		var item platform.DomainBinding
		var rawInstances []byte
		var provider, managedZoneID, managedZoneName, managedState string
		var managedOrigins []string
		var managedProxied bool
		var managedGeneration, managedAppliedGeneration int64
		if err := rows.Scan(&item.ID, &item.Domain, &item.App, &item.Environment, &item.Primary, &item.CheckPath,
			&item.DesiredRevision, &item.AppliedRevision, &item.RouteState, &item.RouteError,
			&item.RouteAppliedAt, &item.RouteObservedAt, &item.ExpectedAddresses,
			&item.DNSState, &item.DNSAddresses, &item.DNSError, &item.DNSObservedAt,
			&item.CertificateState, &item.CertificateNotAfter, &item.CertificateError, &item.CertificateObservedAt,
			&item.PublicState, &item.PublicStatus, &item.PublicError, &item.PublicObservedAt, &item.ObservedAt,
			&rawInstances,
			&provider, &managedZoneID, &managedZoneName, &managedOrigins, &managedProxied, &managedState,
			&managedGeneration, &managedAppliedGeneration); err != nil {
			return nil, unavailable()
		}
		if provider != "" {
			item.ManagedDNS = &platform.ManagedDNSBinding{
				Provider: provider, ZoneID: managedZoneID, ZoneName: managedZoneName,
				Origins: managedOrigins, Proxied: managedProxied, State: managedState,
				Generation: managedGeneration, AppliedGeneration: managedAppliedGeneration,
			}
		}
		if item.ExpectedAddresses == nil {
			item.ExpectedAddresses = []string{}
		}
		if item.DNSAddresses == nil {
			item.DNSAddresses = []string{}
		}
		if len(rawInstances) > 0 {
			var instances []platform.DeploymentInstance
			if err := json.Unmarshal(rawInstances, &instances); err == nil {
				item.Upstreams = platform.WebUpstreams(instances)
			}
		}
		if item.Upstreams == nil {
			item.Upstreams = []string{}
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

const releaseSelect = `SELECT r.id,r.operation_id,a.name,e.name,r.image,r.image_id,r.source,r.build,r.command,r.status,
    r.error_code,r.error,r.created_at,r.completed_at,coalesce(r.build_id,''),r.commit_sha,r.pruned_at
    FROM eregion_platform_releases r
    JOIN eregion_platform_apps a ON a.id=r.app_id
    JOIN eregion_platform_environments e ON e.id=r.environment_id`

func scanReleaseRow(row pgx.Row) (platform.Release, error) {
	var release platform.Release
	var source, build, command []byte
	var buildID string
	err := row.Scan(&release.ID, &release.OperationID, &release.App, &release.Environment, &release.Image, &release.ImageID,
		&source, &build, &command, &release.Status, &release.ErrorCode, &release.Error, &release.CreatedAt, &release.CompletedAt,
		&buildID, &release.CommitSHA, &release.PrunedAt)
	if err != nil {
		return release, err
	}
	release.BuildID = platform.BuildID(buildID)
	if len(source) > 0 {
		if err := json.Unmarshal(source, &release.Source); err != nil {
			return release, err
		}
	}
	if len(build) > 0 {
		var value platform.Build
		if err := json.Unmarshal(build, &value); err != nil {
			return release, err
		}
		release.Build = &value
	}
	if len(command) > 0 {
		var value platform.CommandSpec
		if err := json.Unmarshal(command, &value); err != nil {
			return release, err
		}
		release.Command = &value
	}
	return release, nil
}

func (s *Service) inventoryReleases(ctx context.Context, query platform.InventoryQuery) ([]platform.Release, error) {
	items := []platform.Release{}
	statement := releaseSelect + " WHERE 1=1"
	args := []any{}
	if query.App != "" {
		args = append(args, query.App)
		statement += fmt.Sprintf(" AND a.name=$%d", len(args))
	}
	if query.Environment != "" {
		args = append(args, query.Environment)
		statement += fmt.Sprintf(" AND e.name=$%d", len(args))
	}
	if query.State != "" {
		args = append(args, query.State)
		statement += fmt.Sprintf(" AND r.status=$%d", len(args))
	}
	args = append(args, query.Limit)
	statement += fmt.Sprintf(" ORDER BY r.created_at DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		release, err := scanReleaseRow(rows)
		if err != nil {
			return nil, unavailable()
		}
		items = append(items, release)
	}
	return items, rows.Err()
}

const buildSelect = `SELECT b.id,b.operation_id,a.name,e.name,coalesce(b.revision_id,''),coalesce(b.release_id,''),
    b.source,b.commit_sha,b.state,b.error_code,b.error,b.created_at,b.started_at,b.completed_at
    FROM eregion_platform_builds b
    JOIN eregion_platform_apps a ON a.id=b.app_id
    JOIN eregion_platform_environments e ON e.id=b.environment_id`

func scanBuildRow(row pgx.Row) (platform.BuildRecord, error) {
	var build platform.BuildRecord
	var revisionID, releaseID string
	var source []byte
	err := row.Scan(&build.ID, &build.OperationID, &build.App, &build.Environment,
		&revisionID, &releaseID, &source, &build.CommitSHA, &build.State,
		&build.ErrorCode, &build.Error, &build.CreatedAt, &build.StartedAt, &build.CompletedAt)
	build.RevisionID = platform.RevisionID(revisionID)
	build.ReleaseID = platform.ReleaseID(releaseID)
	if len(source) > 0 {
		if err := json.Unmarshal(source, &build.Source); err != nil {
			return build, err
		}
	}
	return build, err
}

func (s *Service) inventoryBuilds(ctx context.Context, query platform.InventoryQuery) ([]platform.BuildRecord, error) {
	items := []platform.BuildRecord{}
	statement := buildSelect + " WHERE 1=1"
	args := []any{}
	if query.App != "" {
		args = append(args, query.App)
		statement += fmt.Sprintf(" AND a.name=$%d", len(args))
	}
	if query.Environment != "" {
		args = append(args, query.Environment)
		statement += fmt.Sprintf(" AND e.name=$%d", len(args))
	}
	if query.State != "" {
		args = append(args, query.State)
		statement += fmt.Sprintf(" AND b.state=$%d", len(args))
	}
	if query.Build != "" {
		args = append(args, query.Build)
		statement += fmt.Sprintf(" AND b.id=$%d", len(args))
	}
	args = append(args, query.Limit)
	statement += fmt.Sprintf(" ORDER BY b.created_at DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		build, err := scanBuildRow(rows)
		if err != nil {
			return nil, unavailable()
		}
		items = append(items, build)
	}
	return items, rows.Err()
}

func (s *Service) buildByID(ctx context.Context, id platform.BuildID) (platform.BuildRecord, error) {
	build, err := scanBuildRow(s.pool.QueryRow(ctx, buildSelect+" WHERE b.id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return build, contract.NewFault(contract.NotFound, "build not found", "Use a build ID from inventory.")
	}
	if err != nil {
		return build, unavailable()
	}
	return build, nil
}

func (s *Service) buildByOperation(ctx context.Context, id contract.OperationID) (platform.BuildRecord, bool, error) {
	build, err := scanBuildRow(s.pool.QueryRow(ctx, buildSelect+" WHERE b.operation_id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return build, false, nil
	}
	if err != nil {
		return build, false, unavailable()
	}
	return build, true, nil
}

const deploymentSelect = `SELECT d.id,d.operation_id,a.name,e.name,d.action,d.release_id,
    coalesce(d.previous_release_id,''),d.state,d.error_code,d.message,d.created_at,d.updated_at,d.completed_at,
    coalesce(d.revision_id,''),d.secret_revisions,coalesce(d.previous_deployment_id,''),d.instances
    FROM eregion_platform_deployments d
    JOIN eregion_platform_apps a ON a.id=d.app_id
    JOIN eregion_platform_environments e ON e.id=d.environment_id`

func scanDeploymentRow(row pgx.Row) (platform.Deployment, error) {
	var dep platform.Deployment
	var previous, revisionID, previousDeployment string
	var secretRevisions, instances []byte
	err := row.Scan(&dep.ID, &dep.OperationID, &dep.App, &dep.Environment, &dep.Action, &dep.ReleaseID,
		&previous, &dep.State, &dep.ErrorCode, &dep.Message, &dep.CreatedAt, &dep.UpdatedAt, &dep.CompletedAt,
		&revisionID, &secretRevisions, &previousDeployment, &instances)
	dep.PreviousReleaseID = platform.ReleaseID(previous)
	dep.RevisionID = platform.RevisionID(revisionID)
	dep.PreviousDeploymentID = platform.DeploymentID(previousDeployment)
	dep.SecretRevisions = map[string]platform.SecretRevisionRef{}
	if len(secretRevisions) > 0 {
		if err := json.Unmarshal(secretRevisions, &dep.SecretRevisions); err != nil {
			return dep, err
		}
	}
	dep.Instances = []platform.DeploymentInstance{}
	if len(instances) > 0 {
		if err := json.Unmarshal(instances, &dep.Instances); err != nil {
			return dep, err
		}
	}
	return dep, err
}

func (s *Service) inventoryDeployments(ctx context.Context, query platform.InventoryQuery) ([]platform.Deployment, error) {
	items := []platform.Deployment{}
	statement := deploymentSelect + " WHERE 1=1"
	args := []any{}
	if query.Deployment != "" {
		if err := platform.DeploymentID(query.Deployment).Validate(); err != nil {
			return nil, err
		}
		args = append(args, query.Deployment)
		statement += fmt.Sprintf(" AND d.id=$%d", len(args))
	}
	if query.App != "" {
		args = append(args, query.App)
		statement += fmt.Sprintf(" AND a.name=$%d", len(args))
	}
	if query.Environment != "" {
		args = append(args, query.Environment)
		statement += fmt.Sprintf(" AND e.name=$%d", len(args))
	}
	if query.State != "" {
		args = append(args, query.State)
		statement += fmt.Sprintf(" AND d.state=$%d", len(args))
	}
	args = append(args, query.Limit)
	statement += fmt.Sprintf(" ORDER BY d.created_at DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		dep, err := scanDeploymentRow(rows)
		if err != nil {
			return nil, unavailable()
		}
		items = append(items, dep)
	}
	return items, rows.Err()
}

func (s *Service) inventoryChanges(ctx context.Context, query platform.InventoryQuery) ([]platform.Change, error) {
	items := []platform.Change{}
	statement := `SELECT c.id,a.name,coalesce(e.name,''),c.sequence,c.kind,c.operation_id,c.actor_kind,c.actor_subject,c.summary,c.desired,c.applied,c.created_at
        FROM eregion_platform_changes c
        JOIN eregion_platform_apps a ON a.id=c.app_id
        LEFT JOIN eregion_platform_environments e ON e.id=c.environment_id WHERE 1=1`
	args := []any{}
	if query.App != "" {
		args = append(args, query.App)
		statement += fmt.Sprintf(" AND a.name=$%d", len(args))
	}
	if query.Environment != "" {
		args = append(args, query.Environment)
		statement += fmt.Sprintf(" AND e.name=$%d", len(args))
	}
	if query.State != "" {
		args = append(args, query.State)
		statement += fmt.Sprintf(" AND c.kind=$%d", len(args))
	}
	args = append(args, query.Limit)
	statement += fmt.Sprintf(" ORDER BY c.created_at DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		var change platform.Change
		var desired, applied []byte
		if err := rows.Scan(&change.ID, &change.App, &change.Environment, &change.Sequence, &change.Kind,
			&change.OperationID, &change.Actor.Kind, &change.Actor.Subject, &change.Summary, &desired, &applied, &change.CreatedAt); err != nil {
			return nil, unavailable()
		}
		if len(desired) > 0 {
			_ = json.Unmarshal(desired, &change.Desired)
		}
		if len(applied) > 0 {
			_ = json.Unmarshal(applied, &change.Applied)
		}
		items = append(items, change)
	}
	return items, rows.Err()
}

// previousRollbackRelease chooses the release that served before the current
// serving release became active. AppliedRelease is not sufficient: it normally
// equals the current serving release. A latest successful deployment's
// previous_release_id is the authoritative one-step history.
func previousRollbackRelease(ctx context.Context, tx pgx.Tx, env platform.Environment) (platform.ReleaseID, error) {
	if env.ServingRelease != "" {
		var previous string
		err := tx.QueryRow(ctx, `SELECT coalesce(previous_release_id,'') FROM eregion_platform_deployments
            WHERE environment_id=$1 AND state='succeeded' AND release_id=$2 AND previous_release_id IS NOT NULL
            ORDER BY completed_at DESC NULLS LAST, created_at DESC, id DESC LIMIT 1`, env.ID, env.ServingRelease).Scan(&previous)
		if err == nil && previous != "" {
			return platform.ReleaseID(previous), nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", unavailable()
		}
	}
	var previous string
	err := tx.QueryRow(ctx, `SELECT release_id FROM eregion_platform_deployments
        WHERE environment_id=$1 AND state='succeeded' AND release_id IS DISTINCT FROM $2
        ORDER BY completed_at DESC NULLS LAST, created_at DESC, id DESC LIMIT 1`, env.ID, nullableReleaseID(env.ServingRelease)).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", unavailable()
	}
	return platform.ReleaseID(previous), nil
}

func (s *Service) previousRollbackRelease(ctx context.Context, env platform.Environment) (platform.ReleaseID, error) {
	if env.ServingRelease != "" {
		var previous string
		err := s.pool.QueryRow(ctx, `SELECT coalesce(previous_release_id,'') FROM eregion_platform_deployments
            WHERE environment_id=$1 AND state='succeeded' AND release_id=$2 AND previous_release_id IS NOT NULL
            ORDER BY completed_at DESC NULLS LAST, created_at DESC, id DESC LIMIT 1`, env.ID, env.ServingRelease).Scan(&previous)
		if err == nil && previous != "" {
			return platform.ReleaseID(previous), nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", unavailable()
		}
	}
	var previous string
	err := s.pool.QueryRow(ctx, `SELECT release_id FROM eregion_platform_deployments
        WHERE environment_id=$1 AND state='succeeded' AND release_id IS DISTINCT FROM $2
        ORDER BY completed_at DESC NULLS LAST, created_at DESC, id DESC LIMIT 1`, env.ID, nullableReleaseID(env.ServingRelease)).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", unavailable()
	}
	return platform.ReleaseID(previous), nil
}

// --------------------------------------------------------------- tx lookups

func appByNameTx(ctx context.Context, tx pgx.Tx, name string) (platform.App, error) {
	return appByNameQueryTx(ctx, tx, name, false)
}

// appByNameForUpdateTx serializes environment creation and deletion against
// the application row, so "the last environment" is decided against a stable
// set and an application deletion cannot race a sibling environment creation.
func appByNameForUpdateTx(ctx context.Context, tx pgx.Tx, name string) (platform.App, error) {
	return appByNameQueryTx(ctx, tx, name, true)
}

func appByNameQueryTx(ctx context.Context, tx pgx.Tx, name string, forUpdate bool) (platform.App, error) {
	var app platform.App
	statement := `SELECT id,name,created_at,updated_at FROM eregion_platform_apps WHERE name=$1`
	if forUpdate {
		statement += " FOR UPDATE"
	}
	err := tx.QueryRow(ctx, statement, name).
		Scan(&app.ID, &app.Name, &app.CreatedAt, &app.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return app, contract.NewFault(contract.NotFound, "application not found", "Create the application first.")
	}
	if err != nil {
		return app, unavailable()
	}
	return app, nil
}

func nodeByNameTx(ctx context.Context, tx pgx.Tx, name string) (platform.Node, error) {
	return loadNode(ctx, tx, name)
}

func environmentByNameTx(ctx context.Context, tx pgx.Tx, app, name string) (platform.Environment, error) {
	var id platform.EnvironmentID
	err := tx.QueryRow(ctx, `SELECT e.id FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1 AND e.name=$2 FOR UPDATE OF e`, app, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.Environment{}, contract.NewFault(contract.NotFound, "environment not found", "Create the environment under this app first.")
	}
	if err != nil {
		return platform.Environment{}, unavailable(err)
	}
	return loadLockedEnvironmentTx(ctx, tx, id)
}

// environmentByIDTx locks and reads the exact environment row an admission
// observed. The opaque ID, not the app/name pair, proves the environment was not
// deleted and recreated while the push waited in the queue.
func environmentByIDTx(ctx context.Context, tx pgx.Tx, id platform.EnvironmentID) (platform.Environment, error) {
	var lockedID platform.EnvironmentID
	err := tx.QueryRow(ctx, `SELECT id FROM eregion_platform_environments WHERE id=$1 FOR UPDATE`, id).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.Environment{}, contract.NewFault(contract.NotFound, "environment not found", "The environment was deleted after the push was admitted.")
	}
	if err != nil {
		return platform.Environment{}, unavailable(err)
	}
	return loadLockedEnvironmentTx(ctx, tx, lockedID)
}

// A separate statement gets a fresh READ COMMITTED snapshot after the row lock.
func loadLockedEnvironmentTx(ctx context.Context, tx pgx.Tx, id platform.EnvironmentID) (platform.Environment, error) {
	env, err := scanEnvironmentRow(tx.QueryRow(ctx, environmentSelect+" WHERE e.id=$1", id))
	if err != nil {
		return env, err
	}
	if env.Revision == nil || env.RevisionID == "" || env.Revision.ID != env.RevisionID {
		return env, contract.NewFault(contract.PreconditionFailed, "environment current revision is missing or inconsistent", "Inspect the environment revision before retrying.")
	}
	return env, nil
}

func releaseByIDTx(ctx context.Context, tx pgx.Tx, id platform.ReleaseID) (platform.Release, error) {
	release, err := scanReleaseRow(tx.QueryRow(ctx, releaseSelect+" WHERE r.id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return release, contract.NewFault(contract.NotFound, "release not found", "Use a release ID from inventory.")
	}
	if err != nil {
		return release, unavailable()
	}
	return release, nil
}

// ---------------------------------------------------------------- dry run

// Preview implements the optional dry-run planner used by the API. When the
// actor already submitted this kind and target with idempotencyKey, the
// preview reports the original operation and skips every conflict check: a
// retry must reach the same submission without repeating its side effects.
func (s *Service) Preview(ctx context.Context, actor contract.Actor, normalized platform.Normalized, idempotencyKey string) (platform.Preview, error) {
	if err := actor.Validate(); err != nil {
		return platform.Preview{}, err
	}
	preview := platform.Preview{Kind: normalized.Kind, Target: normalized.Target.String(), DryRun: true, Input: normalized.Input.Preview(), Summary: normalized.Summary}
	if idempotencyKey != "" {
		// Preview must use the same normalized-input hash fence as submit: an
		// existing key with identical input is a replay, while the same key
		// with changed input is a conflict and must not hide behind a preview.
		if op, found, err := s.replaySubmission(ctx, normalized.Kind, normalized.Target.String(), actor, idempotencyKey, hashInput(normalized.Canonical)); err != nil {
			return preview, err
		} else if found {
			preview.Replay, preview.Operation = true, &op
			preview.Summary = "Replay the accepted operation " + string(op.ID) + "; the same submission already exists."
			return preview, nil
		}
	}
	switch input := normalized.Input.(type) {
	case *platform.DeployInput:
		if input.ReleaseID == "" {
			env, err := s.environment(ctx, normalized.Target.App, normalized.Target.Environment)
			if err != nil {
				return preview, err
			}
			if env.Revision == nil {
				return preview, contract.NewFault(contract.PreconditionFailed, "environment has no current revision", "Save an environment revision before deploying.")
			}
			preview.Steps = deploymentSteps()
			preview.Summary = fmt.Sprintf("Build %s from %s source, then deploy it to %s/%s with a blue/green switch.", env.App, env.Revision.Source.Kind, env.App, env.Name)
			if env.ServingRelease != "" {
				preview.Warnings = append(preview.Warnings, "The serving release stays until the candidate passes health and the public route check.")
			}
			break
		}
		env, release, err := s.planDeployment(ctx, normalized.Target, input.ReleaseID)
		if err != nil {
			return preview, err
		}
		preview.Steps = deploymentSteps()
		preview.Summary = fmt.Sprintf("Deploy %s to %s/%s with the current revision after candidate health and public verification.", release.Image, env.App, env.Name)
		addReleaseWarnings(&preview, release, env)
	case *platform.PromoteInput:
		return s.previewPromotion(ctx, normalized, input)
	case *platform.BuildStopInput:
		build, err := s.buildByID(ctx, input.BuildID)
		if err != nil {
			return preview, err
		}
		preview.Steps = []string{"mark the build cancelled", "mark the build operation cancelling", "signal the running pack process"}
		preview.Summary = fmt.Sprintf("Stop build %s for %s/%s.", build.ID, build.App, build.Environment)
	case *platform.BuildRetryInput:
		build, err := s.buildByID(ctx, input.BuildID)
		if err != nil {
			return preview, err
		}
		preview.Steps = []string{"record a new build of the same commit", "pack build", "verify image digest"}
		preview.Summary = fmt.Sprintf("Retry build %s at commit %s as a new build; it does not deploy.", build.ID, build.CommitSHA)
	case *platform.RestartInput:
		env, err := s.environment(ctx, normalized.Target.App, normalized.Target.Environment)
		if err != nil {
			return preview, err
		}
		if env.ServingRelease == "" {
			return preview, contract.NewFault(contract.PreconditionFailed, "no serving deployment is available to restart", "Deploy a release before restarting the environment.")
		}
		preview.Steps = deploymentSteps()
		preview.Summary = fmt.Sprintf("Reapply the serving release %s to %s/%s with the current revision.", env.ServingRelease, env.App, env.Name)
	case *platform.RollbackInput:
		target := input.ToReleaseID
		env, err := s.environment(ctx, normalized.Target.App, normalized.Target.Environment)
		if err != nil {
			return preview, err
		}
		if target == "" {
			target, err = s.previousRollbackRelease(ctx, env)
			if err != nil {
				return preview, err
			}
		}
		if target == "" {
			return preview, contract.NewFault(contract.PreconditionFailed, "environment has no previous release", "Deploy a release before rolling back, or provide to_release_id explicitly.")
		}
		_, release, err := s.planDeployment(ctx, normalized.Target, target)
		if err != nil {
			return preview, err
		}
		preview.Steps = deploymentSteps()
		preview.Summary = fmt.Sprintf("Roll back %s/%s to %s using the same health-first path.", env.App, env.Name, release.Image)
		addReleaseWarnings(&preview, release, env)
	case *platform.AppCreateInput:
		if err := s.checkPreviewAppAvailable(ctx, input.Name); err != nil {
			return preview, err
		}
		if len(input.Environments) == 0 {
			preview.Summary = "Create application " + input.Name + " without an environment."
			break
		}
		preview.Steps = []string{"record application"}
		preview.Summary = "Create application " + input.Name + " with environments"
		domains := map[string]string{}
		for index := range input.Environments {
			spec := &input.Environments[index]
			if err := s.checkPreviewPlacement(ctx, spec.Node, spec.BuildNode); err != nil {
				return preview, err
			}
			if index == 0 {
				if err := checkPreviewDeploySource(spec, input.Deploy); err != nil {
					return preview, err
				}
			}
			for _, name := range environmentSpecDomains(spec) {
				if other, exists := domains[name]; exists {
					return preview, contract.NewFault(contract.Conflict,
						name+" is declared for both "+other+" and "+spec.Name,
						"Each hostname can route to one environment; put distinct domains in each environment override.")
				}
				domains[name] = spec.Name
				if err := s.checkPreviewDomainAvailable(ctx, name); err != nil {
					return preview, err
				}
			}
			preview.Steps = append(preview.Steps, "record environment "+spec.Name+" and its first revision", "allocate ports for "+spec.Name)
			preview.Summary += " " + spec.Name
			if spec.Domain != "" {
				preview.Steps = append(preview.Steps, "record the primary domain binding for "+spec.Name+" before any first deploy")
			}
			if spec.BuildNode != "" && spec.BuildNode != spec.Node && spec.Source.Kind == "git" {
				preview.Warnings = append(preview.Warnings, "A distinct build node needs a shared registry to move the image to the application node; release.create refuses that path until registry transfer is configured.")
			}
		}
		preview.Summary += "."
		if input.Deploy {
			preview.Steps = append(preview.Steps, "queue the first deploy from the first environment's new revision")
			preview.Summary += " A first deploy is queued from " + input.Environments[0].Name + "'s new revision."
		}
	case *platform.EnvironmentProtectInput:
		env, err := s.environment(ctx, normalized.Target.App, normalized.Target.Environment)
		if err != nil {
			return preview, err
		}
		state := "protected"
		if !input.Protected {
			state = "unprotected"
		}
		preview.Steps = []string{"record the environment protection state"}
		preview.Summary = "Set " + input.App + "/" + input.Environment + " to " + state + "."
		if platform.ProtectionChangeRequiresHumanFactor(env.Protected, input.Protected) {
			preview.Warnings = append(preview.Warnings, "Lowering protection requires the D7 TOTP human factor at a human terminal.")
		}
	case *platform.EnvironmentCreateInput:
		if err := s.checkPreviewApp(ctx, input.App); err != nil {
			return preview, err
		}
		if err := s.checkPreviewEnvironmentAvailable(ctx, input.App, input.Name); err != nil {
			return preview, err
		}
		if err := s.checkPreviewPlacement(ctx, input.Node, input.BuildNode); err != nil {
			return preview, err
		}
		if err := checkPreviewDeploySource(&input.EnvironmentSpec, input.Deploy); err != nil {
			return preview, err
		}
		if err := s.checkPreviewDomainAvailable(ctx, input.Domain); err != nil {
			return preview, err
		}
		preview.Steps = []string{"record environment", "record first desired-state revision", "allocate stable and candidate ports"}
		preview.Summary = "Create environment " + input.Name + " on node " + input.Node + "."
		if input.Domain != "" {
			preview.Steps = append(preview.Steps, "record the primary domain binding before any first deploy")
			preview.Summary += " Domain " + input.Domain + " is bound in the same submission."
		}
		if input.BuildNode != "" {
			preview.Summary += " Builds run on builder node " + input.BuildNode + "."
			if input.BuildNode != input.Node && input.Source.Kind == "git" {
				preview.Warnings = append(preview.Warnings, "A distinct build node needs a shared registry to move the image to the application node; release.create refuses that path until registry transfer is configured.")
			}
		}
		if input.Deploy {
			preview.Steps = append(preview.Steps, "queue the first deploy from the new revision")
			preview.Summary += " A first deploy is queued from the new revision."
		}
	case *platform.EnvironmentUpdateInput:
		env, err := s.environment(ctx, normalized.Target.App, normalized.Target.Environment)
		if err != nil {
			return preview, err
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
			if err := s.checkPreviewNode(ctx, nodeName, platform.RoleApplication, platform.CapabilityDocker); err != nil {
				return preview, err
			}
		}
		if input.BuildNode != nil && buildNode != "" {
			if err := s.checkPreviewNode(ctx, buildNode, platform.RoleBuilder, platform.CapabilityBuildpacks); err != nil {
				return preview, err
			}
		}
		preview.Steps = []string{"load current revision", "merge the patch", "append a new revision"}
		preview.Summary = "Update environment " + input.Environment + " desired state as a new immutable revision; no deploy is queued."
	case *platform.ProcessScaleInput:
		preview.Steps = []string{"load current revision", "change replicas", "append a new revision"}
		preview.Summary = "Scale process " + input.Process + " to " + fmt.Sprint(input.Replicas) + " replica(s) in a new revision; a serving environment also restarts its release through the blue/green path."
	case *platform.ConfigSetInput, *platform.ConfigUnsetInput:
		preview.Steps = []string{"load current revision", "change plain configuration", "append a new revision"}
		preview.Summary = "Change plain environment configuration as a new revision; no deploy is queued."
	case *platform.EnvironmentSecretAttachInput:
		if _, err := s.environment(ctx, normalized.Target.App, normalized.Target.Environment); err != nil {
			return preview, err
		}
		preview.Steps = []string{"load current revision", "attach the secret name", "append a new revision"}
		preview.Summary = "Attach vault secret name " + input.Name + " to the environment revision; no value enters the operation."
	case *platform.EnvironmentResourceInput:
		env, err := s.environment(ctx, normalized.Target.App, normalized.Target.Environment)
		if err != nil {
			return preview, err
		}
		if _, err := s.resourceByName(ctx, input.App, input.Environment, input.Resource); err != nil {
			return preview, err
		}
		preview.Steps = []string{"load current revision", normalized.Kind + " " + input.Resource, "append a new revision"}
		if normalized.Kind == platform.KindEnvironmentResourceDetach {
			preview.Summary = "Detach managed resource " + input.Resource + " from " + env.App + "/" + env.Name + "; its container and named volumes are retained and no deploy is queued."
		} else {
			preview.Summary = "Attach managed resource " + input.Resource + " to " + env.App + "/" + env.Name + " as desired state; deployment stays blocked until the resource reports ready."
		}
	case *platform.ReleaseCreateInput:
		preview.Summary = "Record release from " + input.Source.Type + " source."
		if input.Source.Type == "image" {
			preview.Steps = []string{"verify image on node", "mark release ready"}
		} else {
			env, err := s.environment(ctx, normalized.Target.App, normalized.Target.Environment)
			if err != nil {
				return preview, err
			}
			if err := crossNodeBuildUnsupported(env, input.Source.Type); err != nil {
				return preview, err
			}
			preview.Steps = []string{"prepare source", "pack build", "verify image", "mark release ready"}
			preview.Summary = "Build on " + env.BuildNodeName() + ", then mark the release ready."
			preview.Warnings = append(preview.Warnings, "Buildpacks execution requires pack on the environment's builder-capable node.")
		}
	case *platform.NodeCheckInput:
		preview.Steps = []string{"check docker", "check pack"}
		preview.Summary = "Observe runtime availability on node " + input.Node + "."
	case *platform.NodeRegisterInput:
		preview.Summary = "Record placement facts for node " + input.Name + "; this does not install or verify host state."
	case *platform.NodeEnrollInput:
		preview.Steps = []string{"pin SSH host key", "verify node identity", "probe host capabilities", "configure role directories", "record enrollment"}
		preview.Summary = "Enroll node " + input.Node + " over pinned SSH; it receives work only after it proves its identity and the probe satisfies its roles."
		if input.Retry {
			preview.Summary = "Re-run the recorded enrollment of node " + input.Node + "; completed steps are not repeated."
		}
		if input.PrivateNetwork != nil {
			preview.Warnings = append(preview.Warnings, "Private network keys are generated on the node and reconciled by platform.node.reconcile; enrollment itself does not change WireGuard.")
		}
	case *platform.NodeReconcileInput:
		preview.Steps = []string{"converge role directories", "generate node WireGuard key", "render peer configuration", "apply peers"}
		preview.Summary = "Converge host configuration and WireGuard peers on node " + input.Node + "; rerunning changes nothing when the node already matches."
	case *platform.ResourceApplyInput:
		env, err := s.environment(ctx, input.App, input.Environment)
		if err != nil {
			return preview, err
		}
		if err := s.checkPreviewNode(ctx, env.Node, platform.RoleApplication, platform.CapabilityDocker); err != nil {
			return preview, err
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
				return preview, err
			}
			preview.Summary = "Create resource " + input.Name + " for " + input.App + "/" + input.Environment + " from image " + input.Image + " on the environment's private network."
		} else {
			preview.Summary = "Apply resource " + input.Name + " for " + input.App + "/" + input.Environment + " from image " + input.Image + "; the same resource identity and named volumes are retained."
			if existing.Node != env.Node {
				preview.Warnings = append(preview.Warnings, "This resource is bound to node "+existing.Node+" and cannot be moved implicitly; apply would be refused until it is destroyed on that node.")
			}
		}
		preview.Warnings = append(preview.Warnings, "A tag is resolved to an immutable image ID at apply time. The resource is not ready until its exec health check succeeds, and no host port is published.")
		if len(input.SecretEnv) > 0 {
			preview.Warnings = append(preview.Warnings, "Secret values are resolved from the environment vault only inside the worker; this preview and the operation input contain secret names only.")
		}
	case *platform.ResourceDestroyInput:
		resource, err := s.resourceByName(ctx, input.App, input.Environment, input.Name)
		if err != nil {
			return preview, err
		}
		if resource.State == platform.ResourceDestroyed && !input.DeleteData {
			preview.Summary = "Resource " + input.Name + " is already destroyed; applying it again reactivates the same identity and retained volumes, or --delete-data deletes the retained data explicitly."
			preview.Steps = []string{"observe that no container is running"}
			break
		}
		preview.Steps = []string{"verify resource ownership labels", "remove the resource container", "retain named volumes and the resource identity"}
		preview.Summary = "Destroy resource " + input.Name + " for " + input.App + "/" + input.Environment + "; its container is removed and its named volumes are retained."
		preview.Warnings = append(preview.Warnings, "Named volumes are not deleted by a default destroy; use resource destroy --delete-data to remove this resource's owned volumes explicitly.")
		if input.DeleteData {
			preview.Steps = []string{"verify resource ownership labels", "remove the resource container", "verify and remove each resource-scoped named volume"}
			preview.Summary = "Destroy resource " + input.Name + " for " + input.App + "/" + input.Environment + " and permanently delete its named volumes."
			preview.Warnings = []string{"DATA DESTRUCTION: --delete-data permanently removes the resource's named volumes after the container is removed. This cannot be undone and is refused for volumes without this resource's ownership labels."}
		}
	case *platform.AppDeleteInput, *platform.EnvironmentDeleteInput:
		return s.deletionPreview(ctx, normalized)
	case *platform.RetentionPruneInput:
		plan, err := s.RetentionPlan(ctx, input.Node, "", "")
		if err != nil {
			return preview, err
		}
		preview.Steps = retentionPlanSteps(plan)
		preview.Summary = fmt.Sprintf("Prune %d planned item(s), reclaiming %d byte(s) on node %s.", len(retentionRemovalItems(plan.Items)), plan.PlannedBytes, input.Node)
		preview.Warnings = append(preview.Warnings, plan.Warnings...)
		for _, item := range plan.Items {
			if item.Kind == retention.ItemRegistrySkipped {
				preview.Warnings = append(preview.Warnings, "registry skipped: "+item.Reason)
			}
		}
	case *platform.DomainInput:
		if normalized.Kind == platform.KindDomainRemove {
			preview.Summary = "Remove the binding and route for " + input.Domain + " from " + input.App + "/" + input.Environment + "."
		} else {
			preview.Summary = "Bind " + input.Domain + " to " + input.App + "/" + input.Environment + " through Caddy."
		}
		var ledger *domainDNSLedger
		if s.pool != nil {
			loaded, err := s.loadDomainDNSLedger(ctx, input.App, input.Environment, input.Domain)
			if err != nil {
				return preview, err
			}
			ledger = loaded
		}
		if normalized.Kind == platform.KindDomainAdd && input.DNSMode != "cloudflare" && ledger != nil {
			preview.Data = map[string]any{"dns_managed": managedDNSBinding(*ledger)}
			preview.Steps = append(preview.Steps, "preserve the persisted managed-DNS zone, origins and proxy desired state")
			preview.Summary += " Managed DNS remains zone " + ledger.ZoneName + " with origins " + strings.Join(ledger.DesiredOrigins, ", ") + "; this operation does not change provider intent."
			preview.Warnings = append(preview.Warnings, "The binding is already provider-managed; use explicit --dns cloudflare flags to change or reconcile its DNS.")
			break
		}
		managed := input.DNSMode == "cloudflare" || ledger != nil
		if managed {
			previewInput := *input
			previewInput.DNSMode = "cloudflare"
			if ledger != nil && previewInput.DNSZone == "" {
				previewInput.DNSZone = ledger.ZoneID
			}
			if normalized.Kind == platform.KindDomainAdd {
				if previewInput.DNSProxied == nil {
					return preview, contract.NewFault(contract.ValidationFailed, "Cloudflare DNS needs an explicit proxy mode", "Use --dns-proxy dns-only or --dns-proxy proxied; the choice is never inferred.")
				}
				if len(previewInput.DNSOrigins) == 0 {
					return preview, contract.NewFault(contract.ValidationFailed, "Cloudflare DNS needs explicit public origin addresses", "Pass --dns-origin for each public origin address; the WireGuard management endpoint is not used.")
				}
			}
			plan, err := s.previewDomainDNS(ctx, actor, &previewInput, normalized.Kind)
			if err != nil {
				return preview, err
			}
			preview.Data = map[string]any{"dns_provider": plan}
			preview.Steps = append(preview.Steps, "read the Cloudflare provider snapshot for "+input.Domain)
			if normalized.Kind == platform.KindDomainRemove {
				preview.Steps = append(preview.Steps, "remove only records whose comment is this binding's exact ownership marker")
				preview.Summary += " Explicit Cloudflare removal targets only this binding's exact owned records."
			} else {
				preview.Steps = append(preview.Steps, "create or update only the exact A/AAAA records this binding owns")
				preview.Steps = append(preview.Steps, "explicit public origins: "+strings.Join(previewInput.DNSOrigins, ", "))
				preview.Summary += " Explicit Cloudflare DNS will ensure the exact desired record from " + strings.Join(previewInput.DNSOrigins, ", ") + "."
			}
			preview.Warnings = append(preview.Warnings, plan.Warnings...)
			preview.Warnings = append(preview.Warnings, "No provider change happens until this plan is submitted; manual DNS remains the default.")
		} else {
			preview.Summary += " DNS stays manual; no provider change is planned."
		}
	default:
		if preview.Summary == "" {
			preview.Summary = "Execute registered platform action " + normalized.Kind + "."
		}
	}
	return preview, nil
}

// checkPreviewPlacement applies the same node placement facts a create
// submission checks, so a dry run refuses before the CLI writes vault values.
// Transport identity is still rechecked by the worker at dispatch.
func (s *Service) checkPreviewPlacement(ctx context.Context, nodeName, buildNode string) error {
	if err := s.checkPreviewNode(ctx, nodeName, platform.RoleApplication, platform.CapabilityDocker); err != nil {
		return err
	}
	if buildNode != "" {
		if err := s.checkPreviewNode(ctx, buildNode, platform.RoleBuilder, platform.CapabilityBuildpacks); err != nil {
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
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_apps WHERE name=$1)`, name).Scan(&exists); err != nil {
		return unavailable()
	}
	if exists {
		return contract.NewFault(contract.Conflict, "app "+name+" already exists", "Choose another app name, or inspect the existing app.")
	}
	return nil
}

func (s *Service) checkPreviewApp(ctx context.Context, name string) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_apps WHERE name=$1)`, name).Scan(&exists); err != nil {
		return unavailable()
	}
	if !exists {
		return contract.NewFault(contract.NotFound, "app "+name+" was not found", "Create the app first, or choose an existing app.")
	}
	return nil
}

func (s *Service) checkPreviewDomainAvailable(ctx context.Context, domain string) error {
	if domain == "" {
		return nil
	}
	var environment string
	err := s.pool.QueryRow(ctx, `SELECT environment_id FROM eregion_platform_domains WHERE name=$1`, domain).Scan(&environment)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return unavailable()
	}
	return contract.NewFault(contract.Conflict, domain+" is already routed to another environment", "Remove it from that environment first, or use another hostname.")
}

func (s *Service) checkPreviewEnvironmentAvailable(ctx context.Context, app, environment string) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM eregion_platform_environments e
		JOIN eregion_platform_apps a ON a.id=e.app_id
		WHERE a.name=$1 AND e.name=$2)`, app, environment).Scan(&exists); err != nil {
		return unavailable()
	}
	if exists {
		return contract.NewFault(contract.Conflict, "environment "+app+"/"+environment+" already exists", "Change it with eregion env update, or choose another name.")
	}
	return nil
}

// checkPreviewDeploySource mirrors the deterministic refusals of a first
// deploy from a new revision, before the CLI writes any vault value.
func checkPreviewDeploySource(spec *platform.EnvironmentSpec, deploy bool) error {
	if !deploy || spec.Source.Kind != "git" {
		return nil
	}
	if spec.BuildNode != "" && spec.BuildNode != spec.Node {
		return contract.NewFault(contract.PreconditionFailed,
			"build_node is distinct from the application node; moving the built image requires a shared registry",
			"Use build_node equal to the environment node, or create with --no-deploy and deploy an image release.")
	}
	return nil
}

func deploymentSteps() []string {
	return []string{"preflight", "build from the revision when needed", "release command", "candidate containers per process replica on dynamic loopback ports", "candidate web health", "Caddy upstream switch", "public route verification", "remove the previous release containers"}
}

func addReleaseWarnings(preview *platform.Preview, release platform.Release, env platform.Environment) {
	if release.Status != platform.ReleaseReady {
		preview.Warnings = append(preview.Warnings, "Release is not ready; submission will fail until its release operation succeeds.")
	}
	if env.ServingRelease == "" {
		preview.Warnings = append(preview.Warnings, "No release is serving yet; a candidate failure leaves the environment without a serving container.")
	}
}

func (s *Service) planDeployment(ctx context.Context, target platform.Target, releaseID platform.ReleaseID) (platform.Environment, platform.Release, error) {
	env, err := s.environment(ctx, target.App, target.Environment)
	if err != nil {
		return env, platform.Release{}, err
	}
	release, err := s.releaseByID(ctx, releaseID)
	if err != nil {
		return env, release, err
	}
	if release.App != env.App || release.Environment != env.Name {
		return env, release, contract.NewFault(contract.ValidationFailed, "release belongs to another environment", "Use a release created for this app and environment.")
	}
	if err := refusePrunedRelease(release); err != nil {
		return env, release, err
	}
	node, err := s.node(ctx, env.Node)
	if err != nil {
		return env, release, err
	}
	if err := s.checkPlacement(node, platform.RoleApplication, platform.CapabilityDocker); err != nil {
		return env, release, err
	}
	if release.Status != platform.ReleaseReady {
		return env, release, contract.NewFault(contract.PreconditionFailed, "release is not ready", "Wait for the release operation to succeed, then deploy it.")
	}
	return env, release, nil
}

func (s *Service) environment(ctx context.Context, app, name string) (platform.Environment, error) {
	env, err := scanEnvironmentRow(s.pool.QueryRow(ctx, environmentSelect+" WHERE a.name=$1 AND e.name=$2", app, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return env, contract.NewFault(contract.NotFound, "environment not found", "Create the environment under this app first.")
	}
	if err != nil {
		return env, unavailable()
	}
	missing, err := s.resourceDeployability(ctx, env)
	if err != nil {
		return env, err
	}
	env.Deployability.Missing = append(env.Deployability.Missing, missing...)
	env.Deployability.Deployable = len(env.Deployability.Missing) == 0
	return env, nil
}

func (s *Service) releaseByID(ctx context.Context, id platform.ReleaseID) (platform.Release, error) {
	release, err := scanReleaseRow(s.pool.QueryRow(ctx, releaseSelect+" WHERE r.id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return release, contract.NewFault(contract.NotFound, "release not found", "Use a release ID from inventory.")
	}
	if err != nil {
		return release, unavailable()
	}
	return release, nil
}

func (s *Service) node(ctx context.Context, name string) (platform.Node, error) {
	return loadNode(ctx, s.pool, name)
}
