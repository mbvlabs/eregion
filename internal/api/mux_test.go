package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func tagged(tag string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tag)) })
}

func TestMuxRoutesByPathClass(t *testing.T) {
	mux := NewMux(MuxConfig{Default: tagged("default"), Vault: tagged("vault"), Webhook: tagged("webhook"), Private: tagged("private")})
	for path, want := range map[string]string{
		"/v1/secrets": "vault", "/v1/secrets/shop/production/NAME": "vault", "/v1/secrets/archive": "vault",
		"/v1/webhooks/github": "webhook", "/private/grafana": "private",
		"/v1/operations": "default", "/v1/secretsx": "default", "/v1/webhooks/github/extra": "default", "/privateer": "default", "/healthz": "default",
	} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if got := recorder.Body.String(); got != want {
			t.Errorf("%s routed to %s, want %s", path, got, want)
		}
	}
}

func TestMuxWithoutOptionalHandlersFallsThrough(t *testing.T) {
	mux := NewMux(MuxConfig{Default: tagged("default")})
	for _, path := range []string{"/v1/secrets", "/v1/webhooks/github", "/private/x"} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
		if recorder.Body.String() != "default" {
			t.Errorf("%s was not left to the default handler", path)
		}
	}
	recorder := httptest.NewRecorder()
	NewMux(MuxConfig{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("empty mux status = %d", recorder.Code)
	}
}
