package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

type Service struct {
	pool    *pgxpool.Pool
	enqueue func(context.Context, pgx.Tx, contract.OperationID) (int64, error)
	// enqueuePlatform inserts the one River job for a platform action in the
	// same transaction as its domain rows.
	enqueuePlatform func(context.Context, pgx.Tx, contract.Operation) (int64, error)
	// transport is the node-dispatch boundary. Local today; SSH/WireGuard later.
	transport node.Transport
	// afterNode is a test-only seam between a node effect and its database
	// acknowledgement. Production leaves it nil.
	afterNode func(contract.OperationID, string) error
	// secrets resolves environment secret references into transient values. It is
	// intentionally nil until the M3 vault agent installs a resolver.
	secrets SecretResolver
	// sourceRoots is the symlink-resolved directory-build allowlist.
	sourceRoots []string
}

// SecretResolver resolves opaque environment secret references without
// persisting values. Implementations must return only the requested keys.
type SecretResolver interface {
	Resolve(context.Context, map[string]string) (map[string]string, error)
}

func (s *Service) buildSourceRoots() []string {
	if len(s.sourceRoots) > 0 {
		return s.sourceRoots
	}
	return platform.BuildSourceRoots()
}

var _ operations.Service = (*Service)(nil)

const columns = `id, kind, target, actor_kind, actor_subject, state, attempt, last_sequence, error_code, created_at, updated_at`

func scanOperation(row pgx.Row) (contract.Operation, error) {
	var op contract.Operation
	err := row.Scan(&op.ID, &op.Kind, &op.Target, &op.Actor.Kind, &op.Actor.Subject, &op.State, &op.Attempt, &op.LastSequence, &op.ErrorCode, &op.CreatedAt, &op.UpdatedAt)
	return op, err
}

func (s *Service) Submit(ctx context.Context, sub operations.Submission) (contract.Operation, error) {
	var zero contract.Operation
	if err := sub.Actor.Validate(); err != nil {
		return zero, err
	}
	if err := contract.ValidateIdempotencyKey(sub.IdempotencyKey); err != nil {
		return zero, err
	}
	if platform.IsKind(sub.Kind) {
		return s.submitPlatform(ctx, sub)
	}
	if sub.Kind != operations.TestKind || sub.Target != operations.TestTarget || sub.Confirmation != "" {
		return zero, contract.NewFault(contract.ValidationFailed, "unsupported operation or target", "Only diagnostic.test on control-plane is implemented; it needs no confirmation.")
	}
	input, err := operations.NormalizeTestInput(sub.Input)
	if err != nil {
		return zero, err
	}
	canonical, _ := json.Marshal(input)
	hash := sha256.Sum256(canonical)
	inputHash := hex.EncodeToString(hash[:])
	id, err := contract.NewOperationID()
	if err != nil {
		return zero, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return zero, unavailable()
	}
	defer rollback(tx)
	// The uniqueness constraint arbitrates concurrent submissions across processes.
	// ON CONFLICT waits for the winning transaction; the next READ COMMITTED query
	// observes its complete operation/job/event, never a partially accepted record.
	inserted, err := tx.Exec(ctx, `INSERT INTO eregion_operations
        (id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'queued')
        ON CONFLICT (actor_kind,actor_subject,kind,target,idempotency_key) DO NOTHING`,
		id, sub.Kind, sub.Target, sub.Actor.Kind, sub.Actor.Subject, sub.IdempotencyKey, inputHash, canonical)
	if err != nil {
		return zero, unavailable()
	}
	if inserted.RowsAffected() == 0 {
		var existingID contract.OperationID
		var existingHash string
		if err := tx.QueryRow(ctx, `SELECT id,input_hash FROM eregion_operations WHERE actor_kind=$1 AND actor_subject=$2 AND kind=$3 AND target=$4 AND idempotency_key=$5`, sub.Actor.Kind, sub.Actor.Subject, sub.Kind, sub.Target, sub.IdempotencyKey).Scan(&existingID, &existingHash); err != nil {
			return zero, unavailable()
		}
		if existingHash != inputHash {
			return zero, contract.NewFault(contract.Conflict, "idempotency key already binds different inputs", "Use the original inputs to recover the existing operation; use a new key only for deliberately new work.")
		}
		return scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", existingID))
	}
	jobID, err := s.enqueue(ctx, tx, id)
	if err != nil {
		return zero, unavailable()
	}
	if _, err := tx.Exec(ctx, "UPDATE eregion_operations SET job_id=$2 WHERE id=$1", id, jobID); err != nil {
		return zero, unavailable()
	}
	op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", id))
	if err != nil {
		return zero, unavailable()
	}
	if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "state", State: contract.Queued, Message: "Diagnostic operation accepted durably."}); err != nil {
		return zero, unavailable()
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, unavailable()
	}
	return op, nil
}

func (s *Service) Get(ctx context.Context, actor contract.Actor, id contract.OperationID) (contract.Operation, error) {
	if err := actor.Validate(); err != nil {
		return contract.Operation{}, err
	}
	if err := id.Validate(); err != nil {
		return contract.Operation{}, err
	}
	op, err := scanOperation(s.pool.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 AND actor_kind=$2 AND actor_subject=$3", id, actor.Kind, actor.Subject))
	if errors.Is(err, pgx.ErrNoRows) {
		return op, contract.NewFault(contract.NotFound, "operation not found", "Check the ID and authenticated identity.")
	}
	if err != nil {
		if ctx.Err() != nil {
			return op, ctx.Err()
		}
		return op, unavailable()
	}
	return op, nil
}

func (s *Service) Events(ctx context.Context, actor contract.Actor, cursor contract.Cursor, limit int) (operations.EventPage, error) {
	page := operations.EventPage{Events: []contract.Event{}, Cursor: cursor}
	if limit < 1 || limit > 200 {
		return page, contract.NewFault(contract.ValidationFailed, "event limit out of range", "Use an event limit from 1 to 200.")
	}
	op, err := s.Get(ctx, actor, cursor.OperationID)
	if err != nil {
		return page, err
	}
	if cursor.After > op.LastSequence {
		return page, contract.NewFault(contract.PreconditionFailed, "event cursor is ahead of the operation", "Refresh the operation and resume from a previously observed cursor.")
	}
	// No pruning in M0.2. A missing retained event is an explicit gap, not silent loss.
	rows, err := s.pool.Query(ctx, `SELECT sequence,event FROM eregion_operation_events WHERE operation_id=$1 AND sequence>$2 AND sequence<=$3 ORDER BY sequence LIMIT $4`, cursor.OperationID, int64(cursor.After), int64(op.LastSequence), limit)
	if err != nil {
		if ctx.Err() != nil {
			return page, ctx.Err()
		}
		return page, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		var sequence uint64
		var raw []byte
		if err := rows.Scan(&sequence, &raw); err != nil {
			return page, unavailable()
		}
		if sequence != page.Cursor.After+1 {
			return page, contract.NewFault(contract.PreconditionFailed, "event history has a retention gap", "Refresh the snapshot; do not silently skip missing history.")
		}
		var event contract.Event
		if err := json.Unmarshal(raw, &event); err != nil {
			return page, err
		}
		page.Events = append(page.Events, event)
		page.Cursor.After = sequence
	}
	if err := rows.Err(); err != nil {
		return page, unavailable()
	}
	if len(page.Events) == 0 && cursor.After < op.LastSequence {
		return page, contract.NewFault(contract.PreconditionFailed, "event history is unavailable", "Refresh the snapshot; retained history is incomplete.")
	}
	return page, nil
}

func (s *Service) RequestCancel(ctx context.Context, actor contract.Actor, id contract.OperationID) (contract.Operation, error) {
	if _, err := s.Get(ctx, actor, id); err != nil {
		return contract.Operation{}, err
	}
	return s.mutate(ctx, id, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State.Terminal() || op.State == contract.Cancelling {
			return nil
		}
		return transition(ctx, tx, op, contract.Cancelling, "", "Cancellation requested; waiting for a worker safe boundary.")
	})
}

// mutate holds a row lock only for a short database mutation, never during work.
func (s *Service) mutate(ctx context.Context, id contract.OperationID, change func(pgx.Tx, *contract.Operation) error) (contract.Operation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return contract.Operation{}, unavailable()
	}
	defer rollback(tx)
	op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", id))
	if err != nil {
		return op, unavailable()
	}
	if err := change(tx, &op); err != nil {
		return op, err
	}
	if err := tx.Commit(ctx); err != nil {
		return op, unavailable()
	}
	return op, nil
}

// mutateOwned is mutate with an attempt fence. A stale or terminal operation is
// returned unchanged and change does not run. River's completion query is not
// attempt-fenced, so every worker domain write must pass through here.
func (s *Service) mutateOwned(ctx context.Context, id contract.OperationID, attempt uint32, change func(pgx.Tx, *contract.Operation) error) (contract.Operation, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return contract.Operation{}, false, unavailable()
	}
	defer rollback(tx)
	op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", id))
	if err != nil {
		return op, false, unavailable()
	}
	if op.State.Terminal() || op.Attempt != attempt {
		if err := tx.Commit(ctx); err != nil {
			return op, false, unavailable()
		}
		return op, false, nil
	}
	if err := change(tx, &op); err != nil {
		return op, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return op, false, unavailable()
	}
	return op, true, nil
}

// transition moves the operation to a new state. A failed transition carries
// the machine-readable error code that the operation snapshot and the state
// event expose, so a caller never has to read the message text.
func transition(ctx context.Context, tx pgx.Tx, op *contract.Operation, state contract.State, code contract.Code, message string) error {
	if err := contract.ValidateTransition(op.State, state); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE eregion_operations SET state=$2,error_code=$3 WHERE id=$1", op.ID, state, string(code)); err != nil {
		return unavailable()
	}
	op.State = state
	op.ErrorCode = code
	return appendEvent(ctx, tx, op, contract.Event{Kind: "state", State: state, ErrorCode: code, Message: message})
}

func appendEvent(ctx context.Context, tx pgx.Tx, op *contract.Operation, event contract.Event) error {
	event.OperationID, event.Sequence, event.Attempt = op.ID, op.LastSequence+1, op.Attempt
	event.At = time.Now().UTC()
	if err := event.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO eregion_operation_events(operation_id,sequence,event) VALUES ($1,$2,$3)", op.ID, int64(event.Sequence), raw); err != nil {
		return err
	}
	op.LastSequence = event.Sequence
	op.UpdatedAt = event.At
	_, err = tx.Exec(ctx, "UPDATE eregion_operations SET last_sequence=$2,updated_at=$3 WHERE id=$1", op.ID, int64(op.LastSequence), op.UpdatedAt)
	return err
}
