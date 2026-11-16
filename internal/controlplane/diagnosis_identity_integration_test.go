package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// TestIntegrationDiagnosisKeepsRecordedImageIDWhileObservingRuntimeIdentity
// drives the real PostgreSQL-backed application-service diagnosis path with
// typed external resolver/observer observations. The canonical manifest M, the
// recorded config identity C and the Docker runtime identity R are deliberately
// distinct. The report must keep C as release.image_id, use R only for the
// image observation and candidate comparison, and never rewrite the row.
func TestIntegrationDiagnosisKeepsRecordedImageIDWhileObservingRuntimeIdentity(t *testing.T) {
	t.Setenv("EREGION_NODE_STATE_DIR", "")
	t.Setenv("EREGION_DATABASE_URL", "")
	pool, _ := testPool(t, true)
	run, err := New(newTestEntities(t, pool), integrationDiscardLogger(), WorkerConfig{TestOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	ctx := context.Background()

	manifest := "sha256:" + strings.Repeat("b", 64)
	configID := "sha256:" + strings.Repeat("a", 64)
	runtimeID := "sha256:" + strings.Repeat("c", 64)
	canonical := "registry.example/identity@" + manifest
	ids := seedDiagnosisIdentityFixture(t, ctx, pool, configID, canonical)

	var resolvedImage, observedImageID string
	s.diagnosisImageResolver = func(_ context.Context, image string) (string, string) {
		resolvedImage = image
		return runtimeID, ""
	}
	s.diagnosisImageObserver = func(_ context.Context, imageID string) (*platform.DiagnosisImage, string) {
		observedImageID = imageID
		return &platform.DiagnosisImage{
			ImageID:      imageID,
			Buildpacks:   []platform.DiagnosisBuildpack{},
			ProcessTypes: []string{},
			Layers:       []platform.DiagnosisLayer{},
		}, ""
	}

	app := api.AppService{Operations: s}
	report, err := app.Diagnose(ctx, operations.OperatorAuthority(testActor), ids.operationID, 0, 1000, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	assertDiagnosisIdentityReport(t, ctx, pool, report, ids.releaseID, manifest, configID, runtimeID, canonical, resolvedImage, observedImageID)
}

type diagnosisIdentityIDs struct {
	releaseID   platform.ReleaseID
	operationID contract.OperationID
}

func seedDiagnosisIdentityFixture(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	configID, canonical string,
) diagnosisIdentityIDs {
	t.Helper()
	appID, err := platform.NewAppID()
	if err != nil {
		t.Fatal(err)
	}
	envID, err := platform.NewEnvironmentID()
	if err != nil {
		t.Fatal(err)
	}
	revisionID := platform.RevisionID("erv_" + strings.Repeat("d", 32))
	releaseID, err := platform.NewReleaseID()
	if err != nil {
		t.Fatal(err)
	}
	deploymentID, err := platform.NewDeploymentID()
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureNode(ctx, pool, "fixture", []string{"application"}, []string{"docker"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureApp(ctx, pool, string(appID), "identity", "identity"); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureEnvironment(ctx, pool, string(envID), string(appID), "production", "fixture", 20112, 30112, 8080); err != nil {
		t.Fatal(err)
	}
	if err := insertFixtureRevision(ctx, pool, string(revisionID), string(envID), 1, `{"node":"fixture","health_path":"/"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations
		(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt,completed_steps,last_sequence,created_at,updated_at,error_code)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'succeeded',1,0,0,clock_timestamp(),clock_timestamp(),'')`,
		operationID, platform.KindDeploy, "env/identity/production",
		testActor.Kind, testActor.Subject, string(operationID), "hash", `{"seed":"diagnosis-identity"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_releases
		(id,operation_id,app_id,environment_id,image,image_id,source,status,error_code,error,created_at,commit_sha)
		VALUES($1,$2,$3,$4,$5,$6,'{}','ready','','',clock_timestamp(),'commit-identity')`,
		releaseID, operationID, appID, envID, canonical, configID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_deployments
		(id,operation_id,app_id,environment_id,action,release_id,revision_id,state,error_code,message,created_at,updated_at,secret_revisions,instances)
		VALUES($1,$2,$3,$4,'deploy',$5,$6,'succeeded','','',clock_timestamp(),clock_timestamp(),'{}','[]')`,
		deploymentID, operationID, appID, envID, releaseID, revisionID); err != nil {
		t.Fatal(err)
	}
	return diagnosisIdentityIDs{releaseID: releaseID, operationID: operationID}
}

func assertDiagnosisIdentityReport(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	report platform.Diagnosis,
	releaseID platform.ReleaseID,
	manifest, configID, runtimeID, canonical, resolvedImage, observedImageID string,
) {
	t.Helper()
	if report.Release == nil {
		t.Fatal("diagnosis did not project the recorded release")
	}
	if report.Release.ImageID != configID {
		t.Fatalf("reported release image_id=%q, want the recorded config identity %s", report.Release.ImageID, configID)
	}
	if report.Release.ImageID == runtimeID || report.Release.ImageID == manifest {
		t.Fatalf("reported release image_id conflated the recorded config identity with runtime/manifest identity: %q", report.Release.ImageID)
	}
	if report.Release.ImageDigest != manifest {
		t.Fatalf("reported release image_digest=%q, want the canonical manifest identity %s", report.Release.ImageDigest, manifest)
	}
	if report.Image == nil || report.Image.ImageID != runtimeID {
		t.Fatalf("image observation=%+v, want the runtime identity %s", report.Image, runtimeID)
	}
	if resolvedImage != canonical {
		t.Fatalf("resolver received %q, want the canonical manifest reference %q", resolvedImage, canonical)
	}
	if observedImageID != runtimeID {
		t.Fatalf("observer received %q, want the resolved runtime identity %s", observedImageID, runtimeID)
	}
	var storedImageID string
	if err := pool.QueryRow(ctx, `SELECT image_id FROM eregion_platform_releases WHERE id=$1`, releaseID).Scan(&storedImageID); err != nil {
		t.Fatal(err)
	}
	if storedImageID != configID {
		t.Fatalf("stored release image_id=%q, diagnosis rewrote it from %s", storedImageID, configID)
	}
}
