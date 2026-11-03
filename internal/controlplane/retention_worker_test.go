package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// retentionFakeTransport answers the retention inventory and records every
// node effect. It never touches Docker or a real journal.
type retentionFakeTransport struct {
	inventory retention.Inventory
	steps     []string
	payloads  []platform.RetentionRemovePayload
	failSteps map[string]int
	onRun     func(string)
}

func (*retentionFakeTransport) Name() string         { return "retention-fake" }
func (*retentionFakeTransport) NodeIdentity() string { return "local" }

func (f *retentionFakeTransport) Call(_ context.Context, request node.Request, _ func(node.Event)) (node.Response, error) {
	switch request.Method {
	case node.MethodInventory:
		raw, _ := json.Marshal(f.inventory)
		return node.Response{Status: engine.StatusCompleted, Result: raw}, nil
	case node.MethodStatus:
		return node.Response{Status: engine.StatusAbsent}, nil
	case node.MethodRun:
		step := request.Execution.Step
		if f.onRun != nil {
			f.onRun(step)
		}
		var payload platform.RetentionRemovePayload
		if json.Unmarshal(request.Payload, &payload) == nil && payload.Kind != "" {
			f.payloads = append(f.payloads, payload)
		}
		f.steps = append(f.steps, step)
		if f.failSteps[step] > 0 {
			f.failSteps[step]--
			return node.Response{Status: engine.StatusFailed, ErrorCode: string(contract.Unavailable), Retryable: true, Detail: "transient node failure"}, nil
		}
		return node.Response{Status: engine.StatusCompleted, Result: json.RawMessage(`{"removed":true}`)}, nil
	default:
		return node.Response{}, fmt.Errorf("unexpected method %q", request.Method)
	}
}

type retentionFixture struct {
	service   *Service
	pool      *pgxpool.Pool
	transport *retentionFakeTransport
	app       string
	env       string
	oldID     string
	newID     string
	orphan    string
}

func newRetentionFixture(t *testing.T) *retentionFixture {
	t.Helper()
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	app, environment := "retention", "production"
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "retention-worker-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, "platform", "retention-worker-app", map[string]any{
		"name": app,
		"environment": map[string]any{
			"name": environment, "node": "local",
			"source":           map[string]any{"kind": "image", "image": "127.0.0.1:5000/api:new"},
			"domain":           "retention-worker.example.com",
			"domain_addresses": []string{"203.0.113.10"},
			"retention": map[string]any{
				"keep_releases": 1, "keep_newer_than_days": 1, "artifact_grace_days": 1,
			},
		},
	})
	now := time.Now().UTC()
	oldConfig := "sha256:" + strings.Repeat("a", 64)
	newConfig := "sha256:" + strings.Repeat("b", 64)
	oldID := insertRetentionWorkerRelease(t, pool, app, environment, "127.0.0.1:5000/api:old", oldConfig, now.Add(-30*24*time.Hour))
	newID := insertRetentionWorkerRelease(t, pool, app, environment, "127.0.0.1:5000/api:new", newConfig, now.Add(-29*24*time.Hour))
	keptDigest := "sha256:" + strings.Repeat("c", 64)
	orphanDigest := "sha256:" + strings.Repeat("d", 64)
	old := now.Add(-30 * 24 * time.Hour)
	transport := &retentionFakeTransport{
		inventory: retention.Inventory{
			CollectedAt: now,
			RegistryArtifacts: []retention.InventoryRegistryArtifact{
				{Repository: "api", Digest: keptDigest, ConfigDigest: newConfig, Bytes: 10, CreatedAt: &old},
				{Repository: "api", Digest: orphanDigest, Bytes: 100, CreatedAt: &old},
			},
			CacheVolumes: []retention.CacheVolume{
				{Name: "eregion-build-cache-617069_70726f64756374696f6e", Kind: "build", App: "api", Environment: "production", SizeBytes: 10},
				{Name: "pack-cache-abc.build", Kind: "pack", Ownerless: true, SizeBytes: 20},
			},
			Containers:     []retention.InventoryContainer{},
			DanglingImages: []retention.InventoryImage{},
			LocalImages: []retention.LocalImage{
				{ID: oldConfig, RepoTags: []string{"127.0.0.1:5000/api:old"}},
				{ID: newConfig, RepoTags: []string{"127.0.0.1:5000/api:new"}},
			},
			Disk: retention.Disk{TotalBytes: 1 << 40, FreeBytes: 1 << 39},
		},
		failSteps: map[string]int{},
	}
	s.transport = transport
	return &retentionFixture{
		service: s, pool: pool, transport: transport, app: app, env: environment,
		oldID: oldID, newID: newID, orphan: "api@" + orphanDigest,
	}
}

func insertRetentionWorkerRelease(t *testing.T, pool *pgxpool.Pool, app, environment, image, imageID string, createdAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	var appID, environmentID string
	if err := pool.QueryRow(ctx, `SELECT e.app_id,e.id FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1 AND e.name=$2`, app, environment).
		Scan(&appID, &environmentID); err != nil {
		t.Fatal(err)
	}
	releaseID, err := platform.NewReleaseID()
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations
        (id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state)
        VALUES($1,$2,$3,'system','retention-worker-test',$4,'retention-worker-test','{}'::jsonb,'succeeded')`,
		operationID, platform.KindReleaseCreate, "env/"+app+"/"+environment, "retention-worker-release:"+string(releaseID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_releases
        (id,operation_id,app_id,environment_id,image,image_id,source,status,created_at)
        VALUES($1,$2,$3,$4,$5,$6,'{"type":"image"}'::jsonb,'ready',$7)`,
		releaseID, operationID, appID, environmentID, image, imageID, createdAt); err != nil {
		t.Fatal(err)
	}
	return string(releaseID)
}

func submitRetentionWorkerPrune(t *testing.T, s *Service, key string) contract.Operation {
	t.Helper()
	input, _ := json.Marshal(platform.RetentionPruneInput{Node: "local"})
	op, err := s.Submit(context.Background(), operations.Submission{
		Kind: platform.KindRetentionPrune, Target: "node/local", Actor: testActor,
		IdempotencyKey: key, Input: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func retentionWorkerJob(id contract.OperationID, attempt, maxAttempts int) *river.Job[retentionArgs] {
	return &river.Job[retentionArgs]{
		JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: maxAttempts},
		Args:   retentionArgs{OperationID: id},
	}
}

func TestIntegrationRetentionShowAndPlanAreReadOnly(t *testing.T) {
	fixture := newRetentionFixture(t)
	s := fixture.service
	ctx := context.Background()
	view, err := s.RetentionShow(ctx, fixture.app, fixture.env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Scope != "environment" || view.Effective.KeepReleases != 1 ||
		view.Sources["keep_releases"] != "environment" || view.Sources["keep_newer_than_days"] != "environment" {
		t.Fatalf("view = %+v", view)
	}
	plan, err := s.RetentionPlan(ctx, "local", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Node != "local" || len(plan.Items) == 0 {
		t.Fatalf("plan = %+v", plan)
	}
	var runs int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM eregion_retention_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("read-only planning wrote %d retention run(s)", runs)
	}
}

func TestIntegrationRetentionWorkerMarksRemovesThenGCs(t *testing.T) {
	fixture := newRetentionFixture(t)
	s := fixture.service
	op := submitRetentionWorkerPrune(t, s, "retention-worker-order")
	firstStepPruned := false
	calls := 0
	fixture.transport.onRun = func(step string) {
		calls++
		if calls == 1 && !firstStepPruned {
			firstStepPruned = true
			var pruned *time.Time
			if err := fixture.pool.QueryRow(context.Background(),
				`SELECT pruned_at FROM eregion_platform_releases WHERE id=$1`, fixture.oldID).Scan(&pruned); err != nil {
				t.Fatal(err)
			}
			if pruned == nil {
				t.Fatal("the first removal effect ran before the release row was marked pruned_at")
			}
		}
	}
	worker := &retentionWorker{service: s}
	if err := worker.Work(context.Background(), retentionWorkerJob(op.ID, 1, 8)); err != nil {
		t.Fatalf("%v: %v", err, errors.Unwrap(err))
	}

	var storedOp contract.Operation
	if err := fixture.pool.QueryRow(context.Background(), "SELECT "+columns+" FROM eregion_operations WHERE id=$1", op.ID).
		Scan(&storedOp.ID, &storedOp.Kind, &storedOp.Target, &storedOp.Actor.Kind, &storedOp.Actor.Subject,
			&storedOp.State, &storedOp.Attempt, &storedOp.LastSequence, &storedOp.ErrorCode, &storedOp.CreatedAt, &storedOp.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if storedOp.State != contract.Succeeded {
		t.Fatalf("operation state = %s, want succeeded", storedOp.State)
	}
	var oldPruned, newPruned *time.Time
	if err := fixture.pool.QueryRow(context.Background(), `SELECT pruned_at FROM eregion_platform_releases WHERE id=$1`, fixture.oldID).Scan(&oldPruned); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(context.Background(), `SELECT pruned_at FROM eregion_platform_releases WHERE id=$1`, fixture.newID).Scan(&newPruned); err != nil {
		t.Fatal(err)
	}
	if oldPruned == nil {
		t.Fatal("the old release was not marked pruned")
	}
	if newPruned != nil {
		t.Fatal("the retained release was pruned")
	}

	var status string
	var plannedBytes, reclaimedBytes int64
	var plannedRaw, removedRaw, errorsRaw []byte
	if err := fixture.pool.QueryRow(context.Background(), `SELECT status,planned_bytes,reclaimed_bytes,planned_items,removed_items,errors
        FROM eregion_retention_runs WHERE operation_id=$1`, op.ID).
		Scan(&status, &plannedBytes, &reclaimedBytes, &plannedRaw, &removedRaw, &errorsRaw); err != nil {
		t.Fatal(err)
	}
	if status != string(retention.RunSucceeded) || plannedBytes != 100 || reclaimedBytes != 100 {
		t.Fatalf("run = %s planned=%d reclaimed=%d errors=%s", status, plannedBytes, reclaimedBytes, errorsRaw)
	}
	var cacheBytes *int64
	var cacheMeasuredAt *time.Time
	if err := fixture.pool.QueryRow(context.Background(), `SELECT cache_bytes,cache_measured_at
        FROM eregion_retention_runs WHERE operation_id=$1`, op.ID).Scan(&cacheBytes, &cacheMeasuredAt); err != nil {
		t.Fatal(err)
	}
	if cacheBytes == nil || *cacheBytes != 30 || cacheMeasuredAt == nil {
		t.Fatalf("cache measurement = %v at %v, want 30 bytes and a time", cacheBytes, cacheMeasuredAt)
	}
	var removed []retention.Item
	if err := json.Unmarshal(removedRaw, &removed); err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(removed))
	for _, item := range removed {
		kinds = append(kinds, string(item.Kind))
	}
	if len(kinds) != 4 || kinds[0] != "release" || kinds[3] != "registry_gc" {
		t.Fatalf("removed kinds = %v", kinds)
	}
	seenImage, seenRegistry := false, false
	for _, item := range removed {
		switch item.Kind {
		case retention.ItemImage:
			seenImage = true
		case retention.ItemRegistryArtifact:
			seenRegistry = true
		}
	}
	if !seenImage || !seenRegistry {
		t.Fatalf("the run did not remove both the local image and the registry manifest: %+v", removed)
	}

	if len(fixture.transport.payloads) != 2 {
		t.Fatalf("removal payloads = %+v", fixture.transport.payloads)
	}
	seenImage, seenRegistry = false, false
	for _, payload := range fixture.transport.payloads {
		switch payload.Kind {
		case string(retention.ItemImage):
			seenImage = true
		case string(retention.ItemRegistryArtifact):
			if payload.Reference != fixture.orphan {
				t.Fatalf("registry effect = %+v, want the orphan manifest", payload)
			}
			seenRegistry = true
		}
	}
	if !seenImage || !seenRegistry {
		t.Fatalf("removal payloads = %+v", fixture.transport.payloads)
	}
	if got := fixture.transport.steps[len(fixture.transport.steps)-1]; got != "platform-retention-gc-01-v1" {
		t.Fatalf("last step = %s, want the registry garbage collection", got)
	}
}

func TestIntegrationRetentionWorkerRetriesAFailedEffect(t *testing.T) {
	fixture := newRetentionFixture(t)
	s := fixture.service
	op := submitRetentionWorkerPrune(t, s, "retention-worker-retry")
	orphanStep := platform.RetentionRemoveStepID(retention.ItemRegistryArtifact, fixture.orphan)
	fixture.transport.failSteps[orphanStep] = 1
	worker := &retentionWorker{service: s}

	if err := worker.Work(context.Background(), retentionWorkerJob(op.ID, 1, 8)); err == nil {
		t.Fatal("the first attempt did not report the transient effect failure")
	}
	var status string
	var runErrors []byte
	if err := fixture.pool.QueryRow(context.Background(), `SELECT status,errors FROM eregion_retention_runs WHERE operation_id=$1`, op.ID).
		Scan(&status, &runErrors); err != nil {
		t.Fatal(err)
	}
	if status != string(retention.RunFailed) || !strings.Contains(string(runErrors), "transient node failure") {
		t.Fatalf("failed run = %s errors=%s", status, runErrors)
	}
	var state contract.State
	if err := fixture.pool.QueryRow(context.Background(), `SELECT state FROM eregion_operations WHERE id=$1`, op.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != contract.Running {
		t.Fatalf("operation state after a retryable failure = %s, want running", state)
	}

	if err := worker.Work(context.Background(), retentionWorkerJob(op.ID, 2, 8)); err != nil {
		t.Fatalf("%v: %v", err, errors.Unwrap(err))
	}
	if err := fixture.pool.QueryRow(context.Background(), `SELECT status,errors FROM eregion_retention_runs WHERE operation_id=$1`, op.ID).
		Scan(&status, &runErrors); err != nil {
		t.Fatal(err)
	}
	if status != string(retention.RunSucceeded) {
		t.Fatalf("retried run status = %s errors=%s", status, runErrors)
	}
	if err := fixture.pool.QueryRow(context.Background(), `SELECT state FROM eregion_operations WHERE id=$1`, op.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != contract.Succeeded {
		t.Fatalf("operation state after retry = %s, want succeeded", state)
	}
	dispatches := 0
	for _, step := range fixture.transport.steps {
		if step == orphanStep {
			dispatches++
		}
	}
	if dispatches != 2 {
		t.Fatalf("registry effect dispatched %d times, want the failed attempt plus one retry", dispatches)
	}
	if got := fixture.transport.steps[len(fixture.transport.steps)-1]; got != "platform-retention-gc-01-v1" {
		t.Fatalf("the retried registry delete was not followed by GC: %s", got)
	}
}

func TestIntegrationRetentionWorkerProtectsAReleaseThatBecomesServing(t *testing.T) {
	fixture := newRetentionFixture(t)
	s := fixture.service
	op := submitRetentionWorkerPrune(t, s, "retention-worker-race")
	s.beforeRetentionMark = func(ctx context.Context, _ contract.OperationID) {
		if _, err := fixture.pool.Exec(ctx, `UPDATE eregion_platform_environments e
            SET serving_release_id=$1, serving_deployment_id=NULL
            FROM eregion_platform_apps a
            WHERE e.app_id=a.id AND a.name=$2 AND e.name=$3`, fixture.oldID, fixture.app, fixture.env); err != nil {
			t.Fatal(err)
		}
	}
	if err := (&retentionWorker{service: s}).Work(context.Background(), retentionWorkerJob(op.ID, 1, 8)); err != nil {
		t.Fatal(err)
	}
	var pruned *time.Time
	if err := fixture.pool.QueryRow(context.Background(), `SELECT pruned_at FROM eregion_platform_releases WHERE id=$1`, fixture.oldID).Scan(&pruned); err != nil {
		t.Fatal(err)
	}
	if pruned != nil {
		t.Fatal("a release that became serving between plan and mark was pruned")
	}
	var plannedRaw []byte
	if err := fixture.pool.QueryRow(context.Background(), `SELECT planned_items FROM eregion_retention_runs WHERE operation_id=$1`, op.ID).Scan(&plannedRaw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plannedRaw), fixture.oldID) {
		t.Fatalf("the protected release remained in the planned items: %s", plannedRaw)
	}
	for _, payload := range fixture.transport.payloads {
		if payload.Kind == string(retention.ItemRegistryArtifact) {
			t.Fatalf("a registry artifact was removed while a protected release was present: %+v", payload)
		}
	}
}

func TestIntegrationRetentionWorkerKeepsSharedReleaseImage(t *testing.T) {
	fixture := newRetentionFixture(t)
	s := fixture.service
	// A retained release already owns the planned release's image ID because
	// its ImageID is the same config digest.
	sharedConfig := "sha256:" + strings.Repeat("f", 64)
	if _, err := fixture.pool.Exec(context.Background(), `UPDATE eregion_platform_releases SET image_id=$1 WHERE id=$2`,
		sharedConfig, fixture.newID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `UPDATE eregion_platform_releases SET image_id=$1 WHERE id=$2`,
		sharedConfig, fixture.oldID); err != nil {
		t.Fatal(err)
	}
	op := submitRetentionWorkerPrune(t, s, "retention-worker-shared-image")
	if err := (&retentionWorker{service: s}).Work(context.Background(), retentionWorkerJob(op.ID, 1, 8)); err != nil {
		t.Fatal(err)
	}
	for _, payload := range fixture.transport.payloads {
		if payload.Kind == string(retention.ItemImage) && payload.Reference == sharedConfig {
			t.Fatalf("a shared release image was removed: %+v", payload)
		}
	}
	var pruned *time.Time
	if err := fixture.pool.QueryRow(context.Background(), `SELECT pruned_at FROM eregion_platform_releases WHERE id=$1`, fixture.oldID).Scan(&pruned); err != nil {
		t.Fatal(err)
	}
	if pruned == nil {
		t.Fatal("the old release row was not marked pruned")
	}
}

func TestIntegrationRetentionWorkerRemovesAPrunedReleasesPresentImage(t *testing.T) {
	fixture := newRetentionFixture(t)
	s := fixture.service
	if _, err := fixture.pool.Exec(context.Background(), `UPDATE eregion_platform_releases SET pruned_at=clock_timestamp() WHERE id=$1`, fixture.oldID); err != nil {
		t.Fatal(err)
	}
	op := submitRetentionWorkerPrune(t, s, "retention-worker-pruned-image")
	if err := (&retentionWorker{service: s}).Work(context.Background(), retentionWorkerJob(op.ID, 1, 8)); err != nil {
		t.Fatal(err)
	}
	oldConfig := "sha256:" + strings.Repeat("a", 64)
	found := false
	for _, payload := range fixture.transport.payloads {
		if payload.Kind == string(retention.ItemImage) && payload.Reference == oldConfig {
			found = true
		}
	}
	if !found {
		t.Fatalf("a pruned release's still-present image was not removed: %+v", fixture.transport.payloads)
	}
}

func TestIntegrationRetentionWorkerRevalidatesOnRetry(t *testing.T) {
	fixture := newRetentionFixture(t)
	s := fixture.service
	orphanConfig := "sha256:" + strings.Repeat("e", 64)
	fixture.transport.inventory.RegistryArtifacts[1].ConfigDigest = orphanConfig
	op := submitRetentionWorkerPrune(t, s, "retention-worker-revalidate")
	orphanStep := platform.RetentionRemoveStepID(retention.ItemRegistryArtifact, fixture.orphan)
	fixture.transport.failSteps[orphanStep] = 1
	worker := &retentionWorker{service: s}
	if err := worker.Work(context.Background(), retentionWorkerJob(op.ID, 1, 8)); err == nil {
		t.Fatal("the first attempt did not fail the registry effect")
	}
	// The new retained release owns the orphan manifest's config digest, so a
	// retry must not delete the now-protected registry artifact.
	insertRetentionWorkerRelease(t, fixture.pool, fixture.app, fixture.env,
		"127.0.0.1:5000/api:protector", orphanConfig, time.Now().UTC())
	if err := worker.Work(context.Background(), retentionWorkerJob(op.ID, 2, 8)); err != nil {
		t.Fatal(err)
	}
	dispatches := 0
	for _, step := range fixture.transport.steps {
		if step == orphanStep {
			dispatches++
		}
		if step == "platform-retention-gc-01-v1" {
			t.Fatalf("GC ran after the registry deletion was dropped: %v", fixture.transport.steps)
		}
	}
	if dispatches != 1 {
		t.Fatalf("the protected registry artifact was dispatched %d times, want only the failed first attempt", dispatches)
	}
	var removedRaw []byte
	if err := fixture.pool.QueryRow(context.Background(), `SELECT removed_items FROM eregion_retention_runs WHERE operation_id=$1`, op.ID).Scan(&removedRaw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(removedRaw), "registry_artifact") {
		t.Fatalf("the retry recorded a protected registry artifact as removed: %s", removedRaw)
	}
}

func TestIntegrationRetentionDailySubmissionIsIdempotent(t *testing.T) {
	fixture := newRetentionFixture(t)
	s := fixture.service
	ctx := context.Background()
	if err := s.submitDailyRetention(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.submitDailyRetention(ctx); err != nil {
		t.Fatal(err)
	}
	var operations, jobs int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operations WHERE kind=$1`, platform.KindRetentionPrune).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind='platform_retention'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if operations != 1 || jobs != 1 {
		t.Fatalf("daily retention operations=%d jobs=%d, want one operation and one job", operations, jobs)
	}
}

func TestIntegrationRetentionRetryRevalidatesPolicyBeforeMarking(t *testing.T) {
	fixture := newRetentionFixture(t)
	s := fixture.service
	ctx := context.Background()
	op := submitRetentionWorkerPrune(t, s, "retention-worker-policy-retry")
	runID, err := retention.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	run := retention.Run{
		ID: runID, OperationID: string(op.ID), Status: retention.RunFailed,
		Planned: []retention.Item{{
			Kind: retention.ItemRelease, App: fixture.app, Environment: fixture.env, Reference: fixture.oldID,
		}},
		StartedAt: time.Now().UTC(),
	}
	if err := s.insertRetentionRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	platformSubmit(t, s, platform.KindEnvironmentUpdate, "env/"+fixture.app+"/"+fixture.env,
		"retention-worker-policy-raised", map[string]any{
			"retention": map[string]any{"keep_releases": 10},
		})
	if err := (&retentionWorker{service: s}).Work(ctx, retentionWorkerJob(op.ID, 1, 8)); err != nil {
		t.Fatal(err)
	}
	var pruned *time.Time
	if err := fixture.pool.QueryRow(ctx, `SELECT pruned_at FROM eregion_platform_releases WHERE id=$1`, fixture.oldID).Scan(&pruned); err != nil {
		t.Fatal(err)
	}
	if pruned != nil {
		t.Fatal("a release no longer eligible under the raised policy was marked pruned")
	}
	var plannedRaw []byte
	if err := fixture.pool.QueryRow(ctx, `SELECT planned_items FROM eregion_retention_runs WHERE operation_id=$1`, op.ID).Scan(&plannedRaw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plannedRaw), fixture.oldID) {
		t.Fatalf("the ineligible release remained in the persisted plan: %s", plannedRaw)
	}
}

func TestIntegrationRetentionFailedMarkIsRetried(t *testing.T) {
	fixture := newRetentionFixture(t)
	s := fixture.service
	ctx := context.Background()
	op := submitRetentionWorkerPrune(t, s, "retention-worker-failed-mark")
	realMark := s.markRetentionRelease
	first := true
	s.retentionMark = func(ctx context.Context, releaseID string) (bool, error) {
		if first {
			first = false
			return false, errors.New("transient mark failure")
		}
		return realMark(ctx, releaseID)
	}
	worker := &retentionWorker{service: s}
	if err := worker.Work(ctx, retentionWorkerJob(op.ID, 1, 8)); err == nil {
		t.Fatal("the failed mark did not fail the attempt")
	}
	var plannedRaw []byte
	if err := fixture.pool.QueryRow(ctx, `SELECT planned_items FROM eregion_retention_runs WHERE operation_id=$1`, op.ID).Scan(&plannedRaw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plannedRaw), fixture.oldID) {
		t.Fatalf("the failed release mark was dropped from the retry plan: %s", plannedRaw)
	}
	if err := worker.Work(ctx, retentionWorkerJob(op.ID, 2, 8)); err != nil {
		t.Fatal(err)
	}
	var pruned *time.Time
	if err := fixture.pool.QueryRow(ctx, `SELECT pruned_at FROM eregion_platform_releases WHERE id=$1`, fixture.oldID).Scan(&pruned); err != nil {
		t.Fatal(err)
	}
	if pruned == nil {
		t.Fatal("the retried mark did not mark the release pruned")
	}
	oldConfig := "sha256:" + strings.Repeat("a", 64)
	foundImage := false
	for _, payload := range fixture.transport.payloads {
		if payload.Kind == string(retention.ItemImage) && payload.Reference == oldConfig {
			foundImage = true
		}
	}
	if !foundImage {
		t.Fatalf("the retried release image was not removed: %+v", fixture.transport.payloads)
	}
}

func TestIntegrationRetentionPendingGCRunsWithoutNewDeletions(t *testing.T) {
	fixture := newRetentionFixture(t)
	s := fixture.service
	ctx := context.Background()
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	fixture.transport.inventory.RegistryArtifacts = []retention.InventoryRegistryArtifact{{
		Repository: "api", Digest: "sha256:" + strings.Repeat("c", 64),
		ConfigDigest: "sha256:" + strings.Repeat("b", 64), Bytes: 10, CreatedAt: &old,
	}}
	fixture.transport.inventory.LocalImages = nil
	fixture.transport.inventory.DanglingImages = nil
	if _, err := fixture.pool.Exec(ctx, `UPDATE eregion_platform_releases SET pruned_at=clock_timestamp() WHERE id=$1`, fixture.oldID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `INSERT INTO eregion_server_settings (singleton,retention_gc_pending)
        VALUES (true,true) ON CONFLICT (singleton) DO UPDATE SET retention_gc_pending=true`); err != nil {
		t.Fatal(err)
	}
	fixture.transport.failSteps["platform-retention-gc-01-v1"] = 1
	first := submitRetentionWorkerPrune(t, s, "retention-worker-gc-pending-1")
	if err := (&retentionWorker{service: s}).Work(ctx, retentionWorkerJob(first.ID, 1, 8)); err == nil {
		t.Fatal("the first GC attempt did not fail")
	}
	var pending bool
	if err := fixture.pool.QueryRow(ctx, `SELECT retention_gc_pending FROM eregion_server_settings WHERE singleton`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("a failed GC did not leave the durable pending flag")
	}
	second := submitRetentionWorkerPrune(t, s, "retention-worker-gc-pending-2")
	if err := (&retentionWorker{service: s}).Work(ctx, retentionWorkerJob(second.ID, 1, 8)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `SELECT retention_gc_pending FROM eregion_server_settings WHERE singleton`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("a successful follow-up GC did not clear the pending flag")
	}
	var removedRaw []byte
	if err := fixture.pool.QueryRow(ctx, `SELECT removed_items FROM eregion_retention_runs WHERE operation_id=$1`, second.ID).Scan(&removedRaw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(removedRaw), string(retention.ItemRegistryGC)) {
		t.Fatalf("the follow-up run did not record GC: %s", removedRaw)
	}
}
