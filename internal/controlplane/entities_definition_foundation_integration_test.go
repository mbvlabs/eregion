package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

func insertDefinitionOperation(t *testing.T, store *entities.Store, kind, target, key string, input json.RawMessage) contract.OperationID {
	t.Helper()
	id := mustOperationID(t)
	if err := store.WithinTx(context.Background(), func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(context.Background(), entities.OperationInsert{
			ID: id, Key: entities.OperationKey{Kind: kind, Target: target, Actor: testActor, IdempotencyKey: key},
			InputHash: "hash-" + key, Input: input,
		})
		if err != nil {
			return err
		}
		if !inserted {
			return errors.New("operation insert conflicted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestIntegrationEntitiesDefinitionFoundation drives the run/step journal,
// planning reads, provider/secret locks and workspace identity against real
// PostgreSQL. It also proves the narrow operation adapters used by the
// purpose methods.
func TestIntegrationEntitiesDefinitionFoundation(t *testing.T) {
	pool, _ := testPool(t, true)
	store := newTestEntities(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	t.Run("workspace identity", func(t *testing.T) {
		if _, found, err := store.Workspace().Get(ctx); err != nil || found {
			t.Fatalf("empty workspace found=%v err=%v", found, err)
		}
		var first bool
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			var err error
			first, err = tx.Workspace().Insert(ctx, entities.WorkspaceInsert{
				ID: "ws_0123456789abcdef01234567", Name: "foundation-workspace",
				RegistryHostNode: "node-a", RegistryHostKeyType: "ssh-ed25519",
				RegistryHostKeyFingerprint: "SHA256:test", RegistryHostStateDir: "/var/lib/eregion",
			})
			return err
		}); err != nil || !first {
			t.Fatalf("workspace insert=%v err=%v", first, err)
		}
		record, found, err := store.Workspace().Get(ctx)
		if err != nil || !found || record.ID != "ws_0123456789abcdef01234567" || !record.WorkspaceRegistryHost() {
			t.Fatalf("workspace record=%+v found=%v err=%v", record, found, err)
		}
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			second, err := tx.Workspace().Insert(ctx, entities.WorkspaceInsert{ID: "ws_ffffffffffffffffffffffff", Name: "other"})
			if err != nil || second {
				t.Fatalf("second workspace insert=%v err=%v", second, err)
			}
			record, found, err := tx.Workspace().Get(ctx)
			if err != nil || !found || record.Name != "foundation-workspace" {
				t.Fatalf("tx workspace=%+v found=%v err=%v", record, found, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	fixture := newPlatformFixture(t, pool, "definition-foundation")
	parent := insertDefinitionOperation(t, store, platform.KindDefinitionApply, "env/"+fixture.app.Name+"/"+fixture.env.Name, "definition-parent", json.RawMessage(`{"definition":"test"}`))
	child := insertDefinitionOperation(t, store, platform.KindResourceApply, "resource/"+fixture.app.Name+"/"+fixture.env.Name+"/web", "definition-child", json.RawMessage(`{"name":"web"}`))

	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if err := tx.DefinitionRuns().Insert(ctx, entities.DefinitionRunInsert{
			OperationID: parent, PlanNonce: "nonce-foundation", EnvironmentID: string(fixture.env.ID),
			Approved: json.RawMessage(`{"input":{"definition":"test"}}`), NextStep: 0,
		}); err != nil {
			return err
		}
		return tx.DefinitionSteps().Insert(ctx, entities.DefinitionStepInsert{
			ParentOperationID: parent, Ordinal: 0, Kind: platform.KindResourceApply,
			Target: "resource/" + fixture.app.Name + "/" + fixture.env.Name + "/web",
			Input:  json.RawMessage(`{"name":"web"}`),
		})
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("run journal", func(t *testing.T) {
		run, found, err := store.DefinitionRuns().Get(ctx, parent)
		if err != nil || !found || run.OperationID != parent || run.NextStep != 0 || run.FinishedAt != nil {
			t.Fatalf("run=%+v found=%v err=%v", run, found, err)
		}
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			if _, found, err := tx.DefinitionRuns().Get(ctx, parent); err != nil || !found {
				t.Fatalf("tx get run found=%v err=%v", found, err)
			}
			if _, found, err := tx.DefinitionRuns().Lock(ctx, parent); err != nil || !found {
				t.Fatalf("lock run found=%v err=%v", found, err)
			}
			journal, found, err := tx.DefinitionRuns().LockJournal(ctx, parent)
			if err != nil || !found || !journal.SameInput || journal.Finished || journal.EnvironmentID != string(fixture.env.ID) {
				t.Fatalf("journal=%+v found=%v err=%v", journal, found, err)
			}
			if advanced, err := tx.DefinitionRuns().Advance(ctx, parent, 0); err != nil || !advanced {
				t.Fatalf("advance=%v err=%v", advanced, err)
			}
			if advanced, err := tx.DefinitionRuns().Advance(ctx, parent, 0); err != nil || advanced {
				t.Fatalf("stale advance=%v err=%v", advanced, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			if ok, err := tx.DefinitionRuns().SetFailure(ctx, parent, contract.Unavailable, "foundation failure"); err != nil || !ok {
				t.Fatalf("set failure=%v err=%v", ok, err)
			}
			if ok, err := tx.DefinitionRuns().SetFailure(ctx, parent, contract.Internal, "second"); err != nil || ok {
				t.Fatalf("second failure=%v err=%v", ok, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		run, _, _ = store.DefinitionRuns().Get(ctx, parent)
		if run.FailureCode != contract.Unavailable || run.FailureMessage != "foundation failure" || run.NextStep != 1 {
			t.Fatalf("run after failure=%+v", run)
		}
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			finished, err := tx.DefinitionRuns().Finish(ctx, parent)
			if err != nil || !finished {
				t.Fatalf("finish parent=%v err=%v", finished, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("step journal", func(t *testing.T) {
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			step, found, err := tx.DefinitionSteps().Lock(ctx, parent, 0)
			if err != nil || !found || step.Kind != platform.KindResourceApply || step.ChildOperationID != "" {
				t.Fatalf("step=%+v found=%v err=%v", step, found, err)
			}
			if linked, err := tx.DefinitionSteps().SetChild(ctx, parent, 0, child); err != nil || !linked {
				t.Fatalf("set child=%v err=%v", linked, err)
			}
			if exists, err := tx.DefinitionSteps().ChildExists(ctx, child); err != nil || !exists {
				t.Fatalf("child exists=%v err=%v", exists, err)
			}
			children, err := tx.DefinitionSteps().ListChildJournal(ctx, parent)
			if err != nil || len(children) != 1 || children[0].ChildID != child || children[0].ChildState != contract.Queued {
				t.Fatalf("children=%+v err=%v", children, err)
			}
			active, err := tx.DefinitionSteps().ListActiveChildren(ctx, parent)
			if err != nil || len(active) != 1 || active[0] != child {
				t.Fatalf("active=%+v err=%v", active, err)
			}
			bounds, err := tx.DefinitionSteps().CursorBounds(ctx, parent, 0)
			if err != nil || !bounds.Later || !bounds.Active {
				t.Fatalf("bounds=%+v err=%v", bounds, err)
			}
			outside, err := tx.DefinitionSteps().ActiveOutsideOrdinal(ctx, parent, 1)
			if err != nil || !outside {
				t.Fatalf("outside=%v err=%v", outside, err)
			}
			saved, err := tx.DefinitionSteps().SaveDNSProgress(ctx, parent, 0, json.RawMessage(`[{"index":0}]`), json.RawMessage(`[]`), child)
			if err != nil || !saved {
				t.Fatalf("save progress=%v err=%v", saved, err)
			}
			stale, err := tx.DefinitionSteps().SaveDNSProgress(ctx, parent, 0, json.RawMessage(`[]`), json.RawMessage(`[]`), child)
			if err != nil || stale {
				t.Fatalf("stale progress=%v err=%v", stale, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		children, err := store.DefinitionSteps().ListChildJournal(ctx, parent)
		if err != nil || len(children) != 1 {
			t.Fatalf("store child journal=%+v err=%v", children, err)
		}
	})

	t.Run("narrow operation adapters", func(t *testing.T) {
		op, found, err := store.Operations().Get(ctx, parent)
		if err != nil || !found || op.ID != parent {
			t.Fatalf("get op=%+v found=%v err=%v", op, found, err)
		}
		state, attempt, found, err := store.Operations().StateAttempt(ctx, parent)
		if err != nil || !found || state != contract.Queued || attempt != 0 {
			t.Fatalf("state/attempt=%v/%d found=%v err=%v", state, attempt, found, err)
		}
		sequence, found, err := store.Operations().LastSequence(ctx, parent)
		if err != nil || !found || sequence != 0 {
			t.Fatalf("last sequence=%d found=%v err=%v", sequence, found, err)
		}
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			txOp, found, err := tx.Operations().Get(ctx, parent)
			if err != nil || !found || txOp.Target != op.Target {
				t.Fatalf("tx get found=%v err=%v", found, err)
			}
			if _, _, found, err := tx.Operations().StateAttempt(ctx, parent); err != nil || !found {
				t.Fatalf("tx state/attempt found=%v err=%v", found, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("planning reads and locks", func(t *testing.T) {
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			if err := tx.DefinitionDNSPolicy().EnsureSingleton(ctx, []string{}, "dns-only", time.Now().UTC()); err != nil {
				return err
			}
			policy, found, err := tx.DefinitionDNSPolicy().LockShared(ctx)
			if err != nil || !found || policy.Revision != 0 || policy.Enabled {
				t.Fatalf("shared policy=%+v found=%v err=%v", policy, found, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			if _, err := pool.Exec(ctx, `INSERT INTO eregion_provider_credentials
                (provider,zone_id,zone_name,latest_revision,created_by_kind,created_by_subject,updated_by_kind,updated_by_subject,created_at,updated_at)
                VALUES ('cloudflare','zone-foundation','zone-foundation.example',3,'system','test','system','test',clock_timestamp(),clock_timestamp())`); err != nil {
				return err
			}
			revision, found, err := tx.Providers().RevisionForZone(ctx, "cloudflare", "zone-foundation")
			if err != nil || !found || revision != 3 {
				t.Fatalf("provider revision=%d found=%v err=%v", revision, found, err)
			}
			zones, err := tx.Providers().ZoneRevisionsForDefinition(ctx, "cloudflare", "zone-foundation", string(fixture.env.ID))
			if err != nil || len(zones) != 1 || zones[0].Revision != 3 {
				t.Fatalf("provider zones=%+v err=%v", zones, err)
			}
			locked, err := tx.Providers().LockZoneRevisionsForDefinition(ctx, "cloudflare", "zone-foundation", string(fixture.env.ID))
			if err != nil || len(locked) != 1 || locked[0] != "zone-foundation" {
				t.Fatalf("locked zones=%+v err=%v", locked, err)
			}
			shared, found, err := tx.Providers().LockRevisionForZoneShare(ctx, "cloudflare", "zone-foundation")
			if err != nil || !found || shared != 3 {
				t.Fatalf("shared revision=%d found=%v err=%v", shared, found, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO eregion_secrets
            (app,environment,name,latest_revision,state,created_by_kind,created_by_subject,updated_by_kind,updated_by_subject,created_at,updated_at)
            VALUES ($1,$2,'WEBHOOK_SECRET',2,'active','system','test','system','test',clock_timestamp(),clock_timestamp())`,
			fixture.app.Name, fixture.env.Name); err != nil {
			t.Fatal(err)
		}
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			snapshot, err := tx.Secrets().DefinitionSnapshot(ctx, fixture.app.Name, fixture.env.Name)
			if err != nil || len(snapshot) != 1 || snapshot[0].Revision != 2 || snapshot[0].State != "active" {
				t.Fatalf("secret snapshot=%+v err=%v", snapshot, err)
			}
			names, err := tx.Secrets().LockDefinitionSecretNames(ctx, fixture.app.Name, fixture.env.Name)
			if err != nil || len(names) != 1 || names[0] != "WEBHOOK_SECRET" {
				t.Fatalf("secret names=%+v err=%v", names, err)
			}
			locked, err := tx.Secrets().LockBinding(ctx, fixture.app.Name, fixture.env.Name, "WEBHOOK_SECRET")
			if err != nil || !locked {
				t.Fatalf("secret binding=%v err=%v", locked, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO eregion_secrets
            (app,environment,name,latest_revision,state,created_by_kind,created_by_subject,updated_by_kind,updated_by_subject,created_at,updated_at)
            VALUES ($1,$2,'SECOND_SECRET',1,'active','system','test','system','test',clock_timestamp(),clock_timestamp())`,
			fixture.app.Name, fixture.env.Name); err != nil {
			t.Fatal(err)
		}
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			deleted, err := tx.Secrets().DeleteForEnvironment(ctx, fixture.app.Name, fixture.env.Name)
			if err != nil || deleted != 2 {
				t.Fatalf("delete environment secrets=%d err=%v", deleted, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO eregion_secrets
            (app,environment,name,latest_revision,state,created_by_kind,created_by_subject,updated_by_kind,updated_by_subject,created_at,updated_at)
            VALUES ($1,'other','APP_SECRET',1,'active','system','test','system','test',clock_timestamp(),clock_timestamp())`,
			fixture.app.Name); err != nil {
			t.Fatal(err)
		}
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			deleted, err := tx.Secrets().DeleteForApp(ctx, fixture.app.Name)
			if err != nil || deleted != 1 {
				t.Fatalf("delete app secrets=%d err=%v", deleted, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		app, found, err := store.Platform().Apps().Repository(ctx, fixture.app.Name)
		if err != nil || !found || app.ImageRepository == "" {
			t.Fatalf("app repository=%+v found=%v err=%v", app, found, err)
		}
		observation, found, err := store.Platform().Environments().GetByName(ctx, fixture.app.Name, fixture.env.Name)
		if err != nil || !found || observation.Environment.ID != fixture.env.ID {
			t.Fatalf("environment observation=%+v found=%v err=%v", observation, found, err)
		}
		if _, err := store.Platform().Domains().DefinitionSnapshot(ctx, fixture.env.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Resources().DefinitionSnapshot(ctx, fixture.env.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Nodes().DefinitionSnapshot(ctx, []string{fixture.node}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Platform().Builds().ActiveTargetsForApp(ctx, fixture.app.ID, "app/"+fixture.app.Name, "env/"+fixture.app.Name+"/", "resource/"+fixture.app.Name+"/"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Platform().Outcomes().DefinitionReservationExists(ctx, fixture.app.Name, fixture.env.Name,
			parent, platform.KindDefinitionApply, "env/"+fixture.app.Name+"/"+fixture.env.Name,
			json.RawMessage(`{}`), platform.KindDefinitionApply, testActor); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Platform().Builds().ActiveOperationForApp(ctx, fixture.app.ID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("run uniqueness and terminal reconcile", func(t *testing.T) {
		second := insertDefinitionOperation(t, store, platform.KindDefinitionApply, "env/"+fixture.app.Name+"/"+fixture.env.Name, "definition-second", json.RawMessage(`{"definition":"second"}`))
		const workers = 2
		var wg sync.WaitGroup
		results := make(chan bool, workers)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
					return tx.DefinitionRuns().Insert(ctx, entities.DefinitionRunInsert{
						OperationID: second, PlanNonce: "nonce-race-" + string(second)[len(second)-4:],
						EnvironmentID: string(fixture.env.ID), Approved: json.RawMessage(`{}`),
					})
				})
				results <- err == nil
			}()
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
			t.Fatalf("run insertion winners=%d, want 1", winners)
		}
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			return tx.Operations().SetState(ctx, second, contract.Succeeded, "")
		}); err != nil {
			t.Fatal(err)
		}
		ids, err := store.DefinitionRuns().ListUnfinishedTerminal(ctx, 10)
		if err != nil || len(ids) != 1 || ids[0] != second {
			t.Fatalf("terminal definition runs=%+v err=%v", ids, err)
		}
		if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			if finished, err := tx.DefinitionRuns().Finish(ctx, second); err != nil || !finished {
				t.Fatalf("finish=%v err=%v", finished, err)
			}
			if finished, err := tx.DefinitionRuns().Finish(ctx, second); err != nil || finished {
				t.Fatalf("second finish=%v err=%v", finished, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
