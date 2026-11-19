package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/registrymeta"
	"github.com/riverqueue/river"
)

// This fixture requires an isolated Docker daemon with the pinned Eregion
// registry container published at the supplied loopback endpoint, Pack on PATH,
// and the Paketo builder available. It creates a real image and Docker volumes.
func testVerifiedLocalPack(t *testing.T) { //nolint:gocognit,gocyclo // One ordered scenario covers admission, publication and release.
	t.Helper()
	pool, _ := testPool(t, true)
	registry := os.Getenv("EREGION_TEST_REAL_PACK_REGISTRY")
	if registry == "" {
		t.Fatal("set EREGION_TEST_REAL_PACK_REGISTRY to the registry on an isolated real Pack test daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	stateDir := t.TempDir()
	store := newTestEntities(t, pool)
	identity := testRegistryHostIdentityFor("local", stateDir)
	if _, err := AssignWorkspace(ctx, store, "test", identity); err != nil {
		t.Fatal(err)
	}
	s := testRuntime(t, pool, 0, stateDir).Service
	s.registryEndpoint = registry
	if err := insertFixtureNode(ctx, pool, "local", []string{platform.RoleApplication, platform.RoleBuilder}, []string{platform.CapabilityDocker, platform.CapabilityBuildpacks}, ""); err != nil {
		t.Fatal(err)
	}
	for _, op := range []contract.Operation{
		platformSubmit(t, s, platform.KindAppCreate, "platform", "real-pack-app", map[string]any{"name": "packfixture"}),
		platformSubmit(t, s, platform.KindEnvironmentCreate, "app/packfixture", "real-pack-env", map[string]any{"name": "production", "node": "local", "container_port": 8080}),
	} {
		if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
			t.Fatal(err)
		}
		platformAwait(t, s, op, contract.Succeeded)
	}
	source := t.TempDir()
	for name, content := range map[string]string{
		"go.mod":  "module example.com/issue7fixture\n\ngo 1.22\n",
		"main.go": "package main\nfunc main() { println(\"eregion pack fixture\") }\n",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	op, err := s.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor, IdempotencyKey: "real-pack", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("claim attempt: %v %v", ok, err)
	}
	tr, pinned, err := s.buildTransportFor(ctx, "local")
	if err != nil || strings.HasSuffix(pinned, ":unverified") {
		t.Fatalf("verified local transport: %q %v", pinned, err)
	}
	env, err := s.environment(ctx, "packfixture", "production")
	if err != nil {
		t.Fatal(err)
	}
	release := seedDirectoryBuildRelease(t, ctx, s, op.ID, env, source)
	holder, err := s.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor, IdempotencyKey: "real-pack-holder", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.claimAttempt(ctx, holder.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("claim holder attempt: %v %v", ok, err)
	}
	if ok, err := s.claimLocalBuildCapacity(ctx, holder.ID, 1); err != nil || !ok {
		t.Fatalf("reserve holder capacity: %v %v", ok, err)
	}
	w := &platformWorker{service: s}
	_, err = w.buildRelease(ctx, op.ID, 1, env, release, tr, nil)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("full capacity did not snooze: %v", err)
	}
	assertClaimCount(t, ctx, pool, op.ID, 0)
	if _, found, err := s.stepRecord(ctx, op.ID, deploymentPackStep); err != nil || found {
		t.Fatalf("queued build recorded dispatch intent: found=%v err=%v", found, err)
	}
	assertBuildSlotEvent(t, ctx, s, op.ID, "build_queued")
	if err := s.releaseBuildCapacity(ctx, holder.ID, 1, "local"); err != nil {
		t.Fatal(err)
	}
	release, err = w.buildRelease(ctx, op.ID, 1, env, release, tr, nil)
	if err != nil {
		t.Fatalf("real Pack dispatch: %v", err)
	}
	record, found, err := s.stepRecord(ctx, op.ID, deploymentPackStep)
	if err != nil || !found || record.State != "completed" {
		t.Fatalf("completed Pack record: %+v found=%v err=%v", record, found, err)
	}
	result := record.Result
	var artifact struct {
		Reference string `json:"reference"`
		ImageID   string `json:"image_id"`
		RuntimeID string `json:"runtime_id"`
	}
	if err := json.Unmarshal(result, &artifact); err != nil {
		t.Fatal(err)
	}
	if !platform.IsImageDigestReference(artifact.Reference) || !strings.HasPrefix(artifact.ImageID, "sha256:") || !strings.HasPrefix(artifact.RuntimeID, "sha256:") {
		t.Fatalf("Pack result lacks immutable identities: %s", result)
	}
	resolver := registrymeta.Client{HTTPClient: registrymeta.NewHTTPClient(true), Base: "http://" + registry}
	resolved, err := resolver.Resolve(ctx, "packfixture", "fixture", registrymeta.Options{Architecture: runtime.GOARCH})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Reference != registry+"/packfixture@"+resolved.ManifestDigest || artifact.ImageID != resolved.ConfigDigest || artifact.ImageID == resolved.ManifestDigest {
		t.Fatalf("Pack result disagrees with registry manifest/config: %s; %+v", result, resolved)
	}
	if release.Status != platform.ReleaseReady || release.Image != artifact.Reference || release.ImageID != resolved.ConfigDigest {
		t.Fatalf("persisted release disagrees with registry: %+v; %+v", release, resolved)
	}
	build, found, err := s.buildByOperation(ctx, op.ID)
	if err != nil || !found || build.State != platform.BuildSucceeded {
		t.Fatalf("build state: %+v found=%v err=%v", build, found, err)
	}
	var obsolete int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operation_steps
		WHERE operation_id=$1 AND step IN ('platform-pack-02-v1','platform-image-01-v1')`, op.ID).Scan(&obsolete); err != nil || obsolete != 0 {
		t.Fatalf("obsolete build identity steps: %d %v", obsolete, err)
	}
	assertBuildSlotEvent(t, ctx, s, op.ID, "build_running")
	assertClaimCount(t, ctx, pool, op.ID, 0)
}

// Seed a directory-source build at the existing service boundary. Public Git
// deployment admission deliberately refuses local Git endpoints; this fixture
// tests build execution and persistence without weakening that policy.
func seedDirectoryBuildRelease(t *testing.T, ctx context.Context, s *Service, op contract.OperationID, env platform.Environment, sourceDir string) platform.Release {
	t.Helper()
	releaseID, err := platform.NewReleaseID()
	if err != nil {
		t.Fatal(err)
	}
	source := platform.Source{Type: "directory", Path: sourceDir}
	sourceJSON, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	buildJSON, err := json.Marshal(platform.Build{Builder: "paketobuildpacks/builder-jammy-base:latest"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		app, err := appByNameTx(ctx, tx, env.App)
		if err != nil {
			return err
		}
		if _, err := tx.Platform().Releases().Insert(ctx, entities.ReleaseInsert{
			ID: releaseID, OperationID: op, AppID: app.ID, EnvironmentID: env.ID,
			Image: app.ImageRepository + ":fixture", Source: sourceJSON, Build: buildJSON, Status: platform.ReleasePending,
		}); err != nil {
			return err
		}
		buildID, err := insertBuildTx(ctx, tx, op, env, releaseID, env.RevisionID, source, "")
		if err != nil {
			return err
		}
		return setReleaseBuildPointerTx(ctx, tx, env.ID, releaseID, buildID)
	}); err != nil {
		t.Fatal(err)
	}
	release, err := s.releaseByID(ctx, releaseID)
	if err != nil {
		t.Fatal(err)
	}
	return release
}

// A retry is submitted through River using an existing directory-source build;
// the only seeded state is the old failed build, not a build effect or result.
func TestIntegrationReleaseBuildSlotQueuesAndRuns(t *testing.T) { //nolint:gocognit,gocyclo // One scenario spans the River queue and a real build.
	pool, _ := testPool(t, true)
	registry := os.Getenv("EREGION_TEST_REAL_PACK_REGISTRY")
	if registry == "" {
		t.Fatal("set EREGION_TEST_REAL_PACK_REGISTRY to the registry on an isolated real Pack test daemon")
	}
	ctx := context.Background()
	stateDir := t.TempDir()
	if _, err := AssignWorkspace(ctx, newTestEntities(t, pool), "test", testRegistryHostIdentityFor("local", stateDir)); err != nil {
		t.Fatal(err)
	}
	run := testRuntime(t, pool, 2, stateDir)
	s := run.Service
	s.registryEndpoint = registry
	if err := insertFixtureNode(ctx, pool, "local", []string{platform.RoleApplication, platform.RoleBuilder}, []string{platform.CapabilityDocker, platform.CapabilityBuildpacks}, ""); err != nil {
		t.Fatal(err)
	}
	for _, op := range []contract.Operation{
		platformSubmit(t, s, platform.KindAppCreate, "platform", "real-queue-app", map[string]any{"name": "queuefixture"}),
		platformSubmit(t, s, platform.KindEnvironmentCreate, "app/queuefixture", "real-queue-env", map[string]any{"name": "production", "node": "local", "container_port": 8080}),
	} {
		if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
			t.Fatal(err)
		}
		platformAwait(t, s, op, contract.Succeeded)
	}
	source := t.TempDir()
	for name, content := range map[string]string{
		"go.mod": "module example.com/queuefixture\n\ngo 1.22\n", "main.go": "package main\nfunc main() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seed, err := s.Submit(ctx, operations.Submission{Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor, IdempotencyKey: "queue-seed", Input: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.claimAttempt(ctx, seed.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("seed attempt: %v %v", ok, err)
	}
	env, err := s.environment(ctx, "queuefixture", "production")
	if err != nil {
		t.Fatal(err)
	}
	release := seedDirectoryBuildRelease(t, ctx, s, seed.ID, env, source)
	old, found, err := s.buildByOperation(ctx, seed.ID)
	if err != nil || !found {
		t.Fatalf("seed build: %+v %v", old, err)
	}
	if err := s.markBuildFailed(ctx, old.ID, contract.NewFault(contract.Unavailable, "fixture previous failure", ""), release.ID); err != nil {
		t.Fatal(err)
	}
	_, pinned, err := s.buildTransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_build_capacity_claims(operation_id,node_name,builder_identity,attempt) VALUES ($1,'local',$2,1)`, seed.ID, pinned); err != nil {
		t.Fatal(err)
	}
	startWorkers(t, run)
	blocked := platformSubmit(t, s, platform.KindBuildRetry, "build/"+string(old.ID), "queue-retry", map[string]any{})
	await(t, 30*time.Second, func() bool {
		page, err := s.Events(ctx, testActor, contract.Cursor{OperationID: blocked.ID}, 200)
		if err != nil {
			return false
		}
		for _, event := range page.Events {
			if strings.Contains(event.Message, "build_queued") {
				return true
			}
		}
		return false
	})
	assertClaimCount(t, ctx, pool, blocked.ID, 0)
	if _, found, err := s.stepRecord(ctx, blocked.ID, deploymentPackStep); err != nil || found {
		t.Fatalf("queued build dispatched: found=%v err=%v", found, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM eregion_build_capacity_claims WHERE operation_id=$1`, seed.ID); err != nil {
		t.Fatal(err)
	}
	completed := awaitRealBuild(t, s, blocked)
	ready, err := s.releaseForOperation(ctx, completed.ID)
	if err != nil || ready.Status != platform.ReleaseReady || !platform.IsImageDigestReference(ready.Image) || !strings.HasPrefix(ready.ImageID, "sha256:") {
		t.Fatalf("completed build release: %+v %v", ready, err)
	}
	assertClaimCount(t, ctx, pool, blocked.ID, 0)
	assertBuildSlotEvent(t, ctx, s, blocked.ID, "build_running")
	record, found, err := s.stepRecord(ctx, blocked.ID, deploymentPackStep)
	if err != nil || !found || record.State != "completed" {
		t.Fatalf("completed Pack effect: %+v found=%v err=%v", record, found, err)
	}
	page, err := s.Events(ctx, testActor, contract.Cursor{OperationID: blocked.ID}, 200)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	for _, event := range page.Events {
		if strings.Contains(event.Message, "build_running") {
			runs++
		}
	}
	if runs != 1 {
		t.Fatalf("build_running events = %d, want one dispatch", runs)
	}
}

func awaitRealBuild(t *testing.T, s *Service, op contract.Operation) contract.Operation {
	t.Helper()
	ctx := context.Background()
	var current contract.Operation
	await(t, 5*time.Minute, func() bool {
		var err error
		current, err = s.Get(ctx, testActor, op.ID)
		return err == nil && current.State.Terminal()
	})
	if current.State != contract.Succeeded {
		events, _ := s.Events(ctx, testActor, contract.Cursor{OperationID: op.ID}, 200)
		t.Fatalf("build %s ended %s: %+v; events=%+v", op.ID, current.State, current.LatestResult, events.Events)
	}
	return current
}
