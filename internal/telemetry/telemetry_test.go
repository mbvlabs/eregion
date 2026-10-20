package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func validConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Node: Node{Name: "telemetry-1", Role: RoleTelemetry, PrivateAddress: "10.77.0.10"},
		Credentials: []IngestCredential{
			{Node: "app-1", Token: strings.Repeat("a", 43)},
			{Node: "build-1", Token: strings.Repeat("b", 43)},
		},
		GrafanaAdminPassword: "correct-horse-battery-staple",
		RootDir:              t.TempDir(),
		EnableTraces:         true,
	}
}

func TestConfigRejectsUnsafePlacementAndBounds(t *testing.T) {
	base := validConfig(t)
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"not telemetry role", func(c *Config) { c.Node.Role = RoleApplication }},
		{"main node", func(c *Config) { c.Node.IsMain = true }},
		{"control plane", func(c *Config) { c.Node.IsControlPlane = true }},
		{"public bind", func(c *Config) { c.Node.PrivateAddress = "203.0.113.5" }},
		{"loopback bind", func(c *Config) { c.Node.PrivateAddress = "127.0.0.1" }},
		{"unspecified bind", func(c *Config) { c.Node.PrivateAddress = "0.0.0.0" }},
		{"metrics retention", func(c *Config) { c.Retention.MetricsDays = 91 }},
		{"logs retention", func(c *Config) { c.Retention.LogsDays = 91 }},
		{"traces retention", func(c *Config) { c.Retention.TracesDays = 31 }},
		{"no credentials", func(c *Config) { c.Credentials = nil }},
		{"duplicate node", func(c *Config) { c.Credentials[1].Node = c.Credentials[0].Node }},
		{"duplicate token", func(c *Config) { c.Credentials[1].Token = c.Credentials[0].Token }},
		{"short token", func(c *Config) { c.Credentials[0].Token = "too-short" }},
		{"token whitespace", func(c *Config) { c.Credentials[0].Token = strings.Repeat("a ", 20) }},
		{"self credential", func(c *Config) { c.Credentials[0].Node = c.Node.Name }},
		{"moving image tag", func(c *Config) { c.Versions.Grafana = "latest" }},
		{"empty password", func(c *Config) { c.GrafanaAdminPassword = "" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.Credentials = append([]IngestCredential(nil), base.Credentials...)
			test.mutate(&cfg)
			if _, err := Prepare(cfg); err == nil {
				t.Fatal("unsafe telemetry config accepted")
			}
		})
	}
	if _, err := Prepare(base); err != nil {
		t.Fatal(err)
	}
}

func TestRenderPlanKeepsCredentialsOutOfCommandsAndConfig(t *testing.T) {
	cfg := validConfig(t)
	prepared, err := Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := RenderPlan(prepared)
	if err != nil {
		t.Fatal(err)
	}
	secret := cfg.Credentials[0].Token
	foundSecretFile := false
	for _, file := range plan.Files {
		if strings.Contains(string(file.Data), secret) {
			if !file.Secret || file.Mode != 0600 || !strings.HasSuffix(file.Path, "ingest-tokens") {
				t.Fatalf("credential leaked into non-secret file %s", file.Path)
			}
			foundSecretFile = true
		}
		if file.Secret && file.Mode != 0600 {
			t.Fatalf("secret file %s mode %o is not 0600", file.Path, file.Mode)
		}
		if file.Mode&0002 != 0 {
			t.Fatalf("file %s mode %o is world-writable", file.Path, file.Mode)
		}
	}
	if !foundSecretFile {
		t.Fatal("credential token file was not rendered")
	}
	for _, command := range plan.Commands {
		joined := command.Name + " " + strings.Join(command.Args, " ")
		if strings.Contains(joined, secret) || strings.Contains(joined, cfg.GrafanaAdminPassword) {
			t.Fatal("credential appeared on a command line")
		}
	}
	var compose, collector, tokens, datasources string
	for _, file := range plan.Files {
		switch {
		case strings.HasSuffix(file.Path, "compose.yaml"):
			compose = string(file.Data)
		case strings.HasSuffix(file.Path, "collector/config.yaml"):
			collector = string(file.Data)
		case strings.HasSuffix(file.Path, "ingest-tokens"):
			tokens = string(file.Data)
		case strings.HasSuffix(file.Path, "datasources.yaml"):
			datasources = string(file.Data)
		}
	}
	if !strings.Contains(compose, `"10.77.0.10:4317:4317"`) || !strings.Contains(compose, `"10.77.0.10:8427:8427"`) || !strings.Contains(compose, `"10.77.0.10:9427:9427"`) {
		t.Fatal("telemetry ports are not bound to the private address")
	}
	if strings.Contains(compose, "0.0.0.0:") {
		t.Fatal("compose published a wildcard host port")
	}
	if strings.Contains(compose, "8428:8428") || strings.Contains(compose, "9428:9428") {
		t.Fatal("VictoriaMetrics or VictoriaLogs read port was published directly")
	}
	if !strings.Contains(compose, "vmauth") || !strings.Contains(compose, "victoriametrics/vmauth:") {
		t.Fatal("read-only gateways were not rendered")
	}
	if !strings.Contains(collector, "cert_file: /etc/otelcol/tls/server.crt") || !strings.Contains(collector, "key_file: /etc/otelcol/tls/server.key") {
		t.Fatal("OTLP receiver is missing verified TLS")
	}
	if !strings.Contains(datasources, "httpHeaderName1: Authorization") || !strings.Contains(datasources, "Bearer ") {
		t.Fatal("Grafana datasource is not authenticated to the read gateway")
	}
	if !strings.Contains(compose, "grafana/grafana:11.5.1") || strings.Contains(compose, "grafana-oss") {
		t.Fatal("Grafana is not the pinned unmodified upstream image")
	}
	if !strings.Contains(collector, "bearertokenauth/ingest") || !strings.Contains(collector, "authenticator: bearertokenauth/ingest") {
		t.Fatal("OTLP ingress is not credential-gated")
	}
	if !strings.Contains(tokens, "node=app-1") || !strings.Contains(tokens, "node=build-1") {
		t.Fatal("per-node token file is incomplete")
	}
	if !strings.Contains(compose, "-retentionPeriod=15d") || !strings.Contains(compose, "-retentionPeriod=14d") {
		t.Fatal("bounded retention flags are missing")
	}
	if !strings.Contains(collector, "traces:") {
		t.Fatal("traces pipeline missing when traces are enabled")
	}
}

func TestAgentPlanAndRoleRestriction(t *testing.T) {
	material, err := loadOrCreateTLS(t.TempDir(), "10.77.0.10")
	if err != nil {
		t.Fatal(err)
	}
	cfg := AgentConfig{
		Node:             Node{Name: "app-1", Role: RoleApplication, PrivateAddress: "10.77.0.11"},
		TelemetryAddress: "10.77.0.10:4317", Token: strings.Repeat("c", 43), TLSCAPEM: material.CAPEM,
		RootDir: t.TempDir(), LogPaths: []string{"/var/log/containers/*.log"},
	}
	prepared, err := PrepareAgent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := RenderAgentPlan(prepared)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range plan.Commands {
		if strings.Contains(strings.Join(command.Args, " "), cfg.Token) {
			t.Fatal("agent token appeared on a command line")
		}
	}
	var compose, collector string
	for _, file := range plan.Files {
		if strings.Contains(string(file.Data), cfg.Token) && !file.Secret {
			t.Fatal("agent token leaked into a config file")
		}
		if strings.HasSuffix(file.Path, "compose.yaml") {
			compose = string(file.Data)
		}
		if strings.HasSuffix(file.Path, "collector/config.yaml") {
			collector = string(file.Data)
		}
	}
	if !strings.Contains(compose, `"10.77.0.11:4317:4317"`) || !strings.Contains(collector, "hostmetrics") || !strings.Contains(collector, "/var/log/containers/*.log") {
		t.Fatal("agent is missing private bind, host metrics, or container logs")
	}
	if !strings.Contains(collector, "ca_file: /etc/otelcol/tls/ca.crt") || strings.Contains(collector, "insecure: true") {
		t.Fatal("agent exporter does not verify telemetry TLS")
	}
	foundCA := false
	for _, file := range plan.Files {
		if strings.HasSuffix(file.Path, filepath.Join("tls", "ca.crt")) && string(file.Data) == string(material.CAPEM) && file.Mode == 0644 {
			foundCA = true
		}
	}
	if !foundCA {
		t.Fatal("agent TLS CA was not rendered")
	}
	for _, role := range []string{RoleTelemetry, "edge"} {
		bad := cfg
		bad.Node.Role = role
		if _, err := PrepareAgent(bad); err == nil {
			t.Fatalf("agent accepted role %s", role)
		}
	}
}

type fakeRunner struct {
	commands []string
	fail     error
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	r.commands = append(r.commands, name+" "+strings.Join(args, " "))
	return r.fail
}

func TestInstallerWritesPrivateFilesAndRunsArgvOnly(t *testing.T) {
	cfg := validConfig(t)
	prepared, err := Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	installer := NewInstaller(prepared, runner)
	result, err := installer.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) == 0 || len(runner.commands) != 2 || !strings.HasPrefix(runner.commands[0], "docker compose ") {
		t.Fatalf("unexpected apply result: %+v / %+v", result, runner.commands)
	}
	for _, file := range result.Files {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0002 != 0 {
			t.Fatalf("file %s mode %o is world-writable", file, info.Mode().Perm())
		}
	}
	tokenFile := filepath.Join(cfg.RootDir, "stack", "collector", "ingest-tokens")
	raw, err := os.ReadFile(tokenFile)
	if err != nil || !strings.Contains(string(raw), cfg.Credentials[0].Token) {
		t.Fatal("token file missing")
	}
	if got, err := os.Stat(tokenFile); err != nil || got.Mode().Perm() != 0600 {
		t.Fatalf("token mode = %v, %v", got, err)
	}
	// Re-apply is convergent and rewrites the same files.
	before := len(runner.commands)
	if _, err := installer.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != before+2 {
		t.Fatal("re-apply did not run the same convergence commands")
	}
}

func TestInstallerFailureIsHonest(t *testing.T) {
	cfg := validConfig(t)
	prepared, _ := Prepare(cfg)
	runner := &fakeRunner{fail: contract.NewFault(contract.Unavailable, "docker missing", "")}
	installer := NewInstaller(prepared, runner)
	if _, err := installer.Apply(context.Background()); err == nil {
		t.Fatal("installer reported success after runner failure")
	} else if fault := contract.Classify(err); fault.Code != contract.Unavailable {
		t.Fatalf("unexpected failure code %s", fault.Code)
	}
}

func TestUnconfiguredDataSourceIsAbsentNotZero(t *testing.T) {
	source := Unconfigured{}
	window := Window{Start: time.Now().Add(-time.Hour), End: time.Now()}
	logs, err := source.Logs(context.Background(), LogQuery{Window: window, Source: "*"})
	if err != nil || logs.State != StateAbsent || len(logs.Entries) != 0 {
		t.Fatalf("unconfigured logs = %+v, %v", logs, err)
	}
	metrics, err := source.Metrics(context.Background(), MetricQuery{Window: window, Source: "app", Expression: "up", Unit: "count"})
	if err != nil || metrics.State != StateAbsent || len(metrics.Series) != 0 {
		t.Fatalf("unconfigured metrics = %+v, %v", metrics, err)
	}
}

func TestPreparedEndpointsStayPrivate(t *testing.T) {
	cfg := validConfig(t)
	prepared, err := Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.IngestEndpoint() != "10.77.0.10:4317" || prepared.HTTPIngestEndpoint() != "10.77.0.10:4318" {
		t.Fatal("unexpected OTLP endpoints")
	}
	for name, endpoint := range prepared.DataSourceEndpoints() {
		if !strings.HasPrefix(endpoint, "10.77.0.10:") {
			t.Fatalf("%s endpoint is not private: %s", name, endpoint)
		}
	}
}

func TestPlacementValidationAndRuntimeStatus(t *testing.T) {
	hostID := "SHA256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := validateTelemetryPlacement("telemetry-1", Placement{Name: "telemetry-1", Roles: []string{RoleTelemetry}, PrivateAddress: "10.77.0.10", HostID: hostID}, ""); err != nil {
		t.Fatal(err)
	}
	bad := []Placement{
		{Name: "telemetry-1", Roles: []string{RoleApplication}, PrivateAddress: "10.77.0.10", HostID: hostID},
		{Name: "telemetry-1", Roles: []string{RoleTelemetry}, PrivateAddress: "10.77.0.10", HostID: hostID, IsMain: true},
		{Name: "telemetry-1", Roles: []string{RoleTelemetry}, PrivateAddress: "203.0.113.2", HostID: hostID},
		{Name: "telemetry-1", Roles: []string{RoleTelemetry}, PrivateAddress: "10.77.0.10"},
	}
	for _, placement := range bad {
		if err := validateTelemetryPlacement("telemetry-1", placement, ""); err == nil {
			t.Fatalf("unsafe placement accepted: %+v", placement)
		}
	}
}

func TestActionNormalizersRejectSecretsAndBadTargets(t *testing.T) {
	registry := platform.NewRegistry()
	if err := Configure(registry, RuntimeConfig{}); err != nil {
		t.Fatal(err)
	}
	secret := "token-secret-value"
	raw, _ := json.Marshal(map[string]any{
		"credentials_file":      "/etc/eregion/credentials.json",
		"grafana_password_file": "/etc/eregion/grafana",
		"token":                 secret,
	})
	target, _ := platform.NewNodeTarget("telemetry-1")
	if _, err := registry.Normalize(KindInstall, target.String(), raw); err == nil {
		t.Fatal("secret-bearing action input accepted")
	}
	raw, _ = json.Marshal(map[string]any{
		"credentials_file":      "/etc/eregion/credentials.json",
		"grafana_password_file": "/etc/eregion/grafana",
	})
	if _, err := registry.Normalize(KindInstall, "platform", raw); err == nil {
		t.Fatal("non-node telemetry target accepted")
	}
	if _, err := registry.Normalize(KindAgent, target.String(), json.RawMessage(`{"telemetry_address":"10.77.0.10:4317","token_file":"/etc/eregion/token","tls_ca_file":"/etc/eregion/ca.crt"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryActionsExecuteThroughPinnedDispatch(t *testing.T) {
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, "credentials.json")
	passwordPath := filepath.Join(dir, "grafana-password")
	credentials := `[{"node":"app-1","token":"` + strings.Repeat("d", 43) + `"}]`
	if err := os.WriteFile(credentialPath, []byte(credentials), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwordPath, []byte("password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	hostID := "SHA256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	registry := platform.NewRegistry()
	runner := &fakeRunner{}
	identity := TargetIdentity{Node: "telemetry-1", PrivateAddress: "10.77.0.10", HostID: hostID}
	executor := &fakeNodeExecutor{runner: runner, identity: identity}
	dispatch := &fakeDispatch{executor: executor}
	placement := fakePlacement{placement: Placement{Name: "telemetry-1", Roles: []string{RoleTelemetry}, PrivateAddress: "10.77.0.10", HostID: hostID}}
	if err := Configure(registry, RuntimeConfig{Placement: placement, Dispatch: dispatch}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"credentials_file": credentialPath, "grafana_password_file": passwordPath,
		"root_dir": filepath.Join(dir, "telemetry"), "enable_traces": true,
	})
	target, _ := platform.NewNodeTarget("telemetry-1")
	normalized, err := registry.Normalize(KindInstall, target.String(), raw)
	if err != nil {
		t.Fatal(err)
	}
	var stages []string
	result, err := normalized.Executor().Execute(context.Background(), platform.Execution{
		OperationID: "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Attempt: 1,
		Kind: KindInstall, Target: target.String(), Actor: contract.Actor{Kind: "operator", Subject: "ops"},
		Input: normalized.Canonical, Report: func(event platform.Event) { stages = append(stages, event.Stage) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatch.opened) != 1 || !dispatch.opened[0].Equal(identity) {
		t.Fatalf("dispatch was not bound to the pinned target: %+v", dispatch.opened)
	}
	if executor.identity != identity {
		t.Fatal("executor identity was not the authoritative target")
	}
	if len(runner.commands) != 2 || len(stages) == 0 {
		t.Fatalf("install did not run: %+v %+v", runner.commands, stages)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), strings.Repeat("d", 43)) || strings.Contains(string(encoded), "password") {
		t.Fatal("action result leaked a credential")
	}
	if _, err := os.Stat(filepath.Join(dir, "telemetry", "stack", "compose.yaml")); err != nil {
		t.Fatal("stack was not written")
	}
	if _, err := os.Stat(filepath.Join(dir, "telemetry", "tls", "ca.crt")); err != nil {
		t.Fatal("TLS CA was not written")
	}
	// The credential crosses the dispatch boundary only into the 0600 token
	// file; it never appears in a rendered config, a command, or a result.
	token := strings.Repeat("d", 43)
	for path, data := range executor.written {
		if strings.Contains(data, token) && !strings.HasSuffix(path, "ingest-tokens") {
			t.Fatalf("token crossed into %s", path)
		}
	}
}

func TestTelemetryActionRefusesLocalFallbackAndWrongDispatch(t *testing.T) {
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, "credentials.json")
	passwordPath := filepath.Join(dir, "grafana-password")
	if err := os.WriteFile(credentialPath, []byte(`[{"node":"app-1","token":"`+strings.Repeat("e", 43)+`"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwordPath, []byte("password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	hostID := "SHA256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	placement := fakePlacement{placement: Placement{Name: "telemetry-1", Roles: []string{RoleTelemetry}, PrivateAddress: "10.77.0.10", HostID: hostID}}
	raw, _ := json.Marshal(map[string]any{
		"credentials_file": credentialPath, "grafana_password_file": passwordPath,
		"root_dir": filepath.Join(dir, "telemetry"),
	})
	target, _ := platform.NewNodeTarget("telemetry-1")
	execution := platform.Execution{
		OperationID: "op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Attempt: 1,
		Kind: KindInstall, Target: target.String(), Actor: contract.Actor{Kind: "operator", Subject: "ops"},
	}
	registry := platform.NewRegistry()
	if err := Configure(registry, RuntimeConfig{Placement: placement}); err != nil {
		t.Fatal(err)
	}
	normalized, err := registry.Normalize(KindInstall, target.String(), raw)
	if err != nil {
		t.Fatal(err)
	}
	execution.Input = normalized.Canonical
	if _, err := normalized.Executor().Execute(context.Background(), execution); err == nil {
		t.Fatal("install succeeded without a dispatch")
	}
	if _, err := os.Stat(filepath.Join(dir, "telemetry")); !os.IsNotExist(err) {
		t.Fatal("a local fallback wrote the stack")
	}

	// A dispatch that returns an executor for another host must be rejected.
	wrong := &fakeNodeExecutor{runner: &fakeRunner{}, identity: TargetIdentity{Node: "telemetry-1", PrivateAddress: "10.77.0.10", HostID: "SHA256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}}
	registry = platform.NewRegistry()
	if err := Configure(registry, RuntimeConfig{Placement: placement, Dispatch: &fakeDispatch{executor: wrong}}); err != nil {
		t.Fatal(err)
	}
	normalized, err = registry.Normalize(KindInstall, target.String(), raw)
	if err != nil {
		t.Fatal(err)
	}
	execution.Input = normalized.Canonical
	if _, err := normalized.Executor().Execute(context.Background(), execution); err == nil {
		t.Fatal("install accepted an executor for a different host identity")
	}
}

type fakeNodeExecutor struct {
	runner   *fakeRunner
	identity TargetIdentity
	written  map[string]string
}

func (e *fakeNodeExecutor) Run(ctx context.Context, name string, args ...string) error {
	return e.runner.Run(ctx, name, args...)
}

func (e *fakeNodeExecutor) MkdirAll(ctx context.Context, path string, mode os.FileMode) error {
	return LocalFilesystem{}.MkdirAll(ctx, path, mode)
}

func (e *fakeNodeExecutor) WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode) error {
	if e.written == nil {
		e.written = map[string]string{}
	}
	e.written[path] = string(data)
	return LocalFilesystem{}.WriteFile(ctx, path, data, mode)
}

func (e *fakeNodeExecutor) Identity() TargetIdentity { return e.identity }

type fakeDispatch struct {
	executor *fakeNodeExecutor
	opened   []TargetIdentity
}

func (d *fakeDispatch) Open(_ context.Context, identity TargetIdentity) (NodeExecutor, error) {
	d.opened = append(d.opened, identity)
	return d.executor, nil
}

type fakePlacement struct {
	placement Placement
	err       error
}

func (f fakePlacement) ResolveNode(_ context.Context, name string) (Placement, error) {
	if f.err != nil {
		return Placement{}, f.err
	}
	if name != f.placement.Name {
		return Placement{}, contract.NewFault(contract.NotFound, "node not found", "")
	}
	return f.placement, nil
}
