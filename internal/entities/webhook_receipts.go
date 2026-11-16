package entities

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
	"github.com/mbvlabs/eregion/internal/entities/internal/riverquery"
)

// WebhookReceipt is one sanitized durable delivery admission. Secret revisions
// and job IDs are validated into their bounded Go types on read.
type WebhookReceipt struct {
	DeliveryIDHash     string
	BodySHA256         string
	Kind               string
	Target             string
	State              string
	EnvironmentID      string
	RevisionID         string
	SecretRevision     uint64
	SecretRevisionHash string
	Repository         string
	Branch             string
	Commit             string
	OperationID        contract.OperationID
	JobID              int64
	AdmissionAttempts  uint32
	ErrorCode          contract.Code
	ErrorDetail        string
	FirstSeenAt        time.Time
	Actor              contract.Actor
}

// WebhookReceiptInsert is one first admission. A duplicate key is reported as
// inserted=false without an error.
type WebhookReceiptInsert struct {
	DeliveryIDHash     string
	BodySHA256         string
	Actor              contract.Actor
	Kind               string
	Target             string
	EnvironmentID      string
	RevisionID         string
	SecretRevision     uint64
	SecretRevisionHash string
	Repository         string
	Branch             string
	Commit             string
	JobID              int64
}

// WebhookReceiptIdentity is a receipt that needs a replacement River job.
type WebhookReceiptIdentity struct {
	EnvironmentID  string
	DeliveryIDHash string
}

// WebhookStore is the pool-scoped webhook receipt view.
type WebhookStore struct {
	q     *query.Queries
	river *riverquery.Queries
}

// Webhooks returns the webhook receipt view of this Store.
func (s *Store) Webhooks() *WebhookStore {
	return &WebhookStore{q: query.New(s.pool), river: riverquery.New(s.pool)}
}

// WebhookTx is the transaction-scoped webhook receipt view.
type WebhookTx struct{ q *query.Queries }

// Webhooks returns the webhook receipt view of this transaction.
func (tx *StoreTx) Webhooks() *WebhookTx { return &WebhookTx{q: tx.q} }

// Get reads one receipt without a lock.
func (s *WebhookStore) Get(ctx context.Context, environmentID, deliveryIDHash string) (WebhookReceipt, bool, error) {
	return webhookGet(ctx, s.q, environmentID, deliveryIDHash, false)
}

// Lock reads and row-locks one receipt.
func (tx *WebhookTx) Lock(ctx context.Context, environmentID, deliveryIDHash string) (WebhookReceipt, bool, error) {
	return webhookGet(ctx, tx.q, environmentID, deliveryIDHash, true)
}

func webhookGet(ctx context.Context, q *query.Queries, environmentID, deliveryIDHash string, lock bool) (WebhookReceipt, bool, error) {
	row, found, err := webhookReceiptRow(ctx, q, environmentID, deliveryIDHash, lock)
	if err != nil || !found {
		return WebhookReceipt{}, false, err
	}
	secretRevision, err := platformUint64(row.SecretRevision)
	if err != nil {
		return WebhookReceipt{}, false, err
	}
	attempts, err := platformUint32(row.AdmissionAttempts)
	if err != nil {
		return WebhookReceipt{}, false, err
	}
	if row.JobID < 0 {
		return WebhookReceipt{}, false, errors.New("entities: webhook receipt job id is negative")
	}
	firstSeen, err := platformTime(row.FirstSeenAt)
	if err != nil {
		return WebhookReceipt{}, false, err
	}
	return WebhookReceipt{
		DeliveryIDHash: row.DeliveryIDHash, BodySHA256: row.BodySha256,
		Kind: row.Kind, Target: row.Target, State: row.State,
		EnvironmentID: row.EnvironmentID, RevisionID: row.RevisionID,
		SecretRevision: secretRevision, SecretRevisionHash: row.SecretRevisionHash,
		Repository: row.Repository, Branch: row.Branch, Commit: row.CommitSha,
		OperationID: contract.OperationID(row.OperationID), JobID: row.JobID,
		AdmissionAttempts: attempts, ErrorCode: contract.Code(row.ErrorCode),
		FirstSeenAt: firstSeen,
		Actor:       contract.Actor{Kind: row.ActorKind, Subject: row.ActorSubject},
	}, true, nil
}

func webhookReceiptRow(ctx context.Context, q *query.Queries, environmentID, deliveryIDHash string, lock bool) (query.GetWebhookReceiptRow, bool, error) {
	if !lock {
		row, err := q.GetWebhookReceipt(ctx, query.GetWebhookReceiptParams{EnvironmentID: environmentID, DeliveryIDHash: deliveryIDHash})
		if errors.Is(err, pgx.ErrNoRows) {
			return query.GetWebhookReceiptRow{}, false, nil
		}
		if err != nil {
			return query.GetWebhookReceiptRow{}, false, err
		}
		return row, true, nil
	}
	locked, err := q.LockWebhookReceipt(ctx, query.LockWebhookReceiptParams{EnvironmentID: environmentID, DeliveryIDHash: deliveryIDHash})
	if errors.Is(err, pgx.ErrNoRows) {
		return query.GetWebhookReceiptRow{}, false, nil
	}
	if err != nil {
		return query.GetWebhookReceiptRow{}, false, err
	}
	return query.GetWebhookReceiptRow(locked), true, nil
}

// Admit inserts the first receipt and returns its database first_seen_at. false
// means the primary key already exists; the caller re-reads under its lock.
func (tx *WebhookTx) Admit(ctx context.Context, insert WebhookReceiptInsert) (time.Time, bool, error) {
	revision, err := secretBigintValue(insert.SecretRevision)
	if err != nil {
		return time.Time{}, false, err
	}
	if insert.JobID <= 0 {
		return time.Time{}, false, errors.New("entities: webhook receipt requires a positive job id")
	}
	jobID := insert.JobID
	firstSeen, err := tx.q.InsertWebhookReceipt(ctx, query.InsertWebhookReceiptParams{
		DeliveryIDHash: insert.DeliveryIDHash, BodySha256: insert.BodySHA256,
		ActorKind: insert.Actor.Kind, ActorSubject: insert.Actor.Subject,
		Kind: insert.Kind, Target: insert.Target,
		EnvironmentID: insert.EnvironmentID, RevisionID: insert.RevisionID,
		SecretRevision: revision, SecretRevisionHash: insert.SecretRevisionHash,
		Repository: insert.Repository, Branch: insert.Branch, CommitSha: insert.Commit,
		JobID: &jobID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	seen, err := platformTime(firstSeen)
	if err != nil {
		return time.Time{}, false, err
	}
	return seen, true, nil
}

// Submit links the accepted operation under the expected receipt state.
func (tx *WebhookTx) Submit(ctx context.Context, environmentID, deliveryIDHash string, operationID contract.OperationID, expectedState string) (bool, error) {
	rows, err := tx.q.SubmitWebhookReceipt(ctx, query.SubmitWebhookReceiptParams{
		EnvironmentID: environmentID, DeliveryIDHash: deliveryIDHash,
		OperationID: string(operationID), ExpectedState: expectedState,
	})
	return rows == 1, err
}

// Fail records one sanitized terminal failure from the admitted state.
func (tx *WebhookTx) Fail(ctx context.Context, environmentID, deliveryIDHash string, code contract.Code, detail string) (bool, error) {
	rows, err := tx.q.FailWebhookReceipt(ctx, query.FailWebhookReceiptParams{
		EnvironmentID: environmentID, DeliveryIDHash: deliveryIDHash,
		ErrorCode: string(code), ErrorDetail: detail,
	})
	return rows == 1, err
}

// ReassignJob replaces an admitted receipt's River job and increments attempts.
func (tx *WebhookTx) ReassignJob(ctx context.Context, environmentID, deliveryIDHash string, jobID int64) (bool, error) {
	if jobID <= 0 {
		return false, errors.New("entities: webhook receipt requires a positive job id")
	}
	rows, err := tx.q.ReassignWebhookReceiptJob(ctx, query.ReassignWebhookReceiptJobParams{
		EnvironmentID: environmentID, DeliveryIDHash: deliveryIDHash, JobID: jobID,
	})
	return rows == 1, err
}

// ListNeedingRiver returns admitted receipts whose job is missing or terminal,
// ordered by first_seen_at. The River join is read-only metadata.
func (s *WebhookStore) ListNeedingRiver(ctx context.Context, limit int) ([]WebhookReceiptIdentity, error) {
	if limit < 1 {
		return nil, errors.New("entities: webhook receipt limit must be positive")
	}
	rows, err := s.river.ListAdmittedWebhookReceiptsNeedingRiver(ctx, int64(limit))
	if err != nil {
		return nil, err
	}
	result := make([]WebhookReceiptIdentity, 0, len(rows))
	for _, row := range rows {
		result = append(result, WebhookReceiptIdentity{
			EnvironmentID: row.EnvironmentID, DeliveryIDHash: row.DeliveryIDHash,
		})
	}
	return result, nil
}

// Get reads one receipt on the caller transaction without a row lock.
func (tx *WebhookTx) Get(ctx context.Context, environmentID, deliveryIDHash string) (WebhookReceipt, bool, error) {
	return webhookGet(ctx, tx.q, environmentID, deliveryIDHash, false)
}
