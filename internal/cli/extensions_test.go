package cli

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"net/http/httputil"
)

func TestParseAllowedPush(t *testing.T) {
	entry, err := parseAllowedPush(" Acme/Shop@main=shop/production ")
	if err != nil || entry.Repository != "Acme/Shop" || entry.Branch != "main" || entry.App != "shop" || entry.Environment != "production" {
		t.Fatalf("entry=%+v err=%v", entry, err)
	}
	for _, bad := range []string{"", "acme/shop", "acme/shop@main", "acme/shop@main=shop", "acme/shop@=shop/production", "@main=shop/production", "acme/shop@main=/production"} {
		if _, err := parseAllowedPush(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestReadWebhookSecretRequiresPrivateFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "secret")
	if err := os.WriteFile(path, []byte("0123456789abcdef-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret, err := readWebhookSecret(path)
	if err != nil || string(secret) != "0123456789abcdef-secret" {
		t.Fatalf("secret=%q err=%v", secret, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readWebhookSecret(path); err == nil {
		t.Fatal("group/world-readable secret file accepted")
	}
	if _, err := readWebhookSecret(filepath.Join(directory, "missing")); err == nil {
		t.Fatal("missing secret file accepted")
	}
}

func TestPrivateRewriteStripsCredentialsAndForwardingHeaders(t *testing.T) {
	in := httptest.NewRequest(http.MethodGet, "/private/app/health", nil)
	in.Header.Set("Authorization", "Bearer control-plane")
	in.Header.Set("Cookie", "session=secret")
	in.Header.Set("Proxy-Authorization", "Basic proxy")
	in.Header.Set("X-Forwarded-For", "203.0.113.9")
	in.Header.Set("X-Forwarded-Host", "control.example")
	in.Header.Set("X-Forwarded-Proto", "https")
	in.Header.Set("X-Real-IP", "203.0.113.9")
	in.Header.Set("Forwarded", "for=203.0.113.9")
	in.Header.Set("X-Keep", "yes")
	target, err := url.Parse("http://127.0.0.1:9000/base")
	if err != nil {
		t.Fatal(err)
	}
	request := &httputil.ProxyRequest{In: in, Out: in.Clone(in.Context())}
	rewritePrivateRequest(target, request)
	if request.Out.URL.Path != "/app/health" {
		t.Fatalf("path = %q", request.Out.URL.Path)
	}
	for _, header := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP", "Forwarded"} {
		if value := request.Out.Header.Get(header); value != "" {
			t.Fatalf("%s reached the upstream: %q", header, value)
		}
	}
	if request.Out.Header.Get("X-Keep") != "yes" {
		t.Fatal("an unrelated header was dropped")
	}
}
