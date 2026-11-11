package controlplane

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// diagnosticExecutionPlacement is the evidence needed by admission. It is not
// a public submission input: neither image
// nor node can be supplied by the operator.
type diagnosticExecutionPlacement struct {
	OriginalOperationID contract.OperationID
	DeploymentID        platform.DeploymentID
	ReleaseID           platform.ReleaseID
	RevisionID          platform.RevisionID
	ImageID             string
	Node                string
}

func (s *Service) diagnosticNode() string {
	if s.diagnosticAdmissionNode != "" {
		return s.diagnosticAdmissionNode
	}
	return s.localNode
}

// resolveDiagnosticExecutionPlacement resolves exact deployment-owned rows
// using the caller's submission transaction, never current environment state or
// a recent release. Only local execution is supported. A caller must perform
// idempotent replay before resolving placement, and commit the returned pin
// atomically with the operation and River job. Row locks retain the exact
// relationship until commit even if a deployment is concurrently updated.
func resolveDiagnosticExecutionPlacement(ctx context.Context, tx pgx.Tx, original contract.OperationID, localNode string, lock bool) (diagnosticExecutionPlacement, error) {
	var pin diagnosticExecutionPlacement
	if err := original.Validate(); err != nil {
		return pin, err
	}
	refuse := func() error {
		return contract.NewFault(contract.PreconditionFailed, "diagnostic execution placement cannot be proven", "Use an operation with an exact recorded deployment, local node, revision and immutable image ID.")
	}
	if localNode == "" {
		return pin, refuse()
	}
	lockDeployment, lockRevision := "", ""
	if lock {
		lockDeployment, lockRevision = " FOR SHARE OF d", " FOR SHARE OF r"
	}
	dep, err := scanDeploymentRow(tx.QueryRow(ctx, deploymentSelect+" WHERE d.operation_id=$1"+lockDeployment, original))
	if errors.Is(err, pgx.ErrNoRows) {
		return pin, refuse()
	}
	if err != nil {
		return pin, unavailable(err)
	}
	if dep.ReleaseID == "" || dep.RevisionID == "" {
		return pin, refuse()
	}
	release, err := scanReleaseRow(tx.QueryRow(ctx, releaseSelect+" WHERE r.id=$1"+lockRevision, dep.ReleaseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return pin, refuse()
	}
	if err != nil {
		return pin, unavailable(err)
	}
	revision, err := scanRevisionRow(tx.QueryRow(ctx, revisionSelect+" WHERE r.id=$1"+lockRevision, dep.RevisionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return pin, refuse()
	}
	if err != nil {
		return pin, unavailable(err)
	}
	if release.App != dep.App || release.Environment != dep.Environment || revision.App != dep.App || revision.Environment != dep.Environment || revision.Runtime.Node != localNode || !diagnosticImageID.MatchString(release.ImageID) {
		return pin, refuse()
	}
	return diagnosticExecutionPlacement{
		OriginalOperationID: original, DeploymentID: dep.ID, ReleaseID: release.ID,
		RevisionID: revision.ID, ImageID: release.ImageID, Node: revision.Runtime.Node,
	}, nil
}

// SubmitDiagnosticExecution is a trusted server-code boundary, not a generic
// action or an HTTP/RPC endpoint. A future operator SSH adapter must authenticate
// the caller before invoking it; actor.Kind alone is not that authentication.
// Confirmation must exactly name the original operation, including on replay.
// Argv is persisted as ordinary operation input and must contain no secrets.
// The service chooses image and node exclusively from historical relationships.
func (s *Service) SubmitDiagnosticExecution(ctx context.Context, actor contract.Actor, key string, original contract.OperationID, argv []string, confirmation string) (operations.SubmissionResult, error) {
	var zero operations.SubmissionResult
	if err := actor.Validate(); err != nil {
		return zero, err
	}
	if actor.Kind != "operator" {
		return zero, contract.NewFault(contract.ValidationFailed, "diagnostic execution requires operator authority", "Use the authenticated operator SSH channel.")
	}
	if err := contract.ValidateIdempotencyKey(key); err != nil {
		return zero, err
	}
	if err := original.Validate(); err != nil {
		return zero, err
	}
	if err := platform.ValidateDiagnosticExecutionArgv(argv); err != nil {
		return zero, err
	}
	if confirmation != string(original) {
		return zero, contract.NewFault(contract.InputRequired, "diagnostic execution requires exact operation confirmation", "Confirm with the original operation ID.")
	}
	// Hash request intent only. The historical pin is deliberately excluded so
	// an accepted replay never depends on artifact or placement availability.
	request, err := json.Marshal(struct {
		OriginalOperationID contract.OperationID `json:"original_operation_id"`
		Argv                []string             `json:"argv"`
	}{original, argv})
	if err != nil {
		return zero, err
	}
	inputHash := hashInput(request)
	target := "operation/" + string(original)
	// Serialize first admissions for this exact key. In particular, a second
	// request waits for the first commit before its replay lookup and therefore
	// does not re-resolve artifacts when the first request already won.
	lockName, err := json.Marshal([]string{"diagnostic-execution", actor.Kind, actor.Subject, target, key})
	if err != nil {
		return zero, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return zero, unavailable(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", advisoryKey(string(lockName))); err != nil {
		return zero, unavailable(err)
	}
	lookupReplay := func() (operations.SubmissionResult, bool, error) {
		var existingID contract.OperationID
		var existingHash string
		err := tx.QueryRow(ctx, `SELECT id,input_hash FROM eregion_operations
            WHERE actor_kind=$1 AND actor_subject=$2 AND kind=$3 AND target=$4 AND idempotency_key=$5`,
			actor.Kind, actor.Subject, platform.KindDiagnosticExecution, target, key).Scan(&existingID, &existingHash)
		if errors.Is(err, pgx.ErrNoRows) {
			return zero, false, nil
		}
		if err != nil {
			return zero, false, unavailable(err)
		}
		if existingHash != inputHash {
			return zero, true, idempotencyInputConflict()
		}
		op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", existingID))
		if err != nil {
			return zero, true, unavailable(err)
		}
		return operations.SubmissionResult{Operation: op, Replay: true}, true, nil
	}
	if result, found, err := lookupReplay(); err != nil || found {
		return result, err
	}
	pin, err := resolveDiagnosticExecutionPlacement(ctx, tx, original, s.diagnosticNode(), true)
	if err != nil {
		return zero, err
	}
	input := platform.DiagnosticExecutionInput{
		OriginalOperationID: original, Argv: append([]string(nil), argv...), ImageID: pin.ImageID,
		Node: pin.Node, ReleaseID: pin.ReleaseID, DeploymentID: pin.DeploymentID, RevisionID: pin.RevisionID,
	}
	if err := input.Validate(); err != nil {
		return zero, err
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		return zero, err
	}
	id, err := contract.NewOperationID()
	if err != nil {
		return zero, err
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO eregion_operations
        (id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'queued')
        ON CONFLICT (actor_kind,actor_subject,kind,target,idempotency_key) DO NOTHING`,
		id, platform.KindDiagnosticExecution, target, actor.Kind, actor.Subject, key, inputHash, canonical)
	if err != nil {
		return zero, unavailable(err)
	}
	if inserted.RowsAffected() == 0 {
		result, found, err := lookupReplay()
		if err != nil || found {
			return result, err
		}
		return zero, unavailable()
	}
	if s.enqueuePlatform == nil {
		return zero, unavailable()
	}
	op := contract.Operation{ID: id, Kind: platform.KindDiagnosticExecution, Target: target, Actor: actor}
	jobID, err := s.enqueuePlatform(ctx, tx, op)
	if err != nil {
		return zero, unavailable(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE eregion_operations SET job_id=$2 WHERE id=$1", id, jobID); err != nil {
		return zero, unavailable(err)
	}
	op, err = scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", id))
	if err != nil {
		return zero, unavailable(err)
	}
	if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "state", State: contract.Queued, Message: "Isolated diagnostic execution accepted durably; command output will be withheld."}); err != nil {
		return zero, unavailable(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, unavailable(err)
	}
	return operations.SubmissionResult{Operation: op}, nil
}

// PreviewDiagnosticExecution reads one consistent database snapshot without
// node calls, locks, operation insertion or queue effects. An identical replay
// uses the original stored pin instead of re-resolving current relationships.
func (s *Service) PreviewDiagnosticExecution(ctx context.Context, actor contract.Actor, key string, original contract.OperationID, argv []string, confirmation string) (platform.DiagnosticExecutionPreview, error) {
	var preview platform.DiagnosticExecutionPreview
	if err := actor.Validate(); err != nil {
		return preview, err
	}
	if actor.Kind != "operator" {
		return preview, contract.NewFault(contract.ValidationFailed, "diagnostic execution requires operator authority", "Use the authenticated operator SSH channel.")
	}
	if err := contract.ValidateIdempotencyKey(key); err != nil {
		return preview, err
	}
	if err := original.Validate(); err != nil {
		return preview, err
	}
	if err := platform.ValidateDiagnosticExecutionArgv(argv); err != nil {
		return preview, err
	}
	if confirmation != string(original) {
		return preview, contract.NewFault(contract.ConfirmationRequired, "diagnostic execution requires exact operation confirmation", "Confirm with the original operation ID.")
	}
	request, err := json.Marshal(struct {
		OriginalOperationID contract.OperationID `json:"original_operation_id"`
		Argv                []string             `json:"argv"`
	}{original, argv})
	if err != nil {
		return preview, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return preview, unavailable(err)
	}
	defer rollback(tx)
	target := "operation/" + string(original)
	var existingID contract.OperationID
	var existingHash string
	var stored json.RawMessage
	err = tx.QueryRow(ctx, `SELECT id,input_hash,input FROM eregion_operations
        WHERE actor_kind=$1 AND actor_subject=$2 AND kind=$3 AND target=$4 AND idempotency_key=$5`,
		actor.Kind, actor.Subject, platform.KindDiagnosticExecution, target, key).Scan(&existingID, &existingHash, &stored)
	var pin diagnosticExecutionPlacement
	if err == nil {
		if existingHash != hashInput(request) {
			return preview, idempotencyInputConflict()
		}
		input, _, err := platform.NormalizeDiagnosticExecutionInput(stored)
		if err != nil || input.OriginalOperationID != original {
			return preview, contract.NewFault(contract.PreconditionFailed, "recorded diagnostic input is invalid", "Inspect the accepted operation before retrying.")
		}
		op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", existingID))
		if err != nil {
			return preview, unavailable(err)
		}
		preview.Replay = &op
		pin = diagnosticExecutionPlacement{OriginalOperationID: original, Node: input.Node, ImageID: input.ImageID, DeploymentID: input.DeploymentID, ReleaseID: input.ReleaseID, RevisionID: input.RevisionID}
	} else if errors.Is(err, pgx.ErrNoRows) {
		pin, err = resolveDiagnosticExecutionPlacement(ctx, tx, original, s.diagnosticNode(), false)
		if err != nil {
			return preview, err
		}
	} else {
		return preview, unavailable(err)
	}
	if err := (platform.DiagnosticExecutionInput{OriginalOperationID: original, Argv: argv, Node: pin.Node, ImageID: pin.ImageID, DeploymentID: pin.DeploymentID, ReleaseID: pin.ReleaseID, RevisionID: pin.RevisionID}).Validate(); err != nil {
		return preview, err
	}
	preview.DryRun, preview.OriginalOperationID = true, original
	preview.Node, preview.ImageID = pin.Node, pin.ImageID
	preview.ReleaseID, preview.DeploymentID, preview.RevisionID = pin.ReleaseID, pin.DeploymentID, pin.RevisionID
	preview.ArgCount = len(argv)
	preview.Sandbox = []string{"direct absolute executable; no shell, launcher or resolver", "local pinned image only; no pull; image-declared volumes refused", "unprivileged UID, read-only root, no network, dropped capabilities", "bounded CPU, memory, process count, temporary storage and 30-second execution"}
	preview.Note = "Read-only plan; local image availability is checked only during execution. Command output is withheld; argv must contain no secrets."
	return preview, nil
}
