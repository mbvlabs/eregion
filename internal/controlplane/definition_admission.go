package controlplane

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/appdef"
	"github.com/mbvlabs/eregion/internal/contract"
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
	if s == nil || s.pool == nil {
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
		return zero, contract.NewFault(contract.Conflict, "definition plan changed", "Review and approve a new plan.")
	}
	if err := definitionConfirm(plan, input.Confirm); err != nil {
		return zero, err
	}
	if !plan.CanApply {
		return zero, contract.NewFault(contract.PreconditionFailed, "definition plan cannot be applied", "Resolve the reported blockers, then review and approve a new plan.")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return zero, unavailable(err)
	}
	defer rollback(tx)
	// The policy guard precedes app/environment locks, matching policy writes.
	if err = lockDefinitionPolicyTx(ctx, tx); err != nil {
		return zero, unavailable(err)
	}
	if err = lockDefinitionTargetTx(ctx, tx, plan.App, plan.Environment); err != nil {
		return zero, err
	}
	// Serialize same-target submissions and replay a concurrent winner before
	// comparing its newly changed snapshot to our original plan.
	var existingID contract.OperationID
	var existingHash string
	err = tx.QueryRow(ctx, `SELECT id,input_hash FROM eregion_operations WHERE actor_kind=$1 AND actor_subject=$2 AND kind=$3 AND target=$4 AND idempotency_key=$5`, actor.Kind, actor.Subject, platform.KindDefinitionApply, target, input.IdempotencyKey).Scan(&existingID, &existingHash)
	if err == nil {
		if existingHash != requestHash {
			return zero, idempotencyInputConflict()
		}
		op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", existingID))
		if err != nil {
			return zero, unavailable(err)
		}
		return operations.SubmissionResult{Operation: op, Replay: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return zero, unavailable(err)
	}
	var revision int64
	if err = tx.QueryRow(ctx, `SELECT revision FROM eregion_definition_dns_policy WHERE singleton=true FOR SHARE`).Scan(&revision); err != nil {
		return zero, unavailable(err)
	}
	// Lock existing secret and provider metadata rows in deterministic order.
	for _, query := range []string{
		`SELECT name FROM eregion_secrets WHERE app=$1 AND environment=$2 ORDER BY name FOR SHARE`,
		`SELECT zone_id FROM eregion_provider_credentials WHERE provider='cloudflare' AND (zone_id=(SELECT zone_id FROM eregion_definition_dns_policy WHERE singleton=true) OR zone_id IN (SELECT x.zone_id FROM eregion_platform_domain_dns x JOIN eregion_platform_domains d ON d.id=x.domain_id WHERE d.environment_id=$1)) ORDER BY zone_id FOR SHARE`,
	} {
		var rows pgx.Rows
		if strings.Contains(query, "eregion_provider_credentials") {
			rows, err = tx.Query(ctx, query, material.Snapshot.EnvironmentID)
		} else {
			rows, err = tx.Query(ctx, query, plan.App, plan.Environment)
		}
		if err != nil {
			return zero, unavailable(err)
		}
		for rows.Next() {
			var ignored string
			if err = rows.Scan(&ignored); err != nil {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return zero, unavailable(err)
		}
	}
	current, _, err := readDefinitionSnapshotTx(ctx, tx, plan.App, plan.Environment)
	if err != nil {
		return zero, err
	}
	if !reflect.DeepEqual(current, material.Snapshot) {
		return zero, contract.NewFault(contract.Conflict, "definition snapshot changed", "Review and approve a new plan.")
	}
	for _, active := range current.ActiveOperations {
		if active.Target == "app/"+plan.App || active.Target == target || strings.HasPrefix(active.Target, "resource/"+plan.App+"/") || strings.HasPrefix(active.Target, "env/"+plan.App+"/") {
			return zero, contract.NewFault(contract.Conflict, "application has active operations", "Wait for existing work and plan again.")
		}
	}
	var activeBuild bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM eregion_operations o JOIN eregion_platform_builds b ON o.target='build/'||b.id WHERE b.app_id=$1 AND o.state IN ('queued','running','cancelling'))`, current.AppID).Scan(&activeBuild); err != nil {
		return zero, unavailable(err)
	}
	if activeBuild {
		return zero, contract.NewFault(contract.Conflict, "application has active build operations", "Wait for existing work and plan again.")
	}
	id, err := contract.NewOperationID()
	if err != nil {
		return zero, err
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO eregion_operations (id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'queued') ON CONFLICT (actor_kind,actor_subject,kind,target,idempotency_key) DO NOTHING`, id, platform.KindDefinitionApply, target, actor.Kind, actor.Subject, input.IdempotencyKey, requestHash, canonical)
	if err != nil {
		return zero, unavailable(err)
	}
	if inserted.RowsAffected() != 1 {
		return zero, idempotencyInputConflict()
	}
	approved, err := json.Marshal(definitionApproved{Input: input, Snapshot: material.Snapshot, Plan: plan, Actor: actor})
	if err != nil {
		return zero, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO eregion_definition_runs (operation_id,plan_nonce,environment_id,approved) VALUES ($1,$2,$3,$4)`, id, input.Plan.PlanNonce, current.EnvironmentID, approved)
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			return zero, contract.NewFault(contract.Conflict, "definition nonce or environment already reserved", "Review and approve a new plan after active work finishes.")
		}
		return zero, unavailable(err)
	}
	for i, step := range material.Steps {
		var dns, cleanup any
		if step.DNSPlan != nil {
			dns, err = json.Marshal(step.DNSPlan)
			if err != nil {
				return zero, err
			}
		}
		if step.DNSCleanup != nil {
			cleanup, err = json.Marshal(step.DNSCleanup)
			if err != nil {
				return zero, err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO eregion_definition_steps (parent_operation_id,ordinal,kind,target,input,domain_id,allocated_domain_id,dns_plan,dns_cleanup) VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8,$9)`, id, i, step.Kind, step.Target, step.Input, string(step.DomainID), string(step.AllocatedDomainID), dns, cleanup)
		if err != nil {
			var pgerr *pgconn.PgError
			if errors.As(err, &pgerr) && pgerr.Code == "23505" {
				return zero, contract.NewFault(contract.Conflict, "definition domain identity already reserved", "Review and approve a new plan.")
			}
			return zero, unavailable(err)
		}
	}
	if s.enqueuePlatform == nil {
		return zero, unavailable()
	}
	op := contract.Operation{ID: id, Kind: platform.KindDefinitionApply, Target: target, Actor: actor}
	job, err := s.enqueuePlatform(ctx, tx, op)
	if err != nil {
		return zero, unavailable(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE eregion_operations SET job_id=$2 WHERE id=$1`, id, job); err != nil {
		return zero, unavailable(err)
	}
	op, err = scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", id))
	if err != nil {
		return zero, unavailable(err)
	}
	if err = appendEvent(ctx, tx, &op, contract.Event{Kind: "state", State: contract.Queued, Message: "Definition apply accepted durably."}); err != nil {
		return zero, unavailable(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return zero, unavailable(err)
	}
	return operations.SubmissionResult{Operation: op}, nil
}
