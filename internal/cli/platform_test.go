package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

const cliPlatformToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type platformCall struct {
	method string
	path   string
	query  url.Values
	auth   string
	key    string
	kind   string
	target string
	input  json.RawMessage
	dryRun bool
}

type platformServer struct {
	t       *testing.T
	calls   []platformCall
	items   map[string]json.RawMessage
	handler http.Handler
}

func newPlatformServer(t *testing.T) (*platformServer, *httptest.Server) {
	t.Helper()
	p := &platformServer{t: t, items: map[string]json.RawMessage{
		"nodes":        json.RawMessage(`[{"name":"main","status":"ready","roles":["application"],"capabilities":["docker"],"last_seen_at":"2026-10-20T12:00:00Z"}]`),
		"apps":         json.RawMessage(`[{"id":"app_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","name":"api","created_at":"2026-10-20T12:00:00Z","updated_at":"2026-10-20T12:00:00Z"}]`),
		"environments": json.RawMessage(`[{"id":"env_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","app":"api","name":"production","node":"main","serving_release_id":"rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","health":"healthy","updated_at":"2026-10-20T12:00:00Z"}]`),
		"deployments":  json.RawMessage(`[{"id":"dep_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operation_id":"op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","app":"api","environment":"production","action":"deploy","release_id":"rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","state":"failed","created_at":"2026-10-20T12:00:00Z","updated_at":"2026-10-20T12:00:00Z"}]`),
	}}
	p.handler = http.HandlerFunc(p.serve)
	server := httptest.NewServer(p.handler)
	t.Cleanup(server.Close)
	return p, server
}

func (p *platformServer) serve(w http.ResponseWriter, r *http.Request) {
	p.t.Helper()
	call := platformCall{method: r.Method, path: r.URL.Path, query: r.URL.Query(), auth: r.Header.Get("Authorization"), key: r.Header.Get("Idempotency-Key")}
	defer func() { p.calls = append(p.calls, call) }()
	if call.auth != "Bearer "+cliPlatformToken {
		writePlatformEnvelope(w, http.StatusUnauthorized, contract.Failure("test", contract.NewFault(contract.Unauthenticated, "bad credential", "")))
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/platform":
		collection := call.query.Get("collection")
		items, ok := p.items[collection]
		if !ok {
			writePlatformEnvelope(w, http.StatusBadRequest, contract.Failure("test", contract.NewFault(contract.ValidationFailed, "unknown collection", "")))
			return
		}
		limit := 50
		var page struct {
			Collection string          `json:"collection"`
			Items      json.RawMessage `json:"items"`
			Limit      int             `json:"limit"`
		}
		page.Collection, page.Items, page.Limit = collection, items, limit
		writePlatformEnvelope(w, http.StatusOK, contract.Success("test", page, nil))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/operations":
		var request struct {
			Kind   string          `json:"kind"`
			Target string          `json:"target"`
			Input  json.RawMessage `json:"input"`
			DryRun bool            `json:"dry_run"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writePlatformEnvelope(w, http.StatusBadRequest, contract.Failure("test", contract.NewFault(contract.ValidationFailed, "bad request", "")))
			return
		}
		call.kind, call.target, call.input, call.dryRun = request.Kind, request.Target, request.Input, request.DryRun
		if request.DryRun {
			writePlatformEnvelope(w, http.StatusOK, contract.Success("test", map[string]any{
				"kind": request.Kind, "target": request.Target, "dry_run": true,
				"input": json.RawMessage(request.Input), "summary": "validated preview",
			}, nil))
			return
		}
		operation := contract.Operation{
			ID: "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: request.Kind, Target: request.Target,
			Actor: contract.Actor{Kind: "ci", Subject: "test-ci"}, State: contract.Queued,
			LastSequence: 1, CreatedAt: timeNow(), UpdatedAt: timeNow(),
		}
		writePlatformEnvelope(w, http.StatusAccepted, contract.Success("test", operation, nil))
	default:
		writePlatformEnvelope(w, http.StatusNotFound, contract.Failure("test", contract.NewFault(contract.NotFound, "not found", "")))
	}
}

func writePlatformEnvelope(w http.ResponseWriter, status int, envelope contract.Envelope) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope)
}

func timeNow() time.Time { return time.Now().UTC() }

// Exercise the production runner, including parse errors and output selection.
func invokePlatform(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return invoke(t, args...)
}

func decodePlatformEnvelope(t *testing.T, raw string) contract.Envelope {
	t.Helper()
	envelope := decodeOne(t, raw)
	if !envelope.OK {
		t.Fatalf("unexpected failure: %+v", envelope.Error)
	}
	return envelope
}

func platformTestEnvironment(t *testing.T, server *httptest.Server) {
	t.Helper()
	t.Setenv("EREGION_API_URL", server.URL)
	t.Setenv("EREGION_API_TOKEN", cliPlatformToken)
}

func TestPlatformCommandMetadata(t *testing.T) {
	a := newApp()
	if err := a.meta.validate(a.root); err != nil {
		t.Fatal(err)
	}
	platformCommand, _, err := a.root.Find([]string{"platform"})
	if err != nil {
		t.Fatal(err)
	}
	unwired := []string{"secrets", "resources", "telemetry", "dns", "access", "nodes", "fleet", "submit"}
	for _, child := range platformCommand.Commands() {
		for _, name := range unwired {
			if child.Name() == name {
				t.Fatalf("platform advertises an action without a confirmed API handler: %s", name)
			}
		}
	}
	for _, path := range [][]string{
		{"platform", "inventory"},
		{"platform", "releases", "create"}, {"platform", "releases", "list"},
		{"platform", "deployments", "plan"}, {"platform", "deployments", "deploy"},
		{"platform", "deployments", "list"}, {"platform", "deployments", "retry"}, {"platform", "deployments", "rollback"},
	} {
		cmd, remaining, err := a.root.Find(path)
		if err != nil || len(remaining) != 0 || cmd.RunE == nil {
			t.Fatalf("missing implemented command %s: %v", strings.Join(path, " "), err)
		}
	}
}

func TestPlatformRejectsMissingIdempotencyKey(t *testing.T) {
	state, server := newPlatformServer(t)
	platformTestEnvironment(t, server)
	code, stdout, stderr := invokePlatform(t, "platform", "deployments", "deploy", "api", "production", "--release-id", "rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--json")
	assertPlatformJSONFault(t, code, stdout, stderr, contract.InputRequired, "--idempotency-key is required")
	if len(state.calls) != 0 {
		t.Fatal("submission without an idempotency key reached the API")
	}
}

func assertPlatformJSONFault(t *testing.T, exit int, stdout, stderr string, code contract.Code, message string) {
	t.Helper()
	envelope := decodeOne(t, stdout)
	if exit != contract.ExitCode(contract.NewFault(code, message, "")) || stderr != "" || envelope.OK || envelope.Error == nil || envelope.Error.Code != code || !strings.Contains(envelope.Error.Message, message) {
		t.Fatalf("exit=%d stderr=%q envelope=%+v; want %s containing %q", exit, stderr, envelope, code, message)
	}
}
