package controlplane

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/appdef"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// definitionApproved is the durable, secret-free authorization for the worker.
type definitionApproved struct {
	Input    api.DefinitionApplyInput `json:"input"`
	Snapshot definitionSnapshot       `json:"snapshot"`
	Plan     api.DefinitionPlan       `json:"plan"`
	Actor    contract.Actor           `json:"actor"`
}

func definitionHex256(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

func definitionConfirm(plan api.DefinitionPlan, confirmation string) error {
	if plan.ConfirmTarget != "" && confirmation != plan.ConfirmTarget {
		return contract.NewFault(contract.InputRequired, "definition removal requires exact target confirmation", "Confirm with app/environment.")
	}
	for _, d := range plan.Domains {
		if d.Action == "remove" && confirmation != plan.App+"/"+plan.Environment {
			return contract.NewFault(contract.InputRequired, "definition removal requires exact target confirmation", "Confirm with app/environment.")
		}
	}
	return nil
}

func definitionRequestHash(actor contract.Actor, target string, input api.DefinitionApplyInput) (string, error) {
	request, err := json.Marshal(struct {
		Actor  contract.Actor           `json:"actor"`
		Kind   string                   `json:"kind"`
		Target string                   `json:"target"`
		Input  api.DefinitionApplyInput `json:"input"`
	}{actor, platform.KindDefinitionApply, target, input})
	if err != nil {
		return "", err
	}
	return hashInput(request), nil
}

// ApplyDefinition records only a parent intent and frozen work, never child effects.
// Accepted work is resumed by the durable parent worker, not by the caller.
func (s *Service) ApplyDefinition(ctx context.Context, actor contract.Actor, input api.DefinitionApplyInput) (operations.SubmissionResult, error) {
	var zero operations.SubmissionResult
	if err := requireCloudflareOperator(actor); err != nil {
		return zero, err
	}
	if err := contract.ValidateIdempotencyKey(input.IdempotencyKey); err != nil {
		return zero, err
	}
	if !definitionHex256(input.Plan.PlanNonce) || !definitionHex256(input.ApprovedDigest) {
		return zero, contract.NewFault(contract.ValidationFailed, "invalid definition nonce or digest", "Use the nonce and digest returned by the plan.")
	}
	canonical, err := json.Marshal(input)
	if err != nil || len(input.Plan.Definition) > appdef.MaxDefinitionBytes {
		return zero, contract.NewFault(contract.ValidationFailed, "definition input is too large", "Use a bounded definition without secret values.")
	}
	if s == nil || s.entities == nil {
		return zero, unavailable()
	}
	resolved, err := appdef.ResolveSelection([]byte(input.Plan.Definition), input.Plan.App, input.Plan.Environment)
	if err != nil {
		return zero, contract.NewFault(contract.ValidationFailed, "invalid app definition", err.Error())
	}
	target := "env/" + resolved.App + "/" + resolved.Environment
	requestHash, err := definitionRequestHash(actor, target, input)
	if err != nil {
		return zero, err
	}
	if op, found, err := s.replaySubmission(ctx, platform.KindDefinitionApply, target, actor, input.IdempotencyKey, requestHash); err != nil {
		return zero, err
	} else if found {
		return operations.SubmissionResult{Operation: op, Replay: true}, nil
	}
	var material definitionPlanningMaterial
	plan, err := s.planDefinition(ctx, actor, input.Plan, &material)
	if err != nil {
		if op, found, replayErr := s.replaySubmission(ctx, platform.KindDefinitionApply, target, actor, input.IdempotencyKey, requestHash); replayErr != nil {
			return zero, replayErr
		} else if found {
			return operations.SubmissionResult{Operation: op, Replay: true}, nil
		}
		return zero, err
	}
	if plan.Digest != input.ApprovedDigest {
		if op, found, replayErr := s.replaySubmission(ctx, platform.KindDefinitionApply, target, actor, input.IdempotencyKey, requestHash); replayErr != nil {
			return zero, replayErr
		} else if found {
			return operations.SubmissionResult{Operation: op, Replay: true}, nil
		}
		return zero, contract.NewFault(contract.Conflict, "definition plan changed", "Workspace, target, binding/resource/secret metadata, DNS policy/credential, placement, relevant operations or provider observations may have changed. Review and approve a new plan.")
	}
	if err := definitionConfirm(plan, input.Confirm); err != nil {
		return zero, err
	}
	if !plan.CanApply {
		return zero, contract.NewFault(contract.PreconditionFailed, "definition plan cannot be applied", "Resolve the reported blockers, then review and approve a new plan.")
	}
	// Even a converged provider snapshot requires the same operator authority
	// as a DNS child; a no-op must not turn read access into write approval.
	if len(plan.DNS) > 0 {
		if err := s.authorizeDomainDNSWrite(ctx, actor); err != nil {
			return zero, err
		}
	}

	var submitted operations.SubmissionResult
	err = s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		// The policy guard precedes app/environment locks, matching policy writes.
		if err := lockDefinitionPolicyTx(ctx, tx); err != nil {
			return unavailable(err)
		}
		if err := lockDefinitionTargetTx(ctx, tx, plan.App, plan.Environment); err != nil {
			return err
		}
		// Serialize same-target submissions and replay a concurrent winner before
		// comparing its newly changed snapshot to our original plan.
		identity, found, err := tx.Operations().Identity(ctx, entities.OperationKey{
			Kind: platform.KindDefinitionApply, Target: target, Actor: actor, IdempotencyKey: input.IdempotencyKey,
		})
		if err != nil {
			return unavailable(err)
		}
		if found {
			if identity.InputHash != requestHash {
				return idempotencyInputConflict()
			}
			op, found, err := tx.Operations().Get(ctx, identity.ID)
			if err != nil || !found {
				return unavailable(err)
			}
			submitted = operations.SubmissionResult{Operation: op, Replay: true}
			return nil
		}
		// Absence is the typed revision-zero disabled policy, not a storage error.
		// The advisory lock above also serializes creation of the singleton.
		policy, err := lockedDefinitionDNSPolicy(ctx, tx)
		if err != nil {
			return err
		}
		// Lock existing secret and provider metadata rows in deterministic order.
		if _, err := tx.Secrets().LockDefinitionSecretNames(ctx, plan.App, plan.Environment); err != nil {
			return unavailable(err)
		}
		if _, err := tx.Providers().LockZoneRevisionsForDefinition(ctx, "cloudflare", policy.ZoneID, string(material.Snapshot.EnvironmentID)); err != nil {
			return unavailable(err)
		}
		// Pin existing selected placement rows until admission commits. Registration
		// of a missing planned node cannot make an otherwise blocked plan applicable.
		var nodeNames []string
		for _, node := range material.Snapshot.Nodes {
			nodeNames = append(nodeNames, node.Name)
		}
		if _, err := tx.Nodes().LockExistingNames(ctx, nodeNames); err != nil {
			return unavailable(err)
		}
		current, _, err := readDefinitionSnapshotTx(ctx, tx, material.Resolved)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, material.Snapshot) {
			return contract.NewFault(contract.Conflict, definitionSnapshotChange(current, material.Snapshot), "Review and approve a new plan.")
		}
		// Hostnames are installation-wide, not environment-local. Recheck existing
		// bindings even though unrelated application inventory is not fingerprinted.
		// The domain table's global uniqueness constraint remains the final fence
		// against a concurrent insertion after this read.
		for _, domain := range plan.Domains {
			if domain.Action == "remove" {
				continue
			}
			owner, found, err := tx.Platform().Domains().EnvironmentIDByName(ctx, domain.Name)
			if err != nil {
				return unavailable(err)
			}
			if found && owner != current.EnvironmentID {
				return contract.NewFault(contract.Conflict, "hostname belongs to another environment", "Remove the existing binding or review a plan using another hostname.")
			}
		}
		for _, active := range current.ActiveOperations {
			if active.Target == "app/"+plan.App || active.Target == target || strings.HasPrefix(active.Target, "resource/"+plan.App+"/") || strings.HasPrefix(active.Target, "env/"+plan.App+"/") {
				return contract.NewFault(contract.Conflict, "application has active operations", "Wait for existing work and plan again.")
			}
		}
		activeBuild, err := tx.Platform().Builds().ActiveOperationForApp(ctx, current.AppID)
		if err != nil {
			return unavailable(err)
		}
		if activeBuild {
			return contract.NewFault(contract.Conflict, "application has active build operations", "Wait for existing work and plan again.")
		}
		id, err := contract.NewOperationID()
		if err != nil {
			return err
		}
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: id,
			Key: entities.OperationKey{
				Kind: platform.KindDefinitionApply, Target: target, Actor: actor, IdempotencyKey: input.IdempotencyKey,
			},
			InputHash: requestHash, Input: canonical,
		})
		if err != nil {
			return unavailable(err)
		}
		if !inserted {
			return idempotencyInputConflict()
		}
		approved, err := json.Marshal(definitionApproved{Input: input, Snapshot: material.Snapshot, Plan: plan, Actor: actor})
		if err != nil {
			return err
		}
		if err := tx.DefinitionRuns().Insert(ctx, entities.DefinitionRunInsert{
			OperationID: id, PlanNonce: input.Plan.PlanNonce, EnvironmentID: string(current.EnvironmentID),
			Approved: approved,
		}); err != nil {
			if isUniqueConstraint(err) {
				return contract.NewFault(contract.Conflict, "definition nonce or environment already reserved", "Review and approve a new plan after active work finishes.")
			}
			return unavailable(err)
		}
		for ordinal, step := range material.Steps {
			insert := entities.DefinitionStepInsert{
				ParentOperationID: id, Ordinal: ordinal, Kind: step.Kind, Target: step.Target,
				Input: step.Input, DomainID: step.DomainID, AllocatedDomainID: step.AllocatedDomainID,
			}
			if step.DNSPlan != nil {
				insert.DNSPlan, err = json.Marshal(step.DNSPlan)
				if err != nil {
					return err
				}
			}
			if step.DNSCleanup != nil {
				insert.DNSCleanup, err = json.Marshal(step.DNSCleanup)
				if err != nil {
					return err
				}
			}
			if err := tx.DefinitionSteps().Insert(ctx, insert); err != nil {
				if isUniqueConstraint(err) {
					return contract.NewFault(contract.Conflict, "definition domain identity already reserved", "Review and approve a new plan.")
				}
				return unavailable(err)
			}
		}
		op := contract.Operation{ID: id, Kind: platform.KindDefinitionApply, Target: target, Actor: actor}
		job, err := tx.Jobs().EnqueuePlatform(ctx, op)
		if err != nil {
			return unavailable(err)
		}
		if err := tx.Operations().SetJob(ctx, id, job); err != nil {
			return unavailable(err)
		}
		op, found, err = tx.Operations().Get(ctx, id)
		if err != nil || !found {
			return unavailable(err)
		}
		if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "state", State: contract.Queued, Message: "Definition apply accepted durably."}); err != nil {
			return err
		}
		submitted = operations.SubmissionResult{Operation: op}
		return nil
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return zero, unavailable(err)
		}
		var fault *contract.Fault
		if errors.As(err, &fault) {
			return zero, err
		}
		return zero, unavailable(err)
	}
	if submitted.Replay {
		if err := s.populateActionResult(ctx, &submitted.Operation); err != nil {
			return zero, err
		}
	}
	return submitted, nil
}

func isUniqueConstraint(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
