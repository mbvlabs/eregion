package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// WebhookDelivery is the durable replay record for one GitHub delivery. It
// never contains the webhook secret, a raw signature, the request body, or the
// raw X-GitHub-Delivery value; the delivery ID is stored only as SHA-256.
type WebhookDelivery struct {
	DeliveryIDHash string
	BodySHA256     string
	Actor          contract.Actor
	Kind           string
	Target         string
	OperationID    contract.OperationID
	Submitted      bool
	FirstSeenAt    time.Time
}

// WebhookDeliveryStore reserves a delivery before submission and completes it
// with the accepted operation ID. Implementations must be shared by every
// control-plane process and must not share retention with operations.
//
// Reserve returns the existing record when the delivery was already seen and
// created=false. A known delivery with a different body hash is a conflict: the
// same signed delivery ID must never bind two payloads.
type WebhookDeliveryStore interface {
	Reserve(context.Context, WebhookDelivery) (WebhookDelivery, bool, error)
	Complete(context.Context, string, contract.OperationID) error
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

// MemoryWebhookDeliveries is a process-local store for tests and for callers
// that deliberately run a single short-lived handler. Production control planes
// use the PostgreSQL store so a replay survives restart and operation purging.
type MemoryWebhookDeliveries struct {
	mu      sync.Mutex
	records map[string]WebhookDelivery
}

func NewMemoryWebhookDeliveries() *MemoryWebhookDeliveries {
	return &MemoryWebhookDeliveries{records: map[string]WebhookDelivery{}}
}

func (m *MemoryWebhookDeliveries) Reserve(_ context.Context, record WebhookDelivery) (WebhookDelivery, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.records[record.DeliveryIDHash]; ok {
		if existing.BodySHA256 != record.BodySHA256 {
			return existing, false, contract.NewFault(contract.Conflict, "webhook delivery already bound a different payload", "Treat this as a replay conflict; never submit a second operation for one GitHub delivery ID.")
		}
		return existing, false, nil
	}
	record.Submitted = false
	record.OperationID = ""
	if record.FirstSeenAt.IsZero() {
		record.FirstSeenAt = time.Now().UTC()
	}
	m.records[record.DeliveryIDHash] = record
	return record, true, nil
}

func (m *MemoryWebhookDeliveries) Complete(_ context.Context, deliveryIDHash string, operationID contract.OperationID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[deliveryIDHash]
	if !ok {
		return contract.NewFault(contract.PreconditionFailed, "webhook delivery reservation disappeared", "Reserve the delivery before submitting its operation.")
	}
	if record.Submitted {
		if record.OperationID != operationID {
			return contract.NewFault(contract.Conflict, "webhook delivery is already bound to another operation", "Inspect the delivery record before resubmitting.")
		}
		return nil
	}
	record.Submitted = true
	record.OperationID = operationID
	m.records[deliveryIDHash] = record
	return nil
}

// Records returns a copy for test assertions only.
func (m *MemoryWebhookDeliveries) Records() []WebhookDelivery {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]WebhookDelivery, 0, len(m.records))
	for _, record := range m.records {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeliveryIDHash < out[j].DeliveryIDHash })
	return out
}
