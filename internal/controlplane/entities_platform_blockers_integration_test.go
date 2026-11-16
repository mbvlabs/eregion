package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

type platformChain struct {
	ReleaseA platform.ReleaseID
	ReleaseB platform.ReleaseID
	Build    platform.BuildID
	Deploy   platform.DeploymentID
	OwnerOp  contract.OperationID
}

func seedPlatformChain(t *testing.T, f *platformFixture, suffix string) platformChain {
	t.Helper()
	ctx := context.Background()
	releaseA, _ := platform.NewReleaseID()
	releaseB, _ := platform.NewReleaseID()
	buildID, _ := platform.NewBuildID()
	deployID, _ := platform.NewDeploymentID()
	releaseAOp, _ := contract.NewOperationID()
	releaseBOp, _ := contract.NewOperationID()
	buildOp, _ := contract.NewOperationID()
	deployOp, _ := contract.NewOperationID()
	if err := f.store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		for _, operation := range []struct {
			id   contract.OperationID
			kind string
		}{{releaseAOp, "platform.deploy"}, {releaseBOp, "platform.rollback"}, {buildOp, "platform.build.retry"}, {deployOp, "platform.deploy"}} {
			if _, err := tx.Operations().Insert(ctx, entities.OperationInsert{
				ID: operation.id, Key: entities.OperationKey{Kind: operation.kind, Target: "env/" + f.app.Name + "/" + f.env.Name + "/" + suffix, Actor: contract.Actor{Kind: "ci", Subject: "platform"}, IdempotencyKey: string(operation.id)},
				InputHash: "hash", Input: json.RawMessage(`{}`),
			}); err != nil {
				return err
			}
		}
		for _, release := range []struct {
			id platform.ReleaseID
			op contract.OperationID
		}{{releaseA, releaseAOp}, {releaseB, releaseBOp}} {
			if _, err := tx.Platform().Releases().Insert(ctx, entities.ReleaseInsert{
				ID: release.id, OperationID: release.op, AppID: f.app.ID, EnvironmentID: f.env.ID,
				Image: "example/app:" + suffix, ImageID: "sha256:" + suffix, Source: json.RawMessage(`{"type":"image"}`),
				Status: platform.ReleaseReady, CompletedAt: timePtr(time.Now().UTC()),
			}); err != nil {
				return err
			}
		}
		if _, err := tx.Platform().Builds().Insert(ctx, entities.BuildInsert{
			ID: buildID, OperationID: buildOp, AppID: f.app.ID, EnvironmentID: f.env.ID,
			RevisionID: f.revision.ID, ReleaseID: releaseA, Source: json.RawMessage(`{"type":"git"}`), CommitSHA: "0123456789012345678901234567890123456789",
		}); err != nil {
			return err
		}
		if _, err := tx.Platform().Deployments().Insert(ctx, entities.DeploymentInsert{
			ID: deployID, OperationID: deployOp, AppID: f.app.ID, EnvironmentID: f.env.ID,
			Action: platform.ActionDeploy, ReleaseID: releaseA, RevisionID: f.revision.ID, SecretRevisions: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		if _, err := tx.Platform().Owners().Insert(ctx, entities.OwnerInsert{
			EnvironmentID: f.env.ID, OperationID: deployOp, DeploymentID: deployID, Attempt: 1, Phase: "prepared",
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return platformChain{ReleaseA: releaseA, ReleaseB: releaseB, Build: buildID, Deploy: deployID, OwnerOp: deployOp}
}

func lockTimeoutTx(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), "SET LOCAL lock_timeout = '150ms'"); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatal(err)
	}
	return tx
}

func requireLockTimeout(t *testing.T, tx pgx.Tx, action func(pgx.Tx) error) {
	t.Helper()
	err := action(tx)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("lock conflict error = %v, want SQLSTATE 55P03", err)
	}
}

func TestIntegrationEntitiesPlatformBuildReleasePointerAndKeyShare(t *testing.T) {
	pool, _ := testPool(t, true)
	f := newPlatformFixture(t, pool, "pointer")
	chain := seedPlatformChain(t, f, "pointer")
	ctx := context.Background()
	store := f.store

	held := make(chan struct{})
	actualCh := make(chan platform.ReleaseID, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	holderCtx, cancelHolder := context.WithTimeout(ctx, 10*time.Second)
	var releaseOnce sync.Once
	releaseHolder := func() { releaseOnce.Do(func() { close(release) }) }
	holderConsumed := false
	t.Cleanup(func() {
		releaseHolder()
		if !holderConsumed {
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("holder transaction = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Errorf("holder transaction did not finish after release")
			}
		}
		cancelHolder()
	})
	go func() {
		done <- store.WithinTx(holderCtx, func(tx *entities.StoreTx) error {
			pointer, found, err := tx.Platform().Pointers().LockOwnedBuildForRelease(holderCtx, chain.Build, f.app.ID, f.env.ID)
			if err != nil || !found {
				return err
			}
			if pointer.ReleaseID != chain.ReleaseA {
				return errors.New("build release identity was not preserved")
			}
			actualCh <- pointer.ReleaseID
			close(held)
			select {
			case <-release:
				return nil
			case <-holderCtx.Done():
				return holderCtx.Err()
			}
		})
	}()
	select {
	case <-held:
	case err := <-done:
		holderConsumed = true
		t.Fatalf("holder ended before lock: %v", err)
	case <-holderCtx.Done():
		t.Fatalf("holder did not acquire build pointer: %v", holderCtx.Err())
	}
	actualRelease := <-actualCh
	if actualRelease != chain.ReleaseA {
		t.Fatalf("actual build release = %s, want %s", actualRelease, chain.ReleaseA)
	}
	if actualRelease == chain.ReleaseB {
		t.Fatalf("actual build release aliased expected-B %s", chain.ReleaseB)
	}

	// The exact persisted reciprocal is A; an application boundary comparing
	// against B must reject it without entities deciding policy.
	if chain.ReleaseB == chain.ReleaseA {
		t.Fatal("test releases must differ")
	}

	tx := lockTimeoutTx(t, pool)
	defer tx.Rollback(ctx)
	requireLockTimeout(t, tx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM eregion_platform_builds WHERE id=$1", chain.Build)
		return err
	})
	tx2 := lockTimeoutTx(t, pool)
	defer tx2.Rollback(ctx)
	requireLockTimeout(t, tx2, func(tx pgx.Tx) error {
		var id string
		return tx.QueryRow(ctx, "SELECT id FROM eregion_platform_builds WHERE id=$1 FOR UPDATE", chain.Build).Scan(&id)
	})

	// A non-key column update is not blocked by FOR KEY SHARE. release_id is a
	// foreign-key column, not a key column; no key-changing/release_id test is
	// claimed here.
	tx3 := lockTimeoutTx(t, pool)
	defer tx3.Rollback(ctx)
	tag, err := tx3.Exec(ctx, "UPDATE eregion_platform_builds SET error='non-key update' WHERE id=$1", chain.Build)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("non-key update = %v, %v", tag.RowsAffected(), err)
	}
}

func TestIntegrationEntitiesPlatformOwnerRouteCAS(t *testing.T) {
	pool, _ := testPool(t, true)
	f := newPlatformFixture(t, pool, "owner-cas")
	chain := seedPlatformChain(t, f, "owner-cas")
	ctx := context.Background()
	store := f.store
	if renewed, err := store.Platform().Owners().Renew(ctx, f.env.ID, chain.OwnerOp, 2); err != nil || !renewed {
		t.Fatalf("renew = %v, %v", renewed, err)
	}
	if _, found, err := store.Platform().Owners().SetRoute(ctx, f.env.ID, chain.OwnerOp, chain.Deploy, chain.Deploy, "switching", 1); err != nil || found {
		t.Fatalf("stale route CAS = %v, %v", found, err)
	}
	owner, found, err := store.Platform().Owners().SetRoute(ctx, f.env.ID, chain.OwnerOp, chain.Deploy, chain.Deploy, "switching", 3)
	if err != nil || !found || owner.Attempt != 3 || owner.Phase != "switching" {
		t.Fatalf("route CAS = %+v, %v, %v", owner, found, err)
	}
	if renewed, err := store.Platform().Owners().Renew(ctx, f.env.ID, chain.OwnerOp, 2); err != nil || renewed {
		t.Fatalf("stale renew = %v, %v", renewed, err)
	}
	if inserted, err := store.Platform().Owners().Insert(ctx, entities.OwnerInsert{
		EnvironmentID: f.env.ID, OperationID: chain.OwnerOp, DeploymentID: chain.Deploy, Attempt: 4, Phase: "prepared",
	}); err != nil || inserted {
		t.Fatalf("duplicate owner insert = %v, %v", inserted, err)
	}
}

func TestIntegrationEntitiesPlatformResourceFencesAndValidation(t *testing.T) {
	pool, _ := testPool(t, true)
	f := newPlatformFixture(t, pool, "resource-fence")
	ctx := context.Background()
	store := f.store
	resourceID, _ := platform.NewResourceID()
	operationID, _ := contract.NewOperationID()
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: operationID, Key: entities.OperationKey{Kind: platform.KindResourceApply, Target: "resource/" + f.app.Name + "/" + f.env.Name + "/fenced", Actor: contract.Actor{Kind: "ci", Subject: "platform"}, IdempotencyKey: "resource-fence"},
			InputHash: "hash", Input: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		_, err := tx.Resources().Insert(ctx, entities.ResourceInsert{
			ID: resourceID, AppID: f.app.ID, EnvironmentID: f.env.ID, Name: "fenced", Node: f.node,
			Spec: platform.ResourceSpec{Image: "redis:7"}, Image: "redis:7", OperationID: operationID,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wrongOperation, _ := contract.NewOperationID()
	if rows, err := store.Resources().MarkReady(ctx, entities.ResourceReady{ID: resourceID, OperationID: wrongOperation}); err != nil || rows != 0 {
		t.Fatalf("wrong-operation ready = %d, %v", rows, err)
	}
	if rows, err := store.Resources().MarkDestroyed(ctx, entities.ResourceDestroyed{ID: resourceID, OperationID: wrongOperation}); err != nil || rows != 0 {
		t.Fatalf("wrong-operation destroyed = %d, %v", rows, err)
	}
	if rows, err := store.Resources().SetStateForTarget(ctx, wrongOperation, "resource/"+f.app.Name+"/"+f.env.Name+"/fenced", platform.ResourceProvisioning); err != nil || rows != 0 {
		t.Fatalf("wrong-operation state = %d, %v", rows, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE eregion_platform_resources SET state='bogus', spec='{\"image\":\"redis:7\"}'::jsonb, volumes='[{\"name\":\"wrong\"}]'::jsonb WHERE id=$1", resourceID); err != nil {
		t.Fatal(err)
	}
	resource, found, err := store.Resources().GetByName(ctx, f.app.Name, f.env.Name, "fenced", false)
	if err != nil || !found {
		t.Fatalf("raw resource read = %v, %v", found, err)
	}
	if resource.State != platform.ResourceState("bogus") || len(resource.Volumes) != 1 {
		t.Fatalf("resource raw facts were not preserved: %+v", resource)
	}
	if err := validateStoredResource(resource); err == nil {
		t.Fatal("application validator accepted a corrupt resource")
	}
}

func TestIntegrationEntitiesPlatformDeployabilityRawFacts(t *testing.T) {
	pool, _ := testPool(t, true)
	f := newPlatformFixture(t, pool, "deployability")
	ctx := context.Background()
	observation, found, err := f.store.Platform().Environments().GetByName(ctx, f.app.Name, f.env.Name)
	if err != nil || !found {
		t.Fatal(err)
	}
	env := observation.Environment
	if env.Deployability.Deployable || len(env.Deployability.Missing) != 0 || env.Deployability.DNS != "" {
		t.Fatalf("entities computed deployability: %+v", env.Deployability)
	}
	report := platform.EvaluateDeployability(env.Revision, platform.DeployabilityState{
		ArtifactReady: true, BuilderRequired: false, PrimaryDomain: env.PrimaryDomain, DNS: string(observation.PrimaryDNS),
	})
	if report.Deployable {
		t.Fatal("application boundary unexpectedly evaluated as deployable without a primary domain")
	}
}

func TestIntegrationEntitiesPlatformEnvironmentDNSObservation(t *testing.T) {
	pool, _ := testPool(t, true)
	f := newPlatformFixture(t, pool, "dns-observation")
	ctx := context.Background()
	domainID, err := platform.NewDomainID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Platform().Domains().Insert(ctx, entities.DomainInsert{
		ID: domainID, Domain: "dns-wrong.example.test", EnvironmentID: f.env.ID, Primary: true,
		Addresses: []string{"127.0.0.1"}, CheckPath: "/",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE eregion_platform_domains SET dns_state='wrong' WHERE id=$1", domainID); err != nil {
		t.Fatal(err)
	}
	observation, found, err := f.store.Platform().Environments().GetByName(ctx, f.app.Name, f.env.Name)
	if err != nil || !found {
		t.Fatal(err)
	}
	if observation.PrimaryDNS != platform.DNSWrong {
		t.Fatalf("PrimaryDNS = %q, want %q", observation.PrimaryDNS, platform.DNSWrong)
	}
	if observation.Environment.Deployability.Deployable || len(observation.Environment.Deployability.Missing) != 0 || observation.Environment.Deployability.DNS != "" {
		t.Fatalf("entities computed deployability: %+v", observation.Environment.Deployability)
	}
	state := platform.DeployabilityState{
		ArtifactReady:   observation.Environment.ArtifactReady,
		BuilderRequired: observation.Environment.Revision != nil && observation.Environment.Revision.Source.URL != "",
		PrimaryDomain:   observation.Environment.PrimaryDomain,
		DNS:             string(observation.PrimaryDNS),
	}
	report := platform.EvaluateDeployability(observation.Environment.Revision, state)
	if report.Deployable {
		t.Fatal("dns-wrong environment unexpectedly evaluated deployable")
	}
	foundDNS := false
	for _, missing := range report.Missing {
		if missing == "dns" {
			foundDNS = true
		}
	}
	if !foundDNS {
		t.Fatalf("dns-wrong report missing dns blocker: %+v", report)
	}
}

func TestIntegrationEntitiesPlatformCorruptSentinels(t *testing.T) {
	pool, _ := testPool(t, true)
	f := newPlatformFixture(t, pool, "corrupt")
	ctx := context.Background()
	store := f.store
	if _, err := pool.Exec(ctx, "UPDATE eregion_platform_environments SET env='[1]'::jsonb WHERE id=$1", f.env.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Platform().Environments().GetByName(ctx, f.app.Name, f.env.Name); !errors.Is(err, entities.ErrPlatformCorrupt) {
		t.Fatalf("invalid environment JSON = %v, want ErrPlatformCorrupt", err)
	}
	operationID, _ := contract.NewOperationID()
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt,completed_steps,last_sequence,error_code,created_at,updated_at)
		VALUES($1,'platform.test','env/'||$2,'system','test','corrupt-change','hash','{}','queued',0,0,0,'',clock_timestamp(),clock_timestamp())`, operationID, f.app.Name+"/"+f.env.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_changes(id,app_id,environment_id,sequence,kind,operation_id,actor_kind,actor_subject,summary,created_at)
		VALUES('chg_corrupt',$1,$2,-1,'corrupt',$3,'system','test','corrupt',clock_timestamp())`, f.app.ID, f.env.ID, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Platform().Changes().List(ctx, entities.ChangeFilter{App: f.app.Name, Limit: 10}); !errors.Is(err, entities.ErrPlatformCorrupt) {
		t.Fatalf("negative change sequence = %v, want ErrPlatformCorrupt", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_deployment_owners(environment_id,operation_id,deployment_id,owner_attempt,phase,created_at,updated_at)
		VALUES($1,$2,NULL,-1,'ambiguous',clock_timestamp(),clock_timestamp()) ON CONFLICT (environment_id) DO UPDATE SET owner_attempt=-1`, f.env.ID, operationID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Platform().Owners().Get(ctx, f.env.ID); !errors.Is(err, entities.ErrPlatformCorrupt) {
		t.Fatalf("negative owner attempt = %v, want ErrPlatformCorrupt", err)
	}
	resourceID, _ := platform.NewResourceID()
	resourceOperation, _ := contract.NewOperationID()
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: resourceOperation, Key: entities.OperationKey{Kind: platform.KindResourceApply, Target: "resource/" + f.app.Name + "/" + f.env.Name + "/corrupt", Actor: contract.Actor{Kind: "ci", Subject: "platform"}, IdempotencyKey: "corrupt-resource"},
			InputHash: "hash", Input: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		_, err := tx.Resources().Insert(ctx, entities.ResourceInsert{
			ID: resourceID, AppID: f.app.ID, EnvironmentID: f.env.ID, Name: "corrupt", Node: f.node,
			Spec: platform.ResourceSpec{Image: "redis:7"}, Image: "redis:7", OperationID: resourceOperation,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE eregion_platform_resources SET spec='[]'::jsonb WHERE id=$1", resourceID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Resources().GetByName(ctx, f.app.Name, f.env.Name, "corrupt", false); !errors.Is(err, entities.ErrPlatformCorrupt) {
		t.Fatalf("invalid resource JSON = %v, want ErrPlatformCorrupt", err)
	}
}

func advisoryKeyParts(key int64) (uint32, uint32) {
	raw := uint64(key)
	return uint32(raw >> 32), uint32(raw)
}

func waitCatalogCondition(ctx context.Context, conn *pgx.Conn, condition func(context.Context) (bool, error)) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := condition(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func TestIntegrationEntitiesPlatformSessionLeaseUncertainty(t *testing.T) {
	pool, dsn := testPool(t, true)
	f := newPlatformFixture(t, pool, "leases")
	ctx := context.Background()
	store := f.store
	baseKey := time.Now().UnixNano()
	key1, key2, key3 := baseKey, baseKey+1, baseKey+2
	if !(key1 < key2) {
		t.Fatalf("test requires key1 < key2: %d >= %d", key1, key2)
	}
	if high, low := advisoryKeyParts(-1); high != ^uint32(0) || low != ^uint32(0) {
		t.Fatalf("negative advisory key parts = %d/%d", high, low)
	}

	witness, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer witness.Close(ctx)
	if _, err := witness.Exec(ctx, "SELECT pg_advisory_lock($1)", key2); err != nil {
		t.Fatal(err)
	}
	defer witness.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key2)

	check, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close(ctx)

	acqCtx, cancelAcq := context.WithCancel(ctx)
	acqDone := make(chan error, 1)
	acqConsumed := false
	var capturedPID int
	t.Cleanup(func() {
		cancelAcq()
		if !acqConsumed {
			select {
			case <-acqDone:
			case <-time.After(2 * time.Second):
			}
		}
		if t.Failed() && capturedPID != 0 {
			var terminated bool
			_ = pool.QueryRow(context.Background(), "SELECT pg_terminate_backend($1, 2000)", capturedPID).Scan(&terminated)
		}
	})
	go func() {
		_, err := store.Platform().Locks().AcquireTargets(acqCtx, []int64{key1, key2})
		acqDone <- err
	}()

	witnessCtx, cancelWitness := context.WithTimeout(context.Background(), 2*time.Second)
	high1, low1 := advisoryKeyParts(key1)
	high2, low2 := advisoryKeyParts(key2)
	err = waitCatalogCondition(witnessCtx, check, func(conditionCtx context.Context) (bool, error) {
		err := check.QueryRow(conditionCtx, `SELECT l1.pid
			FROM pg_locks l1
			JOIN pg_locks l2 ON l2.pid=l1.pid
			JOIN pg_stat_activity a ON a.pid=l1.pid
			WHERE l1.database=(SELECT oid FROM pg_database WHERE datname=current_database())
			  AND l2.database=l1.database
			  AND l1.locktype='advisory' AND l2.locktype='advisory'
			  AND l1.mode='ExclusiveLock' AND l2.mode='ExclusiveLock'
			  AND l1.objsubid=1 AND l2.objsubid=1
			  AND l1.classid=$1::oid AND l1.objid=$2::oid AND l1.granted
			  AND l2.classid=$3::oid AND l2.objid=$4::oid AND NOT l2.granted
			  AND a.datname=current_database()
			  AND a.wait_event_type='Lock' AND a.wait_event='advisory'
			LIMIT 1`, high1, low1, high2, low2).Scan(&capturedPID)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	})
	cancelWitness()
	if err != nil {
		cancelAcq()
		<-acqDone
		acqConsumed = true
		t.Fatalf("did not witness blocked acquisition: %v", err)
	}
	cancelAcq()
	acqErr := <-acqDone
	acqConsumed = true
	if !errors.Is(acqErr, entities.ErrPlatformLeaseUncertain) {
		t.Fatalf("partial acquisition cancellation = %v, want ErrPlatformLeaseUncertain", acqErr)
	}

	absenceCtx, cancelAbsence := context.WithTimeout(context.Background(), 2*time.Second)
	err = waitCatalogCondition(absenceCtx, check, func(conditionCtx context.Context) (bool, error) {
		var absent bool
		err := check.QueryRow(conditionCtx, `SELECT NOT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE pid=$1
			  AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
			  AND locktype='advisory' AND mode='ExclusiveLock' AND objsubid=1
			  AND classid=$2::oid AND objid=$3::oid AND granted
		) AND NOT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid=$1)`, capturedPID, high1, low1).Scan(&absent)
		if err != nil {
			return false, err
		}
		return absent, nil
	})
	cancelAbsence()
	if err != nil {
		var locks string
		_ = check.QueryRow(ctx, `SELECT coalesce(string_agg(pid::text||':'||database::text||':'||classid::text||':'||objid::text||':'||granted::text, ','), '') FROM pg_locks WHERE locktype='advisory'`).Scan(&locks)
		t.Fatalf("acquisition cleanup did not reach natural absence: pid=%d key1=%d key2=%d locks=%s err=%v", capturedPID, key1, key2, locks, err)
	}

	tx, err := check.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var acquired bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", key1).Scan(&acquired); err != nil || !acquired {
		_ = tx.Rollback(ctx)
		t.Fatalf("key1 not free after natural cleanup: %v, %v", acquired, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	lease, err := store.Platform().Locks().AcquireTargets(ctx, []int64{key3})
	if err != nil {
		t.Fatal(err)
	}
	high3, low3 := advisoryKeyParts(key3)
	var leasePID int
	if err := check.QueryRow(ctx, `SELECT pid FROM pg_locks
		WHERE database=(SELECT oid FROM pg_database WHERE datname=current_database())
		  AND locktype='advisory' AND mode='ExclusiveLock' AND objsubid=1
		  AND classid=$1::oid AND objid=$2::oid AND granted
		LIMIT 1`, high3, low3).Scan(&leasePID); err != nil {
		t.Fatal(err)
	}
	if _, err := check.Exec(ctx, "SELECT pg_terminate_backend($1)", leasePID); err != nil {
		t.Fatal(err)
	}
	firstErr := lease.Release()
	secondErr := lease.Release()
	if !errors.Is(firstErr, entities.ErrPlatformLeaseUncertain) || !errors.Is(secondErr, entities.ErrPlatformLeaseUncertain) || firstErr.Error() != secondErr.Error() {
		t.Fatalf("release errors = %v / %v", firstErr, secondErr)
	}
	usable, err := store.Platform().Locks().AcquireTargets(ctx, []int64{key1})
	if err != nil {
		t.Fatalf("pool not usable after discard: %v", err)
	}
	if err := usable.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationEntitiesPlatformBuildSlotsAndTargetContention(t *testing.T) {
	pool, _ := testPool(t, true)
	f := newPlatformFixture(t, pool, "slots")
	ctx := context.Background()
	store := f.store
	if _, err := store.Nodes().SetBuildConcurrency(ctx, f.node, 2); err != nil {
		t.Fatal(err)
	}
	first, err := store.Nodes().ProbeBuildSlots(ctx, f.node, platform.MaxBuildConcurrency)
	if err != nil || first.Lease == nil {
		t.Fatalf("first probe = %+v, %v", first, err)
	}
	second, err := store.Nodes().ProbeBuildSlots(ctx, f.node, platform.MaxBuildConcurrency)
	if err != nil || second.Lease == nil {
		t.Fatalf("second probe = %+v, %v", second, err)
	}
	if _, err := store.Nodes().SetBuildConcurrency(ctx, f.node, 1); err != nil {
		t.Fatal(err)
	}
	third, err := store.Nodes().ProbeBuildSlots(ctx, f.node, platform.MaxBuildConcurrency)
	if err != nil || third.Lease != nil || third.Busy < 2 {
		t.Fatalf("reduced-capacity probe = %+v, %v", third, err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	fourth, err := store.Nodes().ProbeBuildSlots(ctx, f.node, platform.MaxBuildConcurrency)
	if err != nil || fourth.Lease == nil {
		t.Fatalf("post-release probe = %+v, %v", fourth, err)
	}
	if err := fourth.Release(); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := store.Platform().Locks().TryTargets(ctx, []int64{92001})
	if err != nil || !acquired {
		t.Fatalf("first target = %v, %v", acquired, err)
	}
	if _, acquired, err := store.Platform().Locks().TryTargets(ctx, []int64{92001}); err != nil || acquired {
		t.Fatalf("contended target = %v, %v", acquired, err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if again, acquired, err := store.Platform().Locks().TryTargets(ctx, []int64{92001}); err != nil || !acquired {
		t.Fatalf("released target = %v, %v", acquired, err)
	} else if err := again.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationEntitiesPlatformCrossDomainDecoys(t *testing.T) {
	pool, _ := testPool(t, true)
	f := newPlatformFixture(t, pool, "decoy")
	ctx := context.Background()
	store := f.store
	secondApp, _ := platform.NewAppID()
	secondEnv, _ := platform.NewEnvironmentID()
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, err := tx.Platform().Apps().Insert(ctx, entities.AppInsert{ID: secondApp, Name: "decoy-app", ImageRepository: "example/decoy-other"}); err != nil {
			return err
		}
		_, err := tx.Platform().Environments().Insert(ctx, entities.EnvironmentInsert{
			ID: secondEnv, AppID: secondApp, Name: "production", Node: f.node, HostPort: 27000, CandidatePort: 37000,
			ContainerPort: 8080, Bind: "127.0.0.1", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30,
			Env: map[string]string{}, SecretRefs: map[string]string{},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	secondDomain, _ := platform.NewDomainID()
	if _, err := store.Platform().Domains().Insert(ctx, entities.DomainInsert{
		ID: secondDomain, Domain: "decoy.example.test", EnvironmentID: secondEnv, Primary: true, Addresses: []string{"127.0.0.2"}, CheckPath: "/",
	}); err != nil {
		t.Fatal(err)
	}
	counts, err := store.Platform().Outcomes().DeletionCounts(ctx, f.app.Name, f.env.Name)
	if err != nil || counts.Environments != 1 || counts.Domains != 0 {
		t.Fatalf("decoy deletion counts = %+v, %v", counts, err)
	}
	binding, found, err := store.Platform().Outcomes().WebhookBinding(ctx, f.env.ID)
	if err != nil || !found || binding.Environment != f.env.Name {
		t.Fatalf("decoy webhook binding = %+v, %v, %v", binding, found, err)
	}
}
