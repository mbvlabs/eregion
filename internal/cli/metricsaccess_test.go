package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/telemetry"
)

type metricsAccessBackend struct {
	Backend
	mu sync.Mutex

	endpoint telemetry.EndpointInfo

	rotateErr error
	rotations int
	hash      string
	confirm   string
	dryRun    bool
}

func (b *metricsAccessBackend) MetricsEndpoint(context.Context) (telemetry.EndpointInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.endpoint, nil
}

func (b *metricsAccessBackend) MetricsRotate(_ context.Context, hash, confirm string, dryRun bool) (telemetry.Rotation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rotations++
	b.hash, b.confirm, b.dryRun = hash, confirm, dryRun
	if dryRun {
		return telemetry.Rotation{Schema: telemetry.RotationSchema, Node: b.endpoint.Node, Username: telemetry.Username}, nil
	}
	if b.rotateErr != nil {
		return telemetry.Rotation{}, b.rotateErr
	}
	b.endpoint.Credential = true
	return telemetry.Rotation{
		Schema: telemetry.RotationSchema, Node: b.endpoint.Node, Hostname: b.endpoint.Hostname, Username: telemetry.Username,
		MetricsURL: b.endpoint.MetricsURL, Discovery: b.endpoint.DiscoveryURL,
	}, nil
}

func newMetricsAccessBackend(configured bool) *metricsAccessBackend {
	state := telemetry.EndpointStatePendingCredential
	if configured {
		state = telemetry.EndpointStateConfigured
	}
	return &metricsAccessBackend{endpoint: telemetry.EndpointInfo{
		Schema: telemetry.EndpointSchema, Node: "main", Hostname: "main.example.com", State: state,
		Listen:      telemetry.GatewayListenAddress,
		MetricsPath: telemetry.MetricsPath, DiscoveryPath: telemetry.DiscoveryPath,
		Access: "public_https", Public: true, Authentication: "basic", Credential: configured,
		MetricsURL: "https://main.example.com/metrics/node", DiscoveryURL: "https://main.example.com/metrics/targets.json",
		Upstream: "http://127.0.0.1:9100",
	}}
}

func metricsAccessFixture(t *testing.T, configured bool) (telemetry.Credential, *metricsAccessBackend) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "eregion", "contexts.json")
	t.Setenv("EREGION_WORKSPACE_FILE", configPath)
	store, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	access := host.WorkspaceAccess{Name: "thelab", SSH: &host.WorkspaceSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: "/dev/null"}}
	if _, err := store.PublishWorkspace("ws_0123456789abcdef01234567", "thelab", access, false); err != nil {
		t.Fatal(err)
	}
	credentialDir, err := store.CredentialDirFor(access)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := telemetry.NewCredential()
	if err != nil {
		t.Fatal(err)
	}
	if err := saveMetricsCredential(credentialDir, credential); err != nil {
		t.Fatal(err)
	}
	return credential, newMetricsAccessBackend(configured)
}

func runMetricsAccessCLI(t *testing.T, backend *metricsAccessBackend, args ...string) (int, string, string) {
	t.Helper()
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), args, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	return exit, stdout.String(), stderr.String()
}

func TestMetricsConfigUsesPlaceholderAndWritesOwnerOnlyCredentialFile(t *testing.T) {
	credential, backend := metricsAccessFixture(t, true)
	exit, stdout, stderr := runMetricsAccessCLI(t, backend, "metrics", "endpoint", "--json")
	if exit != 0 || stderr != "" || !strings.Contains(stdout, "main.example.com") ||
		!strings.Contains(stdout, "127.0.0.1:9101") || strings.Contains(stdout, "10.78.0.1") {
		t.Fatalf("endpoint exit=%d stderr=%q stdout=%q", exit, stderr, stdout)
	}
	exit, stdout, stderr = runMetricsAccessCLI(t, backend, "metrics", "config", "--json")
	if exit != 0 || stderr != "" {
		t.Fatalf("config exit=%d stderr=%q stdout=%q", exit, stderr, stdout)
	}
	if strings.Contains(stdout, credential.Password) {
		t.Fatal("ordinary JSON output contained the telemetry password")
	}
	if !strings.Contains(stdout, "metrics-password") || !strings.Contains(stdout, "http_sd_configs") {
		t.Fatalf("config output lacks placeholder/discovery:\n%s", stdout)
	}
	out := filepath.Join(t.TempDir(), "scrape.yml")
	exit, stdout, stderr = runMetricsAccessCLI(t, backend, "metrics", "config", "--format", "prometheus", "--out", out)
	if exit != 0 || stderr != "" {
		t.Fatalf("config out exit=%d stderr=%q stdout=%q", exit, stderr, stdout)
	}
	if strings.Contains(stdout, credential.Password) {
		t.Fatal("config --out printed the telemetry password")
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), credential.Password) || !strings.Contains(string(raw), "http_sd_configs") {
		t.Fatalf("owner-only config did not contain the credential:\n%s", raw)
	}
	info, err := os.Stat(out)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v err=%v", info.Mode().Perm(), err)
	}
}

func TestMetricsConfigRefusesPreexistingOutput(t *testing.T) {
	_, backend := metricsAccessFixture(t, true)
	out := filepath.Join(t.TempDir(), "scrape.yml")
	if err := os.WriteFile(out, []byte("do not overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	exit, stdout, _ := runMetricsAccessCLI(t, backend, "metrics", "config", "--format", "prometheus", "--out", out, "--json")
	if exit == 0 {
		t.Fatalf("config overwrote an existing file:\n%s", stdout)
	}
	raw, err := os.ReadFile(out)
	if err != nil || string(raw) != "do not overwrite" {
		t.Fatalf("existing file changed: %q err=%v", raw, err)
	}
}

func TestMetricsConfigTargetsFileNeedsNoCredential(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "eregion", "contexts.json")
	t.Setenv("EREGION_WORKSPACE_FILE", configPath)
	store, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishWorkspace("ws_0123456789abcdef01234567", "thelab", host.WorkspaceAccess{
		Name: "thelab", SSH: &host.WorkspaceSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: "/dev/null"},
	}, false); err != nil {
		t.Fatal(err)
	}
	backend := newMetricsAccessBackend(true)
	out := filepath.Join(t.TempDir(), "targets.json")
	exit, stdout, stderr := runMetricsAccessCLI(t, backend, "metrics", "config", "--format", "targets", "--out", out)
	if exit != 0 || stderr != "" {
		t.Fatalf("targets out exit=%d stderr=%q stdout=%q", exit, stderr, stdout)
	}
	raw, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(raw), "main.example.com:443") || strings.Contains(string(raw), "10.78.0.1") {
		t.Fatalf("targets file = %s err=%v", raw, err)
	}
}

func TestMetricsConfigRefusesCredentialFileWithoutLocalCredential(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "eregion", "contexts.json")
	t.Setenv("EREGION_WORKSPACE_FILE", configPath)
	store, err := host.LoadWorkspaceStore(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishUnconfiguredAccess(host.WorkspaceAccess{Name: "main", SSH: &host.WorkspaceSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: "/dev/null"}}); err != nil {
		t.Fatal(err)
	}
	backend := newMetricsAccessBackend(true)
	exit, stdout, _ := runMetricsAccessCLI(t, backend, "metrics", "config", "--format", "prometheus", "--out", filepath.Join(t.TempDir(), "scrape.yml"), "--json")
	if exit == 0 {
		t.Fatalf("config without a credential succeeded:\n%s", stdout)
	}
}

func TestMetricsConfigRefusesAnUnconfiguredHostnameWithoutAFallback(t *testing.T) {
	_, backend := metricsAccessFixture(t, true)
	backend.endpoint.State = telemetry.EndpointStateUnconfigured
	backend.endpoint.Hostname = ""
	backend.endpoint.MetricsURL = ""
	backend.endpoint.DiscoveryURL = ""
	backend.endpoint.Public = false
	backend.endpoint.Access = telemetry.EndpointStateUnconfigured
	backend.endpoint.Authentication = "none"
	backend.endpoint.Credential = false
	exit, stdout, _ := runMetricsAccessCLI(t, backend, "metrics", "config", "--json")
	if exit == 0 {
		t.Fatalf("config invented a target for a hostname-less node:\n%s", stdout)
	}
	for _, forbidden := range []string{"10.78.0.1", "127.0.0.1:9101", "127.0.0.1:9100"} {
		if strings.Contains(stdout, forbidden) {
			t.Fatalf("config output contained fallback target %q:\n%s", forbidden, stdout)
		}
	}
}

func TestMetricsRotateSendsOnlyHashSavesBeforeSendingAndNeverPrintsPassword(t *testing.T) {
	_, backend := metricsAccessFixture(t, true)
	argv := []string{"metrics", "rotate", "--confirm", "main/metrics", "--json", "--workspace", "thelab"}
	exit, stdout, stderr := runMetricsAccessCLI(t, backend, argv...)
	if exit != 0 || stderr != "" {
		t.Fatalf("rotate exit=%d stderr=%q stdout=%q", exit, stderr, stdout)
	}
	if backend.confirm != "main/metrics" || backend.dryRun || backend.rotations != 1 {
		t.Fatalf("rotate call = confirm %q dry %t calls %d", backend.confirm, backend.dryRun, backend.rotations)
	}
	if !telemetry.ValidHash(backend.hash) {
		t.Fatalf("rotate hash = %q", backend.hash)
	}
	credential, err := loadMetricsCredential(metricsTestCredentialDir(t))
	if err != nil || credential == nil || !telemetry.VerifyPassword(backend.hash, credential.Password) {
		t.Fatalf("local credential does not match the sent hash: %+v err=%v", credential, err)
	}
	if strings.Contains(stdout, credential.Password) {
		t.Fatal("rotate output contained a telemetry password")
	}
	if !strings.Contains(stdout, "secrets export") || !strings.Contains(stdout, "old password") {
		t.Fatalf("rotate output lacks the scraper warning:\n%s", stdout)
	}
}

func TestMetricsRotateFailureRestoresThePreviousLocalCredential(t *testing.T) {
	old, backend := metricsAccessFixture(t, true)
	backend.rotateErr = contract.NewFault(contract.Unavailable, "node did not answer", "")
	exit, stdout, _ := runMetricsAccessCLI(t, backend, "metrics", "rotate", "--confirm", "main/metrics", "--json", "--workspace", "thelab")
	if exit == 0 {
		t.Fatalf("failed rotation reported success:\n%s", stdout)
	}
	credential, err := loadMetricsCredential(metricsTestCredentialDir(t))
	if err != nil || credential == nil || credential.Password != old.Password {
		t.Fatalf("previous credential was not restored: %+v err=%v", credential, err)
	}
	if strings.Contains(stdout, old.Password) {
		t.Fatal("failure output contained the previous password")
	}
}

func TestMetricsRotateDryRunDoesNotChangeTheCredential(t *testing.T) {
	old, backend := metricsAccessFixture(t, true)
	exit, stdout, stderr := runMetricsAccessCLI(t, backend, "metrics", "rotate", "--dry-run", "--json", "--workspace", "thelab")
	if exit != 0 || stderr != "" {
		t.Fatalf("dry run exit=%d stderr=%q stdout=%q", exit, stderr, stdout)
	}
	if !backend.dryRun || backend.hash != "" || backend.rotations != 1 {
		t.Fatalf("dry run call = hash %q dry %t calls %d", backend.hash, backend.dryRun, backend.rotations)
	}
	credential, err := loadMetricsCredential(metricsTestCredentialDir(t))
	if err != nil || credential == nil || credential.Password != old.Password {
		t.Fatalf("dry run changed the local credential: %+v err=%v", credential, err)
	}
}

func TestMetricsEndpointDescribeIsSafe(t *testing.T) {
	credential, backend := metricsAccessFixture(t, true)
	info, err := backend.MetricsEndpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	view := metricsEndpointView{EndpointInfo: info, LocalCredential: true}
	text := metricsEndpointText(view)
	for _, want := range []string{"main.example.com", "https://main.example.com/metrics/node", "127.0.0.1:9101", "public HTTPS", "credential", "present"} {
		if !strings.Contains(text, want) {
			t.Fatalf("endpoint text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "WireGuard only") || strings.Contains(text, "10.78.0.1") {
		t.Fatalf("endpoint text retained the private endpoint:\n%s", text)
	}
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), credential.Password) || strings.Contains(string(raw), "$2") {
		t.Fatalf("endpoint JSON exposed a credential: %s", raw)
	}
}

func metricsTestCredentialDir(t *testing.T) string {
	t.Helper()
	store, err := host.LoadWorkspaceStore("")
	if err != nil {
		t.Fatal(err)
	}
	access, ok := store.AccessByName("thelab")
	if !ok {
		t.Fatal("thelab access missing")
	}
	dir, err := store.CredentialDirFor(access)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
