package entities

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

// OperationKey is the durable admission identity of one operation. It is the
// same tuple the schema's unique constraint arbitrates.
type OperationKey struct {
	Kind           string
	Target         string
	Actor          contract.Actor
	IdempotencyKey string
}

// OperationIdentity is the idempotent replay answer: the accepted operation and
// the canonical input hash bound to it.
type OperationIdentity struct {
	ID        contract.OperationID
	InputHash string
}

// OperationInsert is one operation admitted in the caller's transaction. Input
// is the canonical normalized JSON, never raw request bytes.
type OperationInsert struct {
	ID        contract.OperationID
	Key       OperationKey
	InputHash string
	Input     json.RawMessage
}

// OperationFilter is one bounded operation-history read. With no app filter the
// history follows AllActors: a bare actor sees its own operations, while the
// operator channel sees every operation. Environment needs App because
// operation targets are env/<app>/<environment>.
type OperationFilter struct {
	AllActors   bool
	Actor       contract.Actor
	App         string
	Environment string
	State       string
	Limit       int
}

// OperationStore is the read view over the operations aggregate. It borrows the
// Store's pool and exposes no query, transaction or driver handle.
type OperationStore struct{ q *query.Queries }

// Operations returns the operation read view of this Store.
func (s *Store) Operations() *OperationStore { return &OperationStore{q: query.New(s.pool)} }

// Get returns one operation snapshot and whether it exists. A missing row is
// (zero, false, nil). LatestResult is not hydrated; the caller reads it
// explicitly when it needs the outcome record.
func (o *OperationStore) Get(ctx context.Context, id contract.OperationID) (contract.Operation, bool, error) {
	row, err := o.q.GetOperation(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.Operation{}, false, nil
	}
	if err != nil {
		return contract.Operation{}, false, err
	}
	op, err := operation(row.ID, row.Kind, row.Target, row.ActorKind, row.ActorSubject,
		row.State, row.Attempt, row.LastSequence, row.ErrorCode, row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return contract.Operation{}, false, err
	}
	return op, true, nil
}

// GetForActor returns one operation only when the actor owns it. A missing or
// foreign row is (zero, false, nil); callers keep the authority decision.
func (o *OperationStore) GetForActor(ctx context.Context, id contract.OperationID, actor contract.Actor) (contract.Operation, bool, error) {
	row, err := o.q.GetOperationForActor(ctx, query.GetOperationForActorParams{
		ID: string(id), ActorKind: actor.Kind, ActorSubject: actor.Subject,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.Operation{}, false, nil
	}
	if err != nil {
		return contract.Operation{}, false, err
	}
	op, err := operation(row.ID, row.Kind, row.Target, row.ActorKind, row.ActorSubject,
		row.State, row.Attempt, row.LastSequence, row.ErrorCode, row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return contract.Operation{}, false, err
	}
	return op, true, nil
}

// JobID returns the River job bound to the operation and whether one is bound.
// A missing operation or an unset job_id is (0, false, nil).
func (o *OperationStore) JobID(ctx context.Context, id contract.OperationID) (int64, bool, error) {
	return operationJobID(ctx, o.q, id)
}

// OperationIdentityPattern is the exact durable identity used by the
// retention-day history read: kind, target, actor and an idempotency-key
// prefix matched with LIKE.
type OperationIdentityPattern struct {
	Kind               string
	Target             string
	Actor              contract.Actor
	IdempotencyPattern string
}

// Input returns the canonical operation input and whether the operation
// exists. A missing row is (nil, false, nil).
func (o *OperationStore) Input(ctx context.Context, id contract.OperationID) (json.RawMessage, bool, error) {
	return operationInputQuery(ctx, o.q, id)
}

// List returns one bounded history page, newest first. The three history
// branches are separate statements so the app/environment filter continues to
// bypass the actor filter exactly as the existing product policy does.
func (o *OperationStore) List(ctx context.Context, filter OperationFilter) ([]contract.Operation, error) {
	if filter.Limit < 1 {
		return nil, errors.New("entities: operation list limit must be positive")
	}
	if filter.App == "" {
		if filter.AllActors {
			rows, err := o.q.ListOperationsAllActors(ctx, query.ListOperationsAllActorsParams{
				State: optionalText(filter.State), MaxRows: int64(filter.Limit),
			})
			if err != nil {
				return nil, err
			}
			return operationsFromAllActors(rows)
		}
		rows, err := o.q.ListOperationsForActor(ctx, query.ListOperationsForActorParams{
			ActorKind: filter.Actor.Kind, ActorSubject: filter.Actor.Subject,
			State: optionalText(filter.State), MaxRows: int64(filter.Limit),
		})
		if err != nil {
			return nil, err
		}
		return operationsFromForActor(rows)
	}
	if filter.Environment != "" {
		rows, err := o.q.ListOperationsByAppEnvironment(ctx, query.ListOperationsByAppEnvironmentParams{
			App: filter.App, Environment: filter.Environment,
			State: optionalText(filter.State), MaxRows: int64(filter.Limit),
		})
		if err != nil {
			return nil, err
		}
		return operationsFromByAppEnvironment(rows)
	}
	rows, err := o.q.ListOperationsByApp(ctx, query.ListOperationsByAppParams{
		App: filter.App, State: optionalText(filter.State), MaxRows: int64(filter.Limit),
	})
	if err != nil {
		return nil, err
	}
	return operationsFromByApp(rows)
}

// ListByIdentityPattern returns the operations matching one exact durable
// identity and an idempotency-key prefix, in creation order. It is the
// retention-day history read.
func (o *OperationStore) ListByIdentityPattern(ctx context.Context, filter OperationIdentityPattern) ([]contract.Operation, error) {
	rows, err := o.q.ListOperationsByIdentityPattern(ctx, query.ListOperationsByIdentityPatternParams{
		Kind: filter.Kind, ActorKind: filter.Actor.Kind, ActorSubject: filter.Actor.Subject,
		Target: filter.Target, IdempotencyPattern: filter.IdempotencyPattern,
	})
	if err != nil {
		return nil, err
	}
	return operationsFromIdentityPattern(rows)
}

// Identity returns the operation already bound to an admission key.
func (o *OperationStore) Identity(ctx context.Context, key OperationKey) (OperationIdentity, bool, error) {
	return operationIdentityQuery(ctx, o.q, key, false)
}

// IdentityForShare returns the identity bound to an admission key while holding
// a share lock until the caller's statement or transaction ends. It is the
// legacy replay read used by webhook reconciliation.
func (o *OperationStore) IdentityForShare(ctx context.Context, key OperationKey) (OperationIdentity, bool, error) {
	return operationIdentityQuery(ctx, o.q, key, true)
}

// Events returns the ascending, After-exclusive retained event page bounded by
// the operation's accepted sequence. A missing event is a retention gap; this
// method does not invent one.
func (o *OperationStore) Events(ctx context.Context, cursor contract.Cursor, through uint64, limit int) ([]contract.Event, error) {
	if limit < 1 {
		return nil, errors.New("entities: operation event limit must be positive")
	}
	after, err := operationBigint(cursor.After)
	if err != nil {
		return nil, err
	}
	last, err := operationBigint(through)
	if err != nil {
		return nil, err
	}
	rows, err := o.q.ListOperationEvents(ctx, query.ListOperationEventsParams{
		OperationID:     string(cursor.OperationID),
		AfterSequence:   after,
		ThroughSequence: last,
		MaxRows:         int64(limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]contract.Event, 0, len(rows))
	for _, row := range rows {
		var event contract.Event
		if err := json.Unmarshal(row.Event, &event); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, nil
}

// LatestResult returns the most recent retained result event through the given
// accepted sequence.
func (o *OperationStore) LatestResult(ctx context.Context, id contract.OperationID, through uint64) (*contract.ActionResult, bool, error) {
	last, err := operationBigint(through)
	if err != nil {
		return nil, false, err
	}
	raw, err := o.q.GetLatestOperationResult(ctx, query.GetLatestOperationResultParams{
		OperationID: string(id), ThroughSequence: last,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var result contract.ActionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, false, err
	}
	return &result, true, nil
}

// ResultExists reports whether this attempt's outcome was already appended.
func (o *OperationStore) ResultExists(ctx context.Context, id contract.OperationID, attempt uint32, status contract.State, final bool) (bool, error) {
	return operationResultExists(ctx, o.q, id, attempt, status, final)
}

// ResultExists reads the exact caller transaction.
func (o *OperationTx) ResultExists(ctx context.Context, id contract.OperationID, attempt uint32, status contract.State, final bool) (bool, error) {
	return operationResultExists(ctx, o.owner.q, id, attempt, status, final)
}

func operationResultExists(ctx context.Context, q *query.Queries, id contract.OperationID, attempt uint32, status contract.State, final bool) (bool, error) {
	return q.OperationResultExists(ctx, query.OperationResultExistsParams{
		OperationID: string(id),
		Attempt:     strconv.FormatUint(uint64(attempt), 10),
		Status:      string(status),
		Final:       strconv.FormatBool(final),
	})
}

// AttemptStartedAt returns the earliest retained event time for one attempt. A
// missing observation is (zero, false, nil).
func (o *OperationStore) AttemptStartedAt(ctx context.Context, id contract.OperationID, attempt uint32) (time.Time, bool, error) {
	return operationAttemptStartedAt(ctx, o.q, id, attempt)
}

// AttemptStartedAt reads the exact caller transaction.
func (o *OperationTx) AttemptStartedAt(ctx context.Context, id contract.OperationID, attempt uint32) (time.Time, bool, error) {
	return operationAttemptStartedAt(ctx, o.owner.q, id, attempt)
}

func operationAttemptStartedAt(ctx context.Context, q *query.Queries, id contract.OperationID, attempt uint32) (time.Time, bool, error) {
	value, err := q.OperationAttemptStartedAt(ctx, query.OperationAttemptStartedAtParams{
		OperationID: string(id), Attempt: strconv.FormatUint(uint64(attempt), 10),
	})
	if err != nil {
		return time.Time{}, false, err
	}
	if !value.Valid {
		return time.Time{}, false, nil
	}
	return value.Time.UTC(), true, nil
}

// FailedStateEvent returns the most recent failed state event inside the
// accepted snapshot for one attempt.
func (o *OperationStore) FailedStateEvent(ctx context.Context, id contract.OperationID, through uint64, attempt uint32) (contract.Event, bool, error) {
	return operationFailedStateEvent(ctx, o.q, id, through, attempt)
}

func operationFailedStateEvent(ctx context.Context, q *query.Queries, id contract.OperationID, through uint64, attempt uint32) (contract.Event, bool, error) {
	last, err := operationBigint(through)
	if err != nil {
		return contract.Event{}, false, err
	}
	raw, err := q.GetFailedStateEventForAttempt(ctx, query.GetFailedStateEventForAttemptParams{
		OperationID: string(id), ThroughSequence: last,
		Attempt: strconv.FormatUint(uint64(attempt), 10),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.Event{}, false, nil
	}
	if err != nil {
		return contract.Event{}, false, err
	}
	var event contract.Event
	if err := json.Unmarshal(raw, &event); err != nil {
		return contract.Event{}, false, err
	}
	return event, true, nil
}

// EventStageExists reports whether any retained event carries this stage.
func (o *OperationStore) EventStageExists(ctx context.Context, id contract.OperationID, stage string) (bool, error) {
	return o.q.OperationEventStageExists(ctx, query.OperationEventStageExistsParams{
		OperationID: string(id), Stage: stage,
	})
}

// EventStageExists reads the exact caller transaction.
func (o *OperationTx) EventStageExists(ctx context.Context, id contract.OperationID, stage string) (bool, error) {
	return o.owner.q.OperationEventStageExists(ctx, query.OperationEventStageExistsParams{
		OperationID: string(id), Stage: stage,
	})
}

// LatestEventMessageForStage returns the newest retained message for one stage,
// or an empty string when the operation has none.
func (o *OperationStore) LatestEventMessageForStage(ctx context.Context, id contract.OperationID, stage string) (string, error) {
	return operationLatestEventMessageForStage(ctx, o.q, id, stage)
}

// LatestEventMessageForStage reads the exact caller transaction.
func (o *OperationTx) LatestEventMessageForStage(ctx context.Context, id contract.OperationID, stage string) (string, error) {
	return operationLatestEventMessageForStage(ctx, o.owner.q, id, stage)
}

func operationLatestEventMessageForStage(ctx context.Context, q *query.Queries, id contract.OperationID, stage string) (string, error) {
	return q.GetLatestOperationEventMessageByStage(ctx, query.GetLatestOperationEventMessageByStageParams{
		OperationID: string(id), Stage: stage,
	})
}

// ActionOutputExists reports whether any action-output snapshot was retained
// for the operation.
func (o *OperationStore) ActionOutputExists(ctx context.Context, id contract.OperationID) (bool, error) {
	return operationActionOutputExists(ctx, o.q, id)
}

// ActionOutputExists reads the exact caller transaction.
func (o *OperationTx) ActionOutputExists(ctx context.Context, id contract.OperationID) (bool, error) {
	return operationActionOutputExists(ctx, o.owner.q, id)
}

func operationActionOutputExists(ctx context.Context, q *query.Queries, id contract.OperationID) (bool, error) {
	return q.OperationActionOutputExists(ctx, string(id))
}

// LatestActionOutput returns the newest retained effect snapshot and its
// availability status. The raw JSON is "null" when the snapshot was omitted.
func (o *OperationStore) LatestActionOutput(ctx context.Context, id contract.OperationID) (json.RawMessage, string, bool, error) {
	return operationLatestActionOutput(ctx, o.q, id)
}

// LatestActionOutput reads the exact caller transaction.
func (o *OperationTx) LatestActionOutput(ctx context.Context, id contract.OperationID) (json.RawMessage, string, bool, error) {
	return operationLatestActionOutput(ctx, o.owner.q, id)
}

func operationLatestActionOutput(ctx context.Context, q *query.Queries, id contract.OperationID) (json.RawMessage, string, bool, error) {
	_, raw, status, found, err := operationLatestActionOutputEvent(ctx, q, id)
	return raw, status, found, err
}

// LatestActionOutputEvent returns the newest retained effect snapshot, its
// availability status and the exact event sequence used by the result cursor.
func (o *OperationTx) LatestActionOutputEvent(ctx context.Context, id contract.OperationID) (uint64, json.RawMessage, string, bool, error) {
	return operationLatestActionOutputEvent(ctx, o.owner.q, id)
}

func operationLatestActionOutputEvent(ctx context.Context, q *query.Queries, id contract.OperationID) (uint64, json.RawMessage, string, bool, error) {
	row, err := q.GetLatestActionOutput(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, "", false, nil
	}
	if err != nil {
		return 0, nil, "", false, err
	}
	sequence, err := operationSequence(row.Sequence)
	if err != nil {
		return 0, nil, "", false, err
	}
	return sequence, row.ActionOutput, row.ActionOutputStatus, true, nil
}

// NodeEvidenceExists reports whether this exact execution outcome was already
// retained, so a replay writes no duplicate evidence event.
func (o *OperationStore) NodeEvidenceExists(ctx context.Context, id contract.OperationID, executionID, outcome string) (bool, error) {
	return operationNodeEvidenceExists(ctx, o.q, id, executionID, outcome)
}

// NodeEvidenceExists reads the exact caller transaction.
func (o *OperationTx) NodeEvidenceExists(ctx context.Context, id contract.OperationID, executionID, outcome string) (bool, error) {
	return operationNodeEvidenceExists(ctx, o.owner.q, id, executionID, outcome)
}

func operationNodeEvidenceExists(ctx context.Context, q *query.Queries, id contract.OperationID, executionID, outcome string) (bool, error) {
	return q.OperationNodeEvidenceExists(ctx, query.OperationNodeEvidenceExistsParams{
		OperationID: string(id), ExecutionID: executionID, Outcome: outcome,
	})
}

// FailedNodeEvidence returns the first failed node-evidence frame for an
// attempt, optionally narrowed to one step.
func (o *OperationStore) FailedNodeEvidence(ctx context.Context, id contract.OperationID, through uint64, attempt uint32, stage string) (uint64, json.RawMessage, bool, error) {
	return operationFailedNodeEvidence(ctx, o.q, id, through, attempt, stage)
}

// FailedNodeEvidence reads the exact caller transaction.
func (o *OperationTx) FailedNodeEvidence(ctx context.Context, id contract.OperationID, through uint64, attempt uint32, stage string) (uint64, json.RawMessage, bool, error) {
	return operationFailedNodeEvidence(ctx, o.owner.q, id, through, attempt, stage)
}

func operationFailedNodeEvidence(ctx context.Context, q *query.Queries, id contract.OperationID, through uint64, attempt uint32, stage string) (uint64, json.RawMessage, bool, error) {
	last, err := operationBigint(through)
	if err != nil {
		return 0, nil, false, err
	}
	row, err := q.GetFailedNodeEvidence(ctx, query.GetFailedNodeEvidenceParams{
		OperationID: string(id), Attempt: strconv.FormatUint(uint64(attempt), 10),
		ThroughSequence: last, Stage: stage,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, err
	}
	sequence, err := operationSequence(row.Sequence)
	if err != nil {
		return 0, nil, false, err
	}
	return sequence, row.NodeEvidence, true, nil
}

// EvidenceLost reports a node-evidence loss that no retained frame repaired.
func (o *OperationStore) EvidenceLost(ctx context.Context, id contract.OperationID, through uint64, attempt uint32) (bool, error) {
	return operationEvidenceLost(ctx, o.q, id, through, attempt)
}

// EvidenceLost reads the exact caller transaction.
func (o *OperationTx) EvidenceLost(ctx context.Context, id contract.OperationID, through uint64, attempt uint32) (bool, error) {
	return operationEvidenceLost(ctx, o.owner.q, id, through, attempt)
}

func operationEvidenceLost(ctx context.Context, q *query.Queries, id contract.OperationID, through uint64, attempt uint32) (bool, error) {
	last, err := operationBigint(through)
	if err != nil {
		return false, err
	}
	return q.OperationEvidenceLost(ctx, query.OperationEvidenceLostParams{
		OperationID: string(id), Attempt: strconv.FormatUint(uint64(attempt), 10),
		ThroughSequence: last,
	})
}

// OperationTx is the operation view of one StoreTx. It exposes persistence
// primitives only: authority, transition validity, message text and result
// synthesis stay in the calling domain.
type OperationTx struct{ owner *StoreTx }

// Operations returns the operation view of this transaction.
func (tx *StoreTx) Operations() *OperationTx { return &OperationTx{owner: tx} }

// Insert admits one operation. It returns false when the admission key already
// exists; the caller re-reads the identity to compare the input hash.
func (o *OperationTx) Insert(ctx context.Context, insert OperationInsert) (bool, error) {
	affected, err := o.owner.q.InsertOperation(ctx, query.InsertOperationParams{
		ID:             string(insert.ID),
		Kind:           insert.Key.Kind,
		Target:         insert.Key.Target,
		ActorKind:      insert.Key.Actor.Kind,
		ActorSubject:   insert.Key.Actor.Subject,
		IdempotencyKey: insert.Key.IdempotencyKey,
		InputHash:      insert.InputHash,
		Input:          insert.Input,
	})
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// Identity returns the operation already bound to an admission key.
func (o *OperationTx) Identity(ctx context.Context, key OperationKey) (OperationIdentity, bool, error) {
	return operationIdentityQuery(ctx, o.owner.q, key, false)
}

// IdentityForShare returns the identity bound to an admission key while holding
// the share lock in the caller's transaction until commit or rollback. A
// pool-scoped read would release the lock at statement end; this method is the
// transaction-scoped form used by speculative receipt reconciliation.
func (o *OperationTx) IdentityForShare(ctx context.Context, key OperationKey) (OperationIdentity, bool, error) {
	return operationIdentityQuery(ctx, o.owner.q, key, true)
}

// Input returns the canonical operation input and whether it exists.
func (o *OperationTx) Input(ctx context.Context, id contract.OperationID) (json.RawMessage, bool, error) {
	return operationInputQuery(ctx, o.owner.q, id)
}

// ListByIdentityPattern returns the operations matching one exact durable
// identity and an idempotency-key prefix, in creation order.
func (o *OperationTx) ListByIdentityPattern(ctx context.Context, filter OperationIdentityPattern) ([]contract.Operation, error) {
	rows, err := o.owner.q.ListOperationsByIdentityPattern(ctx, query.ListOperationsByIdentityPatternParams{
		Kind: filter.Kind, ActorKind: filter.Actor.Kind, ActorSubject: filter.Actor.Subject,
		Target: filter.Target, IdempotencyPattern: filter.IdempotencyPattern,
	})
	if err != nil {
		return nil, err
	}
	return operationsFromIdentityPattern(rows)
}

// Lock locks the operation row until the caller's transaction ends.
func (o *OperationTx) Lock(ctx context.Context, id contract.OperationID) (contract.Operation, bool, error) {
	row, err := o.owner.q.LockOperation(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.Operation{}, false, nil
	}
	if err != nil {
		return contract.Operation{}, false, err
	}
	op, err := operation(row.ID, row.Kind, row.Target, row.ActorKind, row.ActorSubject,
		row.State, row.Attempt, row.LastSequence, row.ErrorCode, row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return contract.Operation{}, false, err
	}
	return op, true, nil
}

// SetJob binds the durable operation to its River job.
func (o *OperationTx) SetJob(ctx context.Context, id contract.OperationID, jobID int64) error {
	return o.owner.q.SetOperationJob(ctx, query.SetOperationJobParams{ID: string(id), JobID: jobID})
}

// JobID returns the River job bound to the operation and whether one is bound.
func (o *OperationTx) JobID(ctx context.Context, id contract.OperationID) (int64, bool, error) {
	return operationJobID(ctx, o.owner.q, id)
}

// ClaimAttempt makes this attempt the owner and returns the stored input. A
// missing row is (nil, false, nil).
func (o *OperationTx) ClaimAttempt(ctx context.Context, id contract.OperationID, attempt uint32) (json.RawMessage, bool, error) {
	next, err := operationAttempt(attempt)
	if err != nil {
		return nil, false, err
	}
	raw, err := o.owner.q.ClaimOperationAttempt(ctx, query.ClaimOperationAttemptParams{
		ID: string(id), Attempt: next,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// SetAttempt writes the attempt counter without reading the input.
func (o *OperationTx) SetAttempt(ctx context.Context, id contract.OperationID, attempt uint32) error {
	next, err := operationAttempt(attempt)
	if err != nil {
		return err
	}
	return o.owner.q.SetOperationAttempt(ctx, query.SetOperationAttemptParams{ID: string(id), Attempt: next})
}

// SetState writes the operation state and its machine-readable error code. It
// does not validate the transition; the caller owns that decision.
func (o *OperationTx) SetState(ctx context.Context, id contract.OperationID, state contract.State, code contract.Code) error {
	return o.owner.q.SetOperationState(ctx, query.SetOperationStateParams{
		ID: string(id), State: string(state), ErrorCode: string(code),
	})
}

// SetCompletedSteps writes the acknowledged completed-step count.
func (o *OperationTx) SetCompletedSteps(ctx context.Context, id contract.OperationID, completed uint32) error {
	next, err := operationAttempt(completed)
	if err != nil {
		return err
	}
	return o.owner.q.SetOperationCompletedSteps(ctx, query.SetOperationCompletedStepsParams{
		ID: string(id), CompletedSteps: next,
	})
}

// AppendEvent inserts one event at the next accepted sequence and advances the
// operation's sequence and update time in the same transaction. The event must
// already be validated and carry any synthesized result; this method performs
// no validation and no result synthesis.
func (o *OperationTx) AppendEvent(ctx context.Context, op *contract.Operation, event contract.Event) error {
	if op.LastSequence == math.MaxUint64 {
		return errors.New("entities: operation event sequence is exhausted")
	}
	sequence := op.LastSequence + 1
	stored, err := operationBigint(sequence)
	if err != nil {
		return err
	}
	at := time.Now().UTC()
	event.OperationID, event.Sequence, event.Attempt, event.At = op.ID, sequence, op.Attempt, at
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if err := o.owner.q.InsertOperationEvent(ctx, query.InsertOperationEventParams{
		OperationID: string(op.ID), Sequence: stored, Event: raw,
	}); err != nil {
		return err
	}
	if err := o.owner.q.SetOperationSequence(ctx, query.SetOperationSequenceParams{
		ID: string(op.ID), LastSequence: stored, UpdatedAt: pgtype.Timestamptz{Time: at, Valid: true},
	}); err != nil {
		return err
	}
	op.LastSequence = sequence
	op.UpdatedAt = at
	return nil
}

func operationIdentityQuery(ctx context.Context, q *query.Queries, key OperationKey, forShare bool) (OperationIdentity, bool, error) {
	params := query.GetOperationIdentityParams{
		ActorKind: key.Actor.Kind, ActorSubject: key.Actor.Subject,
		Kind: key.Kind, Target: key.Target, IdempotencyKey: key.IdempotencyKey,
	}
	var (
		row query.GetOperationIdentityRow
		err error
	)
	if forShare {
		shared, shareErr := q.GetOperationIdentityForShare(ctx, query.GetOperationIdentityForShareParams{
			ActorKind: params.ActorKind, ActorSubject: params.ActorSubject,
			Kind: params.Kind, Target: params.Target, IdempotencyKey: params.IdempotencyKey,
		})
		if shareErr != nil {
			err = shareErr
		} else {
			row = query.GetOperationIdentityRow{ID: shared.ID, InputHash: shared.InputHash}
		}
	} else {
		row, err = q.GetOperationIdentity(ctx, params)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return OperationIdentity{}, false, nil
	}
	if err != nil {
		return OperationIdentity{}, false, err
	}
	identity := OperationIdentity{ID: contract.OperationID(row.ID), InputHash: row.InputHash}
	if err := identity.ID.Validate(); err != nil {
		return OperationIdentity{}, false, err
	}
	return identity, true, nil
}

func operationJobID(ctx context.Context, q *query.Queries, id contract.OperationID) (int64, bool, error) {
	job, err := q.GetOperationJob(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if job == nil {
		return 0, false, nil
	}
	return *job, true, nil
}

func operationInputQuery(ctx context.Context, q *query.Queries, id contract.OperationID) (json.RawMessage, bool, error) {
	raw, err := q.GetOperationInput(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func operationsFromAllActors(rows []query.ListOperationsAllActorsRow) ([]contract.Operation, error) {
	result := make([]contract.Operation, 0, len(rows))
	for _, row := range rows {
		item, err := operation(row.ID, row.Kind, row.Target, row.ActorKind, row.ActorSubject,
			row.State, row.Attempt, row.LastSequence, row.ErrorCode, row.CreatedAt, row.UpdatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func operationsFromForActor(rows []query.ListOperationsForActorRow) ([]contract.Operation, error) {
	result := make([]contract.Operation, 0, len(rows))
	for _, row := range rows {
		item, err := operation(row.ID, row.Kind, row.Target, row.ActorKind, row.ActorSubject,
			row.State, row.Attempt, row.LastSequence, row.ErrorCode, row.CreatedAt, row.UpdatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func operationsFromByApp(rows []query.ListOperationsByAppRow) ([]contract.Operation, error) {
	result := make([]contract.Operation, 0, len(rows))
	for _, row := range rows {
		item, err := operation(row.ID, row.Kind, row.Target, row.ActorKind, row.ActorSubject,
			row.State, row.Attempt, row.LastSequence, row.ErrorCode, row.CreatedAt, row.UpdatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func operationsFromByAppEnvironment(rows []query.ListOperationsByAppEnvironmentRow) ([]contract.Operation, error) {
	result := make([]contract.Operation, 0, len(rows))
	for _, row := range rows {
		item, err := operation(row.ID, row.Kind, row.Target, row.ActorKind, row.ActorSubject,
			row.State, row.Attempt, row.LastSequence, row.ErrorCode, row.CreatedAt, row.UpdatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func operationsFromIdentityPattern(rows []query.ListOperationsByIdentityPatternRow) ([]contract.Operation, error) {
	result := make([]contract.Operation, 0, len(rows))
	for _, row := range rows {
		item, err := operation(row.ID, row.Kind, row.Target, row.ActorKind, row.ActorSubject,
			row.State, row.Attempt, row.LastSequence, row.ErrorCode, row.CreatedAt, row.UpdatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

// operation maps one generated operation row to the contract snapshot. It
// converts every integer without wrapping and rejects a malformed timestamp;
// it does not validate state transitions or business rules.
func operation(id, kind, target, actorKind, actorSubject, state string, attempt, lastSequence int64,
	errorCode string, createdAt, updatedAt pgtype.Timestamptz,
) (contract.Operation, error) {
	current, err := operationAttemptValue(attempt)
	if err != nil {
		return contract.Operation{}, err
	}
	sequence, err := operationSequence(lastSequence)
	if err != nil {
		return contract.Operation{}, err
	}
	created, err := operationTimestamp(createdAt)
	if err != nil {
		return contract.Operation{}, err
	}
	updated, err := operationTimestamp(updatedAt)
	if err != nil {
		return contract.Operation{}, err
	}
	return contract.Operation{
		ID:           contract.OperationID(id),
		Kind:         kind,
		Target:       target,
		Actor:        contract.Actor{Kind: actorKind, Subject: actorSubject},
		State:        contract.State(state),
		Attempt:      current,
		LastSequence: sequence,
		ErrorCode:    contract.Code(errorCode),
		CreatedAt:    created,
		UpdatedAt:    updated,
	}, nil
}

func operationAttempt(attempt uint32) (int64, error) {
	if uint64(attempt) > math.MaxInt64 {
		return 0, errors.New("entities: operation attempt is outside the bigint range")
	}
	return int64(attempt), nil
}

func operationAttemptValue(attempt int64) (uint32, error) {
	if attempt < 0 || attempt > math.MaxUint32 {
		return 0, errors.New("entities: stored operation attempt is outside the supported range")
	}
	return uint32(attempt), nil
}

func operationSequence(sequence int64) (uint64, error) {
	if sequence < 0 {
		return 0, errors.New("entities: stored operation sequence is outside the bigint range")
	}
	return uint64(sequence), nil
}

func operationBigint(sequence uint64) (int64, error) {
	if sequence > math.MaxInt64 {
		return 0, errors.New("entities: operation sequence is outside the bigint range")
	}
	return int64(sequence), nil
}

func operationTimestamp(value pgtype.Timestamptz) (time.Time, error) {
	if !value.Valid {
		return time.Time{}, errors.New("entities: operation timestamp is missing")
	}
	return value.Time.UTC(), nil
}

func optionalText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

// Get reads one operation inside the caller's transaction without a lock.
func (o *OperationTx) Get(ctx context.Context, id contract.OperationID) (contract.Operation, bool, error) {
	row, err := o.owner.q.GetOperation(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.Operation{}, false, nil
	}
	if err != nil {
		return contract.Operation{}, false, err
	}
	op, err := operation(row.ID, row.Kind, row.Target, row.ActorKind, row.ActorSubject,
		row.State, row.Attempt, row.LastSequence, row.ErrorCode, row.CreatedAt, row.UpdatedAt)
	return op, err == nil, err
}

// StateAttempt is the narrow state/attempt read used by DNS and retention
// guards. A missing operation is (zero,false,nil).
func (o *OperationStore) StateAttempt(ctx context.Context, id contract.OperationID) (contract.State, uint32, bool, error) {
	return operationStateAttempt(ctx, o.q, id)
}

// StateAttempt is the transaction-scoped narrow state/attempt read.
func (o *OperationTx) StateAttempt(ctx context.Context, id contract.OperationID) (contract.State, uint32, bool, error) {
	return operationStateAttempt(ctx, o.owner.q, id)
}

func operationStateAttempt(ctx context.Context, q *query.Queries, id contract.OperationID) (contract.State, uint32, bool, error) {
	row, err := q.GetOperationStateAttempt(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	attempt, err := operationAttemptValue(row.Attempt)
	if err != nil {
		return "", 0, false, err
	}
	return contract.State(row.State), attempt, true, nil
}

// LastSequence reads the last accepted event sequence. A missing operation is
// (0,false,nil).
func (o *OperationStore) LastSequence(ctx context.Context, id contract.OperationID) (uint64, bool, error) {
	return operationLastSequence(ctx, o.q, id)
}

// LastSequence reads the exact caller transaction.
func (o *OperationTx) LastSequence(ctx context.Context, id contract.OperationID) (uint64, bool, error) {
	return operationLastSequence(ctx, o.owner.q, id)
}

func operationLastSequence(ctx context.Context, q *query.Queries, id contract.OperationID) (uint64, bool, error) {
	value, err := q.GetOperationLastSequence(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	sequence, err := operationSequence(value)
	return sequence, err == nil, err
}
