package cli

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"net/http/httputil"
)

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
