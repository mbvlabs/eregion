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
	"github.com/mbvlabs/eregion/internal/platform"
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
	unwired := []string{"secrets", "resources", "telemetry", "dns", "access"}
	for _, child := range platformCommand.Commands() {
		for _, name := range unwired {
			if child.Name() == name {
				t.Fatalf("platform advertises an action without a confirmed API handler: %s", name)
			}
		}
	}
	for _, path := range [][]string{
		{"platform", "inventory"},
		{"platform", "apps", "create"}, {"platform", "apps", "list"}, {"platform", "apps", "status"},
		{"platform", "environments", "create"}, {"platform", "deployments", "retry"}, {"platform", "deployments", "rollback"},
	} {
		cmd, remaining, err := a.root.Find(path)
		if err != nil || len(remaining) != 0 || cmd.RunE == nil {
			t.Fatalf("missing implemented command %s: %v", strings.Join(path, " "), err)
		}
	}
}

func TestPlatformAppsCreateAndDryRun(t *testing.T) {
	serverState, server := newPlatformServer(t)
	platformTestEnvironment(t, server)

	code, stdout, stderr := invokePlatform(t, "platform", "apps", "create", "api", "--idempotency-key", "ci:app-api", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	envelope := decodePlatformEnvelope(t, stdout)
	if envelope.Command != "eregion platform apps create" {
		t.Fatalf("wrong command identity: %s", envelope.Command)
	}
	if len(serverState.calls) != 1 {
		t.Fatalf("expected one API call, got %+v", serverState.calls)
	}
	call := serverState.calls[0]
	if call.method != http.MethodPost || call.path != "/v1/operations" || call.key != "ci:app-api" || call.dryRun {
		t.Fatalf("unexpected submission: %+v", call)
	}
	if call.kind != "platform.app.create" || call.target != "platform" || string(call.input) != `{"name":"api"}` {
		t.Fatalf("unexpected action: %+v input=%s", call, call.input)
	}

	code, stdout, stderr = invokePlatform(t, "platform", "apps", "create", "api", "--dry-run", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("dry-run code=%d stderr=%q", code, stderr)
	}
	decodePlatformEnvelope(t, stdout)
	if len(serverState.calls) != 2 {
		t.Fatal("dry-run did not call the API")
	}
	if !serverState.calls[1].dryRun || serverState.calls[1].key != "" {
		t.Fatalf("dry-run sent an idempotency identity: %+v", serverState.calls[1])
	}
}

func TestPlatformInventoryUsesReadAPI(t *testing.T) {
	state, server := newPlatformServer(t)
	platformTestEnvironment(t, server)

	code, stdout, stderr := invokePlatform(t, "platform", "inventory", "--collection", "apps", "--app", "api", "--limit", "10", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	envelope := decodePlatformEnvelope(t, stdout)
	if envelope.Command != "eregion platform inventory" {
		t.Fatalf("wrong command: %s", envelope.Command)
	}
	if len(state.calls) != 1 {
		t.Fatalf("calls=%+v", state.calls)
	}
	call := state.calls[0]
	if call.method != http.MethodGet || call.query.Get("collection") != "apps" || call.query.Get("app") != "api" || call.query.Get("limit") != "10" {
		t.Fatalf("unexpected inventory query: %+v", call)
	}

}
func TestPlatformRollbackNeedsMatchingPlanConfirmation(t *testing.T) {
	state, server := newPlatformServer(t)
	platformTestEnvironment(t, server)

	code, stdout, stderr := invokePlatform(t, "platform", "deployments", "rollback", "api", "production", "--to-release-id", "rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--dry-run", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("plan code=%d stderr=%q", code, stderr)
	}
	envelope := decodePlatformEnvelope(t, stdout)
	var data struct {
		Preview           map[string]any `json:"preview"`
		ConfirmationToken string         `json:"confirmation_token"`
	}
	raw, _ := json.Marshal(envelope.Data)
	if err := json.Unmarshal(raw, &data); err != nil || data.ConfirmationToken == "" {
		t.Fatalf("dry-run did not return a confirmation token: %s", raw)
	}
	if len(state.calls) != 1 || !state.calls[0].dryRun {
		t.Fatalf("plan did not use dry-run: %+v", state.calls)
	}

	code, stdout, stderr = invokePlatform(t, "platform", "deployments", "rollback", "api", "production", "--to-release-id", "rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--idempotency-key", "ci:rollback-1", "--json")
	assertPlatformJSONFault(t, code, stdout, stderr, contract.ConfirmationRequired, "--confirm is required")
	if len(state.calls) != 1 {
		t.Fatal("unconfirmed rollback reached the API")
	}

	code, stdout, stderr = invokePlatform(t, "platform", "deployments", "rollback", "api", "production", "--to-release-id", "rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--confirm", "wrong", "--idempotency-key", "ci:rollback-1", "--json")
	assertPlatformJSONFault(t, code, stdout, stderr, contract.PreconditionFailed, "does not match")
	if len(state.calls) != 1 {
		t.Fatal("mismatched rollback reached the API")
	}

	code, stdout, stderr = invokePlatform(t, "platform", "deployments", "rollback", "api", "production", "--to-release-id", "rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--confirm", data.ConfirmationToken, "--idempotency-key", "ci:rollback-1", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("confirmed rollback code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	decodePlatformEnvelope(t, stdout)
	if len(state.calls) != 2 || state.calls[1].kind != "platform.rollback" || state.calls[1].key != "ci:rollback-1" {
		t.Fatalf("confirmed rollback did not submit: %+v", state.calls)
	}
}

func TestPlatformEnvironmentCreateSendsBuildNode(t *testing.T) {
	state, server := newPlatformServer(t)
	platformTestEnvironment(t, server)
	code, stdout, stderr := invokePlatform(t, "platform", "environments", "create", "api", "production",
		"--node", "main", "--build-node", "builder-1", "--idempotency-key", "ci:env-build-node", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	if len(state.calls) != 1 || state.calls[0].kind != platform.KindEnvironmentCreate {
		t.Fatalf("unexpected call: %+v", state.calls)
	}
	var input platform.EnvironmentCreateInput
	if err := json.Unmarshal(state.calls[0].input, &input); err != nil {
		t.Fatal(err)
	}
	if input.Node != "main" || input.BuildNode != "builder-1" {
		t.Fatalf("build placement was not sent: %+v", input)
	}
}

func TestPlatformDeploymentRetryResubmitsFailedDeployment(t *testing.T) {
	state, server := newPlatformServer(t)
	platformTestEnvironment(t, server)

	code, stdout, stderr := invokePlatform(t, "platform", "deployments", "retry", "--deployment", "dep_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--dry-run", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("retry dry-run code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	decodePlatformEnvelope(t, stdout)
	if len(state.calls) != 2 {
		t.Fatalf("expected inventory read and preview, got %+v", state.calls)
	}
	if state.calls[0].method != http.MethodGet || state.calls[0].query.Get("collection") != "deployments" {
		t.Fatalf("retry did not read deployment inventory: %+v", state.calls[0])
	}
	preview := state.calls[1]
	if !preview.dryRun || preview.kind != "platform.deploy" || preview.target != "env/api/production" {
		t.Fatalf("retry preview did not preserve target/action: %+v", preview)
	}
	if string(preview.input) != `{"release_id":"rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}` {
		t.Fatalf("retry preview did not preserve the release: %s", preview.input)
	}

	code, stdout, stderr = invokePlatform(t, "platform", "deployments", "retry", "--deployment", "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--idempotency-key", "ci:retry-deploy-1", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("retry code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	decodePlatformEnvelope(t, stdout)
	if len(state.calls) != 4 {
		t.Fatalf("retry did not read inventory and submit: %+v", state.calls)
	}
	submit := state.calls[3]
	if submit.method != http.MethodPost || submit.kind != "platform.deploy" || submit.target != "env/api/production" || submit.key != "ci:retry-deploy-1" {
		t.Fatalf("retry did not use the shared submission path: %+v", submit)
	}
	if string(submit.input) != `{"release_id":"rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}` {
		t.Fatalf("retry changed the release: %s", submit.input)
	}
}

func TestPlatformDeploymentRetryRefusesNonFailedDeployment(t *testing.T) {
	state, server := newPlatformServer(t)
	state.items["deployments"] = json.RawMessage(`[{"id":"dep_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operation_id":"op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","app":"api","environment":"production","action":"deploy","release_id":"rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","state":"succeeded"}]`)
	platformTestEnvironment(t, server)

	code, stdout, stderr := invokePlatform(t, "platform", "deployments", "retry", "--deployment", "dep_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--idempotency-key", "ci:retry-deploy-2", "--json")
	assertPlatformJSONFault(t, code, stdout, stderr, contract.PreconditionFailed, "only a terminally failed deployment")
	if len(state.calls) != 1 {
		t.Fatalf("a non-failed retry reached submission: %+v", state.calls)
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
