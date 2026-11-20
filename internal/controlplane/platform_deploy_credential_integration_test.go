package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/jobargs"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const (
	gitReadSecretName = "GIT_READ"
	gitReadRevision   = uint64(7)
	gitReadValue      = "private-repository-read-credential"
)

type secretSnapshotCall struct {
	app         string
	environment string
	names       []string
	refs        map[string]string
}

// snapshotSecretResolver stands in for the vault resolver: it stores named
// revisions and records how the deployment asked for them, so a test can tell
// a current-name resolution from a pinned-revision resolution.
type snapshotSecretResolver struct {
	mu     sync.Mutex
	active map[string]uint64
	values map[string]map[uint64]string
	calls  []secretSnapshotCall
}

func newSnapshotSecretResolver() *snapshotSecretResolver {
	return &snapshotSecretResolver{
		active: map[string]uint64{},
		values: map[string]map[uint64]string{},
	}
}

// set publishes a secret revision and makes it the active revision.
func (r *snapshotSecretResolver) set(name string, revision uint64, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values[name] == nil {
		r.values[name] = map[uint64]string{}
	}
	r.values[name][revision] = value
	r.active[name] = revision
}

func (*snapshotSecretResolver) Resolve(
	context.Context,
	map[string]string,
) (map[string]string, error) {
	return nil, errors.New("namespace-free secret resolution is refused")
}

func (r *snapshotSecretResolver) ResolveEnvironmentSnapshot(
	_ context.Context,
	app, environment string,
	names []string,
	refs map[string]string,
) (map[string]string, map[string]vault.RevisionRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := secretSnapshotCall{
		app:         app,
		environment: environment,
		names:       append([]string(nil), names...),
		refs:        map[string]string{},
	}
	for key, ref := range refs {
		call.refs[key] = ref
	}
	r.calls = append(r.calls, call)

	values := map[string]string{}
	revisions := map[string]vault.RevisionRef{}
	add := func(key, name string, revision uint64) error {
		value, ok := r.values[name][revision]
		if !ok || value == "" {
			return fmt.Errorf("secret %s revision %d is not stored", name, revision)
		}
		values[key] = value
		revisions[key] = vault.RevisionRef{
			Revision:  revision,
			Hash:      strings.Repeat("a", 64),
			KeyID:     "test-key",
			CreatedAt: time.Unix(1, 0).UTC(),
		}
		return nil
	}
	for _, name := range names {
		revision, ok := r.active[name]
		if !ok {
			return nil, nil, fmt.Errorf("secret %s is not stored", name)
		}
		if err := add(name, name, revision); err != nil {
			return nil, nil, err
		}
	}
	for key, text := range refs {
		ref, err := vault.ParseReference(text)
		if err != nil {
			return nil, nil, err
		}
		if ref.Namespace.App != app || ref.Namespace.Environment != environment {
			return nil, nil, fmt.Errorf(
				"secret reference %q is outside %s/%s",
				text,
				app,
				environment,
			)
		}
		if err := add(key, ref.Namespace.Name, ref.Revision); err != nil {
			return nil, nil, err
		}
	}
	return values, revisions, nil
}

func (r *snapshotSecretResolver) snapshot() []secretSnapshotCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]secretSnapshotCall(nil), r.calls...)
}

// runPlatformAttempt dispatches one platform operation the way the River
// worker does, without needing a real River job to be claimed and retried.
func runPlatformAttempt(
	ctx context.Context,
	s *Service,
	id contract.OperationID,
	attempt int,
) error {
	worker := &platformWorker{service: s}
	return worker.Work(ctx, &river.Job[jobargs.PlatformAction]{
		JobRow: &rivertype.JobRow{Attempt: attempt},
		Args:   jobargs.PlatformAction{OperationID: id},
	})
}

// credentialDeployFixture exercises deployment secret preparation at the
// durable boundary, without dispatching a Git clone or Pack effect.
type credentialDeployFixture struct {
	pool     *pgxpool.Pool
	service  *Service
	resolver *snapshotSecretResolver
	deploy   contract.Operation
	secrets  map[string]string
}

func newCredentialDeployFixture(t *testing.T) *credentialDeployFixture {
	t.Helper()
	pool, _ := testPool(t, true)
	resolver := newSnapshotSecretResolver()
	resolver.set(gitReadSecretName, gitReadRevision, gitReadValue)
	run, err := New(newTestEntities(t, pool), slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, NodeStateDir: t.TempDir(),
		LocalNodeName: "local", SecretResolver: resolver, RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := run.Service
	s.routeSwitch = func(context.Context, platform.Environment, []string) error { return nil }
	s.publicVerify = func(context.Context, platform.Environment, string) error { return nil }

	// Setup operations are dispatched synchronously; no River workers run in
	// this test, so the deploy cannot race the manually claimed attempts.
	ctx := context.Background()
	for _, op := range []contract.Operation{
		platformSubmit(t, s, platform.KindNodeRegister, "platform", "credential-node", map[string]any{
			"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder, platform.RoleControlPlane},
			"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
		}),
		platformSubmit(t, s, platform.KindAppCreate, "platform", "credential-app", map[string]any{"name": "private"}),
		platformSubmit(t, s, platform.KindEnvironmentCreate, "app/private", "credential-env", map[string]any{
			"name": "production", "node": "local", "container_port": 80,
			"health": map[string]any{"timeout_seconds": 2},
			"domain": "private.example.com", "domain_primary": true,
			"source": map[string]any{
				"url": "https://github.com/acme/private.git",
				"ref": submittedGitCommit, "builder": "paketobuildpacks/builder-jammy-base",
				"credential_secret_name": gitReadSecretName,
			},
		}),
	} {
		if err := runPlatformAttempt(ctx, s, op.ID, 1); err != nil {
			t.Fatalf("%s setup attempt: %v", op.Kind, err)
		}
	}
	return &credentialDeployFixture{
		pool:     pool,
		service:  s,
		resolver: resolver,
		deploy: platformSubmit(
			t,
			s,
			platform.KindDeploy,
			"env/private/production",
			"credential-deploy",
			map[string]any{},
		),
	}
}

// loseFirstAttempt stops at the durable snapshot boundary without issuing a
// node effect. The next attempt reloads the deployment from PostgreSQL.
func (f *credentialDeployFixture) loseFirstAttempt(t *testing.T) {
	t.Helper()
	f.prepareSecrets(t, 1)
}

func (f *credentialDeployFixture) secretSnapshot(
	t *testing.T,
) (map[string]platform.SecretRevisionRef, []byte) {
	t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT secret_revisions FROM eregion_platform_deployments WHERE operation_id=$1`, f.deploy.ID).
		Scan(&raw); err != nil {
		t.Fatal(err)
	}
	snapshot := map[string]platform.SecretRevisionRef{}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot, raw
}

func (f *credentialDeployFixture) retry(t *testing.T) {
	t.Helper()
	run := f.prepareSecrets(t, 2)
	values := cloneMap(run.secretValues)
	values[run.credentialKey] = run.credentialVal
	build, found, err := f.service.buildByOperation(context.Background(), f.deploy.ID)
	if err != nil || !found {
		t.Fatalf("load retry build: found=%v err=%v", found, err)
	}
	f.secrets, err = buildCredentialSecrets(build, values)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *credentialDeployFixture) buildSecrets(t *testing.T) map[string]string {
	t.Helper()
	return f.secrets
}

func (f *credentialDeployFixture) prepareSecrets(t *testing.T, attempt uint32) *deploymentRun {
	t.Helper()
	ctx := context.Background()
	if _, _, ok, err := f.service.claimAttempt(ctx, f.deploy.ID, attempt, "test"); err != nil || !ok {
		t.Fatalf("claim attempt %d: %v %v", attempt, ok, err)
	}
	dep, found, err := f.service.deploymentByOperation(ctx, f.deploy.ID)
	if err != nil || !found {
		t.Fatalf("load deployment: found=%v err=%v", found, err)
	}
	env, err := f.service.environment(ctx, dep.App, dep.Environment)
	if err != nil {
		t.Fatal(err)
	}
	env, err = f.service.pinnedDeploymentEnvironment(ctx, dep, env)
	if err != nil {
		t.Fatal(err)
	}
	release, err := f.service.releaseByID(ctx, dep.ReleaseID)
	if err != nil {
		t.Fatal(err)
	}
	run := &deploymentRun{id: f.deploy.ID, attempt: attempt, dep: dep, env: env, release: release}
	if done, err := (&platformWorker{service: f.service}).prepareDeploymentSecrets(ctx, run); err != nil || done {
		t.Fatalf("prepare attempt %d secrets: done=%v err=%v", attempt, done, err)
	}
	return run
}

// TestIntegrationDeployRetryResolvesPinnedGitReadCredential reproduces the
// recovered secret-preparation boundary: after the first snapshot, the next
// attempt must resolve the Git credential from that durable pin. Actual Pack
// execution is tested separately with a verified local transport.
func TestIntegrationDeployRetryResolvesPinnedGitReadCredential(t *testing.T) {
	f := newCredentialDeployFixture(t)
	ctx := context.Background()
	f.loseFirstAttempt(t)

	snapshot, rawSnapshot := f.secretSnapshot(t)
	ref, ok := snapshot[gitReadSecretName]
	if !ok || ref.Revision != gitReadRevision {
		t.Fatalf(
			"first attempt snapshot = %s, want the git read credential at revision %d",
			rawSnapshot,
			gitReadRevision,
		)
	}

	// A new attempt reloads the snapshot instead of resolving the active name.
	f.retry(t)
	if secrets := f.buildSecrets(t); secrets[gitReadSecretName] != gitReadValue {
		t.Fatalf(
			"build credential selection = %q, want the resolved value",
			secrets[gitReadSecretName],
		)
	}

	calls := f.resolver.snapshot()
	if len(calls) != 2 {
		t.Fatalf(
			"secret resolution calls = %+v, want one name resolution and one pinned resolution",
			calls,
		)
	}
	if len(calls[0].names) != 1 || calls[0].names[0] != gitReadSecretName ||
		len(calls[0].refs) != 0 {
		t.Fatalf(
			"first resolution = %+v, want the credential name at its active revision",
			calls[0],
		)
	}
	wantPin := fmt.Sprintf("vault:private/production/%s@%d", gitReadSecretName, gitReadRevision)
	if len(calls[1].names) != 0 || calls[1].refs[gitReadSecretName] != wantPin {
		t.Fatalf("retry resolution = %+v, want pinned %q", calls[1], wantPin)
	}

	// The selected value must not appear in durable input, events or snapshot.
	var input []byte
	if err := f.pool.QueryRow(ctx, `SELECT input FROM eregion_operations WHERE id=$1`, f.deploy.ID).
		Scan(&input); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(input), gitReadValue) {
		t.Fatal("the git read credential leaked into operation input")
	}
	var events string
	if err := f.pool.QueryRow(ctx, `SELECT coalesce(string_agg(event::text, ''),'') FROM eregion_operation_events WHERE operation_id=$1`, f.deploy.ID).
		Scan(&events); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(events, gitReadValue) {
		t.Fatal("the git read credential leaked into operation events")
	}
	if strings.Contains(string(rawSnapshot), gitReadValue) {
		t.Fatal("the git read credential leaked into the secret snapshot")
	}
}

// TestIntegrationDeployRetryPinsGitReadCredentialIntoLegacySnapshot covers a
// snapshot written before the credential was pinned: the next attempt must merge
// the credential's revision into the existing snapshot without touching the
// application pins, so rotating the credential afterwards cannot change the
// credential used by that deployment.
func TestIntegrationDeployRetryPinsGitReadCredentialIntoLegacySnapshot(t *testing.T) {
	f := newCredentialDeployFixture(t)
	ctx := context.Background()

	const (
		legacyName     = "LEGACY_APP"
		legacyRevision = uint64(3)
		legacyValue    = "legacy-application-secret"
	)
	f.resolver.set(legacyName, legacyRevision, legacyValue)
	legacyRefs, err := json.Marshal(map[string]platform.SecretRevisionRef{
		legacyName: {
			Revision:  legacyRevision,
			Hash:      strings.Repeat("b", 64),
			KeyID:     "test-key",
			CreatedAt: time.Unix(2, 0).UTC(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(
		ctx,
		`UPDATE eregion_platform_deployments SET secret_revisions=$2 WHERE operation_id=$1`,
		f.deploy.ID,
		string(legacyRefs),
	); err != nil {
		t.Fatal(err)
	}

	f.loseFirstAttempt(t)

	snapshot, rawSnapshot := f.secretSnapshot(t)
	credentialRef, ok := snapshot[gitReadSecretName]
	if !ok || credentialRef.Revision != gitReadRevision {
		t.Fatalf("attempt did not pin the credential into the legacy snapshot: %s", rawSnapshot)
	}
	if appRef := snapshot[legacyName]; appRef.Revision != legacyRevision {
		t.Fatalf("the legacy application pin was lost: %s", rawSnapshot)
	}

	// Rotate the credential between attempts. The retry must keep using the
	// revision pinned into the deployment, not the new active revision.
	const rotatedRevision = uint64(8)
	const rotatedValue = "rotated-private-repository-credential"
	f.resolver.set(gitReadSecretName, rotatedRevision, rotatedValue)

	f.retry(t)
	if secrets := f.buildSecrets(t); secrets[gitReadSecretName] != gitReadValue {
		t.Fatalf(
			"build received credential %q after rotation, want the pinned %q",
			secrets[gitReadSecretName],
			gitReadValue,
		)
	}

	calls := f.resolver.snapshot()
	if len(calls) != 3 {
		t.Fatalf(
			"secret resolution calls = %+v, want two first-attempt calls and one pinned retry",
			calls,
		)
	}
	wantCredentialPin := fmt.Sprintf(
		"vault:private/production/%s@%d",
		gitReadSecretName,
		gitReadRevision,
	)
	wantLegacyPin := fmt.Sprintf("vault:private/production/%s@%d", legacyName, legacyRevision)
	if len(calls[2].names) != 0 || calls[2].refs[gitReadSecretName] != wantCredentialPin ||
		calls[2].refs[legacyName] != wantLegacyPin {
		t.Fatalf(
			"retry resolution = %+v, want pinned %q and %q",
			calls[2],
			wantCredentialPin,
			wantLegacyPin,
		)
	}
}
