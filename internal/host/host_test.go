package host

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
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
	"github.com/mbvlabs/eregion/internal/contract"
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
	put(t, cfg.PlatformPath, "ID=debian\nVERSION_ID=12\n", 0o600)
	cfg.BinaryPath, cfg.NodeBinaryPath = filepath.Join(root, "source-cli"), filepath.Join(root, "source-node")
	put(t, cfg.BinaryPath, "fixture CLI executable", 0o700)
	put(t, cfg.NodeBinaryPath, "fixture node executable", 0o700)
	cfg.DatabaseURL = "postgres://test:private-password@127.0.0.1/test?sslmode=disable"
	cfg.OperatorTokenOut = filepath.Join(root, "exports/operator.token")
	return cfg
}

func TestSupportAndRoleMatrix(t *testing.T) {
	if len(SupportMatrix()) != 8 {
		t.Fatal("matrix must have eight release/architecture pairs")
	}
	for _, entry := range SupportMatrix() {
		raw := fmt.Sprintf("ID=%s\nVERSION_ID=\"%s\"\n", entry.ID, entry.VersionID)
		platform, err := ParsePlatform([]byte(raw), entry.Arch)
		if err != nil || !platform.Supported() {
			t.Fatalf("%+v: %v", entry, err)
		}
	}
	for _, p := range []Platform{{ID: "debian", VersionID: "11", Arch: "amd64"}, {ID: "ubuntu", VersionID: "26.04", Arch: "arm64"}, {ID: "debian", VersionID: "12", Arch: "386"}} {
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
	if cfg := DefaultConfig(RoleMain, ""); cfg.Node != "local" {
		t.Fatal("main must match the core's local execution node")
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

func TestPrivateContextAndExport(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "contexts.json")
	store, err := LoadContextStore(path)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 43)
	view, err := store.Login("production", "https://api.example.com", token, []string{"system:read"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(view)
	if strings.Contains(string(data), token) {
		t.Fatal("view leaks token")
	}
	data, _ = json.Marshal(store.List())
	if strings.Contains(string(data), token) {
		t.Fatal("list leaks token")
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
	out := filepath.Join(root, "export/token")
	report, err := reloaded.Export("", out, false)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(report)
	if strings.Contains(string(data), token) {
		t.Fatal("export result leaks token")
	}
	raw, _ := os.ReadFile(out)
	info, _ = os.Stat(out)
	if strings.TrimSpace(string(raw)) != token || info.Mode().Perm() != 0o600 {
		t.Fatal("bad export")
	}
	other := filepath.Join(root, "other")
	put(t, other, strings.Repeat("b", 43), 0o600)
	if _, err := reloaded.Export("", other, false); err == nil {
		t.Fatal("overwrote another token")
	}
	symlink := filepath.Join(root, "link")
	if err := os.Symlink(other, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := reloaded.Export("", symlink, true); err == nil {
		t.Fatal("followed symlink")
	}
	if _, err := ReadTokenFile(symlink); err == nil {
		t.Fatal("read private input through symlink")
	}
	for _, origin := range []string{"http://api.example.com", "https://api.example.com/path", "https://user:password@api.example.com"} {
		if _, err := store.Login("unsafe", origin, token, nil); err == nil {
			t.Fatal("unsafe API origin", origin)
		}
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
	preview, err := LoadCredentials(cfg, false)
	if err != nil || preview.Operator != "" {
		t.Fatalf("%+v %v", preview, err)
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
	if bytes.Contains(inputs, []byte(credentials.Operator)) || bytes.Contains(inputs, []byte("private-password")) {
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
	if bytes.Contains(raw, []byte(credentials.Operator)) {
		t.Fatal("auth file contains token")
	}
	for _, path := range []string{step.authPath(), step.envPath(), cfg.OperatorTokenOut, filepath.Join(cfg.Layout.StateDir, "secrets/operator.token")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("unsafe credential file %s: %v", path, err)
		}
	}
	if err := os.Chmod(filepath.Join(cfg.Layout.StateDir, "secrets/operator.token"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(cfg, false); err == nil {
		t.Fatal("accepted unsafe cached token")
	}
}

func TestCredentialAddBoundAndScoped(t *testing.T) {
	root := t.TempDir()
	authPath := filepath.Join(root, "auth.json")
	token := strings.Repeat("o", 43)
	if err := WriteAuthFile(authPath, []api.Principal{principal(contract.Actor{Kind: "operator", Subject: "operator"}, token, KnownScopes())}, "", ""); err != nil {
		t.Fatal(err)
	}
	r := CredentialAddRequest{AuthFile: authPath, Actor: contract.Actor{Kind: "ci", Subject: "ci"}, Scopes: []string{"operations:read"}, TokenOut: filepath.Join(root, "ci.token")}
	now := time.Unix(1800000000, 0)
	plan, err := r.Confirmation(now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Apply(plan.Token, now)
	if err != nil {
		t.Fatal(err)
	}
	principals, err := ReadAuthFile(authPath)
	if err != nil || len(principals) != 2 {
		t.Fatalf("%v %v", principals, err)
	}
	if len(principals[1].Scopes) != 1 || principals[1].Scopes[0] != "operations:read" {
		t.Fatal("scope broadened")
	}
	raw, _ := os.ReadFile(r.TokenOut)
	encoded, _ := json.Marshal(result)
	if bytes.Contains(encoded, bytes.TrimSpace(raw)) {
		t.Fatal("credential-add output leaks token")
	}
	if _, err := r.Apply(plan.Token, now); err == nil {
		t.Fatal("stale auth revision accepted")
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

func TestAuthenticatedReadiness(t *testing.T) {
	token := strings.Repeat("r", 43)
	seen := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			seen = r.Header.Get("Authorization") == "Bearer "+token
			if !seen {
				w.WriteHeader(401)
			}
		}
	}))
	defer server.Close()
	cfg := fixture(t, RoleMain)
	cfg.APIListen = strings.TrimPrefix(server.URL, "http://")
	if err := (httpProbe{}).Health(context.Background(), cfg, token); err != nil || !seen {
		t.Fatal("no authenticated readiness", err)
	}
}
