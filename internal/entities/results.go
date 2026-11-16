package entities

import (
	"context"
	"errors"
	"strconv"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

// CandidateDiagnostic is one retained redacted candidate capture event.
type CandidateDiagnostic struct {
	Sequence         uint64
	Status           string
	Message          string
	RedactionContext string
	Step             string
	DeploymentID     string
}

// ResultStore is the result/action projection read view over the Store pool.
type ResultStore struct{ q *query.Queries }

// Results returns the result projection view of this Store.
func (s *Store) Results() *ResultStore { return &ResultStore{q: query.New(s.pool)} }

// ResultTx is the result/action projection view of one StoreTx. It reads the
// caller's exact transaction, including a nested savepoint, so uncommitted
// events and steps are visible until that scope rolls back or commits.
type ResultTx struct{ q *query.Queries }

// Results returns the result projection view of this transaction.
func (tx *StoreTx) Results() *ResultTx { return &ResultTx{q: tx.q} }

// CandidateDiagnostics reads the retained candidate diagnostics for one
// attempt through the cursor, in sequence order.
func (s *ResultStore) CandidateDiagnostics(ctx context.Context, id contract.OperationID, attempt uint32, through uint64) ([]CandidateDiagnostic, error) {
	return candidateDiagnostics(ctx, s.q, id, attempt, through)
}

// CandidateDiagnostics reads uncommitted or committed diagnostics on the exact
// caller transaction.
func (tx *ResultTx) CandidateDiagnostics(ctx context.Context, id contract.OperationID, attempt uint32, through uint64) ([]CandidateDiagnostic, error) {
	return candidateDiagnostics(ctx, tx.q, id, attempt, through)
}

func candidateDiagnostics(ctx context.Context, q *query.Queries, id contract.OperationID, attempt uint32, through uint64) ([]CandidateDiagnostic, error) {
	throughValue, err := operationBigint(through)
	if err != nil {
		return nil, err
	}
	rows, err := q.ListCandidateDiagnosticEvents(ctx, query.ListCandidateDiagnosticEventsParams{
		OperationID: string(id), Attempt: contractAttemptText(attempt), ThroughSequence: throughValue,
	})
	if err != nil {
		return nil, err
	}
	result := make([]CandidateDiagnostic, 0, len(rows))
	for _, row := range rows {
		sequence, err := operationSequence(row.Sequence)
		if err != nil {
			return nil, err
		}
		result = append(result, CandidateDiagnostic{
			Sequence: sequence, Status: row.Column2, Message: row.Column3,
			RedactionContext: row.Column4, Step: row.Column5, DeploymentID: row.Column6,
		})
	}
	return result, nil
}

// FailedStage is the existing warning-then-step fallback used by result
// enrichment. The query always returns one non-empty stage.
func (s *ResultStore) FailedStage(ctx context.Context, id contract.OperationID, attempt uint32) (string, error) {
	return failedStage(ctx, s.q, id, attempt)
}

// FailedStage reads the fallback stage on the exact caller transaction.
func (tx *ResultTx) FailedStage(ctx context.Context, id contract.OperationID, attempt uint32) (string, error) {
	return failedStage(ctx, tx.q, id, attempt)
}

func failedStage(ctx context.Context, q *query.Queries, id contract.OperationID, attempt uint32) (string, error) {
	stage, err := q.GetFailedResultStage(ctx, query.GetFailedResultStageParams{
		OperationID: string(id), AttemptText: contractAttemptText(attempt), Attempt: int64(attempt),
	})
	if err != nil {
		return "", err
	}
	if stage == "" {
		return "", errors.New("entities: failed result stage is empty")
	}
	return stage, nil
}

func contractAttemptText(attempt uint32) string {
	return strconv.FormatUint(uint64(attempt), 10)
}
