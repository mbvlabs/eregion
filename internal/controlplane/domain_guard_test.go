package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func domainGuardOperation(kind string, created time.Time) contract.Operation {
	return contract.Operation{
		ID: "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: kind,
		Target: "env/api/production", CreatedAt: created,
	}
}

func TestManagedDNSInvalidPendingIntentRecovery(t *testing.T) {
	service := &Service{}
	pending := domainDNSLedger{
		Domain: "app.other.example", ZoneID: "0123456789abcdef0123456789abcdef", ZoneName: "example.com",
		State: domainDNSStatePending, AppliedGeneration: 0,
	}
	if err := service.validateDomainDNSCleanupRecovery(pending); err != nil {
		t.Fatalf("invalid pending intent should be safely discardable: %v", err)
	}
	applied := pending
	applied.State = domainDNSStateApplied
	applied.AppliedGeneration = 1
	if err := service.validateDomainDNSCleanupRecovery(applied); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("invalid applied intent = %v", err)
	}
}

func TestValidateDomainOperationIdentityRejectsStaleAndReaddedBindings(t *testing.T) {
	admitted := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	input := &platform.DomainInput{App: "api", Environment: "production", Domain: "app.example.com"}
	sameBinding := &domainBindingIdentity{App: "api", Environment: "production", CreatedAt: admitted.Add(-time.Hour)}

	add := domainGuardOperation(platform.KindDomainAdd, admitted)
	if err := validateDomainOperationIdentity(add, input, add.ID, sameBinding); err != nil {
		t.Fatalf("valid add = %v", err)
	}
	if err := validateDomainOperationIdentity(add, input, "op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", sameBinding); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("newer domain intent = %v", err)
	}
	if err := validateDomainOperationIdentity(add, input, add.ID, nil); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("missing add binding = %v", err)
	}

	remove := domainGuardOperation(platform.KindDomainRemove, admitted)
	if err := validateDomainOperationIdentity(remove, input, remove.ID, sameBinding); err != nil {
		t.Fatalf("valid remove = %v", err)
	}
	promoted := *sameBinding
	promoted.Primary = true
	if err := validateDomainOperationIdentity(remove, input, remove.ID, &promoted); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("promoted binding = %v", err)
	}
	readded := *sameBinding
	readded.CreatedAt = admitted.Add(time.Minute)
	if err := validateDomainOperationIdentity(remove, input, remove.ID, &readded); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("readded binding = %v", err)
	}
	moved := *sameBinding
	moved.Environment = "staging"
	if err := validateDomainOperationIdentity(remove, input, remove.ID, &moved); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("cross-environment binding = %v", err)
	}
	if err := validateDomainOperationIdentity(remove, input, remove.ID, nil); err != nil {
		t.Fatalf("already-removed binding = %v", err)
	}
}

type guardRow struct {
	active int
	err    error
}

func (r guardRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*int)) = r.active
	return nil
}

type guardQuery struct {
	row guardRow
}

func (q guardQuery) QueryRow(context.Context, string, ...any) pgx.Row { return q.row }

func TestEnsureNoActiveDomainChange(t *testing.T) {
	service := &Service{}
	if err := service.ensureNoActiveDomainChangeTx(context.Background(), guardQuery{row: guardRow{active: 1}}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "api", "production"); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("active domain change = %v", err)
	}
	if err := service.ensureNoActiveDomainChangeTx(context.Background(), guardQuery{row: guardRow{}}, "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "api", "production"); err != nil {
		t.Fatalf("idle target = %v", err)
	}
}
