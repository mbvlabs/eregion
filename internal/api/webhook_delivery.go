package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// WebhookDeliveryState is the durable lifecycle of one GitHub delivery. A
// delivery is admitted only after the sanitized push intent and its River job
// commit in one transaction; it becomes submitted when the worker records the
// ordinary platform operation and its link in another single transaction.
type WebhookDeliveryState string

const (
	WebhookDeliveryPending   WebhookDeliveryState = "pending"
	WebhookDeliveryAdmitted  WebhookDeliveryState = "admitted"
	WebhookDeliverySubmitted WebhookDeliveryState = "submitted"
	WebhookDeliveryFailed    WebhookDeliveryState = "failed"
)

// ErrWebhookBindingChanged tells the HTTP handler that the environment,
// revision or webhook secret changed between the resolver read and the
// admission transaction. The handler re-resolves and re-verifies the signature
// once instead of accepting a stale binding.
var ErrWebhookBindingChanged = errors.New("webhook environment binding changed during admission")

// WebhookAdmission is the sanitized durable record of one verified GitHub push.
// It never contains the raw delivery ID, request body, signature, secret,
// sender or commit message. The delivery ID and body are stored only as
// SHA-256 hashes; the commit, repository and branch are validated facts from
// the signed payload and the environment revision.
type WebhookAdmission struct {
	DeliveryIDHash string
	BodySHA256     string
	Actor          contract.Actor
	Kind           string
	Target         string
	EnvironmentID  string
	RevisionID     string
	SecretRevision uint64
	// SecretRevisionHash is the non-secret vault revision identity that was
	// current when the signature was verified.
	SecretRevisionHash string
	Repository         string
	Branch             string
	Commit             string
	State              WebhookDeliveryState
	OperationID        contract.OperationID
	JobID              int64
	AdmissionAttempts  int
	ErrorCode          string
	FirstSeenAt        time.Time
}

// WebhookAdmissionStore admits one verified delivery before the handler
// replies 202. Admit must persist the sanitized intent and enqueue its River
// processing job atomically, and must compare body hash plus actor/kind/target
// when the same delivery ID is seen again. Implementations are shared by every
// control-plane process.
//
// ReconcileLegacy links an old pending row from the pre-admission schema to the
// operation its former synchronous handler may already have submitted. It never
// creates a new operation: found=false means the old operation is unavailable,
// which the handler reports as ambiguous rather than claiming exactly-once.
type WebhookAdmissionStore interface {
	Admit(context.Context, WebhookAdmission) (WebhookAdmission, bool, error)
	ReconcileLegacy(context.Context, WebhookAdmission, string, []byte) (WebhookAdmission, bool, error)
}

// WebhookDeliveryHash hashes the raw delivery ID for storage. Hashing keeps the
// identifier out of logs and database dumps while remaining stable for replay.
func WebhookDeliveryHash(deliveryID string) string {
	sum := sha256.Sum256([]byte(deliveryID))
	return hex.EncodeToString(sum[:])
}

// WebhookBodyHash identifies the exact signed bytes of a delivery.
func WebhookBodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
