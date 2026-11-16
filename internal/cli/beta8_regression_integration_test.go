package cli

// Beta.8 regression harness. Every behavioural assertion below runs through the
// real Cobra command tree (runApp), the real application service and an isolated
// PostgreSQL database. The only substituted boundary is the operator transport
// itself: beta8CLIBackend forwards the methods these commands use to the real
// api.AppService and independently serializes each follow observation/result,
// exactly as the SSH node RPC boundary does. Any method this harness does not
// support dispatches through the embedded nil Backend and panics, so an
// unsupported call can never quietly succeed with canned data.
//
// External DNS observation is pinned to a closed loopback port. It is a
// test-owned read-only stub, never a real resolver or provider mutation.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/viewer"
)

var beta8FixtureActor = contract.Actor{Kind: "ci", Subject: "beta8-cli-fixture"}
var beta8Operator = contract.Actor{Kind: "operator", Subject: "beta8-cli"}

// beta8TestPool creates one uniquely named database per test, migrates it and
// drops it with FORCE cleanup. It never uses package-global state or an ambient
// production DSN.
func beta8TestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("EREGION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set EREGION_TEST_DATABASE_URL to run isolated PostgreSQL CLI integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := controlplane.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	id, err := contract.NewOperationID()
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	name := "eregion_cli_test_" + strings.TrimPrefix(string(id), "op_")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := controlplane.Open(ctx, u.String())
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_, dropErr := admin.Exec(
			dropCtx,
			"DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)",
		)
		admin.Close()
		if dropErr != nil {
			t.Errorf("could not remove test database %s", name)
		}
	})
	if _, err := controlplane.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func beta8TestRuntime(t *testing.T, pool *pgxpool.Pool) *controlplane.Runtime {
	t.Helper()
	store, err := entities.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	run, err := controlplane.New(
		store,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		controlplane.WorkerConfig{
			Concurrency: 1, TestOnly: true, RegistryEndpoint: "127.0.0.1:5000",
			LocalNodeName: "local", NodeStateDir: t.TempDir(),
			TestRouteSync: func(context.Context) error { return nil },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	return run
}

func beta8StartWorkers(t *testing.T, run *controlplane.Runtime) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if err := run.Jobs.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		stopCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := run.Jobs.StopAndCancel(stopCtx); err != nil {
			t.Errorf("stop workers: %v", err)
		}
	})
}

// beta8IsolateWorkspace keeps command initialization away from the operator's
// saved workspace credentials and from the ambient control database.
func beta8IsolateWorkspace(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(dir, "workspaces.json"))
	t.Setenv("EREGION_WORKSPACE", "")
	t.Setenv("EREGION_CONTEXT", "")
	t.Setenv("EREGION_CONTEXT_FILE", "")
	t.Setenv("EREGION_DATABASE_URL", "")
	t.Setenv("EREGION_NODE_STATE_DIR", "")
	t.Setenv("EREGION_NODE_NAME", "")
	t.Setenv("EREGION_NODE_TRANSPORT", "")
	// PG* variables can carry credentials. Only their names are inspected so
	// no value is read or printed; the explicit disposable DSN is the sole
	// database configuration in this test.
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "PG") {
			_ = os.Unsetenv(name)
		}
	}
}

func beta8PinDNSObservation(t *testing.T) {
	t.Helper()
	previous := platform.PublicResolvers
	platform.PublicResolvers = []string{"127.0.0.1:1"}
	t.Cleanup(func() { platform.PublicResolvers = previous })
}

// beta8CLIBackend forwards the used operator methods to the real application
// service and independently decodes wire boundaries. The embedded Backend is
// nil on purpose: an unsupported method panics instead of returning canned data.
type beta8CLIBackend struct {
	Backend
	app   api.AppService
	actor contract.Actor

	mu           sync.Mutex
	sessions     int
	lastSnapshot *contract.Operation
	lastResult   *contract.Operation
}

func (b *beta8CLIBackend) authority() operations.Authority {
	return operations.OperatorAuthority(b.actor)
}

func (b *beta8CLIBackend) SubmitAction(
	ctx context.Context,
	kind, target string,
	input json.RawMessage,
	key, confirm string,
	dryRun bool,
) (json.RawMessage, error) {
	// The SSH node client sends the confirmation separately from the normalised
	// input; PreviewOrSubmitWithFactor has no confirmation parameter because
	// target confirmation is part of the canonical action input for domain
	// removal. The CLI already puts it there.
	_ = confirm
	value, err := b.app.PreviewOrSubmitWithFactor(
		ctx,
		b.actor,
		kind,
		target,
		input,
		key,
		dryRun,
		"",
	)
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func (b *beta8CLIBackend) SubmitActionWithSecrets(
	ctx context.Context,
	kind, target string,
	input json.RawMessage,
	key, confirm string,
	dryRun bool,
	secrets map[string]string,
) (json.RawMessage, error) {
	if len(secrets) != 0 {
		return nil, contract.NewFault(
			contract.InvalidOutputMode,
			"the beta.8 CLI harness does not forward transient secret request values",
			"Use a command that does not require transient secret values.",
		)
	}
	return b.SubmitAction(ctx, kind, target, input, key, confirm, dryRun)
}

func (b *beta8CLIBackend) PlatformInventory(
	ctx context.Context,
	collection string,
	filters url.Values,
) (json.RawMessage, error) {
	copied := make(url.Values, len(filters)+1)
	for key, values := range filters {
		copied[key] = append([]string(nil), values...)
	}
	copied.Set("collection", collection)
	query, err := api.ParseInventoryQueryValues(copied)
	if err != nil {
		return nil, err
	}
	inventory, err := b.app.Inventory(ctx, b.actor, query)
	if err != nil {
		return nil, err
	}
	return json.Marshal(inventory)
}

func (b *beta8CLIBackend) Get(
	ctx context.Context,
	id contract.OperationID,
) (json.RawMessage, error) {
	op, err := b.app.GetOperation(ctx, b.authority(), id)
	if err != nil {
		return nil, err
	}
	return json.Marshal(op)
}

func (b *beta8CLIBackend) Events(
	ctx context.Context,
	cursor contract.Cursor,
	limit int,
) (json.RawMessage, error) {
	page, err := b.app.OperationEvents(ctx, b.authority(), cursor, limit)
	if err != nil {
		return nil, err
	}
	return json.Marshal(page)
}

// FollowSession is the optional viewer.SessionClient boundary. Each snapshot,
// each event page and the final result cross independent JSON encoders and
// decoders so nested result/evidence pointers are freshly allocated, as they
// are on the node RPC wire.
func (b *beta8CLIBackend) FollowSession(
	ctx context.Context,
	cursor contract.Cursor,
	limit int,
	interval time.Duration,
	emit func(viewer.SessionObservation) error,
) (viewer.SessionResult, error) {
	b.mu.Lock()
	b.sessions++
	b.mu.Unlock()
	result, err := b.app.FollowOperation(
		ctx,
		b.authority(),
		cursor,
		limit,
		interval,
		func(observation api.OperationFollowObservation) error {
			wire := viewer.SessionObservation{Cursor: observation.Cursor}
			if observation.Snapshot != nil {
				snapshot, err := beta8RoundTripOperation(*observation.Snapshot)
				if err != nil {
					return err
				}
				wire.Snapshot = &snapshot
				b.mu.Lock()
				b.lastSnapshot = &snapshot
				b.mu.Unlock()
			}
			if observation.Events != nil {
				events, err := beta8RoundTripEvents(observation.Events)
				if err != nil {
					return err
				}
				wire.Events = events
			}
			return emit(wire)
		},
	)
	if err != nil {
		return viewer.SessionResult{}, err
	}
	operation, err := beta8RoundTripOperation(result.Operation)
	if err != nil {
		return viewer.SessionResult{}, err
	}
	b.mu.Lock()
	b.lastResult = &operation
	b.mu.Unlock()
	return viewer.SessionResult{Operation: operation, Cursor: result.Cursor}, nil
}

func beta8RoundTripOperation(op contract.Operation) (contract.Operation, error) {
	raw, err := json.Marshal(op)
	if err != nil {
		return contract.Operation{}, err
	}
	var decoded contract.Operation
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return contract.Operation{}, err
	}
	return decoded, nil
}

func beta8RoundTripEvents(events []contract.Event) ([]contract.Event, error) {
	raw, err := json.Marshal(events)
	if err != nil {
		return nil, err
	}
	var decoded []contract.Event
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

type beta8CLIResult struct {
	Code   int
	Stdout string
	Stderr string
}

func beta8RunCLI(t *testing.T, backend Backend, args ...string) beta8CLIResult {
	t.Helper()
	var out, errOut bytes.Buffer
	a := newApp()
	a.backend = backend
	code := runApp(
		context.Background(),
		args,
		strings.NewReader(""),
		&out,
		&errOut,
		&secrets.Registry{},
		a,
	)
	return beta8CLIResult{Code: code, Stdout: out.String(), Stderr: errOut.String()}
}

func beta8Envelope(t *testing.T, stdout string) contract.Envelope {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(stdout))
	decoder.UseNumber()
	var envelope contract.Envelope
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("decode envelope %q: %v", stdout, err)
	}
	if err := envelope.Validate(); err != nil {
		t.Fatalf("invalid envelope %q: %v", stdout, err)
	}
	return envelope
}

func beta8DataMap(t *testing.T, envelope contract.Envelope) map[string]any {
	t.Helper()
	data, ok := envelope.Data.(map[string]any)
	if !ok {
		t.Fatalf("envelope data is not an object: %#v", envelope.Data)
	}
	return data
}

func beta8OperationIDField(t *testing.T, envelope contract.Envelope) contract.OperationID {
	t.Helper()
	value, _ := beta8DataMap(t, envelope)["operation_id"].(string)
	id := contract.OperationID(value)
	if err := id.Validate(); err != nil {
		t.Fatalf("envelope operation id %q is invalid: %v", value, err)
	}
	return id
}

func beta8Submit(
	t *testing.T,
	service operations.Service,
	key, kind, target string,
	input any,
) contract.Operation {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	op, err := service.Submit(context.Background(), operations.Submission{
		Kind: kind, Target: target, Actor: beta8FixtureActor, IdempotencyKey: key, Input: raw,
	})
	if err != nil {
		t.Fatalf("submit %s %s: %v", kind, target, err)
	}
	return op
}

func beta8OperationCount(t *testing.T, pool *pgxpool.Pool, kind, target string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(
		context.Background(),
		`SELECT count(*) FROM eregion_operations WHERE kind=$1 AND target=$2`,
		kind,
		target,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func beta8EnvironmentID(t *testing.T, pool *pgxpool.Pool, app, environment string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT e.id FROM eregion_platform_environments e
		JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1 AND e.name=$2`, app, environment).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// beta8InsertDomain is the same transactional fixture shape the control-plane
// tests use for a pre-existing binding; admission and reads remain real.
func beta8InsertDomain(
	t *testing.T,
	pool *pgxpool.Pool,
	environment, name string,
	primary bool,
) {
	t.Helper()
	id, err := platform.NewDomainID()
	if err != nil {
		t.Fatal(err)
	}
	environmentID := beta8EnvironmentID(t, pool, "beta8cli", environment)
	if _, err := pool.Exec(context.Background(), `INSERT INTO eregion_platform_domains
		(name,environment_id,created_at,is_primary,desired_revision,applied_revision,route_state,route_error,
		expected_addresses,dns_state,dns_addresses,dns_error,certificate_state,certificate_error,
		public_state,public_error,check_path,id)
		VALUES($1,$2,clock_timestamp(),$3,1,0,'pending','',ARRAY[]::text[],'pending',ARRAY[]::text[],'','pending','','pending','','',$4)`,
		name, environmentID, primary, id); err != nil {
		t.Fatal(err)
	}
}

func beta8SeedDomainApp(t *testing.T, run *controlplane.Runtime) {
	t.Helper()
	beta8Submit(t, run.Service, "beta8-node", platform.KindNodeRegister, "platform", map[string]any{
		"name":         "local",
		"roles":        []string{platform.RoleApplication},
		"capabilities": []string{platform.CapabilityDocker},
	})
	beta8Submit(
		t,
		run.Service,
		"beta8-app",
		platform.KindAppCreate,
		"platform",
		map[string]any{"name": "beta8cli"},
	)
	for _, environment := range []string{"json", "plain", "plainlive", "remove", "removeplain", "primary", "preview", "ndjson"} {
		beta8Submit(
			t,
			run.Service,
			"beta8-env-"+environment,
			platform.KindEnvironmentCreate,
			"app/beta8cli",
			map[string]any{
				"name": environment, "node": "local",
			},
		)
	}
}

func TestIntegrationCLIRegressionDomainAcceptance(t *testing.T) {
	pool := beta8TestPool(t)
	run := beta8TestRuntime(t, pool)
	beta8IsolateWorkspace(t)
	beta8PinDNSObservation(t)
	beta8SeedDomainApp(t, run)
	beta8InsertDomain(t, pool, "remove", "beta8-cli-remove.invalid", false)
	beta8InsertDomain(t, pool, "removeplain", "beta8-cli-remove-plain.invalid", false)
	beta8InsertDomain(t, pool, "primary", "beta8-cli-primary.invalid", true)
	beta8InsertDomain(t, pool, "ndjson", "beta8-cli-ndjson-primary.invalid", true)
	backend := &beta8CLIBackend{app: api.NewAppService(run.Service, nil, nil), actor: beta8Operator}

	t.Run("add JSON detach correlates accepted operation", func(t *testing.T) {
		beta8DomainAddJSON(t, pool, run, backend)
	})
	t.Run("add plain detach and plain acceptance", func(t *testing.T) {
		beta8DomainAddPlain(t, pool, backend)
	})
	t.Run("remove JSON and plain acceptance", func(t *testing.T) {
		beta8DomainRemoveAcceptance(t, pool, run, backend)
	})
	t.Run("confirmation and primary guard reject before admission", func(t *testing.T) {
		beta8DomainGuards(t, pool, backend)
	})
	t.Run("previews leave durable state unchanged", func(t *testing.T) {
		beta8DomainPreviews(t, pool, backend)
	})
	t.Run("add and remove NDJSON follow real worker transitions", func(t *testing.T) {
		beta8DomainNDJSON(t, pool, run, backend)
	})
}

func beta8DomainAddJSON(
	t *testing.T,
	pool *pgxpool.Pool,
	run *controlplane.Runtime,
	backend *beta8CLIBackend,
) {
	t.Helper()
	result := beta8RunCLI(t, backend,
		"domain", "add", "beta8cli", "json", "beta8-cli-json.invalid",
		"--detach", "--json", "--idempotency-key", "beta8-cli-add-json")
	if result.Code != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", result.Code, result.Stderr, result.Stdout)
	}
	envelope := beta8Envelope(t, result.Stdout)
	if !envelope.OK || envelope.Command != "eregion domain add" {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
	id := beta8OperationIDField(t, envelope)
	op, err := run.Service.Get(
		context.Background(),
		operations.OperatorAuthority(beta8FixtureActor),
		id,
	)
	if err != nil {
		t.Fatal(err)
	}
	if op.Kind != platform.KindDomainAdd || op.Target != "env/beta8cli/json" ||
		!op.State.Valid() {
		t.Fatalf("accepted operation does not match the submitted intent: %+v", op)
	}
	if count := beta8OperationCount(
		t,
		pool,
		platform.KindDomainAdd,
		"env/beta8cli/json",
	); count != 1 {
		t.Fatalf("accepted operations=%d, want 1", count)
	}
	var jobs int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM river_job j
		JOIN eregion_operations o ON o.job_id=j.id WHERE o.id=$1`, id).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("river jobs for accepted operation=%d err=%v", jobs, err)
	}

	replay := beta8RunCLI(t, backend,
		"domain", "add", "beta8cli", "json", "beta8-cli-json.invalid",
		"--detach", "--json", "--idempotency-key", "beta8-cli-add-json")
	if replay.Code != 0 {
		t.Fatalf("replay exit=%d stderr=%q", replay.Code, replay.Stderr)
	}
	if replayed := beta8OperationIDField(t, beta8Envelope(t, replay.Stdout)); replayed != id {
		t.Fatalf("same-key replay returned %s, want %s", replayed, id)
	}
	if count := beta8OperationCount(
		t,
		pool,
		platform.KindDomainAdd,
		"env/beta8cli/json",
	); count != 1 {
		t.Fatalf("replay duplicated admission: %d operations", count)
	}

	conflict := beta8RunCLI(t, backend,
		"domain", "add", "beta8cli", "json", "beta8-cli-json-changed.invalid",
		"--detach", "--json", "--idempotency-key", "beta8-cli-add-json")
	if conflict.Code != 5 {
		t.Fatalf("changed-input conflict exit=%d stderr=%q", conflict.Code, conflict.Stderr)
	}
	if envelope := beta8Envelope(
		t,
		conflict.Stdout,
	); envelope.OK || envelope.Error == nil ||
		envelope.Error.Code != contract.Conflict {
		t.Fatalf("changed input did not report conflict: %+v", envelope)
	}
	if count := beta8OperationCount(
		t,
		pool,
		platform.KindDomainAdd,
		"env/beta8cli/json",
	); count != 1 {
		t.Fatalf("conflict duplicated admission: %d operations", count)
	}
}

func beta8DomainAddPlain(
	t *testing.T,
	pool *pgxpool.Pool,
	backend *beta8CLIBackend,
) {
	t.Helper()
	detached := beta8RunCLI(t, backend,
		"domain", "add", "beta8cli", "plain", "beta8-cli-plain.invalid",
		"--detach", "--idempotency-key", "beta8-cli-add-plain")
	if detached.Code != 0 {
		t.Fatalf(
			"plain detach exit=%d stderr=%q stdout=%q",
			detached.Code,
			detached.Stderr,
			detached.Stdout,
		)
	}
	if !strings.Contains(detached.Stdout, "Accepted platform.domain.add") ||
		!strings.Contains(detached.Stdout, "operation: op_") {
		t.Fatalf("plain accepted output missing identity: %q", detached.Stdout)
	}

	accepted := beta8RunCLI(t, backend,
		"domain", "add", "beta8cli", "plainlive", "beta8-cli-plainlive.invalid",
		"--idempotency-key", "beta8-cli-add-plainlive")
	if accepted.Code != 0 {
		t.Fatalf(
			"plain acceptance exit=%d stderr=%q stdout=%q",
			accepted.Code,
			accepted.Stderr,
			accepted.Stdout,
		)
	}
	if !strings.Contains(
		accepted.Stdout,
		"Accepted the domain binding beta8-cli-plainlive.invalid",
	) ||
		!strings.Contains(accepted.Stdout, "operation: op_") {
		t.Fatalf("plain acceptance output missing identity: %q", accepted.Stdout)
	}
	for _, target := range []string{"env/beta8cli/plain", "env/beta8cli/plainlive"} {
		if count := beta8OperationCount(t, pool, platform.KindDomainAdd, target); count != 1 {
			t.Fatalf("%s operations=%d, want 1", target, count)
		}
	}
}

func beta8DomainRemoveAcceptance(
	t *testing.T,
	pool *pgxpool.Pool,
	run *controlplane.Runtime,
	backend *beta8CLIBackend,
) {
	t.Helper()
	jsonResult := beta8RunCLI(t, backend,
		"domain", "remove", "beta8cli", "remove", "beta8-cli-remove.invalid",
		"--confirm", "beta8-cli-remove.invalid", "--detach", "--json",
		"--idempotency-key", "beta8-cli-remove-json")
	if jsonResult.Code != 0 {
		t.Fatalf(
			"remove JSON exit=%d stderr=%q stdout=%q",
			jsonResult.Code,
			jsonResult.Stderr,
			jsonResult.Stdout,
		)
	}
	id := beta8OperationIDField(t, beta8Envelope(t, jsonResult.Stdout))
	op, err := run.Service.Get(
		context.Background(),
		operations.OperatorAuthority(beta8FixtureActor),
		id,
	)
	if err != nil || op.Kind != platform.KindDomainRemove ||
		op.Target != "env/beta8cli/remove" {
		t.Fatalf("remove operation=%+v err=%v", op, err)
	}
	if count := beta8OperationCount(
		t,
		pool,
		platform.KindDomainRemove,
		"env/beta8cli/remove",
	); count != 1 {
		t.Fatalf("remove operations=%d, want 1", count)
	}

	plainResult := beta8RunCLI(t, backend,
		"domain", "remove", "beta8cli", "removeplain", "beta8-cli-remove-plain.invalid",
		"--confirm", "beta8-cli-remove-plain.invalid", "--detach",
		"--idempotency-key", "beta8-cli-remove-plain")
	if plainResult.Code != 0 {
		t.Fatalf(
			"remove plain exit=%d stderr=%q stdout=%q",
			plainResult.Code,
			plainResult.Stderr,
			plainResult.Stdout,
		)
	}
	if !strings.Contains(
		plainResult.Stdout,
		"Accepted platform.domain.remove for beta8-cli-remove-plain.invalid",
	) ||
		!strings.Contains(plainResult.Stdout, "operation: op_") {
		t.Fatalf("plain removal output missing intent: %q", plainResult.Stdout)
	}
}

func beta8DomainGuards(
	t *testing.T,
	pool *pgxpool.Pool,
	backend *beta8CLIBackend,
) {
	t.Helper()
	before := beta8OperationCount(t, pool, platform.KindDomainRemove, "env/beta8cli/remove")
	missing := beta8RunCLI(t, backend,
		"domain", "remove", "beta8cli", "remove", "beta8-cli-remove.invalid", "--json")
	if missing.Code != 5 {
		t.Fatalf("missing confirm exit=%d stderr=%q", missing.Code, missing.Stderr)
	}
	if envelope := beta8Envelope(
		t,
		missing.Stdout,
	); envelope.OK || envelope.Error == nil ||
		envelope.Error.Code != contract.ConfirmationRequired {
		t.Fatalf("missing confirmation code: %+v", envelope)
	}
	wrong := beta8RunCLI(t, backend,
		"domain", "remove", "beta8cli", "remove", "beta8-cli-remove.invalid",
		"--confirm", "other.invalid", "--json")
	if wrong.Code != 2 {
		t.Fatalf("wrong confirm exit=%d stderr=%q", wrong.Code, wrong.Stderr)
	}
	if envelope := beta8Envelope(
		t,
		wrong.Stdout,
	); envelope.OK || envelope.Error == nil ||
		envelope.Error.Code != contract.ValidationFailed {
		t.Fatalf("wrong confirmation code: %+v", envelope)
	}
	primary := beta8RunCLI(t, backend,
		"domain", "remove", "beta8cli", "primary", "beta8-cli-primary.invalid",
		"--confirm", "beta8-cli-primary.invalid", "--json")
	if primary.Code != 5 {
		t.Fatalf(
			"primary removal exit=%d stderr=%q stdout=%q",
			primary.Code,
			primary.Stderr,
			primary.Stdout,
		)
	}
	if envelope := beta8Envelope(
		t,
		primary.Stdout,
	); envelope.OK || envelope.Error == nil ||
		envelope.Error.Code != contract.PreconditionFailed {
		t.Fatalf("primary removal code: %+v", envelope)
	}
	if after := beta8OperationCount(
		t,
		pool,
		platform.KindDomainRemove,
		"env/beta8cli/remove",
	); after != before {
		t.Fatalf("rejected confirmations admitted work: before=%d after=%d", before, after)
	}
	if count := beta8OperationCount(
		t,
		pool,
		platform.KindDomainRemove,
		"env/beta8cli/primary",
	); count != 0 {
		t.Fatalf("primary removal admitted %d operations", count)
	}
}

func beta8DomainPreviews(
	t *testing.T,
	pool *pgxpool.Pool,
	backend *beta8CLIBackend,
) {
	t.Helper()
	var jobsBefore, bindingsBefore int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM river_job`).Scan(&jobsBefore); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_platform_domains`).Scan(&bindingsBefore); err != nil {
		t.Fatal(err)
	}
	preview := beta8RunCLI(
		t,
		backend,
		"domain",
		"add",
		"beta8cli",
		"preview",
		"beta8-cli-preview.invalid",
		"--dry-run",
		"--json",
	)
	if preview.Code != 0 {
		t.Fatalf(
			"add preview exit=%d stderr=%q stdout=%q",
			preview.Code,
			preview.Stderr,
			preview.Stdout,
		)
	}
	if _, present := beta8DataMap(t, beta8Envelope(t, preview.Stdout))["preview"]; !present {
		t.Fatalf("add preview did not return a preview payload: %q", preview.Stdout)
	}
	if count := beta8OperationCount(
		t,
		pool,
		platform.KindDomainAdd,
		"env/beta8cli/preview",
	); count != 0 {
		t.Fatalf("add preview admitted %d operations", count)
	}

	before := beta8OperationCount(t, pool, platform.KindDomainRemove, "env/beta8cli/remove")
	removePreview := beta8RunCLI(t, backend,
		"domain", "remove", "beta8cli", "remove", "beta8-cli-remove.invalid",
		"--confirm", "beta8-cli-remove.invalid", "--dry-run", "--json")
	if removePreview.Code != 0 {
		t.Fatalf(
			"remove preview exit=%d stderr=%q stdout=%q",
			removePreview.Code,
			removePreview.Stderr,
			removePreview.Stdout,
		)
	}
	data := beta8DataMap(t, beta8Envelope(t, removePreview.Stdout))
	if _, present := data["confirmation_token"]; !present {
		t.Fatalf("remove preview missing confirmation token: %#v", data)
	}
	if after := beta8OperationCount(
		t,
		pool,
		platform.KindDomainRemove,
		"env/beta8cli/remove",
	); after != before {
		t.Fatalf("remove preview admitted work: before=%d after=%d", before, after)
	}
	var binding int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_platform_domains WHERE name='beta8-cli-remove.invalid'`).
		Scan(&binding); err != nil ||
		binding != 1 {
		t.Fatalf("preview changed the domain binding: %d err=%v", binding, err)
	}
	var jobsAfter, bindingsAfter int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM river_job`).Scan(&jobsAfter); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_platform_domains`).Scan(&bindingsAfter); err != nil {
		t.Fatal(err)
	}
	if jobsAfter != jobsBefore || bindingsAfter != bindingsBefore {
		t.Fatalf("preview mutated durable state: jobs %d→%d bindings %d→%d", jobsBefore, jobsAfter, bindingsBefore, bindingsAfter)
	}
}

func beta8DomainNDJSON(
	t *testing.T,
	pool *pgxpool.Pool,
	run *controlplane.Runtime,
	backend *beta8CLIBackend,
) {
	t.Helper()
	beta8StartWorkers(t, run)
	add := beta8RunCLI(t, backend,
		"domain", "add", "beta8cli", "ndjson", "beta8-cli-ndjson.invalid",
		"--output", "ndjson", "--idempotency-key", "beta8-cli-add-ndjson")
	if add.Code != 0 {
		t.Fatalf("ndjson add exit=%d stderr=%q stdout=%q", add.Code, add.Stderr, add.Stdout)
	}
	addID := beta8AssertNDJSONFollow(t, add.Stdout)
	op, err := run.Service.Get(
		context.Background(),
		operations.OperatorAuthority(beta8FixtureActor),
		addID,
	)
	if err != nil || op.State != contract.Succeeded || op.Kind != platform.KindDomainAdd {
		t.Fatalf("worker-driven ndjson add operation=%+v err=%v", op, err)
	}
	var bindings int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_platform_domains WHERE name='beta8-cli-ndjson.invalid'`).
		Scan(&bindings); err != nil ||
		bindings != 1 {
		t.Fatalf("worker-driven add binding=%d err=%v", bindings, err)
	}

	remove := beta8RunCLI(t, backend,
		"domain", "remove", "beta8cli", "ndjson", "beta8-cli-ndjson.invalid",
		"--confirm", "beta8-cli-ndjson.invalid", "--output", "ndjson",
		"--idempotency-key", "beta8-cli-remove-ndjson")
	if remove.Code != 0 {
		t.Fatalf(
			"ndjson remove exit=%d stderr=%q stdout=%q",
			remove.Code,
			remove.Stderr,
			remove.Stdout,
		)
	}
	removeID := beta8AssertNDJSONFollow(t, remove.Stdout)
	op, err = run.Service.Get(
		context.Background(),
		operations.OperatorAuthority(beta8FixtureActor),
		removeID,
	)
	if err != nil || op.State != contract.Succeeded || op.Kind != platform.KindDomainRemove {
		t.Fatalf("worker-driven ndjson remove operation=%+v err=%v", op, err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_platform_domains WHERE name='beta8-cli-ndjson.invalid'`).
		Scan(&bindings); err != nil ||
		bindings != 0 {
		t.Fatalf("worker-driven remove left binding=%d err=%v", bindings, err)
	}
}

// beta8SeedWireOperation persists a terminal operation with a real retained
// result and evidence projection, including a large integer and nested cursors.
// Reads still go through the real application service.
func beta8SeedWireOperation(
	t *testing.T,
	pool *pgxpool.Pool,
	service operations.Service,
	state contract.State,
	code contract.Code,
	evidenceMode string,
) contract.Operation {
	t.Helper()
	op, err := service.Submit(context.Background(), operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: beta8FixtureActor,
		IdempotencyKey: "wire-" + string(state) + "-" + evidenceMode, Input: []byte(`{"steps":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := time.Now().UTC()
	if now.Before(op.CreatedAt) {
		now = op.CreatedAt
	}
	result := contract.ActionResult{
		OperationID:   op.ID,
		Action:        op.Kind,
		Target:        op.Target,
		Attempt:       1,
		Status:        state,
		Final:         true,
		StartedAt:     op.CreatedAt,
		CompletedAt:   now,
		TimingBasis:   "operation_created",
		Stage:         "action",
		Code:          code,
		Message:       "durable terminal result",
		Excerpt:       "retained failure excerpt",
		ExcerptCursor: &contract.Cursor{OperationID: op.ID, After: 1},
		Data: json.RawMessage(
			`{"tokens":9007199254740993,"nested":{"count":9007199254740995},"label":"wire"}`,
		),
		DataStatus: "available",
		DataCursor: &contract.Cursor{OperationID: op.ID, After: 1},
		Evidence:   "not_collected",
		History:    contract.Cursor{OperationID: op.ID, After: 1},
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("fixture result: %v", err)
	}
	if _, err := tx.Exec(
		ctx,
		`UPDATE eregion_operations SET state=$2,attempt=1,error_code=$3 WHERE id=$1`,
		op.ID,
		state,
		code,
	); err != nil {
		t.Fatal(err)
	}
	op.State, op.Attempt, op.ErrorCode = state, 1, code
	if err := beta8InsertEvent(
		ctx,
		tx,
		&op,
		contract.Event{Kind: "state", State: state, ErrorCode: code, Result: &result},
	); err != nil {
		t.Fatal(err)
	}
	switch evidenceMode {
	case "present":
		evidence := contract.Event{Kind: "step", Stage: "deploy", Message: "candidate failed",
			NodeEvidence: &contract.NodeExecutionEvidence{
				Attempt:            1,
				Outcome:            "failed",
				Stage:              "candidate",
				OutputAvailability: "available",
				RedactionContext:   "not_required",
				Stdout:             "bounded stdout",
				Stderr:             "bounded stderr\nsecond line",
				StdoutTruncated:    false,
				StderrTruncated:    false,
			}}
		if err := beta8InsertEvent(ctx, tx, &op, evidence); err != nil {
			t.Fatal(err)
		}
	case "unavailable":
		loss := contract.Event{
			Kind:    "step",
			Stage:   "evidence",
			Message: "Node execution evidence could not be retained.",
			EvidenceLoss: &contract.NodeEvidenceLoss{
				ExecutionID: string(op.ID) + ":1:capture-v1",
				Attempt:     1,
			},
		}
		if err := beta8InsertEvent(ctx, tx, &op, loss); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(
		ctx,
		`UPDATE eregion_operations SET state=$2,attempt=1,error_code=$3,last_sequence=$4,
		updated_at=clock_timestamp() WHERE id=$1`,
		op.ID,
		state,
		code,
		op.LastSequence,
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := service.Get(ctx, operations.OperatorAuthority(beta8FixtureActor), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LatestResult == nil {
		t.Fatal("fixture did not persist an operation result")
	}
	if stored.Evidence == nil {
		t.Fatal("fixture did not persist an evidence projection")
	}
	if evidenceMode == "absent" && stored.Evidence.Availability != "not_collected" {
		t.Fatalf("absent evidence mode availability=%q", stored.Evidence.Availability)
	}
	return stored
}

func beta8InsertEvent(
	ctx context.Context,
	tx pgx.Tx,
	op *contract.Operation,
	event contract.Event,
) error {
	event.OperationID, event.Sequence, event.Attempt = op.ID, op.LastSequence+1, op.Attempt
	event.At = time.Now().UTC()
	if err := event.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		ctx,
		`INSERT INTO eregion_operation_events(operation_id,sequence,event) VALUES ($1,$2,$3)`,
		op.ID,
		int64(event.Sequence),
		raw,
	); err != nil {
		return err
	}
	op.LastSequence = event.Sequence
	return nil
}

func beta8EventCount(t *testing.T, pool *pgxpool.Pool, id contract.OperationID) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_operation_events WHERE operation_id=$1`, id).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func beta8AssertNoFollowMutation(
	t *testing.T,
	service operations.Service,
	pool *pgxpool.Pool,
	before contract.Operation,
) {
	t.Helper()
	eventsBefore := beta8EventCount(t, pool, before.ID)
	after, err := service.Get(
		context.Background(),
		operations.OperatorAuthority(beta8FixtureActor),
		before.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != before.State || after.LastSequence != before.LastSequence ||
		after.Attempt != before.Attempt {
		t.Fatalf("follow changed durable state: before=%+v after=%+v", before, after)
	}
	if eventsAfter := beta8EventCount(t, pool, before.ID); eventsAfter != eventsBefore {
		t.Fatalf("follow changed event history: before=%d after=%d", eventsBefore, eventsAfter)
	}
}

func beta8AssertIndependentDecode(t *testing.T, backend *beta8CLIBackend) {
	t.Helper()
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.lastSnapshot == nil || backend.lastResult == nil {
		t.Fatal("follow session did not decode both a snapshot and a final result")
	}
	snapshot, result := backend.lastSnapshot, backend.lastResult
	if snapshot.LatestResult == nil || result.LatestResult == nil || snapshot.Evidence == nil ||
		result.Evidence == nil {
		t.Fatalf("wire fixture lost result/evidence: snapshot=%+v result=%+v", snapshot, result)
	}
	if snapshot.LatestResult == result.LatestResult {
		t.Fatal(
			"snapshot and final result reused one LatestResult pointer instead of independent wire decoding",
		)
	}
	if snapshot.Evidence == result.Evidence {
		t.Fatal(
			"snapshot and final result reused one Evidence pointer instead of independent wire decoding",
		)
	}
	if !reflect.DeepEqual(snapshot.LatestResult, result.LatestResult) ||
		!reflect.DeepEqual(snapshot.Evidence, result.Evidence) {
		t.Fatalf(
			"independently decoded wire values differ: snapshot=%+v result=%+v",
			snapshot.LatestResult,
			result.LatestResult,
		)
	}
}

func TestIntegrationCLIRegressionFollowWireEvidence(t *testing.T) {
	pool := beta8TestPool(t)
	run := beta8TestRuntime(t, pool)
	beta8IsolateWorkspace(t)
	backend := &beta8CLIBackend{
		app:   api.NewAppService(run.Service, nil, nil),
		actor: beta8FixtureActor,
	}

	wire := beta8SeedWireOperation(t, pool, run.Service, contract.Succeeded, "", "present")
	for _, mode := range []struct {
		name string
		args []string
	}{
		{"json", []string{"operations", "follow", string(wire.ID), "--json"}},
		{"plain", []string{"operations", "follow", string(wire.ID)}},
		{"ndjson", []string{"operations", "follow", string(wire.ID), "--output", "ndjson"}},
	} {
		session := &beta8CLIBackend{app: backend.app, actor: backend.actor}
		result := beta8RunCLI(t, session, mode.args...)
		if result.Code != 0 {
			t.Fatalf(
				"%s follow exit=%d stderr=%q stdout=%q",
				mode.name,
				result.Code,
				result.Stderr,
				result.Stdout,
			)
		}
		if session.sessions == 0 {
			t.Fatalf(
				"%s follow did not use the independently serialized session boundary",
				mode.name,
			)
		}
		beta8AssertIndependentDecode(t, session)
		beta8AssertNoFollowMutation(t, run.Service, pool, wire)
		if mode.name == "ndjson" {
			beta8AssertNDJSON(t, result.Stdout, wire.LastSequence)
		}
	}

	if count := beta8OperationCount(
		t,
		pool,
		operations.TestKind,
		operations.TestTarget,
	); count < 1 {
		t.Fatalf("wire fixture missing: %d operations", count)
	}
}

type beta8NDJSONRecord struct {
	Type   string             `json:"type"`
	Event  *contract.Event    `json:"event"`
	Result *contract.Envelope `json:"result"`
}

func beta8ParseNDJSONRecord(t *testing.T, index int, line string) beta8NDJSONRecord {
	t.Helper()
	var record beta8NDJSONRecord
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatalf("ndjson record %d: %v", index, err)
	}
	return record
}

func beta8ApplyNDJSONEvent(
	t *testing.T,
	index int,
	event *contract.Event,
	last uint64,
) (uint64, contract.OperationID) {
	t.Helper()
	if event == nil || event.Sequence <= last || event.OperationID.Validate() != nil {
		t.Fatalf("ndjson event record %d is invalid: %+v", index, event)
	}
	return event.Sequence, event.OperationID
}

func beta8ApplyNDJSONResult(
	t *testing.T,
	index, total int,
	result *contract.Envelope,
	results int,
	id contract.OperationID,
) (contract.OperationID, int) {
	t.Helper()
	if index != total-1 || results != 0 {
		t.Fatalf("ndjson result record is not the single final record at %d", index)
	}
	if result == nil || !result.OK {
		t.Fatalf("ndjson terminal result failed: %+v", result)
	}
	data, _ := result.Data.(map[string]any)
	operation, _ := data["operation"].(map[string]any)
	value, _ := operation["id"].(string)
	parsed := contract.OperationID(value)
	if err := parsed.Validate(); err != nil {
		t.Fatalf("ndjson result operation id %q: %v", value, err)
	}
	if id != "" && id != parsed {
		t.Fatalf("ndjson result operation %s does not match streamed %s", parsed, id)
	}
	return parsed, results + 1
}

func beta8AssertNDJSONFollow(t *testing.T, stdout string) contract.OperationID {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 2 {
		t.Fatalf("ndjson follow produced %d records: %q", len(lines), stdout)
	}
	var last uint64
	var id contract.OperationID
	results := 0
	for index, line := range lines {
		record := beta8ParseNDJSONRecord(t, index, line)
		switch record.Type {
		case "event":
			last, id = beta8ApplyNDJSONEvent(t, index, record.Event, last)
		case "result":
			id, results = beta8ApplyNDJSONResult(t, index, len(lines), record.Result, results, id)
		default:
			t.Fatalf("unexpected ndjson record type %q", record.Type)
		}
	}
	if results != 1 || id == "" {
		t.Fatalf("ndjson results=%d operation=%q", results, id)
	}
	return id
}

func beta8AssertNDJSONFailure(t *testing.T, stdout string, id contract.OperationID, state contract.State) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 1 {
		t.Fatalf("empty ndjson failure stream")
	}
	var last uint64
	results := 0
	for index, line := range lines {
		var record struct {
			Type   string             `json:"type"`
			Event  *contract.Event    `json:"event"`
			Result *contract.Envelope `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("ndjson failure record %d: %v", index, err)
		}
		switch record.Type {
		case "event":
			if record.Event == nil || record.Event.Sequence <= last {
				t.Fatalf("ndjson failure event not monotonic: %+v", record.Event)
			}
			last = record.Event.Sequence
		case "result":
			if index != len(lines)-1 || results != 0 {
				t.Fatalf("ndjson failure result is not the single final record")
			}
			if record.Result == nil || record.Result.OK || record.Result.Error == nil ||
				record.Result.Error.Details["operation_id"] != string(id) ||
				record.Result.Error.Details["state"] != string(state) {
				t.Fatalf("ndjson failure record incomplete: %+v", record.Result)
			}
			results++
		default:
			t.Fatalf("unexpected ndjson record type %q", record.Type)
		}
	}
	if results != 1 {
		t.Fatalf("ndjson failure results=%d", results)
	}
}

func beta8AssertNDJSON(t *testing.T, stdout string, lastSequence uint64) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 2 {
		t.Fatalf("ndjson follow produced %d records: %q", len(lines), stdout)
	}
	var last uint64
	results := 0
	for index, line := range lines {
		var record struct {
			Type   string             `json:"type"`
			Event  *contract.Event    `json:"event"`
			Result *contract.Envelope `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("ndjson record %d: %v", index, err)
		}
		switch record.Type {
		case "event":
			if record.Event == nil || record.Event.Sequence <= last {
				t.Fatalf("event record %d is not monotonic: %+v", index, record.Event)
			}
			last = record.Event.Sequence
		case "result":
			if index != len(lines)-1 || results != 0 {
				t.Fatalf("ndjson result record is not the single final record at %d", index)
			}
			if record.Result == nil || !record.Result.OK {
				t.Fatalf("ndjson result record failed: %+v", record.Result)
			}
			results++
		default:
			t.Fatalf("unexpected ndjson record type %q", record.Type)
		}
	}
	if results != 1 || last != lastSequence {
		t.Fatalf("ndjson results=%d last=%d want=%d", results, last, lastSequence)
	}
}

type beta8FollowOutcome struct {
	name  string
	op    contract.Operation
	code  int
	state contract.State
}

type beta8FollowMode struct {
	name string
	args []string
}

func TestIntegrationCLIRegressionFollowOutcomeExitCodes(t *testing.T) {
	pool := beta8TestPool(t)
	run := beta8TestRuntime(t, pool)
	beta8IsolateWorkspace(t)
	backend := &beta8CLIBackend{
		app:   api.NewAppService(run.Service, nil, nil),
		actor: beta8FixtureActor,
	}

	beta8StartWorkers(t, run)
	ctx := context.Background()
	failed := beta8SubmitFollowTest(t, ctx, run, "cli-outcome-failed", `{"steps":1,"fail_until_attempt":3}`)
	failed = beta8WaitFollowState(t, ctx, run, failed.ID, contract.Failed)
	cancelled := beta8SubmitFollowTest(t, ctx, run, "cli-outcome-cancelled", `{"steps":5,"step_delay_ms":150}`)
	cancelled = beta8WaitFollowState(t, ctx, run, cancelled.ID, contract.Running)
	if _, err := run.Service.RequestCancel(ctx, operations.OperatorAuthority(beta8FixtureActor), cancelled.ID); err != nil {
		t.Fatal(err)
	}
	cancelled = beta8WaitFollowState(t, ctx, run, cancelled.ID, contract.Canceled)
	outcomes := []beta8FollowOutcome{
		{"failed", failed, 6, contract.Failed},
		{"cancelled", cancelled, 130, contract.Canceled},
	}
	for _, outcome := range outcomes {
		modes := []beta8FollowMode{
			{"json", []string{"operations", "follow", string(outcome.op.ID), "--json"}},
			{"plain", []string{"operations", "follow", string(outcome.op.ID)}},
			{"ndjson", []string{"operations", "follow", string(outcome.op.ID), "--output", "ndjson"}},
		}
		for _, mode := range modes {
			beta8AssertFollowOutcomeMode(t, run, pool, backend, outcome, mode)
		}
	}
}

func beta8SubmitFollowTest(
	t *testing.T,
	ctx context.Context,
	run *controlplane.Runtime,
	key, input string,
) contract.Operation {
	t.Helper()
	op, err := run.Service.Submit(ctx, operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: beta8FixtureActor,
		IdempotencyKey: key, Input: []byte(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func beta8WaitFollowState(
	t *testing.T,
	ctx context.Context,
	run *controlplane.Runtime,
	id contract.OperationID,
	want contract.State,
) contract.Operation {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		current, err := run.Service.Get(ctx, operations.OperatorAuthority(beta8FixtureActor), id)
		if err != nil {
			t.Fatal(err)
		}
		if current.State == want {
			return current
		}
		if current.State.Terminal() {
			t.Fatalf("operation %s terminal %s, want %s", id, current.State, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s did not reach %s (last %s)", id, want, current.State)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func beta8AssertFollowOutcomeMode(
	t *testing.T,
	run *controlplane.Runtime,
	pool *pgxpool.Pool,
	backend *beta8CLIBackend,
	outcome beta8FollowOutcome,
	mode beta8FollowMode,
) {
	t.Helper()
	session := &beta8CLIBackend{app: backend.app, actor: backend.actor}
	result := beta8RunCLI(t, session, mode.args...)
	if result.Code != outcome.code {
		t.Fatalf(
			"%s %s exit=%d, want %d; stderr=%q stdout=%q",
			outcome.name,
			mode.name,
			result.Code,
			outcome.code,
			result.Stderr,
			result.Stdout,
		)
	}
	if session.sessions == 0 {
		t.Fatalf(
			"%s %s did not use the real follow session boundary",
			outcome.name,
			mode.name,
		)
	}
	if session.lastResult == nil || session.lastResult.State != outcome.state {
		t.Fatalf(
			"%s %s did not decode the terminal state: %+v",
			outcome.name,
			mode.name,
			session.lastResult,
		)
	}
	beta8AssertNoFollowMutation(t, run.Service, pool, outcome.op)
	switch mode.name {
	case "json":
		beta8AssertFollowJSONFailure(t, outcome, result.Stdout)
	case "plain":
		beta8AssertFollowPlainFailure(t, outcome, result.Stdout)
	case "ndjson":
		beta8AssertNDJSONFailure(t, result.Stdout, outcome.op.ID, outcome.state)
	}
}

func beta8AssertFollowJSONFailure(
	t *testing.T,
	outcome beta8FollowOutcome,
	stdout string,
) {
	t.Helper()
	envelope := beta8Envelope(t, stdout)
	if envelope.OK || envelope.Error == nil ||
		envelope.Error.Code == contract.ValidationFailed {
		t.Fatalf(
			"%s json follow returned the wrong failure class: %+v",
			outcome.name,
			envelope.Error,
		)
	}
	if _, hasID := envelope.Error.Details["operation_id"]; !hasID {
		t.Fatalf("%s json failure lacks operation identity: %+v", outcome.name, envelope.Error)
	}
}

func beta8AssertFollowPlainFailure(
	t *testing.T,
	outcome beta8FollowOutcome,
	stdout string,
) {
	t.Helper()
	text := strings.ToLower(stdout)
	if !strings.Contains(text, string(outcome.state)) ||
		!strings.Contains(text, string(outcome.op.ID)) {
		t.Fatalf("%s plain failure record incomplete: %q", outcome.name, stdout)
	}
}

func TestIntegrationCLIRegressionExplicitBuilder(t *testing.T) {
	pool := beta8TestPool(t)
	run := beta8TestRuntime(t, pool)
	beta8IsolateWorkspace(t)
	beta8Submit(t, run.Service, "builder-node", platform.KindNodeRegister, "platform", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication, platform.RoleBuilder},
		"capabilities": []string{platform.CapabilityDocker, platform.CapabilityBuildpacks},
	})
	beta8Submit(t, run.Service, "builder-app", platform.KindAppCreate, "platform", map[string]any{"name": "buildercli"})
	beta8Submit(t, run.Service, "builder-env", platform.KindEnvironmentCreate, "app/buildercli", map[string]any{
		"name": "incomplete", "node": "local", "container_port": 80,
		"domain": "builder-cli.example.com", "domain_primary": true,
		"source": map[string]any{
			"url": "https://github.com/acme/app.git",
			"ref": "1234567890abcdef1234567890abcdef12345678",
		},
	})
	backend := &beta8CLIBackend{app: api.NewAppService(run.Service, nil, nil), actor: beta8Operator}
	result := beta8RunCLI(t, backend,
		"deploy", "buildercli", "incomplete",
		"--ref", "1234567890abcdef1234567890abcdef12345678",
		"--json", "--idempotency-key", "cli-explicit-builder-missing")
	if result.Code != 5 {
		t.Fatalf("missing builder exit=%d stderr=%q stdout=%q", result.Code, result.Stderr, result.Stdout)
	}
	envelope := beta8Envelope(t, result.Stdout)
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.PreconditionFailed {
		t.Fatalf("missing builder error: %+v", envelope)
	}
	if !strings.Contains(strings.ToLower(envelope.Error.Message), "builder") ||
		(!strings.Contains(strings.ToLower(envelope.Error.Hint), "--builder") &&
			!strings.Contains(strings.ToLower(envelope.Error.Hint), "source.builder")) {
		t.Fatalf("missing builder CLI error is not actionable: %+v", envelope.Error)
	}
	var operations int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_operations WHERE idempotency_key=$1`, "cli-explicit-builder-missing").Scan(&operations); err != nil || operations != 0 {
		t.Fatalf("missing builder left %d operation(s), err=%v", operations, err)
	}
}
