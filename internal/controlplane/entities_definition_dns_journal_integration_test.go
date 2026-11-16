package controlplane

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

// TestIntegrationEntitiesDefinitionDNSJournal drives the DNS child journal and
// recovery reads under the real parent/run/step/child lock order, then proves
// the progress CAS rejects a stale concurrent writer.
func TestIntegrationEntitiesDefinitionDNSJournal(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	parent := insertDefinitionOperation(t, store, platform.KindDefinitionApply, "env/dns-foundation/staging", "definition:dns-parent",
		json.RawMessage(`{"dns_mode":"cloudflare","app":"dns-foundation","environment":"staging","domain":"app.example.test"}`))
	child := insertDefinitionOperation(t, store, platform.KindDomainAdd, "env/dns-foundation/staging", "definition:"+string(parent)+":0",
		json.RawMessage(`{"app":"dns-foundation","environment":"staging","domain":"app.example.test","dns_mode":"cloudflare"}`))
	domainID := platform.DomainID("dom_dns_foundation")
	approved := json.RawMessage(`{"input":{"app":"dns-foundation","environment":"staging","domain":"app.example.test","dns_mode":"cloudflare"}}`)
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if err := tx.DefinitionRuns().Insert(ctx, entities.DefinitionRunInsert{
			OperationID: parent, PlanNonce: "nonce-dns-foundation", EnvironmentID: "env-dns-foundation",
			Approved: approved,
		}); err != nil {
			return err
		}
		if err := tx.DefinitionSteps().Insert(ctx, entities.DefinitionStepInsert{
			ParentOperationID: parent, Ordinal: 0, Kind: platform.KindDomainAdd,
			Target:   "env/dns-foundation/staging",
			Input:    json.RawMessage(`{"app":"dns-foundation","environment":"staging","domain":"app.example.test","dns_mode":"cloudflare"}`),
			DomainID: domainID, DNSPlan: json.RawMessage(`{"domain_id":"dom_dns_foundation","host":"app.example.test"}`),
		}); err != nil {
			return err
		}
		_, err := tx.DefinitionSteps().SetChild(ctx, parent, 0, child)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	verifyDefinitionDNSJournalLocks(t, ctx, store, parent, child, domainID)
	verifyDefinitionDNSCleanupWarning(t, ctx, store)
	verifyDefinitionDNSProgressCAS(t, ctx, store, parent, child)
}

func verifyDefinitionDNSJournalLocks(t *testing.T, ctx context.Context, store *entities.Store, parent, child contract.OperationID, domainID platform.DomainID) {
	t.Helper()
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, found, err := tx.Operations().Lock(ctx, parent); err != nil || !found {
			t.Fatalf("parent lock found=%v err=%v", found, err)
		}
		if _, found, err := tx.DefinitionRuns().Lock(ctx, parent); err != nil || !found {
			t.Fatalf("run lock found=%v err=%v", found, err)
		}
		if _, found, err := tx.DefinitionSteps().Lock(ctx, parent, 0); err != nil || !found {
			t.Fatalf("step lock found=%v err=%v", found, err)
		}
		if _, found, err := tx.Operations().Lock(ctx, child); err != nil || !found {
			t.Fatalf("child lock found=%v err=%v", found, err)
		}
		journal, found, err := tx.DefinitionDNS().LoadJournal(ctx, parent, 0, child)
		if err != nil || !found {
			t.Fatalf("load journal found=%v err=%v", found, err)
		}
		if journal.ParentID != parent || journal.DomainID != domainID ||
			journal.ParentState != contract.Queued || journal.ChildState != contract.Queued ||
			journal.StepKind != platform.KindDomainAdd || string(journal.DNSPlan) == "" {
			t.Fatalf("journal=%+v", journal)
		}
		verifyDefinitionDNSJournalReadsTx(t, ctx, tx, parent, child)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func verifyDefinitionDNSJournalReadsTx(t *testing.T, ctx context.Context, tx *entities.StoreTx, parent, child contract.OperationID) {
	t.Helper()
	linked, err := tx.DefinitionDNS().LinkedCleanupPending(ctx, child)
	if err != nil || !linked {
		t.Fatalf("linked cleanup pending=%v err=%v", linked, err)
	}
	exists, err := tx.DefinitionSteps().ChildExists(ctx, child)
	if err != nil || !exists {
		t.Fatalf("child exists=%v err=%v", exists, err)
	}
	marker, found, err := tx.DefinitionDNS().TerminalMarker(ctx, parent)
	if err != nil || !found || !marker.DefinitionKey || !marker.DNSEvidence {
		t.Fatalf("terminal marker=%+v found=%v err=%v", marker, found, err)
	}
	verified, err := tx.DefinitionDNS().CleanupVerifiedWithoutLink(ctx, child)
	if err != nil || verified {
		t.Fatalf("verified without link=%v err=%v", verified, err)
	}
}

func verifyDefinitionDNSCleanupWarning(t *testing.T, ctx context.Context, store *entities.Store) {
	t.Helper()
	// A verified cleanup event with no definition step is the warning case.
	cleanupOp := insertDefinitionOperationWithContext(ctx, t, store, platform.KindDomainRemove, "env/dns-foundation/staging", "cleanup-only",
		json.RawMessage(`{"domain":"app.example.test"}`))
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		op, found, err := tx.Operations().Lock(ctx, cleanupOp)
		if err != nil || !found {
			t.Fatalf("cleanup op found=%v err=%v", found, err)
		}
		return tx.Operations().AppendEvent(ctx, &op, contract.Event{
			Kind: "step", Stage: "dns", Message: "Verified Cloudflare cleanup for app.example.test",
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		verified, err := tx.DefinitionDNS().CleanupVerifiedWithoutLink(ctx, cleanupOp)
		if err != nil || !verified {
			t.Fatalf("cleanup verified without link=%v err=%v", verified, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func verifyDefinitionDNSProgressCAS(t *testing.T, ctx context.Context, store *entities.Store, parent, child contract.OperationID) {
	t.Helper()
	// Concurrent CAS writers from the same expected progress: exactly one wins.
	const writers = 4
	var wg sync.WaitGroup
	results := make(chan bool, writers)
	expected := json.RawMessage(`[]`)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			progress := json.RawMessage(`[{"writer":` + string(rune('0'+index)) + `}]`)
			err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
				ok, err := tx.DefinitionSteps().SaveDNSProgress(ctx, parent, 0, progress, expected, child)
				if err != nil {
					return err
				}
				results <- ok
				return nil
			})
			if err != nil {
				results <- false
			}
		}(i)
	}
	wg.Wait()
	close(results)
	winners := 0
	for ok := range results {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("DNS progress CAS winners=%d, want 1", winners)
	}
}
