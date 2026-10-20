package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func externalOnlyAgentConfig(t *testing.T) AgentConfig {
	t.Helper()
	return AgentConfig{
		Node:            Node{Name: "app-1", Role: RoleApplication, PrivateAddress: "10.77.0.11"},
		ExternalOnly:    true,
		ExternalExports: []ExternalExport{{Endpoint: "https://otlp.example.com:4318"}},
		RootDir:         t.TempDir(),
		LogPaths:        []string{"/var/log/containers/*.log"},
	}
}

func renderedAgent(t *testing.T, cfg AgentConfig) (PreparedAgent, Plan, string, string, map[string]PlanFile) {
	t.Helper()
	prepared, err := PrepareAgent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := RenderAgentPlan(prepared)
	if err != nil {
		t.Fatal(err)
	}
	var compose, collector string
	files := map[string]PlanFile{}
	for _, file := range plan.Files {
		files[file.Path] = file
		switch {
		case strings.HasSuffix(file.Path, "compose.yaml"):
			compose = string(file.Data)
		case strings.HasSuffix(file.Path, "collector/config.yaml"):
			collector = string(file.Data)
		}
	}
	return prepared, plan, compose, collector, files
}

func TestExternalOnlyAgentRendersNoLocalTelemetryMaterial(t *testing.T) {
	cfg := externalOnlyAgentConfig(t)
	cfg.ExternalExports = []ExternalExport{
		{
			Endpoint: "https://otlp.example.com:4318/otlp", TokenFile: "/etc/eregion/telemetry/external-token",
			TLSCAFile: "/etc/eregion/telemetry/external-ca.crt",
		},
		{Endpoint: "https://otlp.example.com:4317", Protocol: "otlp_grpc"},
	}
	prepared, plan, compose, collector, files := renderedAgent(t, cfg)
	if !prepared.ExternalOnly || prepared.TelemetryAddress != "" || prepared.Token != "" || len(prepared.TLSCAPEM) != 0 {
		t.Fatalf("external-only config retained local telemetry material: %+v", prepared.AgentConfig)
	}
	for _, unexpected := range []string{"ingest-token", "./tls", "/etc/otelcol/tls"} {
		if strings.Contains(compose, unexpected) {
			t.Fatalf("external-only compose still references %q:\n%s", unexpected, compose)
		}
	}
	for _, unexpected := range []string{"otlp/telemetry", "bearertokenauth/telemetry", "ingest-token"} {
		if strings.Contains(collector, unexpected) {
			t.Fatalf("external-only collector still contains %q:\n%s", unexpected, collector)
		}
	}
	for _, want := range []string{
		"otlphttp/external-0:", "endpoint: https://otlp.example.com:4318/otlp",
		"otlp/external-1:", "endpoint: https://otlp.example.com:4317",
		"filename: /etc/otelcol/external-0-token",
		"ca_file: /etc/otelcol/external-0-ca.crt",
		"exporters: [otlphttp/external-0, otlp/external-1]",
		"hostmetrics", "filelog", "receivers: [otlp, hostmetrics]",
	} {
		if !strings.Contains(collector, want) {
			t.Fatalf("external-only collector missing %q:\n%s", want, collector)
		}
	}
	for _, want := range []string{
		`"/etc/eregion/telemetry/external-token:/etc/otelcol/external-0-token:ro"`,
		`"/etc/eregion/telemetry/external-ca.crt:/etc/otelcol/external-0-ca.crt:ro"`,
	} {
		if !strings.Contains(compose, want) {
			t.Fatalf("external-only compose missing %q:\n%s", want, compose)
		}
	}
	if _, ok := files[filepath.Join(cfg.RootDir, "stack", "collector", "ingest-token")]; ok {
		t.Fatal("external-only plan rendered a local ingest token file")
	}
	if _, ok := files[filepath.Join(cfg.RootDir, "stack", "tls", "ca.crt")]; ok {
		t.Fatal("external-only plan rendered a local TLS CA file")
	}
	for _, file := range plan.Files {
		if file.Secret && file.Mode != 0600 {
			t.Fatalf("secret file %s mode %o is not 0600", file.Path, file.Mode)
		}
		for _, command := range plan.Commands {
			if strings.Contains(strings.Join(command.Args, " "), "/etc/eregion/telemetry/external-token") {
				t.Fatal("external token path leaked onto a command line")
			}
		}
	}
}

func TestExternalOnlyAgentFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*AgentConfig)
	}{
		{"missing export", func(c *AgentConfig) { c.ExternalExports = nil }},
		{"telemetry address", func(c *AgentConfig) { c.TelemetryAddress = "10.77.0.10:4317" }},
		{"local token", func(c *AgentConfig) { c.Token = strings.Repeat("a", 43) }},
		{"local CA", func(c *AgentConfig) { c.TLSCAPEM = []byte("ca") }},
		{"duplicate export", func(c *AgentConfig) {
			c.ExternalExports = append(c.ExternalExports, c.ExternalExports[0])
		}},
		{"too many exports", func(c *AgentConfig) {
			c.ExternalExports = nil
			for index := 0; index <= MaxExternalExports; index++ {
				c.ExternalExports = append(c.ExternalExports, ExternalExport{Endpoint: "https://otlp.example.com:4318"})
			}
		}},
		{"public plain HTTP", func(c *AgentConfig) {
			c.ExternalExports = []ExternalExport{{Endpoint: "http://203.0.113.5:4318"}}
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := externalOnlyAgentConfig(t)
			test.mutate(&cfg)
			if _, err := PrepareAgent(cfg); err == nil {
				t.Fatal("unsafe external-only agent config was accepted")
			}
		})
	}
}

func TestAdditiveAgentKeepsLocalTelemetryAndAddsExports(t *testing.T) {
	material, err := loadOrCreateTLS(t.TempDir(), "10.77.0.10")
	if err != nil {
		t.Fatal(err)
	}
	cfg := AgentConfig{
		Node:             Node{Name: "app-1", Role: RoleApplication, PrivateAddress: "10.77.0.11"},
		TelemetryAddress: "10.77.0.10:4317", Token: strings.Repeat("c", 43), TLSCAPEM: material.CAPEM,
		ExternalExports: []ExternalExport{{
			Endpoint: "https://otlp.example.com:4318", TokenFile: "/etc/eregion/telemetry/external-token",
			TLSCAFile: "/etc/eregion/telemetry/external-ca.crt",
		}},
		RootDir: t.TempDir(), LogPaths: []string{"/var/log/containers/*.log"},
	}
	prepared, _, compose, collector, files := renderedAgent(t, cfg)
	if prepared.ExternalOnly || prepared.TelemetryAddress != "10.77.0.10:4317" || prepared.Token != cfg.Token {
		t.Fatalf("additive agent lost local telemetry configuration: %+v", prepared.AgentConfig)
	}
	for _, want := range []string{"otlp/telemetry:", "bearertokenauth/telemetry", "exporters: [otlp/telemetry, otlphttp/external-0]"} {
		if !strings.Contains(collector, want) {
			t.Fatalf("additive collector missing %q:\n%s", want, collector)
		}
	}
	for _, want := range []string{"ingest-token:/etc/otelcol/ingest-token:ro", "./tls:/etc/otelcol/tls:ro", "external-token:/etc/otelcol/external-0-token:ro"} {
		if !strings.Contains(compose, want) {
			t.Fatalf("additive compose missing %q:\n%s", want, compose)
		}
	}
	if _, ok := files[filepath.Join(cfg.RootDir, "stack", "collector", "ingest-token")]; !ok {
		t.Fatal("additive plan did not render the local ingest token file")
	}
	if _, ok := files[filepath.Join(cfg.RootDir, "stack", "tls", "ca.crt")]; !ok {
		t.Fatal("additive plan did not render the local TLS CA file")
	}
}

type agentActionFixture struct {
	registry *platform.Registry
	executor *fakeNodeExecutor
	dispatch *fakeDispatch
	identity TargetIdentity
	target   string
}

func newAgentActionFixture(t *testing.T) agentActionFixture {
	t.Helper()
	hostID := "SHA256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	placement := fakePlacement{placement: Placement{
		Name: "app-1", Roles: []string{RoleApplication}, PrivateAddress: "10.77.0.11", HostID: hostID,
	}}
	identity := TargetIdentity{Node: "app-1", PrivateAddress: "10.77.0.11", HostID: hostID}
	executor := &fakeNodeExecutor{runner: &fakeRunner{}, identity: identity}
	dispatch := &fakeDispatch{executor: executor}
	registry := platform.NewRegistry()
	if err := Configure(registry, RuntimeConfig{Placement: placement, Dispatch: dispatch}); err != nil {
		t.Fatal(err)
	}
	target, _ := platform.NewNodeTarget("app-1")
	return agentActionFixture{registry: registry, executor: executor, dispatch: dispatch, identity: identity, target: target.String()}
}

func (f agentActionFixture) execute(t *testing.T, raw map[string]any) (platform.Result, error) {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := f.registry.Normalize(KindAgent, f.target, encoded)
	if err != nil {
		return platform.Result{}, err
	}
	return normalized.Executor().Execute(context.Background(), platform.Execution{
		OperationID: contract.OperationID("op_cccccccccccccccccccccccccccccccc"), Attempt: 1,
		Kind: KindAgent, Target: f.target, Actor: contract.Actor{Kind: "operator", Subject: "ops"},
		Input: normalized.Canonical,
	})
}

func TestExternalOnlyAgentActionWritesNoLocalCredential(t *testing.T) {
	fixture := newAgentActionFixture(t)
	root := t.TempDir()
	result, err := fixture.execute(t, map[string]any{
		"external_only": true,
		"external_exports": []map[string]any{{
			"endpoint": "https://otlp.example.com:4318", "token_file": "/etc/eregion/telemetry/external-token",
			"tls_ca_file": "/etc/eregion/telemetry/external-ca.crt",
		}},
		"root_dir":  root,
		"log_paths": []string{"/var/log/containers/*.log"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.dispatch.opened) != 1 || !fixture.dispatch.opened[0].Equal(fixture.identity) {
		t.Fatal("external-only agent was not dispatched to the pinned target")
	}
	compose := fixture.executor.written[filepath.Join(root, "stack", "compose.yaml")]
	for _, unexpected := range []string{"ingest-token", "./tls"} {
		if strings.Contains(compose, unexpected) {
			t.Fatalf("external-only compose mounted local material %q:\n%s", unexpected, compose)
		}
	}
	if _, ok := fixture.executor.written[filepath.Join(root, "stack", "collector", "ingest-token")]; ok {
		t.Fatal("external-only action wrote a local ingest token")
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "telemetry_endpoint") || strings.Contains(string(encoded), "ingest-token") {
		t.Fatalf("external-only result claims a local telemetry endpoint: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"external_only":true`) || !strings.Contains(string(encoded), "otlp.example.com:4318") {
		t.Fatalf("external-only result does not report the external export: %s", encoded)
	}
}

func TestAdditiveAgentActionKeepsLocalEndpointAndExternalReference(t *testing.T) {
	fixture := newAgentActionFixture(t)
	material, err := loadOrCreateTLS(t.TempDir(), "10.77.0.10")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	tokenPath := filepath.Join(root, "ingest-token")
	caPath := filepath.Join(root, "ca.crt")
	token := strings.Repeat("f", 43)
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, material.CAPEM, 0644); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.execute(t, map[string]any{
		"telemetry_address": "10.77.0.10:4317", "token_file": tokenPath, "tls_ca_file": caPath,
		"external_exports": []map[string]any{{"endpoint": "https://otlp.example.com:4318", "token_file": "/etc/eregion/telemetry/external-token"}},
		"root_dir":         root,
	})
	if err != nil {
		t.Fatal(err)
	}
	compose := fixture.executor.written[filepath.Join(root, "stack", "compose.yaml")]
	for _, want := range []string{"ingest-token:/etc/otelcol/ingest-token:ro", "external-token:/etc/otelcol/external-0-token:ro"} {
		if !strings.Contains(compose, want) {
			t.Fatalf("additive compose missing %q:\n%s", want, compose)
		}
	}
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), `"telemetry_endpoint":"10.77.0.10:4317"`) {
		t.Fatalf("additive result does not report the local endpoint: %s", encoded)
	}
	if strings.Contains(string(encoded), token) {
		t.Fatal("additive result leaked the local ingest token")
	}
}

func TestAgentActionNormalizationRefusesExternalOnlyMismatches(t *testing.T) {
	fixture := newAgentActionFixture(t)
	cases := []map[string]any{
		{"external_only": true},
		{"external_only": true, "telemetry_address": "10.77.0.10:4317", "external_exports": []map[string]any{{"endpoint": "https://otlp.example.com:4318"}}},
		{"external_only": true, "token_file": "/etc/eregion/token", "external_exports": []map[string]any{{"endpoint": "https://otlp.example.com:4318"}}},
		{"external_only": true, "tls_ca_file": "/etc/eregion/ca.crt", "external_exports": []map[string]any{{"endpoint": "https://otlp.example.com:4318"}}},
		{"telemetry_address": "10.77.0.10:4317", "token_file": "/etc/eregion/token", "tls_ca_file": "/etc/eregion/ca.crt", "external_exports": []map[string]any{{"endpoint": "http://203.0.113.5:4318"}}},
	}
	for index, raw := range cases {
		encoded, _ := json.Marshal(raw)
		if _, err := fixture.registry.Normalize(KindAgent, fixture.target, encoded); err == nil {
			t.Fatalf("case %d was accepted: %s", index, encoded)
		}
	}
}
