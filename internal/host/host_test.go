package host

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/engine"
)

func put(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

// Every test uses temporary paths. No apt, usermod, Docker, systemctl, database
// migration, or host apply is executed by these tests.
func fixture(t *testing.T, role Role) Config {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig(role, "")
	cfg.Layout = Layout{
		StateDir: filepath.Join(root, "data/bootstrap"), NodeStateDir: filepath.Join(root, "data/node"),
		ConfigDir: filepath.Join(root, "etc"), DataDir: filepath.Join(root, "data"),
		BinDir: filepath.Join(root, "bin"), SystemdDir: filepath.Join(root, "systemd"), RegistryDir: filepath.Join(root, "registry"),
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Layout.User, cfg.Layout.NodeUser = account.Username, account.Username
	cfg.Layout.Group, cfg.Layout.NodeGroup = group.Name, group.Name
	cfg.PlatformPath, cfg.Arch = filepath.Join(root, "os-release"), "amd64"
	put(t, cfg.PlatformPath, "ID=debian\nVERSION_ID=13\n", 0o600)
	cfg.BinaryPath = filepath.Join(root, "source-cli")
	put(t, cfg.BinaryPath, "fixture CLI executable", 0o700)
	cfg.DatabaseURL = "postgres://test:private-password@127.0.0.1/test?sslmode=disable"
	return cfg
}

func TestSupportAndRoleMatrix(t *testing.T) {
	if len(SupportMatrix()) != 4 {
		t.Fatal("matrix must have four release/architecture pairs")
	}
	for _, entry := range SupportMatrix() {
		raw := fmt.Sprintf("ID=%s\nVERSION_ID=\"%s\"\n", entry.ID, entry.VersionID)
		platform, err := ParsePlatform([]byte(raw), entry.Arch)
		if err != nil || !platform.Supported() {
			t.Fatalf("%+v: %v", entry, err)
		}
	}
	for _, p := range []Platform{{ID: "debian", VersionID: "11", Arch: "amd64"}, {ID: "ubuntu", VersionID: "24.04", Arch: "arm64"}, {ID: "debian", VersionID: "12", Arch: "amd64"}, {ID: "debian", VersionID: "13", Arch: "386"}} {
		if p.Supported() {
			t.Fatalf("unexpected support: %+v", p)
		}
	}
	main, _ := ConfigForRole(RoleMain)
	if !main.ControlPlane || !main.Runtime || !main.Builder || !main.Registry || main.Telemetry {
		t.Fatal(main)
	}
	for _, role := range []Role{"edge", "typo"} {
		if _, err := ConfigForRole(role); err == nil {
			t.Fatal(role)
		}
	}
	telemetry, err := ConfigForRole(RoleTelemetry)
	if err != nil || !telemetry.Runtime || !telemetry.Telemetry || !telemetry.NodeAgent || telemetry.ControlPlane {
		t.Fatalf("telemetry role: %+v %v", telemetry, err)
	}
	if cfg := DefaultConfig(RoleMain, ""); cfg.Node != "main" {
		t.Fatal("the default node name is main")
	}
}

func TestConfirmationBindingAndExpiry(t *testing.T) {
	now := time.Unix(1800000000, 0)
	inputs := []byte("input")
	confirmation, err := issueConfirmation("host.apply", "local/main", "revision", inputs, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyConfirmation(confirmation.Token, "host.apply", "local/main", "revision", inputs, now); err != nil {
		t.Fatal(err)
	}
	for _, args := range []struct {
		target, revision string
		at               time.Time
	}{{"other/main", "revision", now}, {"local/main", "changed", now}, {"local/main", "revision", now.Add(ConfirmationTTL + time.Second)}} {
		if err := verifyConfirmation(confirmation.Token, "host.apply", args.target, args.revision, inputs, args.at); err == nil {
			t.Fatal("accepted wrong/expired binding")
		}
	}
}

func TestContextStore(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "contexts.json")
	store, err := LoadContextStore(path)
	if err != nil {
		t.Fatal(err)
	}
	route := &ContextSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: filepath.Join(root, "known_hosts")}
	view, err := store.SaveContext(Context{Name: "production", SSH: route, AutoUpgradeNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	if view.Route == "" || !view.AutoUpgradeNodes || !view.Current {
		t.Fatalf("unexpected view: %+v", view)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatal("unsafe store mode")
	}
	reloaded, err := LoadContextStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Current != "production" {
		t.Fatal("current not persisted")
	}
	if _, err := reloaded.Get("production"); err != nil {
		t.Fatal(err)
	}
	if !reloaded.List()[0].AutoUpgradeNodes {
		t.Fatal("auto upgrade flag not persisted")
	}
	if _, err := reloaded.Use(""); err != nil {
		t.Fatal(err)
	}
	if ok, err := reloaded.Remove("production"); err != nil || !ok {
		t.Fatalf("remove: %v %v", ok, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadContextStore(path); err == nil {
		t.Fatal("accepted public credential store")
	}
}

func TestCredentialHandoffNoPlaintextInputs(t *testing.T) {
	cfg := fixture(t, RoleMain)
	if _, err := LoadCredentials(cfg, false); err != nil {
		t.Fatal(err)
	}
	credentials, err := LoadCredentials(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	step := &credentialsStep{cfg: cfg, credentials: credentials}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(inputs, []byte("private-password")) {
		t.Fatal("secret in engine inputs")
	}
	// Use the fixture's existing account, not a real service account.
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	auth, err := api.LoadAuth(step.authPath())
	if err != nil || auth == nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(step.authPath())
	if bytes.Contains(raw, []byte("token_sha256")) {
		t.Fatal("setup minted an API token")
	}
	for _, path := range []string{step.authPath(), step.envPath()} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("unsafe credential file %s: %v", path, err)
		}
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDownloaderChecksumLimitAndRedirect(t *testing.T) {
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("artifact")), Header: http.Header{}, Request: r}, nil
	})}
	downloader := HTTPDownloader{Client: client}
	if _, err := downloader.Get(context.Background(), "https://example.com/file", 100, contentSHA256([]byte("artifact"))); err != nil {
		t.Fatal(err)
	}
	if _, err := downloader.Get(context.Background(), "https://example.com/file", 100, strings.Repeat("0", 64)); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	if _, err := downloader.Get(context.Background(), "https://example.com/file", 2, contentSHA256([]byte("artifact"))); err == nil {
		t.Fatal("oversized artifact accepted")
	}
	if _, err := downloader.Get(context.Background(), "http://example.com/file", 100, ""); err == nil {
		t.Fatal("plaintext artifact accepted")
	}
	client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"Location": {"http://untrusted.example/file"}}, Request: r}, nil
	})
	if _, err := downloader.Get(context.Background(), "https://example.com/file", 100, ""); err == nil {
		t.Fatal("downgrade redirect accepted")
	}
}

func TestPackExtractionRejectsLinksAndTraversal(t *testing.T) {
	archive := func(name string, kind byte) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		header := &tar.Header{Name: name, Typeflag: kind, Mode: 0o755}
		if kind == tar.TypeReg {
			header.Size = 4
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			_, _ = tw.Write([]byte("pack"))
		}
		_ = tw.Close()
		_ = gz.Close()
		return buf.Bytes()
	}
	if raw, err := extractPack(archive("pack", tar.TypeReg), 10); err != nil || string(raw) != "pack" {
		t.Fatalf("%q %v", raw, err)
	}
	for _, raw := range [][]byte{archive("../pack", tar.TypeReg), archive("pack", tar.TypeSymlink), []byte("not-gzip")} {
		if _, err := extractPack(raw, 10); err == nil {
			t.Fatal("unsafe archive accepted")
		}
	}
	if _, err := extractPack(archive("pack", tar.TypeReg), 2); err == nil {
		t.Fatal("oversized member accepted")
	}
}

func TestPublicReadiness(t *testing.T) {
	seen := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			seen = true
			if r.Header.Get("Authorization") != "" {
				w.WriteHeader(400)
			}
		}
	}))
	defer server.Close()
	cfg := fixture(t, RoleMain)
	cfg.APIListen = strings.TrimPrefix(server.URL, "http://")
	if err := (httpProbe{}).Health(context.Background(), cfg); err != nil || !seen {
		t.Fatal("no public readiness", err)
	}
}
