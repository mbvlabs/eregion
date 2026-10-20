package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
)

type fakeRow struct {
	values []any
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("fake row column count mismatch")
	}
	for index, target := range dest {
		value := reflect.ValueOf(r.values[index])
		slot := reflect.ValueOf(target).Elem()
		if value.Type().AssignableTo(slot.Type()) {
			slot.Set(value)
			continue
		}
		return errors.New("fake row column type mismatch")
	}
	return nil
}

type fakeQueryer struct {
	row pgx.Row
}

func (f fakeQueryer) QueryRow(context.Context, string, ...any) pgx.Row { return f.row }

func TestTablePlacementUsesRecordedRolesAndAddress(t *testing.T) {
	placement := &tablePlacement{pool: fakeQueryer{row: fakeRow{values: []any{[]string{"telemetry"}, "10.77.0.10:22"}}}}
	got, err := placement.ResolveNode(context.Background(), "telemetry-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "telemetry-1" || got.PrivateAddress != "10.77.0.10" || got.IsMain || got.IsControlPlane {
		t.Fatalf("unexpected placement: %+v", got)
	}
	if got.HostID != hostIdentity("10.77.0.10:22") || len(got.HostID) < 16 {
		t.Fatalf("host identity was not derived from the recorded address: %q", got.HostID)
	}
	if err := (TargetIdentity{Node: got.Name, PrivateAddress: got.PrivateAddress, HostID: got.HostID}).Validate(); err != nil {
		t.Fatalf("derived identity is not usable: %v", err)
	}
}

func TestTablePlacementRefusesControlPlaneAndBadAddresses(t *testing.T) {
	cases := []struct {
		name string
		row  fakeRow
		code contract.Code
	}{
		{"telemetry on control plane", fakeRow{values: []any{[]string{"control-plane", "telemetry"}, "10.77.0.10:22"}}, contract.PreconditionFailed},
		{"hostname address", fakeRow{values: []any{[]string{"telemetry"}, "telemetry.example:22"}}, contract.PreconditionFailed},
		{"public address", fakeRow{values: []any{[]string{"telemetry"}, "203.0.113.5:22"}}, contract.PreconditionFailed},
		{"missing port", fakeRow{values: []any{[]string{"telemetry"}, "10.77.0.10"}}, contract.PreconditionFailed},
		{"missing node", fakeRow{err: pgx.ErrNoRows}, contract.NotFound},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			placement := &tablePlacement{pool: fakeQueryer{row: test.row}}
			_, err := placement.ResolveNode(context.Background(), "telemetry-1")
			if fault := contract.Classify(err); fault.Code != test.code {
				t.Fatalf("error = %+v, want %s", err, test.code)
			}
		})
	}
}

func TestTableStatusAbsentAndUnknownAreHonest(t *testing.T) {
	absent := newTableStatus(fakeQueryer{row: fakeRow{err: pgx.ErrNoRows}}, nil)
	status, err := absent.TelemetryStatus(context.Background())
	if err != nil || status.Configured || status.State != StatusAbsent || status.Retention != DefaultRetention() {
		t.Fatalf("absent status = %+v, %v", status, err)
	}

	configured := newTableStatus(fakeQueryer{row: fakeRow{values: []any{"telemetry-1", "10.77.0.10:22", []string{"telemetry"}}}}, nil)
	status, err = configured.TelemetryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Configured || status.State != StatusUnknown || status.Observed || status.PrivateAddress != "10.77.0.10" {
		t.Fatalf("configured status fabricated health: %+v", status)
	}
	if status.IngestEndpoint != "10.77.0.10:4317" || status.Endpoints["metrics"] != "10.77.0.10:8427" {
		t.Fatalf("endpoints are not derived from placement: %+v", status)
	}
}

func TestTransportDispatchRequiresAnOperationIdentity(t *testing.T) {
	identity := TargetIdentity{Node: "telemetry-1", PrivateAddress: "10.77.0.10", HostID: hostIdentity("10.77.0.10:22")}
	dispatch := &transportDispatch{}
	if _, err := dispatch.Open(context.Background(), identity); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("Open without a durable identity = %v", err)
	}
	if _, err := dispatch.OpenOperation(context.Background(), identity, contract.OperationID("op_"+strings.Repeat("a", 32)), 1, t.TempDir()); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("missing transport = %v", err)
	}
}

func TestTransportDispatchAppliesFileStepsThroughTheNodeProtocol(t *testing.T) {
	stateDir := t.TempDir()
	local, err := node.NewLocal(stateDir, "telemetry-1", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	identity := TargetIdentity{Node: "telemetry-1", PrivateAddress: "10.77.0.10", HostID: hostIdentity("10.77.0.10:22")}
	operation := contract.OperationID("op_" + strings.Repeat("a", 32))
	root := t.TempDir()
	configPath := filepath.Join(root, "stack", "collector", "config.yaml")
	run := func(data string) error {
		executor, err := (&transportDispatch{resolver: staticTransportResolver{transport: local}}).OpenOperation(context.Background(), identity, operation, 1, root)
		if err != nil {
			return err
		}
		if err := executor.MkdirAll(context.Background(), filepath.Join(root, "stack", "collector"), 0750); err != nil {
			return err
		}
		return executor.WriteFile(context.Background(), configPath, []byte(data), 0640)
	}
	if err := run("receivers: {}\n"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil || string(raw) != "receivers: {}\n" {
		t.Fatalf("file was not written through node dispatch: %q %v", raw, err)
	}
	// A retry under the same operation, attempt, and step index is convergent.
	if err := run("receivers: {}\n"); err != nil {
		t.Fatalf("idempotent retry failed: %v", err)
	}
	// Reusing the same effect under different content is an explicit precondition
	// failure, never a silent overwrite.
	if err := run("receivers: {changed: true}\n"); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("changed inputs = %v", err)
	}
}

type fakeResolver struct {
	transports map[string]node.Transport
	calls      []string
	err        error
}

func (r *fakeResolver) TransportFor(_ context.Context, nodeName string) (node.Transport, error) {
	r.calls = append(r.calls, nodeName)
	if r.err != nil {
		return nil, r.err
	}
	transport, ok := r.transports[nodeName]
	if !ok {
		return nil, errors.New("no transport for " + nodeName)
	}
	return transport, nil
}

func TestTransportDispatchResolvesTheTargetNode(t *testing.T) {
	stateDir := t.TempDir()
	telemetryTransport, err := node.NewLocal(stateDir, "telemetry-1", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	otherTransport, err := node.NewLocal(t.TempDir(), "app-1", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	resolver := &fakeResolver{transports: map[string]node.Transport{
		"telemetry-1": telemetryTransport, "app-1": otherTransport,
	}}
	identity := TargetIdentity{Node: "telemetry-1", PrivateAddress: "10.77.0.10", HostID: hostIdentity("10.77.0.10:22")}
	root := t.TempDir()
	executor, err := (&transportDispatch{resolver: resolver}).OpenOperation(context.Background(), identity, contract.OperationID("op_"+strings.Repeat("b", 32)), 1, root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "collector", "config.yaml")
	if err := executor.MkdirAll(context.Background(), filepath.Dir(path), 0750); err != nil {
		t.Fatal(err)
	}
	if err := executor.WriteFile(context.Background(), path, []byte("receivers: {}\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if len(resolver.calls) != 1 || resolver.calls[0] != "telemetry-1" {
		t.Fatalf("dispatch did not resolve the target node: %+v", resolver.calls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the resolved transport did not execute the step")
	}
}

func TestTransportDispatchRejectsAResolverForAnotherIdentity(t *testing.T) {
	transport, err := node.NewLocal(t.TempDir(), "app-1", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	resolver := &fakeResolver{transports: map[string]node.Transport{"telemetry-1": transport}}
	identity := TargetIdentity{Node: "telemetry-1", PrivateAddress: "10.77.0.10", HostID: hostIdentity("10.77.0.10:22")}
	_, err = (&transportDispatch{resolver: resolver}).OpenOperation(context.Background(), identity, contract.OperationID("op_"+strings.Repeat("c", 32)), 1, t.TempDir())
	if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("an unpinned resolver identity was accepted: %v", err)
	}
}

func TestConfigureProductionWithResolverRegistersDurableActions(t *testing.T) {
	registry := platform.NewRegistry()
	resolver := &fakeResolver{transports: map[string]node.Transport{"telemetry-1": &noopTransport{name: "telemetry-1"}}}
	if err := ConfigureProductionWithResolver(registry, fakePool(t), resolver); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{KindInstall, KindAgent, KindCredential} {
		def, ok := registry.Lookup(kind)
		if !ok || def.Executor == nil {
			t.Fatalf("action %s was not registered with a real executor", kind)
		}
	}
	if _, ok := registry.InventoryProvider(InventoryCollection); !ok {
		t.Fatal("telemetry inventory was not registered")
	}
}

func TestCredentialActionIssuesATokenWithoutLeakingIt(t *testing.T) {
	registry := platform.NewRegistry()
	if err := Configure(registry, RuntimeConfig{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ingest-tokens.json")
	target, _ := platform.NewNodeTarget("app-1")
	raw, _ := json.Marshal(map[string]string{"credentials_file": path})
	normalized, err := registry.Normalize(KindCredential, target.String(), raw)
	if err != nil {
		t.Fatal(err)
	}
	result, err := normalized.Executor().Execute(context.Background(), platform.Execution{
		OperationID: contract.OperationID("op_" + strings.Repeat("d", 32)), Attempt: 1,
		Kind: KindCredential, Target: target.String(), Actor: contract.Actor{Kind: "operator", Subject: "ops"},
		Input: normalized.Canonical,
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("credential file mode = %v, %v", info, err)
	}
	credentials, err := readOrEmptyCredentials(path)
	if err != nil || len(credentials) != 1 || credentials[0].Node != "app-1" {
		t.Fatalf("credential file content = %+v, %v", credentials, err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), credentials[0].Token) {
		t.Fatal("action result leaked the ingest token")
	}
	if !strings.Contains(string(encoded), "token_sha256") {
		t.Fatal("action result did not report the token digest")
	}

	// A second node is added, preserving the first entry.
	build, _ := platform.NewNodeTarget("build-1")
	raw, _ = json.Marshal(map[string]string{"credentials_file": path})
	normalized, err = registry.Normalize(KindCredential, build.String(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := normalized.Executor().Execute(context.Background(), platform.Execution{
		OperationID: contract.OperationID("op_" + strings.Repeat("e", 32)), Attempt: 1,
		Kind: KindCredential, Target: build.String(), Actor: contract.Actor{Kind: "operator", Subject: "ops"},
		Input: normalized.Canonical,
	}); err != nil {
		t.Fatal(err)
	}
	credentials, err = readOrEmptyCredentials(path)
	if err != nil || len(credentials) != 2 {
		t.Fatalf("credentials after a second issue = %+v, %v", credentials, err)
	}
}

func TestCredentialActionRejectsTokenInput(t *testing.T) {
	registry := platform.NewRegistry()
	if err := Configure(registry, RuntimeConfig{}); err != nil {
		t.Fatal(err)
	}
	target, _ := platform.NewNodeTarget("app-1")
	raw := json.RawMessage(`{"credentials_file":"/etc/eregion/ingest.json","token":"plaintext"}`)
	if _, err := registry.Normalize(KindCredential, target.String(), raw); err == nil {
		t.Fatal("credential action accepted a token in its input")
	}
}

func TestConfigureProductionRegistersDurableActions(t *testing.T) {
	registry := platform.NewRegistry()
	pool := fakePool(t)
	transport := &noopTransport{name: "telemetry-1"}
	if err := ConfigureProduction(registry, pool, transport); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{KindInstall, KindAgent} {
		def, ok := registry.Lookup(kind)
		if !ok || def.Executor == nil {
			t.Fatalf("action %s was not registered with a real executor", kind)
		}
	}
	if _, ok := registry.InventoryProvider(InventoryCollection); !ok {
		t.Fatal("telemetry inventory was not registered")
	}
}

// fakePool is only here to satisfy *pgxpool.Pool's shape at the Configure call;
// the production adapters are exercised with NodeQueryer directly.
func fakePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return &pgxpool.Pool{}
}

type noopTransport struct{ name string }

func (n *noopTransport) Name() string         { return "noop" }
func (n *noopTransport) NodeIdentity() string { return n.name }
func (n *noopTransport) Call(context.Context, node.Request, func(node.Event)) (node.Response, error) {
	return node.Response{}, errors.New("noop transport")
}
