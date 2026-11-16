package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

// TestIntegrationCandidateCapturePinnedRotationAndClaims covers the service's
// pinned secret snapshot, redaction projection and durable claim semantics. The
// node read is a typed external result; PostgreSQL, vault and the application
// service are real.
func TestIntegrationCandidateCapturePinnedRotationAndClaims(t *testing.T) {
	t.Run("pinned rotation", func(t *testing.T) {
		checkCandidateCapture(t, "old-sensitive-value", true, false, false)
	})
	t.Run("missing pin retains metadata", func(t *testing.T) {
		checkCandidateCapture(t, "old-sensitive-value", false, true, false)
	})
	t.Run("created is not exited", func(t *testing.T) {
		checkCandidateCapture(t, "old-sensitive-value", true, false, true)
	})
}

func checkCandidateCapture(
	t *testing.T,
	old string,
	wantLogs bool,
	missingPin bool,
	neverStarted bool,
) {
	t.Helper()
	s, pool := gitDeployFixture(t)
	ctx := context.Background()
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	store, err := newTestVaultService(t, pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	s.secrets = vault.NewEnvironmentResolver(store)
	ns, err := vault.NewNamespace("shop", "production", "PASSWORD")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.Set(ctx, ns, []byte(old), 0, testActor)
	if err != nil {
		t.Fatal(err)
	}
	platformSubmit(t, s, platform.KindEnvironmentUpdate, "env/shop/production", "capture-secret-revision", map[string]any{"secret_names": []string{"PASSWORD"}})
	s.resolveGitRef = func(context.Context, string, string, []byte) (string, error) { return submittedGitCommit, nil }
	op := platformSubmit(t, s, platform.KindDeploy, "env/shop/production", "capture-pinned", map[string]any{})
	env, err := s.environment(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	dep, ok, err := s.deploymentByOperation(ctx, op.ID)
	if err != nil || !ok {
		t.Fatalf("deployment: %v %v", ok, err)
	}
	ref := meta.Revision
	pins := map[string]platform.SecretRevisionRef{"PASSWORD": {Revision: ref.Revision, Hash: ref.Hash, KeyID: ref.KeyID, CreatedAt: ref.CreatedAt}}
	if !missingPin {
		if err := s.recordDeploymentSecretSnapshot(ctx, op.ID, env.RevisionID, pins); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Set(ctx, ns, []byte("new-rotated-value"), meta.LatestRevision, testActor); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='running',attempt=1 WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	insertDeploymentOwner(t, pool, env, deploymentOwner{EnvironmentID: env.ID, OperationID: op.ID, DeploymentID: dep.ID, Attempt: 1, Phase: deploymentOwnerPrepared})

	plan, err := platform.PlanDeployment(env.App, env.Name, env.Revision.RevisionState, dep.ReleaseID, dep.ID)
	if err != nil || len(plan) != 1 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	containers := candidateContainers(plan)
	observedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	observation := candidateObservation(containers[0], 0, observedAt)
	if neverStarted {
		observation.State = "created"
		observation.Running = false
		observation.StartedAt = time.Time{}
		observation.FinishedAt = time.Time{}
		observation.ExitCode = nil
	}
	result := platform.CandidateDiagnosticResult{
		Observations: []platform.CandidateObservation{observation},
		Lines: []platform.CandidateDiagnosticLine{
			candidateLine(containers[0], 0, observedAt, "before"),
			candidateLine(containers[0], 0, observedAt, old),
			candidateLine(containers[0], 0, observedAt, "after"),
		},
		LogTimestampSource: "observation_time",
	}
	transport := &candidateCaptureTransport{
		call: func(context.Context, platform.CandidateDiagnosticPayload) (platform.CandidateDiagnosticResult, error) {
			return result, nil
		},
	}
	s.resolver = newTestNodeResolver(t, pool, map[string]node.Transport{"local": transport})
	s.captureCandidateDiagnostics(ctx, env, dep, 1)
	s.captureCandidateDiagnostics(ctx, env, dep, 1)
	var claims, observations int
	var events string
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE event->'candidate_diagnostic'->>'status'='unconfirmed'), count(*) FILTER (WHERE event->'candidate_diagnostic'->>'status'='observed'), string_agg(event::text, E'\n') FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'=$2`, op.ID, platform.CandidateDiagnosticStage).Scan(&claims, &observations, &events); err != nil {
		t.Fatal(err)
	}
	if claims != 1 || observations != 1 || strings.Contains(events, old) ||
		(neverStarted && strings.Contains(events, `"exit_code"`)) ||
		(!neverStarted && !strings.Contains(events, `"exit_code": 3`)) ||
		(wantLogs && !strings.Contains(events, "[REDACTED]")) ||
		(!wantLogs && !strings.Contains(events, `"status": "partial"`)) {
		t.Fatalf("claims=%d observations=%d events=%s", claims, observations, events)
	}
	// A newly recorded container execution in the same attempt produces a
	// distinct capture claim, rather than suppressing the later observation.
	step := "platform-container-evidence-v1"
	execution := string(op.ID) + ":1:" + step
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operation_steps(operation_id,step,inputs_hash,state,attempt,execution_id,started_at,error,error_code) VALUES($1,$2,'test','failed',1,$3,clock_timestamp(),'','')`, op.ID, step, execution); err != nil {
		t.Fatal(err)
	}
	s.captureCandidateDiagnostics(ctx, env, dep, 1)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operation_events WHERE operation_id=$1 AND event->'candidate_diagnostic'->>'status'='unconfirmed'`, op.ID).Scan(&claims); err != nil || claims != 2 {
		t.Fatalf("distinct execution claims=%d err=%v", claims, err)
	}
}

// TestIntegrationImageDeploymentCaptureDoesNotRequireGitCredential keeps the
// service/DB capture path for a ready image whose current recipe names a Git
// credential. The node read is a typed external result.
func TestIntegrationImageDeploymentCaptureDoesNotRequireGitCredential(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	if err := f.service.markBuildSucceeded(ctx, f.build, f.release, "registry.example/shop@sha256:"+strings.Repeat("b", 64), submittedGitCommit, "sha256:"+strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	platformSubmit(t, f.service, platform.KindEnvironmentUpdate, "env/shop/production", "credential-recipe", map[string]any{"source": map[string]any{"url": "https://github.com/acme/app.git", "ref": "main", "builder": "paketobuildpacks/builder-jammy-base", "credential_secret_name": "GIT_READ"}})
	op := platformSubmit(t, f.service, platform.KindDeploy, "env/shop/production", "capture-ready-image", map[string]any{"release_id": f.release})
	env, err := f.service.environment(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	dep, ok, err := f.service.deploymentByOperation(ctx, op.ID)
	if err != nil || !ok {
		t.Fatalf("deployment err=%v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_operations SET state='running',attempt=1 WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	insertDeploymentOwner(t, f.pool, env, deploymentOwner{EnvironmentID: env.ID, OperationID: op.ID, DeploymentID: dep.ID, Attempt: 1, Phase: deploymentOwnerPrepared})
	observedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	transport := &candidateCaptureTransport{
		call: func(_ context.Context, payload platform.CandidateDiagnosticPayload) (platform.CandidateDiagnosticResult, error) {
			if len(payload.Containers) == 0 {
				return platform.CandidateDiagnosticResult{}, nil
			}
			container := payload.Containers[0]
			observation := candidateObservation(container, 0, observedAt)
			observation.State = "created"
			observation.Running = false
			observation.StartedAt = time.Time{}
			observation.FinishedAt = time.Time{}
			observation.ExitCode = nil
			return platform.CandidateDiagnosticResult{
				Observations:       []platform.CandidateObservation{observation},
				Lines:              []platform.CandidateDiagnosticLine{candidateLine(container, 0, observedAt, "image-evidence")},
				LogTimestampSource: "observation_time",
			}, nil
		},
	}
	f.service.resolver = newTestNodeResolver(t, f.pool, map[string]node.Transport{"local": transport})
	f.service.captureCandidateDiagnostics(ctx, env, dep, 1)
	var observed, logged bool
	if err := f.pool.QueryRow(ctx, `SELECT bool_or(event->'candidate_diagnostic'->>'status'='observed'),bool_or(event->>'message' LIKE '%image-evidence%') FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'=$2`, op.ID, platform.CandidateDiagnosticStage).Scan(&observed, &logged); err != nil || !observed || !logged {
		t.Fatalf("observed=%v logged=%v err=%v", observed, logged, err)
	}
}
