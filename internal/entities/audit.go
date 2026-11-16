package entities

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
	"github.com/mbvlabs/eregion/internal/integrations"
)

// integrationAuditLock serializes audit ID allocation until the transaction
// commits or rolls back. It preserves ordered, gap-free identity allocation
// without a database sequence.
const integrationAuditLock int64 = 0x6572656175646974

// AuditStore is the standalone durable audit sink. It borrows the Store's pool
// and starts exactly one transaction per entry; it exposes no transaction or
// driver handle.
type AuditStore struct{ store *Store }

// Audit returns the durable integration audit sink for this Store.
func (s *Store) Audit() *AuditStore { return &AuditStore{store: s} }

// The integrations package requires this exact port; a missing or renamed
// method fails the build instead of reaching production as a runtime assertion.
var _ integrations.AuditSink = (*AuditStore)(nil)

// Record appends one audit entry in its own transaction.
func (a *AuditStore) Record(ctx context.Context, entry integrations.AuditEntry) error {
	if a == nil || a.store == nil {
		return auditUnavailable(errors.New("audit store is not configured"))
	}
	err := a.store.WithinTx(ctx, func(tx *StoreTx) error {
		return tx.Audit().Record(ctx, entry)
	})
	if err != nil {
		var fault *contract.FaultError
		if errors.As(err, &fault) {
			return fault
		}
		return auditUnavailable(err)
	}
	return nil
}

// AuditTx is the audit view of one StoreTx. Record uses the existing
// transaction and never starts another one.
type AuditTx struct{ owner *StoreTx }

// Audit returns the audit view of this transaction.
func (tx *StoreTx) Audit() *AuditTx { return &AuditTx{owner: tx} }

// Record serializes all audit writers, allocates the next identity under that
// lock, and inserts the entry in the caller's transaction. Identity allocation
// is gapless across a rollback, and the lock remains held until commit.
func (a *AuditTx) Record(ctx context.Context, entry integrations.AuditEntry) error {
	if a == nil || a.owner == nil {
		return auditUnavailable(errors.New("audit transaction is not configured"))
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return contract.NewFault(contract.Internal, "could not encode the audit entry", "Do not perform the action; report the binary version.")
	}
	if err := a.owner.q.LockIntegrationAuditWriters(ctx, integrationAuditLock); err != nil {
		return auditUnavailable(err)
	}
	last, err := a.owner.q.LastIntegrationAuditID(ctx)
	if err != nil {
		return auditUnavailable(err)
	}
	next, err := nextAuditID(last)
	if err != nil {
		return auditUnavailable(err)
	}
	if err := a.owner.q.InsertIntegrationAuditEntry(ctx, query.InsertIntegrationAuditEntryParams{ID: next, Entry: raw}); err != nil {
		return auditUnavailable(err)
	}
	return nil
}

func nextAuditID(last int64) (int64, error) {
	if last < 0 || last == math.MaxInt64 {
		return 0, errors.New("invalid or exhausted integration audit identity")
	}
	return last + 1, nil
}

func auditUnavailable(cause error) *contract.FaultError {
	fault := contract.NewFault(contract.Unavailable, "durable integration audit is unavailable", "Repair database connectivity before performing the action.")
	fault.Cause = cause
	return fault
}
