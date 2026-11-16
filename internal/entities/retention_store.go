package entities

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

// RetentionRun is one durable retention plan/result. JSON payloads are exact;
// timestamps and optional cache columns stay nullable.
type RetentionRun struct {
	ID                    string
	OperationID           contract.OperationID
	Status                string
	PlannedItems          json.RawMessage
	RemovedItems          json.RawMessage
	PlannedBytes          int64
	ReclaimedBytes        int64
	Errors                json.RawMessage
	StartedAt             time.Time
	FinishedAt            *time.Time
	CacheBytes            *int64
	CacheMeasuredAt       *time.Time
	CacheMeasurementError string
	GCGeneration          int
}

// RetentionRunInsert is one run row, including a retry after a failed plan.
type RetentionRunInsert struct {
	ID                    string
	OperationID           contract.OperationID
	Status                string
	PlannedItems          json.RawMessage
	RemovedItems          json.RawMessage
	PlannedBytes          int64
	ReclaimedBytes        int64
	Errors                json.RawMessage
	StartedAt             time.Time
	FinishedAt            *time.Time
	CacheBytes            *int64
	CacheMeasuredAt       *time.Time
	CacheMeasurementError string
	GCGeneration          int
}

// RetentionServerSettings is the singleton policy and node-wide GC flag.
type RetentionServerSettings struct {
	Retention json.RawMessage
	GCPending bool
	UpdatedAt time.Time
}

// RetentionRunFacts is the doctor-facing bounded summary of the newest run.
type RetentionRunFacts struct {
	Status                string
	StartedAt             time.Time
	FinishedAt            *time.Time
	PlannedItems          int
	RemovedItems          int
	PlannedBytes          int64
	ReclaimedBytes        int64
	Errors                int
	ErrorsRaw             json.RawMessage
	FirstError            string
	CacheBytes            *int64
	CacheMeasuredAt       *time.Time
	CacheMeasurementError string
}

// RetentionStore is the retention run/settings view over the Store pool.
type RetentionStore struct{ q *query.Queries }

// Retention returns the retention view of this Store.
func (s *Store) Retention() *RetentionStore { return &RetentionStore{q: query.New(s.pool)} }

// RetentionTx is the retention view of one StoreTx. It reads and writes the
// caller's exact transaction, including nested savepoints, so uncommitted run
// and settings rows are visible only inside that scope.
type RetentionTx struct{ q *query.Queries }

// Retention returns the retention view of this transaction.
func (tx *StoreTx) Retention() *RetentionTx { return &RetentionTx{q: tx.q} }

// GetServerSettings reads the singleton. A missing row is (zero,false,nil).
func (s *RetentionStore) GetServerSettings(ctx context.Context) (RetentionServerSettings, bool, error) {
	return retentionServerSettings(ctx, s.q)
}

// GetServerSettings reads the singleton on the caller transaction.
func (tx *RetentionTx) GetServerSettings(ctx context.Context) (RetentionServerSettings, bool, error) {
	return retentionServerSettings(ctx, tx.q)
}

func retentionServerSettings(ctx context.Context, q *query.Queries) (RetentionServerSettings, bool, error) {
	row, err := q.GetServerRetentionSettings(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return RetentionServerSettings{}, false, nil
	}
	if err != nil {
		return RetentionServerSettings{}, false, err
	}
	updated, err := platformTime(row.UpdatedAt)
	if err != nil {
		return RetentionServerSettings{}, false, err
	}
	return RetentionServerSettings{
		Retention: row.Retention, GCPending: row.RetentionGcPending,
		UpdatedAt: updated,
	}, true, nil
}

// SetServerPolicy upserts only the policy block; the GC flag is preserved.
func (s *RetentionStore) SetServerPolicy(ctx context.Context, retention json.RawMessage) error {
	return retentionSetServerPolicy(ctx, s.q, retention)
}

// SetServerPolicy upserts the policy block on the caller transaction.
func (tx *RetentionTx) SetServerPolicy(ctx context.Context, retention json.RawMessage) error {
	return retentionSetServerPolicy(ctx, tx.q, retention)
}

func retentionSetServerPolicy(ctx context.Context, q *query.Queries, retention json.RawMessage) error {
	if len(retention) == 0 {
		return errors.New("entities: retention policy is required")
	}
	return q.SetServerRetentionPolicy(ctx, retention)
}

// GCPending reads the node-wide GC follow-up flag, false when absent.
func (s *RetentionStore) GCPending(ctx context.Context) (bool, error) {
	return s.q.GetRetentionGCPending(ctx)
}

// GCPending reads the node-wide GC follow-up flag on the caller transaction.
func (tx *RetentionTx) GCPending(ctx context.Context) (bool, error) {
	return tx.q.GetRetentionGCPending(ctx)
}

// SetGCPending upserts the node-wide GC follow-up flag.
func (s *RetentionStore) SetGCPending(ctx context.Context, pending bool) error {
	return s.q.SetRetentionGCPending(ctx, pending)
}

// SetGCPending upserts the node-wide flag on the caller transaction.
func (tx *RetentionTx) SetGCPending(ctx context.Context, pending bool) error {
	return tx.q.SetRetentionGCPending(ctx, pending)
}

// LatestRunForOperation returns the newest run, preserving retry reuse.
func (s *RetentionStore) LatestRunForOperation(ctx context.Context, operationID contract.OperationID) (RetentionRun, bool, error) {
	return retentionLatestRun(ctx, s.q, operationID)
}

// LatestRunForOperation reads the newest run on the caller transaction.
func (tx *RetentionTx) LatestRunForOperation(ctx context.Context, operationID contract.OperationID) (RetentionRun, bool, error) {
	return retentionLatestRun(ctx, tx.q, operationID)
}

func retentionLatestRun(ctx context.Context, q *query.Queries, operationID contract.OperationID) (RetentionRun, bool, error) {
	row, err := q.GetLatestRetentionRunForOperation(ctx, string(operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RetentionRun{}, false, nil
	}
	if err != nil {
		return RetentionRun{}, false, err
	}
	generation, err := platformInt(row.GcGeneration)
	if err != nil {
		return RetentionRun{}, false, err
	}
	started, err := platformTime(row.StartedAt)
	if err != nil {
		return RetentionRun{}, false, err
	}
	return RetentionRun{
		ID: row.ID, OperationID: contract.OperationID(row.OperationID), Status: row.Status,
		PlannedItems: row.PlannedItems, RemovedItems: row.RemovedItems,
		PlannedBytes: row.PlannedBytes, ReclaimedBytes: row.ReclaimedBytes, Errors: row.Errors,
		StartedAt: started, FinishedAt: platformTimePtr(row.FinishedAt),
		CacheBytes: row.CacheBytes, CacheMeasuredAt: platformTimePtr(row.CacheMeasuredAt),
		CacheMeasurementError: row.CacheMeasurementError, GCGeneration: generation,
	}, true, nil
}

// InsertRun writes one retention run row.
func (s *RetentionStore) InsertRun(ctx context.Context, insert RetentionRunInsert) error {
	return retentionInsertRun(ctx, s.q, insert)
}

// InsertRun writes one run on the caller transaction.
func (tx *RetentionTx) InsertRun(ctx context.Context, insert RetentionRunInsert) error {
	return retentionInsertRun(ctx, tx.q, insert)
}

func retentionInsertRun(ctx context.Context, q *query.Queries, insert RetentionRunInsert) error {
	if insert.GCGeneration < 0 {
		return errors.New("entities: retention GC generation must be non-negative")
	}
	return q.InsertRetentionRun(ctx, query.InsertRetentionRunParams{
		ID: insert.ID, OperationID: retentionOperation(insert.OperationID), Status: insert.Status,
		PlannedItems: insert.PlannedItems, RemovedItems: insert.RemovedItems,
		PlannedBytes: insert.PlannedBytes, ReclaimedBytes: insert.ReclaimedBytes,
		Errors: insert.Errors, StartedAt: retentionTime(insert.StartedAt),
		FinishedAt: platformOptionalTime(insert.FinishedAt), CacheBytes: insert.CacheBytes,
		CacheMeasuredAt:       platformOptionalTime(insert.CacheMeasuredAt),
		CacheMeasurementError: insert.CacheMeasurementError, GcGeneration: int64(insert.GCGeneration),
	})
}

// UpdateRun rewrites the mutable run fields for one ID.
func (s *RetentionStore) UpdateRun(ctx context.Context, run RetentionRun) error {
	return retentionUpdateRun(ctx, s.q, run)
}

// UpdateRun rewrites one run on the caller transaction.
func (tx *RetentionTx) UpdateRun(ctx context.Context, run RetentionRun) error {
	return retentionUpdateRun(ctx, tx.q, run)
}

func retentionUpdateRun(ctx context.Context, q *query.Queries, run RetentionRun) error {
	if run.GCGeneration < 0 {
		return errors.New("entities: retention GC generation must be non-negative")
	}
	return q.UpdateRetentionRun(ctx, query.UpdateRetentionRunParams{
		ID: run.ID, Status: run.Status, PlannedItems: run.PlannedItems,
		RemovedItems: run.RemovedItems, PlannedBytes: run.PlannedBytes,
		ReclaimedBytes: run.ReclaimedBytes, Errors: run.Errors,
		FinishedAt: platformOptionalTime(run.FinishedAt), CacheBytes: run.CacheBytes,
		CacheMeasuredAt:       platformOptionalTime(run.CacheMeasuredAt),
		CacheMeasurementError: run.CacheMeasurementError, GcGeneration: int64(run.GCGeneration),
	})
}

// LastRunFacts reads the doctor-facing facts of the newest run.
func (s *RetentionStore) LastRunFacts(ctx context.Context) (RetentionRunFacts, bool, error) {
	return retentionLastRunFacts(ctx, s.q)
}

// LastRunFacts reads the newest run facts on the caller transaction.
func (tx *RetentionTx) LastRunFacts(ctx context.Context) (RetentionRunFacts, bool, error) {
	return retentionLastRunFacts(ctx, tx.q)
}

func retentionLastRunFacts(ctx context.Context, q *query.Queries) (RetentionRunFacts, bool, error) {
	row, err := q.GetLastRetentionRunFacts(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return RetentionRunFacts{}, false, nil
	}
	if err != nil {
		return RetentionRunFacts{}, false, err
	}
	planned, err := platformInt(int64(row.JsonbArrayLength))
	if err != nil {
		return RetentionRunFacts{}, false, err
	}
	removed, err := platformInt(int64(row.JsonbArrayLength_2))
	if err != nil {
		return RetentionRunFacts{}, false, err
	}
	errorsCount, err := platformInt(int64(row.JsonbArrayLength_3))
	if err != nil {
		return RetentionRunFacts{}, false, err
	}
	started, err := platformTime(row.StartedAt)
	if err != nil {
		return RetentionRunFacts{}, false, err
	}
	return RetentionRunFacts{
		Status: row.Status, StartedAt: started,
		FinishedAt:   platformTimePtr(row.FinishedAt),
		PlannedItems: planned, RemovedItems: removed,
		PlannedBytes: row.PlannedBytes, ReclaimedBytes: row.ReclaimedBytes,
		Errors: errorsCount, ErrorsRaw: row.Errors, FirstError: row.Column10,
		CacheBytes: row.CacheBytes, CacheMeasuredAt: platformTimePtr(row.CacheMeasuredAt),
		CacheMeasurementError: row.CacheMeasurementError,
	}, true, nil
}

// LastSuccessAt returns the newest succeeded run time, or false when none exists.
func (s *RetentionStore) LastSuccessAt(ctx context.Context) (*time.Time, bool, error) {
	return retentionLastSuccess(ctx, s.q)
}

// LastSuccessAt reads the newest successful run on the caller transaction.
func (tx *RetentionTx) LastSuccessAt(ctx context.Context) (*time.Time, bool, error) {
	return retentionLastSuccess(ctx, tx.q)
}

func retentionLastSuccess(ctx context.Context, q *query.Queries) (*time.Time, bool, error) {
	value, err := q.GetLastSuccessfulRetentionAt(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return platformTimePtr(value), value.Valid, nil
}

func retentionOperation(id contract.OperationID) pgtype.Text {
	if id == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: string(id), Valid: true}
}

func retentionTime(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: !value.IsZero()}
}
