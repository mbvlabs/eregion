package controlplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestIntegrationEntitiesPlatformDomainDNSAndLeases(t *testing.T) {
	pool, _ := testPool(t, true)
	f := newPlatformFixture(t, pool, "dns")
	ctx := context.Background()
	store := f.store
	domainID, err := platform.NewDomainID()
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, err := tx.Platform().Domains().Insert(ctx, entities.DomainInsert{
			ID: domainID, Domain: "dns.example.test", EnvironmentID: f.env.ID, Primary: true,
			Addresses: []string{"127.0.0.1"}, CheckPath: "/health",
		}); err != nil {
			return err
		}
		if _, err := tx.Platform().DomainDNS().UpsertIntent(ctx, domainID, "zone-1", "example.test", "marker-dns", []string{"127.0.0.1"}, false, operationID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if ledger, found, err := store.Platform().DomainDNS().GetByAppEnvironmentDomain(ctx, f.app.Name, f.env.Name, "dns.example.test"); err != nil || !found || ledger.ZoneID != "zone-1" {
		t.Fatalf("ledger = %+v, %v, %v", ledger, found, err)
	}
	if zone, found, err := store.Platform().DomainDNS().LockLedger(ctx, domainID); err != nil || !found || zone != "zone-1" {
		t.Fatalf("ledger lock = %v, %v, %v", zone, found, err)
	}
	if _, err := store.Platform().DomainDNS().MarkApplied(ctx, domainID, operationID); err != nil {
		t.Fatal(err)
	}
	if state, found, err := store.Platform().DomainDNS().StateForDefinitionStep(ctx, domainID); err != nil || !found || state.State != "applied" {
		t.Fatalf("ledger state = %+v, %v, %v", state, found, err)
	}
	if snapshots, err := store.Platform().Domains().DefinitionSnapshot(ctx, f.env.ID); err != nil || len(snapshots) != 1 {
		t.Fatalf("domain snapshot = %d, %v", len(snapshots), err)
	}
	if targets, err := store.Platform().Domains().ObservationTargets(ctx, "", 10); err != nil || len(targets) != 1 {
		t.Fatalf("observation targets = %d, %v", len(targets), err)
	}
	if _, err := store.Platform().Domains().BumpDesiredRevision(ctx, f.env.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Platform().Domains().MarkRoutesApplied(ctx, []string{"dns.example.test"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if targets, err := store.Platform().Domains().RouteTargets(ctx); err != nil || len(targets) != 1 {
		t.Fatalf("route targets = %d, %v", len(targets), err)
	}
	if err := store.Platform().Domains().LockRouteWrite(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if facts, found, err := store.Platform().Outcomes().ProtectionFacts(ctx, f.app.Name, f.env.Name); err != nil || !found || !facts.Protected {
		t.Fatalf("protection facts = %+v, %v, %v", facts, found, err)
	}
	if counts, err := store.Platform().Outcomes().DeletionCounts(ctx, f.app.Name, f.env.Name); err != nil || counts.Environments != 1 || counts.Domains != 1 {
		t.Fatalf("deletion counts = %+v, %v", counts, err)
	}
	if binding, found, err := store.Platform().Outcomes().WebhookBinding(ctx, f.env.ID); err != nil || !found || binding.App != f.app.Name {
		t.Fatalf("webhook binding = %+v, %v, %v", binding, found, err)
	}
	if exists, err := store.Platform().Outcomes().DefinitionReservationExists(ctx, f.app.Name, f.env.Name, operationID, "platform.test", "app/"+f.app.Name, json.RawMessage(`{}`), platform.KindDefinitionApply, contract.Actor{Kind: "system", Subject: "test"}); err != nil || exists {
		t.Fatalf("definition reservation = %v, %v", exists, err)
	}

	lease, err := store.Platform().Locks().AcquireTargets(ctx, []int64{11, 12})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	tryLease, acquired, err := store.Platform().Locks().TryTargets(ctx, []int64{11})
	if err != nil || !acquired {
		t.Fatalf("try targets = %v, %v", acquired, err)
	}
	if err := tryLease.Release(); err != nil {
		t.Fatal(err)
	}
	upload, acquired, err := store.Platform().Locks().AcquireImageUpload(ctx, 21)
	if err != nil || !acquired {
		t.Fatalf("image upload lease = %v, %v", acquired, err)
	}
	if err := upload.Release(); err != nil {
		t.Fatal(err)
	}
	probe, err := store.Nodes().ProbeBuildSlots(ctx, f.node, platform.MaxBuildConcurrency)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Lease != nil {
		if err := probe.Release(); err != nil {
			t.Fatal(err)
		}
	}
}
