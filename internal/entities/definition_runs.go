package entities

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

// DefinitionRun is one immutable approved definition journal header.
type DefinitionRun struct {
	OperationID    contract.OperationID
	PlanNonce      string
	EnvironmentID  string
	Approved       json.RawMessage
	NextStep       int
	FailureCode    contract.Code
	FailureMessage string
	FinishedAt     *time.Time
}

// DefinitionRunInsert freezes an approved run before its steps are inserted.
type DefinitionRunInsert struct {
	OperationID    contract.OperationID
	PlanNonce      string
	EnvironmentID  string
	Approved       json.RawMessage
	NextStep       int
	FailureCode    contract.Code
	FailureMessage string
}

// DefinitionRunJournal is the locked journal header joined with its operation.
type DefinitionRunJournal struct {
	Approved       json.RawMessage
	EnvironmentID  string
	NextStep       int
	FailureCode    contract.Code
	FailureMessage string
	SameInput      bool
	Finished       bool
}

// DefinitionRunStore is the read view of definition-run journal rows.
type DefinitionRunStore struct{ q *query.Queries }

// DefinitionRuns returns the run-journal read view of this Store.
func (s *Store) DefinitionRuns() *DefinitionRunStore {
	return &DefinitionRunStore{q: query.New(s.pool)}
}

// DefinitionRunTx is the run-journal view of one StoreTx.
type DefinitionRunTx struct{ q *query.Queries }

// DefinitionRuns returns the run-journal view of this transaction.
func (tx *StoreTx) DefinitionRuns() *DefinitionRunTx { return &DefinitionRunTx{q: tx.q} }

func (s *DefinitionRunStore) Get(ctx context.Context, id contract.OperationID) (DefinitionRun, bool, error) {
	return definitionRunGet(ctx, s.q, id)
}

func (tx *DefinitionRunTx) Get(ctx context.Context, id contract.OperationID) (DefinitionRun, bool, error) {
	return definitionRunGet(ctx, tx.q, id)
}

func definitionRunGet(ctx context.Context, q *query.Queries, id contract.OperationID) (DefinitionRun, bool, error) {
	row, err := q.GetDefinitionRun(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return DefinitionRun{}, false, nil
	}
	if err != nil {
		return DefinitionRun{}, false, err
	}
	run, err := definitionRun(row)
	return run, err == nil, err
}

// Lock reads and row-locks the run. It is the run-only step in the defined
// parent -> run -> step -> child lock order.
func (tx *DefinitionRunTx) Lock(ctx context.Context, id contract.OperationID) (DefinitionRun, bool, error) {
	row, err := tx.q.LockDefinitionRun(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return DefinitionRun{}, false, nil
	}
	if err != nil {
		return DefinitionRun{}, false, err
	}
	run, err := definitionRun(row)
	return run, err == nil, err
}

// LockJournal locks only the run row and returns the frozen header plus the
// operation's input equality in the same SQL snapshot.
func (tx *DefinitionRunTx) LockJournal(ctx context.Context, id contract.OperationID) (DefinitionRunJournal, bool, error) {
	row, err := tx.q.LockDefinitionRunJournal(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return DefinitionRunJournal{}, false, nil
	}
	if err != nil {
		return DefinitionRunJournal{}, false, err
	}
	next, err := platformInt(row.NextStep)
	if err != nil {
		return DefinitionRunJournal{}, false, err
	}
	return DefinitionRunJournal{
		Approved: row.Approved, EnvironmentID: row.EnvironmentID, NextStep: next,
		FailureCode: contract.Code(row.FailureCode), FailureMessage: row.FailureMessage,
		SameInput: row.SameInput, Finished: row.Finished,
	}, true, nil
}

// Insert appends the approved run. A unique violation remains the caller's
// active-environment or nonce conflict.
func (tx *DefinitionRunTx) Insert(ctx context.Context, insert DefinitionRunInsert) error {
	next, err := definitionNonNegativeInt(insert.NextStep)
	if err != nil {
		return err
	}
	return tx.q.InsertDefinitionRun(ctx, query.InsertDefinitionRunParams{
		OperationID: string(insert.OperationID), PlanNonce: insert.PlanNonce,
		EnvironmentID: insert.EnvironmentID, Approved: insert.Approved,
		NextStep: next, FailureCode: string(insert.FailureCode), FailureMessage: insert.FailureMessage,
	})
}

// SetFailure records the first failure intent only while the run is unfinished.
func (tx *DefinitionRunTx) SetFailure(ctx context.Context, id contract.OperationID, code contract.Code, message string) (bool, error) {
	rows, err := tx.q.SetDefinitionRunFailure(ctx, query.SetDefinitionRunFailureParams{
		OperationID: string(id), FailureCode: string(code), FailureMessage: message,
	})
	return rows == 1, err
}

// Finish stamps the run finished once.
func (tx *DefinitionRunTx) Finish(ctx context.Context, id contract.OperationID) (bool, error) {
	rows, err := tx.q.FinishDefinitionRun(ctx, string(id))
	return rows == 1, err
}

// Advance moves the cursor only when it still equals the expected ordinal.
func (tx *DefinitionRunTx) Advance(ctx context.Context, id contract.OperationID, expectedNext int) (bool, error) {
	next, err := definitionNonNegativeInt(expectedNext)
	if err != nil {
		return false, err
	}
	rows, err := tx.q.AdvanceDefinitionRunNextStep(ctx, query.AdvanceDefinitionRunNextStepParams{
		OperationID: string(id), ExpectedNextStep: next,
	})
	return rows == 1, err
}

// ListUnfinishedTerminal returns terminal operations whose run header was not
// finished, ordered by operation update time.
func (s *DefinitionRunStore) ListUnfinishedTerminal(ctx context.Context, limit int) ([]contract.OperationID, error) {
	if limit < 1 {
		return nil, errors.New("entities: definition run limit must be positive")
	}
	ids, err := s.q.ListUnfinishedTerminalDefinitionRuns(ctx, int64(limit))
	if err != nil {
		return nil, err
	}
	result := make([]contract.OperationID, 0, len(ids))
	for _, id := range ids {
		result = append(result, contract.OperationID(id))
	}
	return result, nil
}

func definitionRun(row query.EregionDefinitionRun) (DefinitionRun, error) {
	next, err := platformInt(row.NextStep)
	if err != nil {
		return DefinitionRun{}, err
	}
	return DefinitionRun{
		OperationID: contract.OperationID(row.OperationID), PlanNonce: row.PlanNonce,
		EnvironmentID: row.EnvironmentID, Approved: row.Approved, NextStep: next,
		FailureCode: contract.Code(row.FailureCode), FailureMessage: row.FailureMessage,
		FinishedAt: platformTimePtr(row.FinishedAt),
	}, nil
}

func definitionNonNegativeInt(value int) (int64, error) {
	if value < 0 {
		return 0, errors.New("entities: definition integer must be non-negative")
	}
	return int64(value), nil
}

// DefinitionAdmissionGuard is the exact identity of one submission that an
// unfinished definition run may already own as its next linked child.
type DefinitionAdmissionGuard struct {
	App            string
	Environment    string
	ChildOperation contract.OperationID
	Kind           string
	Target         string
	Input          json.RawMessage
	ParentKind     string
	Actor          contract.Actor
}

// BlocksAdmission reports whether an unfinished run reserves the target for
// work other than the exact linked child described by the guard.
func (tx *DefinitionRunTx) BlocksAdmission(ctx context.Context, guard DefinitionAdmissionGuard) (bool, error) {
	input := guard.Input
	if len(input) == 0 {
		input = json.RawMessage("null")
	}
	return tx.q.DefinitionRunBlocksAdmission(ctx, query.DefinitionRunBlocksAdmissionParams{
		App: guard.App, Environment: guard.Environment,
		ChildOperationID: string(guard.ChildOperation), Kind: guard.Kind, Target: guard.Target,
		Input: input, ParentKind: guard.ParentKind,
		ActorKind: guard.Actor.Kind, ActorSubject: guard.Actor.Subject,
	})
}
