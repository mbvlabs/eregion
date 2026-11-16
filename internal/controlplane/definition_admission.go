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
	admission, err := s.prepareDefinitionAdmission(actor, input)
	if err != nil {
		return zero, err
	}
	if result, found, err := s.replayDefinitionSubmission(ctx, admission.Target, actor, input.IdempotencyKey, admission.RequestHash); err != nil {
		return zero, err
	} else if found {
		return result, nil
	}
	plan, err := s.planDefinition(ctx, actor, input.Plan, &admission.Material)
	if err != nil {
		return s.replayDefinitionFailure(ctx, admission, err)
	}
	admission.Plan = plan
	if plan.Digest != input.ApprovedDigest {
		return s.replayDefinitionFailure(ctx, admission, contract.NewFault(contract.Conflict, "definition plan changed", "Workspace, target, binding/resource/secret metadata, DNS policy/credential, placement, relevant operations or provider observations may have changed. Review and approve a new plan."))
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
	return s.submitDefinitionAdmission(ctx, admission)
}

type definitionAdmission struct {
	Actor       contract.Actor
	Input       api.DefinitionApplyInput
	Target      string
	RequestHash string
	Canonical   []byte
	Plan        api.DefinitionPlan
	Material    definitionPlanningMaterial
}

func (s *Service) prepareDefinitionAdmission(actor contract.Actor, input api.DefinitionApplyInput) (definitionAdmission, error) {
	var admission definitionAdmission
	if err := requireCloudflareOperator(actor); err != nil {
		return admission, err
	}
	if err := contract.ValidateIdempotencyKey(input.IdempotencyKey); err != nil {
		return admission, err
	}
	if !definitionHex256(input.Plan.PlanNonce) || !definitionHex256(input.ApprovedDigest) {
		return admission, contract.NewFault(contract.ValidationFailed, "invalid definition nonce or digest", "Use the nonce and digest returned by the plan.")
	}
	canonical, err := json.Marshal(input)
	if err != nil || len(input.Plan.Definition) > appdef.MaxDefinitionBytes {
		return admission, contract.NewFault(contract.ValidationFailed, "definition input is too large", "Use a bounded definition without secret values.")
	}
	if s == nil || s.entities == nil {
		return admission, unavailable()
	}
	resolved, err := appdef.ResolveSelection([]byte(input.Plan.Definition), input.Plan.App, input.Plan.Environment)
	if err != nil {
		return admission, contract.NewFault(contract.ValidationFailed, "invalid app definition", err.Error())
	}
	target := "env/" + resolved.App + "/" + resolved.Environment
	requestHash, err := definitionRequestHash(actor, target, input)
	if err != nil {
		return admission, err
	}
	admission.Actor, admission.Input, admission.Target = actor, input, target
	admission.RequestHash, admission.Canonical = requestHash, canonical
	return admission, nil
}

func (s *Service) replayDefinitionSubmission(ctx context.Context, target string, actor contract.Actor, key, requestHash string) (operations.SubmissionResult, bool, error) {
	op, found, err := s.replaySubmission(ctx, platform.KindDefinitionApply, target, actor, key, requestHash)
	if err != nil {
		return operations.SubmissionResult{}, false, err
	}
	if !found {
		return operations.SubmissionResult{}, false, nil
	}
	return operations.SubmissionResult{Operation: op, Replay: true}, true, nil
}

func (s *Service) replayDefinitionFailure(ctx context.Context, admission definitionAdmission, failure error) (operations.SubmissionResult, error) {
	result, found, err := s.replayDefinitionSubmission(ctx, admission.Target, admission.Actor, admission.Input.IdempotencyKey, admission.RequestHash)
	if err != nil {
		return operations.SubmissionResult{}, err
	}
	if found {
		return result, nil
	}
	return operations.SubmissionResult{}, failure
}

func (s *Service) submitDefinitionAdmission(ctx context.Context, admission definitionAdmission) (operations.SubmissionResult, error) {
	var submitted operations.SubmissionResult
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		submitted, err = s.admitDefinitionTx(ctx, tx, admission)
		return err
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return operations.SubmissionResult{}, unavailable(err)
		}
		var fault *contract.FaultError
		if errors.As(err, &fault) {
			return operations.SubmissionResult{}, err
		}
		return operations.SubmissionResult{}, unavailable(err)
	}
	if submitted.Replay {
		if err := s.populateActionResult(ctx, &submitted.Operation); err != nil {
			return operations.SubmissionResult{}, err
		}
	}
	return submitted, nil
}

func (s *Service) admitDefinitionTx(ctx context.Context, tx *entities.StoreTx, admission definitionAdmission) (operations.SubmissionResult, error) {
	// The policy guard precedes app/environment locks, matching policy writes.
	if err := lockDefinitionPolicyTx(ctx, tx); err != nil {
		return operations.SubmissionResult{}, unavailable(err)
	}
	if err := lockDefinitionTargetTx(ctx, tx, admission.Plan.App, admission.Plan.Environment); err != nil {
		return operations.SubmissionResult{}, err
	}
	// Serialize same-target submissions and replay a concurrent winner before
	// comparing its newly changed snapshot to our original plan.
	if result, found, err := replayAdmissionIdentityTx(ctx, tx, admission); err != nil {
		return operations.SubmissionResult{}, err
	} else if found {
		return result, nil
	}
	// Absence is the typed revision-zero disabled policy, not a storage error.
	// The advisory lock above also serializes creation of the singleton.
	policy, err := lockedDefinitionDNSPolicy(ctx, tx)
	if err != nil {
		return operations.SubmissionResult{}, err
	}
	if err := lockDefinitionAdmissionRowsTx(ctx, tx, admission, policy); err != nil {
		return operations.SubmissionResult{}, err
	}
	current, _, err := readDefinitionSnapshotTx(ctx, tx, admission.Material.Resolved)
	if err != nil {
		return operations.SubmissionResult{}, err
	}
	if !reflect.DeepEqual(current, admission.Material.Snapshot) {
		return operations.SubmissionResult{}, contract.NewFault(contract.Conflict, definitionSnapshotChange(current, admission.Material.Snapshot), "Review and approve a new plan.")
	}
	if err := checkDefinitionAdmissionFencesTx(ctx, tx, admission.Plan, current); err != nil {
		return operations.SubmissionResult{}, err
	}
	id, err := contract.NewOperationID()
	if err != nil {
		return operations.SubmissionResult{}, err
	}
	if err := insertDefinitionAdmissionTx(ctx, tx, admission, id, current); err != nil {
		return operations.SubmissionResult{}, err
	}
	return finishDefinitionAdmissionTx(ctx, tx, admission, id)
}

func replayAdmissionIdentityTx(ctx context.Context, tx *entities.StoreTx, admission definitionAdmission) (operations.SubmissionResult, bool, error) {
	identity, found, err := tx.Operations().Identity(ctx, entities.OperationKey{
		Kind: platform.KindDefinitionApply, Target: admission.Target, Actor: admission.Actor, IdempotencyKey: admission.Input.IdempotencyKey,
	})
	if err != nil {
		return operations.SubmissionResult{}, false, unavailable(err)
	}
	if !found {
		return operations.SubmissionResult{}, false, nil
	}
	if identity.InputHash != admission.RequestHash {
		return operations.SubmissionResult{}, false, idempotencyInputConflict()
	}
	op, found, err := tx.Operations().Get(ctx, identity.ID)
	if err != nil || !found {
		return operations.SubmissionResult{}, false, unavailable(err)
	}
	return operations.SubmissionResult{Operation: op, Replay: true}, true, nil
}

func lockDefinitionAdmissionRowsTx(ctx context.Context, tx *entities.StoreTx, admission definitionAdmission, policy api.DefinitionDNSPolicy) error {
	// Lock existing secret and provider metadata rows in deterministic order.
	if _, err := tx.Secrets().LockDefinitionSecretNames(ctx, admission.Plan.App, admission.Plan.Environment); err != nil {
		return unavailable(err)
	}
	if _, err := tx.Providers().LockZoneRevisionsForDefinition(ctx, "cloudflare", policy.ZoneID, string(admission.Material.Snapshot.EnvironmentID)); err != nil {
		return unavailable(err)
	}
	// Pin existing selected placement rows until admission commits. Registration
	// of a missing planned node cannot make an otherwise blocked plan applicable.
	nodeNames := make([]string, 0, len(admission.Material.Snapshot.Nodes))
	for _, node := range admission.Material.Snapshot.Nodes {
		nodeNames = append(nodeNames, node.Name)
	}
	if _, err := tx.Nodes().LockExistingNames(ctx, nodeNames); err != nil {
		return unavailable(err)
	}
	return nil
}

func checkDefinitionAdmissionFencesTx(ctx context.Context, tx *entities.StoreTx, plan api.DefinitionPlan, current definitionSnapshot) error {
	if err := checkDefinitionHostnamesTx(ctx, tx, plan, current); err != nil {
		return err
	}
	if err := checkDefinitionActiveOperationsTx(plan, current); err != nil {
		return err
	}
	activeBuild, err := tx.Platform().Builds().ActiveOperationForApp(ctx, current.AppID)
	if err != nil {
		return unavailable(err)
	}
	if activeBuild {
		return contract.NewFault(contract.Conflict, "application has active build operations", "Wait for existing work and plan again.")
	}
	return nil
}

func checkDefinitionHostnamesTx(ctx context.Context, tx *entities.StoreTx, plan api.DefinitionPlan, current definitionSnapshot) error {
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
	return nil
}

func checkDefinitionActiveOperationsTx(plan api.DefinitionPlan, current definitionSnapshot) error {
	target := "env/" + plan.App + "/" + plan.Environment
	for _, active := range current.ActiveOperations {
		if active.Target == "app/"+plan.App || active.Target == target || strings.HasPrefix(active.Target, "resource/"+plan.App+"/") || strings.HasPrefix(active.Target, "env/"+plan.App+"/") {
			return contract.NewFault(contract.Conflict, "application has active operations", "Wait for existing work and plan again.")
		}
	}
	return nil
}

func insertDefinitionAdmissionTx(ctx context.Context, tx *entities.StoreTx, admission definitionAdmission, id contract.OperationID, current definitionSnapshot) error {
	inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
		ID: id,
		Key: entities.OperationKey{
			Kind: platform.KindDefinitionApply, Target: admission.Target, Actor: admission.Actor, IdempotencyKey: admission.Input.IdempotencyKey,
		},
		InputHash: admission.RequestHash, Input: admission.Canonical,
	})
	if err != nil {
		return unavailable(err)
	}
	if !inserted {
		return idempotencyInputConflict()
	}
	approved, err := json.Marshal(definitionApproved{Input: admission.Input, Snapshot: admission.Material.Snapshot, Plan: admission.Plan, Actor: admission.Actor})
	if err != nil {
		return err
	}
	if err := insertDefinitionRunTx(ctx, tx, admission, id, current, approved); err != nil {
		return err
	}
	if err := insertDefinitionStepsTx(ctx, tx, id, admission.Material.Steps); err != nil {
		return err
	}
	return nil
}

func insertDefinitionRunTx(ctx context.Context, tx *entities.StoreTx, admission definitionAdmission, id contract.OperationID, current definitionSnapshot, approved []byte) error {
	if err := tx.DefinitionRuns().Insert(ctx, entities.DefinitionRunInsert{
		OperationID: id, PlanNonce: admission.Input.Plan.PlanNonce, EnvironmentID: string(current.EnvironmentID),
		Approved: approved,
	}); err != nil {
		if isUniqueConstraint(err) {
			return contract.NewFault(contract.Conflict, "definition nonce or environment already reserved", "Review and approve a new plan after active work finishes.")
		}
		return unavailable(err)
	}
	return nil
}

func insertDefinitionStepsTx(ctx context.Context, tx *entities.StoreTx, parent contract.OperationID, steps []definitionWorkStep) error {
	for ordinal, step := range steps {
		insert := entities.DefinitionStepInsert{
			ParentOperationID: parent, Ordinal: ordinal, Kind: step.Kind, Target: step.Target,
			Input: step.Input, DomainID: step.DomainID, AllocatedDomainID: step.AllocatedDomainID,
		}
		var err error
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
	return nil
}

func finishDefinitionAdmissionTx(ctx context.Context, tx *entities.StoreTx, admission definitionAdmission, id contract.OperationID) (operations.SubmissionResult, error) {
	op := contract.Operation{ID: id, Kind: platform.KindDefinitionApply, Target: admission.Target, Actor: admission.Actor}
	job, err := tx.Jobs().EnqueuePlatform(ctx, op)
	if err != nil {
		return operations.SubmissionResult{}, unavailable(err)
	}
	if err := tx.Operations().SetJob(ctx, id, job); err != nil {
		return operations.SubmissionResult{}, unavailable(err)
	}
	op, found, err := tx.Operations().Get(ctx, id)
	if err != nil || !found {
		return operations.SubmissionResult{}, unavailable(err)
	}
	if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "state", State: contract.Queued, Message: "Definition apply accepted durably."}); err != nil {
		return operations.SubmissionResult{}, err
	}
	return operations.SubmissionResult{Operation: op}, nil
}

func isUniqueConstraint(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
