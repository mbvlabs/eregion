package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/sshfixture"
	"golang.org/x/crypto/ssh"
)

// The enrollment integration tests run real control-plane workers against the
// disposable SSH nodes of internal/sshfixture. Each fixture is a Debian 13
// container running the real eregion binary as its node agent behind a real sshd; the host
// tools the probe looks for (docker, pack) are small stand-ins installed per
// node, so a node's capabilities are exactly what the test gives it.

const stubSHA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

const dockerStub = `#!/bin/sh
case "$1" in
  version) echo 27.0.0 ;;
  image) echo ` + stubSHA + ` ;;
  *) echo "stub docker: unsupported: $*" >&2; exit 1 ;;
esac
`

const packStub = `#!/bin/sh
echo 0.36.0
`

type nodeLab struct {
	pool    *pgxpool.Pool
	run     *Runtime
	s       *Service
	keyFile string
	keyDir  string
}

func newNodeLab(t *testing.T) *nodeLab {
	t.Helper()
	pool, _ := testPool(t, true)
	dir := t.TempDir()
	keyFile, err := sshfixture.NewClientKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	run := testRuntime(t, pool, 2)
	run.Service.resolver.config = ResolverConfig{
		SSHIdentityFile: keyFile, WireGuardKeyDir: filepath.Join(dir, "wg"),
		Timeout: 30 * time.Second, PollInterval: 100 * time.Millisecond, LockDir: dir,
	}
	startWorkers(t, run)
	t.Cleanup(run.Close)
	return &nodeLab{pool: pool, run: run, s: run.Service, keyFile: keyFile, keyDir: dir}
}

// fixture starts a node that trusts the control plane's one SSH key.
func (l *nodeLab) fixture(t *testing.T, name string, options sshfixture.Options) *sshfixture.Fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	options.NodeName, options.ClientKeyFile = name, l.keyFile
	fixture, err := sshfixture.Start(ctx, options)
	if err != nil {
		if errors.Is(err, sshfixture.ErrUnavailable) {
			t.Skipf("the SSH fixture is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.Close(context.Background()) })
	return fixture
}

func hostKeyLine(f *sshfixture.Fixture) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(f.HostKey)))
}

func enrollInput(f *sshfixture.Fixture, roles ...string) map[string]any {
	return map[string]any{"roles": roles, "address": f.Address, "host_key": hostKeyLine(f)}
}

func (l *nodeLab) node(t *testing.T, name string) platform.Node {
	t.Helper()
	n, err := l.s.node(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (l *nodeLab) enroll(t *testing.T, key, name string, input map[string]any, want contract.State) contract.Operation {
	t.Helper()
	op := platformSubmit(t, l.s, platform.KindNodeEnroll, "node/"+name, key, input)
	return platformAwait(t, l.s, op, want)
}

func (l *nodeLab) events(t *testing.T, op contract.Operation) string {
	t.Helper()
	page, err := l.s.Events(context.Background(), testActor, contract.Cursor{OperationID: op.ID}, 200)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(page)
	return string(raw)
}

func appFixture(t *testing.T, l *nodeLab, name string) *sshfixture.Fixture {
	return l.fixture(t, name, sshfixture.Options{Files: map[string]string{"/usr/local/bin/docker": dockerStub}})
}

func builderFixture(t *testing.T, l *nodeLab, name string) *sshfixture.Fixture {
	return l.fixture(t, name, sshfixture.Options{Files: map[string]string{"/usr/local/bin/docker": dockerStub, "/usr/local/bin/pack": packStub}})
}

// everythingTheControlPlaneStores concatenates every durable text surface that a
// secret could leak into: node rows, operation events, and node step results.
func (l *nodeLab) everythingTheControlPlaneStores(t *testing.T) string {
	t.Helper()
	var out strings.Builder
	for _, query := range []string{
		`SELECT coalesce(string_agg(row_to_json(n)::text, ' '), '') FROM eregion_platform_nodes n`,
		`SELECT coalesce(string_agg(row_to_json(e)::text, ' '), '') FROM eregion_operation_events e`,
		`SELECT coalesce(string_agg(row_to_json(s)::text, ' '), '') FROM eregion_operation_steps s`,
		`SELECT coalesce(string_agg(row_to_json(o)::text, ' '), '') FROM eregion_operations o`,
		`SELECT coalesce(string_agg(row_to_json(j)::text, ' '), '') FROM river_job j`,
	} {
		var text string
		if err := l.pool.QueryRow(context.Background(), query).Scan(&text); err != nil {
			t.Fatal(err)
		}
		out.WriteString(text)
	}
	return out.String()
}

// TestIntegrationNodesApplicationAndBuilderEnrollAndReceiveOnlyTheirWork is the
// M2 gate in one scenario: an application node and a builder node enroll over
// pinned SSH, work lands on the node it is placed on, and work outside a node's
// declared placement or capabilities is refused with a structured violation.
func TestIntegrationNodesApplicationAndBuilderEnrollAndReceiveOnlyTheirWork(t *testing.T) {
	l := newNodeLab(t)
	ctx := context.Background()
	appNode := appFixture(t, l, "app-1")
	buildNode := builderFixture(t, l, "build-1")

	// ---- enrollment
	appOp := l.enroll(t, "enroll-app-1", "app-1", enrollInput(appNode, platform.RoleApplication), contract.Succeeded)
	buildInput := enrollInput(buildNode, platform.RoleBuilder)
	buildInput["build_concurrency"] = 2
	l.enroll(t, "enroll-build-1", "build-1", buildInput, contract.Succeeded)

	app, builder := l.node(t, "app-1"), l.node(t, "build-1")
	if app.EnrollmentState != platform.EnrollmentEnrolled || builder.EnrollmentState != platform.EnrollmentEnrolled {
		t.Fatalf("not enrolled: %+v %+v", app, builder)
	}
	if strings.Join(app.Capabilities, ",") != "docker" || strings.Join(builder.Capabilities, ",") != "buildpacks,docker" {
		t.Fatalf("capabilities must be what the probe found: %v %v", app.Capabilities, builder.Capabilities)
	}
	if app.OS != "debian 13" || app.Arch == "" || builder.BuildConcurrency != 2 {
		t.Fatalf("probe facts were not recorded: %+v %+v", app, builder)
	}
	if !strings.HasPrefix(app.IdentityFingerprint, "SHA256:") || app.IdentityFingerprint == builder.IdentityFingerprint || app.EnrolledAt == nil || app.LastCheckAt == nil || app.LastCheckState != "ok" {
		t.Fatalf("identity and freshness must be recorded per node: %+v %+v", app, builder)
	}
	if !strings.HasPrefix(app.HostKeyFingerprint, "SHA256:") || app.Transport != "ssh" {
		t.Fatalf("the pinned host key and transport must be recorded: %+v", app)
	}
	if !strings.Contains(l.events(t, appOp), "enrolled") {
		t.Fatal("the enrollment operation must report that the node enrolled")
	}

	// ---- role-specific host configuration landed on the right node
	buildDirs, _ := buildNode.List(ctx, "/var/lib/eregion/node/build")
	if strings.Join(buildDirs, ",") != "cache,docker-config,home,tmp" {
		t.Fatalf("the builder needs isolated build directories: %v", buildDirs)
	}
	if names, _ := appNode.List(ctx, "/var/lib/eregion/node/build"); len(names) != 0 {
		t.Fatalf("an application node must have no build directories: %v", names)
	}
	if names, _ := appNode.List(ctx, "/var/lib/eregion/node"); !contains(names, "runtime") {
		t.Fatalf("an application node needs its runtime directory: %v", names)
	}

	// ---- the node generates its identity itself; no private key reaches PostgreSQL
	seed, err := appNode.Read(ctx, "/var/lib/eregion/node/identity/node.ed25519")
	if err != nil || strings.TrimSpace(seed) == "" {
		t.Fatalf("the node must hold its own identity key: %q %v", seed, err)
	}
	if everything := l.everythingTheControlPlaneStores(t); strings.Contains(everything, strings.TrimSpace(seed)) {
		t.Fatal("the node's private identity key reached the control-plane database")
	}

	// ---- work dispatches to the node it is placed on, over SSH
	platformAwait(t, l.s, platformSubmit(t, l.s, platform.KindAppCreate, "platform", "app-api", map[string]any{"name": "api"}), contract.Succeeded)
	platformAwait(t, l.s, platformSubmit(t, l.s, platform.KindEnvironmentCreate, "app/api", "env-api", map[string]any{"name": "production", "node": "app-1"}), contract.Succeeded)
	release := platformSubmit(t, l.s, platform.KindReleaseCreate, "env/api/production", "rel-1", map[string]any{"source": map[string]any{"type": "image", "image": "example/api:1"}})
	platformAwait(t, l.s, release, contract.Succeeded)
	onApp, onBuilder := nodeEffects(ctx, appNode, release.ID), nodeEffects(ctx, buildNode, release.ID)
	if onApp == 0 || onBuilder != 0 {
		t.Fatalf("the release must run only on the application node: app=%d builder=%d", onApp, onBuilder)
	}
	if r, err := l.s.releaseForOperation(ctx, release.ID); err != nil || r.ImageID != stubSHA {
		t.Fatalf("release %+v %v", r, err)
	}

	// ---- work outside the declared placement is refused, with a reason
	// A runtime workload never lands on a builder.
	_, err = l.s.Submit(ctx, platformSubmission(platform.KindEnvironmentCreate, "app/api", "env-on-builder", map[string]any{"name": "staging", "node": "build-1"}))
	wantViolation(t, err, ReasonRoleMissing)
	// A build never runs on a node that only has the application role.
	_, err = l.s.Submit(ctx, platformSubmission(platform.KindReleaseCreate, "env/api/production", "build-on-app", map[string]any{
		"source": map[string]any{"type": "git", "url": "https://github.com/example/api.git"}}))
	wantViolation(t, err, ReasonBuilderRequired)
	// A node that is not enrolled receives nothing, even if it was registered.
	platformAwait(t, l.s, platformSubmit(t, l.s, platform.KindNodeRegister, "platform", "reg-ghost", map[string]any{
		"name": "ghost", "roles": []string{"application"}, "capabilities": []string{"docker"}}), contract.Succeeded)
	_, err = l.s.Submit(ctx, platformSubmission(platform.KindEnvironmentCreate, "app/api", "env-on-ghost", map[string]any{"name": "ghosty", "node": "ghost"}))
	wantViolation(t, err, ReasonIdentityMismatch) // registered, and no transport authenticates as it

	// The refusal reaches the operation as an event an agent can read: an
	// environment pinned to an application node whose role is later revoked would
	// fail in the worker; here we check the worker path directly.
	if resolver, rerr := l.s.resolver.TransportFor(ctx, "ghost"); rerr == nil || resolver != nil {
		t.Fatalf("a registered node must not resolve to the worker's own transport: %v", rerr)
	} else {
		wantViolation(t, rerr, ReasonIdentityMismatch)
	}
	if tr, rerr := l.s.resolver.TransportFor(ctx, "build-1"); rerr != nil || tr.Name() != "ssh" {
		t.Fatalf("an enrolled node resolves to its own SSH transport: %v", rerr)
	}
}

// TestIntegrationNodesNeverTrustAHostnameOrAnAddress proves the identity rules:
// a wrong pinned host key, a wrong node name, an identity key that is not the
// expected one, and a cloned identity are all refused, and none leaves a node
// that can receive work.
func TestIntegrationNodesNeverTrustAHostnameOrAnAddress(t *testing.T) {
	l := newNodeLab(t)
	good := appFixture(t, l, "app-1")
	other := appFixture(t, l, "app-2")
	l.enroll(t, "enroll-1", "app-1", enrollInput(good, platform.RoleApplication), contract.Succeeded)
	goodFingerprint := l.node(t, "app-1").IdentityFingerprint

	cases := []struct {
		name   string
		node   string
		input  map[string]any
		reason string
	}{
		{"another host's key is pinned for this address", "wrongkey", func() map[string]any {
			in := enrollInput(other, platform.RoleApplication)
			in["host_key"] = hostKeyLine(good)
			return in
		}(), ""},
		{"the node reports another name than the one being enrolled", "wrongname", enrollInput(good, platform.RoleApplication), ""},
		{"the operator expected another identity key", "app-2", func() map[string]any {
			in := enrollInput(other, platform.RoleApplication)
			in["identity_fingerprint"] = goodFingerprint
			return in
		}(), ReasonIdentityMismatch},
	}
	for _, tc := range cases {
		op := l.enroll(t, "enroll-"+tc.node, tc.node, tc.input, contract.Failed)
		n := l.node(t, tc.node)
		if n.EnrollmentState != platform.EnrollmentFailed || n.EnrollmentError == "" {
			t.Errorf("%s: the node must be failed with a reason: %+v", tc.name, n)
		}
		if tc.reason != "" && !strings.Contains(l.events(t, op), "reason="+tc.reason) {
			t.Errorf("%s: the failure must carry the reason %q: %s", tc.name, tc.reason, l.events(t, op))
		}
		// A failed enrollment can never be placed work.
		if err := l.s.checkPlacement(n, platform.RoleApplication, platform.CapabilityDocker); err == nil {
			t.Errorf("%s: a failed node accepted work", tc.name)
		} else {
			wantViolation(t, err, ReasonNotEnrolled)
		}
		if _, err := l.s.resolver.TransportFor(context.Background(), tc.node); err == nil {
			t.Errorf("%s: a failed node resolved to a transport", tc.name)
		}
	}

	// The same machine cannot be enrolled twice under two names.
	clone := enrollInput(good, platform.RoleApplication)
	cloneOp := platformSubmit(t, l.s, platform.KindNodeEnroll, "node/app-1", "re-enroll-same", clone)
	platformAwait(t, l.s, cloneOp, contract.Succeeded)
	if l.node(t, "app-1").EnrollmentState != platform.EnrollmentEnrolled {
		t.Fatal("re-enrolling the same facts must keep the node enrolled")
	}
	// A different host key for an enrolled node is a conflict, not an update.
	_, err := l.s.Submit(context.Background(), platformSubmission(platform.KindNodeEnroll, "node/app-1", "rekey", enrollInputWithKey(good, other, platform.RoleApplication)))
	if fault := contract.Classify(err); fault.Code != contract.Conflict {
		t.Fatalf("replacing an enrolled node's host key must conflict: %v", err)
	}
	// Registration can never overwrite an enrolled node's verified facts.
	_, err = l.s.Submit(context.Background(), platformSubmission(platform.KindNodeRegister, "platform", "overwrite", map[string]any{
		"name": "app-1", "roles": []string{"builder"}, "capabilities": []string{"docker", "buildpacks"}}))
	if fault := contract.Classify(err); fault.Code != contract.Conflict {
		t.Fatalf("registering over an enrolled node must conflict: %v", err)
	}
	if got := l.node(t, "app-1"); strings.Join(got.Roles, ",") != "application" {
		t.Fatalf("the enrolled node's roles changed: %+v", got)
	}
}

func enrollInputWithKey(f, keySource *sshfixture.Fixture, roles ...string) map[string]any {
	in := enrollInput(f, roles...)
	in["host_key"] = hostKeyLine(keySource)
	return in
}

// TestIntegrationNodesCloneCannotShareAnIdentity proves one node-generated
// identity key identifies exactly one node row.
func TestIntegrationNodesCloneCannotShareAnIdentity(t *testing.T) {
	l := newNodeLab(t)
	f := appFixture(t, l, "app-1")
	l.enroll(t, "enroll-1", "app-1", enrollInput(f, platform.RoleApplication), contract.Succeeded)
	// The same host answers as the same name, so enroll it under that name again
	// (allowed) but also try a second record pointing at it.
	f2 := l.fixture(t, "app-1", sshfixture.Options{Files: map[string]string{"/usr/local/bin/docker": dockerStub}})
	seed, err := f.Read(context.Background(), "/var/lib/eregion/node/identity/node.ed25519")
	if err != nil {
		t.Fatal(err)
	}
	// Copy the identity to a second machine: a cloned VM.
	if _, err := f2.Exec(context.Background(), "mkdir -p /var/lib/eregion/node/identity && printf '%s' '"+strings.TrimSpace(seed)+"' > /var/lib/eregion/node/identity/node.ed25519 && chown -R eregion:eregion /var/lib/eregion && chmod 600 /var/lib/eregion/node/identity/node.ed25519"); err != nil {
		t.Fatal(err)
	}
	// The clone reports the name "app-1"; enroll it under the name it reports.
	// The name is free only because the first node has another one in the database:
	// rename the first node's row to prove the identity (not the name) is the guard.
	if _, err := l.pool.Exec(context.Background(), `UPDATE eregion_platform_nodes SET name='app-0' WHERE name='app-1'`); err != nil {
		t.Fatal(err)
	}
	op := l.enroll(t, "enroll-clone", "app-1", enrollInput(f2, platform.RoleApplication), contract.Failed)
	if !strings.Contains(l.events(t, op), "already enrolled as another node") {
		t.Fatalf("a cloned identity must be refused: %s", l.events(t, op))
	}
	if l.node(t, "app-1").EnrollmentState != platform.EnrollmentFailed {
		t.Fatal("the clone must not become enrolled")
	}
}

// TestIntegrationNodesEnrollmentRetryIsIdempotent proves a failed enrollment can
// be retried once the cause is fixed, and that re-submission never duplicates it.
func TestIntegrationNodesEnrollmentRetryIsIdempotent(t *testing.T) {
	l := newNodeLab(t)
	// A builder with docker but no pack: the probe proves it cannot be a builder.
	f := l.fixture(t, "build-1", sshfixture.Options{Files: map[string]string{"/usr/local/bin/docker": dockerStub}})
	input := enrollInput(f, platform.RoleBuilder)

	first := l.enroll(t, "enroll-build", "build-1", input, contract.Failed)
	n := l.node(t, "build-1")
	if n.EnrollmentState != platform.EnrollmentFailed || !strings.Contains(n.EnrollmentError, "buildpacks") {
		t.Fatalf("the probe must explain what is missing: %+v", n)
	}
	if !strings.Contains(l.events(t, first), "reason=capability_missing") {
		t.Fatalf("missing capability must be a placement violation: %s", l.events(t, first))
	}
	// Same idempotency key: the same operation, never a second one.
	again := platformSubmit(t, l.s, platform.KindNodeEnroll, "node/build-1", "enroll-build", input)
	if again.ID != first.ID {
		t.Fatalf("a duplicate submission created %s, want %s", again.ID, first.ID)
	}

	// Fix the host, then retry with the recorded facts.
	if err := f.InstallFile(context.Background(), "/usr/local/bin/pack", packStub); err != nil {
		t.Fatal(err)
	}
	retry := platformSubmit(t, l.s, platform.KindNodeEnroll, "node/build-1", "retry-build", map[string]any{"retry": true})
	platformAwait(t, l.s, retry, contract.Succeeded)
	n = l.node(t, "build-1")
	if n.EnrollmentState != platform.EnrollmentEnrolled || strings.Join(n.Capabilities, ",") != "buildpacks,docker" || n.EnrollmentError != "" {
		t.Fatalf("retry must enroll the node: %+v", n)
	}
	// A retry of an enrolled node is a harmless re-verification.
	platformAwait(t, l.s, platformSubmit(t, l.s, platform.KindNodeEnroll, "node/build-1", "retry-build-2", map[string]any{"retry": true}), contract.Succeeded)
	if l.node(t, "build-1").EnrollmentState != platform.EnrollmentEnrolled {
		t.Fatal("a re-verification must keep the node enrolled")
	}
	// The identity the first verification pinned is the one later checks use.
	var count int
	if err := l.pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_platform_nodes WHERE name='build-1'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("one node row: %d %v", count, err)
	}
	// Retry of a node that was never submitted for enrollment is refused.
	if _, err := l.s.Submit(context.Background(), platformSubmission(platform.KindNodeEnroll, "node/missing", "retry-missing", map[string]any{"retry": true})); contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("retry of an unknown node: %v", err)
	}
}

// TestIntegrationNodesCheckReverifiesIdentityAndQuarantinesASwappedHost proves
// node check is an authenticated observation: it records freshness, and a host
// that no longer holds the pinned identity key is quarantined from work.
func TestIntegrationNodesCheckReverifiesIdentityAndQuarantinesASwappedHost(t *testing.T) {
	l := newNodeLab(t)
	ctx := context.Background()
	f := appFixture(t, l, "app-1")
	l.enroll(t, "enroll-1", "app-1", enrollInput(f, platform.RoleApplication), contract.Succeeded)
	before := l.node(t, "app-1").LastCheckAt

	time.Sleep(20 * time.Millisecond)
	platformAwait(t, l.s, platformSubmit(t, l.s, platform.KindNodeCheck, "node/app-1", "check-1", map[string]any{}), contract.Succeeded)
	after := l.node(t, "app-1")
	if after.LastCheckAt == nil || before == nil || !after.LastCheckAt.After(*before) || after.LastCheckState != "ok" || after.Status != platform.NodeReady {
		t.Fatalf("a check must refresh the observation: before=%v after=%+v", before, after)
	}

	// The host is rebuilt: it generates a fresh identity key.
	if out, err := f.Exec(ctx, "cp -a /var/lib/eregion/node/identity /tmp/identity.bak && rm -rf /var/lib/eregion/node/identity"); err != nil {
		t.Fatal(err, out)
	}
	op := platformSubmit(t, l.s, platform.KindNodeCheck, "node/app-1", "check-2", map[string]any{})
	platformAwait(t, l.s, op, contract.Failed)
	swapped := l.node(t, "app-1")
	if swapped.LastCheckState != ReasonIdentityMismatch || swapped.Status != platform.NodeUnreachable {
		t.Fatalf("a swapped host must be flagged: %+v", swapped)
	}
	if !strings.Contains(l.events(t, op), "reason=identity_mismatch") {
		t.Fatalf("the violation must be an event: %s", l.events(t, op))
	}
	_, err := l.s.Submit(ctx, platformSubmission(platform.KindEnvironmentCreate, "app/api", "env-swapped", map[string]any{"name": "x", "node": "app-1"}))
	if fault := contract.Classify(err); fault.Code == contract.NotFound {
		// The app does not exist in this scenario; create it so placement is what is under test.
		platformAwait(t, l.s, platformSubmit(t, l.s, platform.KindAppCreate, "platform", "app-api", map[string]any{"name": "api"}), contract.Succeeded)
		_, err = l.s.Submit(ctx, platformSubmission(platform.KindEnvironmentCreate, "app/api", "env-swapped-2", map[string]any{"name": "x", "node": "app-1"}))
	}
	wantViolation(t, err, ReasonIdentityMismatch)

	// The original identity returns: a check verifies it and lifts the quarantine.
	if out, err := f.Exec(ctx, "rm -rf /var/lib/eregion/node/identity && cp -a /tmp/identity.bak /var/lib/eregion/node/identity"); err != nil {
		t.Fatal(err, out)
	}
	platformAwait(t, l.s, platformSubmit(t, l.s, platform.KindNodeCheck, "node/app-1", "check-3", map[string]any{}), contract.Succeeded)
	if got := l.node(t, "app-1"); got.LastCheckState != "ok" || got.Status != platform.NodeReady {
		t.Fatalf("the quarantine must lift once the identity verifies: %+v", got)
	}
	if err := l.s.checkPlacement(l.node(t, "app-1"), platform.RoleApplication, platform.CapabilityDocker); err != nil {
		t.Fatalf("a verified node takes work again: %v", err)
	}
}

// TestIntegrationBuildSlotsBoundConcurrentBuildsPerNode proves the per-node build
// concurrency limit across callers, and that a released slot admits the next.
func TestIntegrationBuildSlotsBoundConcurrentBuildsPerNode(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	ctx := context.Background()
	oldPoll, oldWait := buildSlotPoll, buildSlotWait
	buildSlotPoll, buildSlotWait = 20*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { buildSlotPoll, buildSlotWait = oldPoll, oldWait })

	first, err := s.acquireBuildSlot(ctx, "build-1", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.acquireBuildSlot(ctx, "build-1", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A third build on the same node waits, then is refused as a capacity violation.
	var queued sync.WaitGroup
	queued.Add(1)
	_, err = s.acquireBuildSlot(ctx, "build-1", 2, queued.Done)
	wantViolation(t, err, ReasonBuildCapacity)
	queued.Wait() // the queued callback fired exactly once

	// Another node has its own slots.
	other, err := s.acquireBuildSlot(ctx, "build-2", 2, nil)
	if err != nil {
		t.Fatalf("slots are per node: %v", err)
	}
	other()

	// A released slot admits a waiting build.
	buildSlotWait = 5 * time.Second
	got := make(chan func(), 1)
	go func() {
		release, err := s.acquireBuildSlot(ctx, "build-1", 2, nil)
		if err != nil {
			t.Error(err)
			close(got)
			return
		}
		got <- release
	}()
	select {
	case <-got:
		t.Fatal("a build started while every slot was busy")
	case <-time.After(150 * time.Millisecond):
	}
	first()
	select {
	case release := <-got:
		release()
	case <-time.After(3 * time.Second):
		t.Fatal("a released slot did not admit the waiting build")
	}
	second()

	// With a limit of one the second caller is serialized behind the first.
	one, err := s.acquireBuildSlot(ctx, "build-3", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := s.acquireBuildSlot(cancelCtx, "build-3", 1, nil); err == nil {
		t.Fatal("a node with one slot ran two builds")
	}
	one()
}

// TestIntegrationNodesReconcileWireGuardOverARealTunnel enrolls two nodes into
// the private network, reconciles their peers, and then dispatches work to a
// node through the userspace WireGuard tunnel into kernel WireGuard. The node
// keys are generated on the nodes; no private key reaches PostgreSQL.
//
// The node account is unprivileged, so a small root-side loop in the container
// stands in for the operator-installed privileged helper (a systemd path unit
// in production): it loads the rendered configuration with wg setconf.
func TestIntegrationNodesReconcileWireGuardOverARealTunnel(t *testing.T) {
	l := newNodeLab(t)
	ctx := context.Background()
	options := func() sshfixture.Options {
		return sshfixture.Options{
			NetAdmin:       true,
			WireGuardApply: []string{"/usr/local/sbin/eregion-wg-apply"},
			Files: map[string]string{
				"/usr/local/bin/docker":               dockerStub,
				"/usr/local/sbin/eregion-wg-apply":    wgApplyHelper,
				"/usr/local/sbin/eregion-wg-rootloop": wgRootLoop,
			},
		}
	}
	one, two := l.fixture(t, "app-1", options()), l.fixture(t, "app-2", options())
	for _, f := range []*sshfixture.Fixture{one, two} {
		if err := f.ExecDetached(ctx, "/usr/local/sbin/eregion-wg-rootloop"); err != nil {
			t.Fatal(err)
		}
	}
	private := func(name string) map[string]any {
		f := map[string]*sshfixture.Fixture{"app-1": one, "app-2": two}[name]
		in := enrollInput(f, platform.RoleApplication)
		in["private_network"] = map[string]any{"endpoint": f.UDPEndpoint, "ssh_port": 2222}
		return in
	}
	l.enroll(t, "enroll-1", "app-1", private("app-1"), contract.Succeeded)
	l.enroll(t, "enroll-2", "app-2", private("app-2"), contract.Succeeded)
	a1, a2 := l.node(t, "app-1"), l.node(t, "app-2")
	if a1.WireGuard == nil || a2.WireGuard == nil || a1.WireGuard.Address != "10.77.0.2" || a2.WireGuard.Address != "10.77.0.3" {
		t.Fatalf("each node gets its own tunnel address: %+v %+v", a1.WireGuard, a2.WireGuard)
	}
	if a1.WireGuard.State != platform.WireGuardNone {
		t.Fatalf("enrollment itself does not touch WireGuard: %+v", a1.WireGuard)
	}

	reconcile := func(key, node string, input map[string]any) contract.Operation {
		op := platformSubmit(t, l.s, platform.KindNodeReconcile, "node/"+node, key, input)
		return platformAwait(t, l.s, op, contract.Succeeded)
	}
	reconcile("rec-1", "app-1", map[string]any{})
	a1 = l.node(t, "app-1")
	if a1.WireGuard.State != platform.WireGuardApplied || a1.WireGuard.PublicKey == "" || a1.WireGuard.ReconciledAt == nil {
		t.Fatalf("peers must be applied: %+v", a1.WireGuard)
	}
	nodePrivate, err := one.Read(ctx, "/var/lib/eregion/node/wireguard/private.key")
	if err != nil || strings.TrimSpace(nodePrivate) == "" {
		t.Fatalf("the node generated its own WireGuard key: %q %v", nodePrivate, err)
	}
	controlPrivate, err := l.readControlKey("app-1")
	if err != nil {
		t.Fatal(err)
	}
	everything := l.everythingTheControlPlaneStores(t)
	for name, secret := range map[string]string{"node": nodePrivate, "control": controlPrivate} {
		if strings.Contains(everything, strings.TrimSpace(secret)) {
			t.Fatalf("the %s WireGuard private key reached PostgreSQL", name)
		}
	}
	var controlPublic string
	if err := l.pool.QueryRow(ctx, `SELECT control_wg_public_key FROM eregion_platform_nodes WHERE name='app-1'`).Scan(&controlPublic); err != nil || controlPublic == "" {
		t.Fatalf("the control plane's public key is stored: %q %v", controlPublic, err)
	}

	// From now on the node is reached through the tunnel, and the tunnel works.
	rec, _ := loadNodeConn(ctx, l.pool, "app-1")
	if !l.s.resolver.usesTunnel(rec, false) {
		t.Fatal("an applied node must be reached through its tunnel")
	}
	platformAwait(t, l.s, platformSubmit(t, l.s, platform.KindNodeCheck, "node/app-1", "check-wg", map[string]any{}), contract.Succeeded)
	handshakes, err := one.Exec(ctx, "wg show wg0 latest-handshakes")
	if err != nil || !strings.Contains(handshakes, "\t") || strings.HasSuffix(strings.TrimSpace(handshakes), "\t0") {
		t.Fatalf("the check must have crossed the real tunnel: %q %v", handshakes, err)
	}
	if peers, _ := one.Exec(ctx, "wg show wg0 peers"); strings.Count(strings.TrimSpace(peers), "\n") != 0 {
		t.Fatalf("node 1 knows only the control plane so far: %q", peers)
	}

	// Reconciling the second node, then the first again, builds the mesh. The
	// first uses the direct path, which is also how a broken tunnel is repaired.
	reconcile("rec-2", "app-2", map[string]any{})
	reconcile("rec-1b", "app-1", map[string]any{"via": "direct"})
	if peers, _ := one.Exec(ctx, "wg show wg0 peers"); len(strings.Fields(peers)) != 2 {
		t.Fatalf("node 1 must peer with the control plane and node 2: %q", peers)
	}
	conf, _ := one.Read(ctx, "/var/lib/eregion/node/wireguard/wg0.conf")
	if !strings.Contains(conf, "AllowedIPs = 10.77.0.3/32") || !strings.Contains(conf, "Endpoint = "+two.UDPEndpoint) {
		t.Fatalf("node 1's configuration must name node 2:\n%s", conf)
	}

	// Reconciling again changes nothing: the privileged applier does not run.
	applied := func() string {
		out, _ := one.Exec(ctx, "cat /run/wg-apply-count 2>/dev/null | wc -l")
		return strings.TrimSpace(out)
	}
	before := applied()
	reconcile("rec-1c", "app-1", map[string]any{"via": "direct"})
	if after := applied(); after != before {
		t.Fatalf("an unchanged reconcile reloaded WireGuard: %s -> %s", before, after)
	}
}

func (l *nodeLab) readControlKey(node string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(l.keyDir, "wg", node+".key"))
	return string(raw), err
}

// wgApplyHelper is the node-account side of the privileged apply: it waits for
// the root applier to have loaded exactly this configuration.
const wgApplyHelper = `#!/bin/sh
want=$(sha256sum "$1" | cut -d' ' -f1)
i=0
while [ "$i" -lt 150 ]; do
  [ "$(cat /run/wg-applied 2>/dev/null)" = "$want" ] && exit 0
  sleep 0.2
  i=$((i+1))
done
echo "the privileged applier did not load $want" >&2
exit 1
`

// wgRootLoop plays the operator-installed root service that loads the rendered
// configuration into the kernel.
const wgRootLoop = `#!/bin/sh
conf=/var/lib/eregion/node/wireguard/wg0.conf
while true; do
  if [ -f "$conf" ]; then
    want=$(sha256sum "$conf" | cut -d' ' -f1)
    have=$(cat /run/wg-applied 2>/dev/null)
    if [ "$want" != "$have" ]; then
      addr=$(sed -n 's/^Address = //p' "$conf")
      grep -v '^Address' "$conf" > /run/wg0.setconf
      ip link show wg0 >/dev/null 2>&1 || ip link add wg0 type wireguard
      if wg setconf wg0 /run/wg0.setconf && ip address replace "$addr" dev wg0 && ip link set wg0 up; then
        echo "$want" > /run/wg-applied
        echo x >> /run/wg-apply-count
      fi
    fi
  fi
  sleep 0.2
done
`

// nodeEffects counts the durable effect records a node holds for an operation.
func nodeEffects(ctx context.Context, f *sshfixture.Fixture, id contract.OperationID) int {
	names, _ := f.Effects(ctx, string(id))
	count := 0
	for _, name := range names {
		if strings.HasSuffix(name, ".json") {
			count++
		}
	}
	return count
}
