package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServingVersionAndHealth(t *testing.T) {
	t.Setenv("FAIL_HEALTH", "0")
	h := handler("v2")
	for _, tc := range []struct {
		path string
		body string
	}{{"/", "eregion sample: v2\n"}, {"/healthz", "ok\n"}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if w.Code != http.StatusOK || w.Body.String() != tc.body {
			t.Fatalf("%s: status=%d body=%q", tc.path, w.Code, w.Body.String())
		}
	}
}

func TestCandidateCanFailHealthWithoutExiting(t *testing.T) {
	t.Setenv("FAIL_HEALTH", "1")
	h := handler("bad")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("health status=%d", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("app should still serve while unhealthy: status=%d", w.Code)
	}
}
