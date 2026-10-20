package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
)

// PostgresWebhookDeliveries is the shared durable replay guard for GitHub
// deliveries. It lives in eregion_webhook_deliveries, separate from operation
// retention, and stores only SHA-256 hashes plus the accepted operation ID.
type PostgresWebhookDeliveries struct {
	pool *pgxpool.Pool
}

var _ api.WebhookDeliveryStore = (*PostgresWebhookDeliveries)(nil)

func NewWebhookDeliveryStore(pool *pgxpool.Pool) *PostgresWebhookDeliveries {
	return &PostgresWebhookDeliveries{pool: pool}
}

// Reserve inserts a pending record or returns the existing one. The primary key
// arbitrates concurrent replays across control planes. A known delivery bound
// to different body bytes is refused as a conflict.
func (s *PostgresWebhookDeliveries) Reserve(ctx context.Context, record api.WebhookDelivery) (api.WebhookDelivery, bool, error) {
	if err := validateWebhookDeliveryRecord(record); err != nil {
		return api.WebhookDelivery{}, false, err
	}
	inserted, err := s.pool.Exec(ctx, `INSERT INTO eregion_webhook_deliveries
        (delivery_id_hash, body_sha256, actor_kind, actor_subject, kind, target, state)
        VALUES ($1,$2,$3,$4,$5,$6,'pending')
        ON CONFLICT (delivery_id_hash) DO NOTHING`,
		record.DeliveryIDHash, record.BodySHA256, record.Actor.Kind, record.Actor.Subject, record.Kind, record.Target)
	if err != nil {
		return api.WebhookDelivery{}, false, err
	}
	stored, err := s.load(ctx, record.DeliveryIDHash)
	if err != nil {
		return api.WebhookDelivery{}, false, err
	}
	if stored.BodySHA256 != record.BodySHA256 {
		return stored, false, contract.NewFault(contract.Conflict, "webhook delivery already bound a different payload", "Treat this as a replay conflict; never submit a second operation for one GitHub delivery ID.")
	}
	return stored, inserted.RowsAffected() == 1, nil
}

// Complete binds the accepted operation ID to the reservation. It is
// idempotent for the same operation and refuses a different one.
func (s *PostgresWebhookDeliveries) Complete(ctx context.Context, deliveryIDHash string, operationID contract.OperationID) error {
	if !validHash(deliveryIDHash) || operationID == "" {
		return contract.NewFault(contract.ValidationFailed, "webhook delivery completion is malformed", "Reserve the delivery and complete it with the accepted operation ID.")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_webhook_deliveries
        SET state='submitted', operation_id=$2, submitted_at=clock_timestamp()
        WHERE delivery_id_hash=$1 AND state='pending'`, deliveryIDHash, operationID); err != nil {
		return err
	}
	stored, err := s.load(ctx, deliveryIDHash)
	if err != nil {
		return err
	}
	if !stored.Submitted {
		return contract.NewFault(contract.PreconditionFailed, "webhook delivery reservation disappeared", "Reserve the delivery before submitting its operation.")
	}
	if stored.OperationID != operationID {
		return contract.NewFault(contract.Conflict, "webhook delivery is already bound to another operation", "Inspect the delivery record before resubmitting.")
	}
	return nil
}

// Lookup reads one record. It is exposed for tests and status reporting.
func (s *PostgresWebhookDeliveries) Lookup(ctx context.Context, deliveryIDHash string) (api.WebhookDelivery, bool, error) {
	stored, err := s.load(ctx, deliveryIDHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.WebhookDelivery{}, false, nil
	}
	if err != nil {
		return api.WebhookDelivery{}, false, err
	}
	return stored, true, nil
}

func (s *PostgresWebhookDeliveries) load(ctx context.Context, deliveryIDHash string) (api.WebhookDelivery, error) {
	var record api.WebhookDelivery
	var state string
	var operationID *string
	var submittedAt *time.Time
	err := s.pool.QueryRow(ctx, `SELECT delivery_id_hash, body_sha256, actor_kind, actor_subject,
        kind, target, state, operation_id, first_seen_at, submitted_at
        FROM eregion_webhook_deliveries WHERE delivery_id_hash=$1`, deliveryIDHash).Scan(
		&record.DeliveryIDHash, &record.BodySHA256, &record.Actor.Kind, &record.Actor.Subject,
		&record.Kind, &record.Target, &state, &operationID, &record.FirstSeenAt, &submittedAt)
	if err != nil {
		return api.WebhookDelivery{}, err
	}
	if operationID != nil {
		record.OperationID = contract.OperationID(*operationID)
	}
	record.Submitted = state == "submitted" && submittedAt != nil
	return record, nil
}

func validateWebhookDeliveryRecord(record api.WebhookDelivery) error {
	if !validHash(record.DeliveryIDHash) || !validHash(record.BodySHA256) {
		return contract.NewFault(contract.ValidationFailed, "webhook delivery hashes are malformed", "Store only lowercase SHA-256 hex digests of the delivery ID and body.")
	}
	if record.Actor.Kind != "webhook" || record.Actor.Subject == "" || len(record.Actor.Subject) > 256 {
		return contract.NewFault(contract.ValidationFailed, "webhook delivery actor is invalid", "Use the webhook actor resolved from the signed payload.")
	}
	if record.Kind == "" || record.Target == "" {
		return contract.NewFault(contract.ValidationFailed, "webhook delivery has no operation identity", "Record the normalized action kind and target before submission.")
	}
	return nil
}

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
