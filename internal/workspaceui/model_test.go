package workspaceui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
)

type fakeAPI struct {
	inventory func(context.Context, string, url.Values) (json.RawMessage, error)
	submit    func(context.Context, string, string, json.RawMessage, string, bool) (json.RawMessage, error)
	get       func(context.Context, contract.OperationID) (json.RawMessage, error)
	events    func(context.Context, contract.Cursor, int) (json.RawMessage, error)

	inventoryCalls int
	submits        []fakeSubmit
}

type fakeSubmit struct {
	kind   string
	target string
	input  json.RawMessage
	key    string
	dryRun bool
}

func (f *fakeAPI) PlatformInventory(ctx context.Context, collection string, filters url.Values) (json.RawMessage, error) {
	f.inventoryCalls++
	if f.inventory == nil {
		return nil, contract.NewFault(contract.Unavailable, "no fake inventory", "test")
	}
	return f.inventory(ctx, collection, filters)
}

func (f *fakeAPI) SubmitAction(ctx context.Context, kind, target string, input json.RawMessage, key string, dryRun bool) (json.RawMessage, error) {
	f.submits = append(f.submits, fakeSubmit{kind: kind, target: target, input: append(json.RawMessage(nil), input...), key: key, dryRun: dryRun})
	if f.submit == nil {
		return nil, contract.NewFault(contract.Unavailable, "no fake submit", "test")
	}
	return f.submit(ctx, kind, target, input, key, dryRun)
}

func (f *fakeAPI) Get(ctx context.Context, id contract.OperationID) (json.RawMessage, error) {
	if f.get == nil {
		return nil, contract.NewFault(contract.Unavailable, "no fake get", "test")
	}
	return f.get(ctx, id)
}

func (f *fakeAPI) Events(ctx context.Context, cursor contract.Cursor, limit int) (json.RawMessage, error) {
	if f.events == nil {
		return nil, contract.NewFault(contract.Unavailable, "no fake events", "test")
	}
	return f.events(ctx, cursor, limit)
}

const testAppID = "app_00000000000000000000000000000001"
const testEnvID = "env_00000000000000000000000000000001"
const testReleaseID = "rel_00000000000000000000000000000001"
const testOperationID = "op_00000000000000000000000000000001"

func testArgs(t *testing.T) (Model, *fakeAPI, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	clock := &now
	api := &fakeAPI{}
	model := New(Options{
		API:        api,
		Host:       "127.0.0.1:8080",
		NoColor:    true,
		ASCII:      true,
		InitialTab: Fleet,
		Width:      80,
		Height:     24,
		Now:        func() time.Time { return *clock },
		Key:        func() string { return "ui:test-key" },
	})
	return model, api, clock
}

func press(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Text: string(code)}
}

func special(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code}
}

func typeText(m Model, text string) Model {
	for _, r := range text {
		next, _ := m.Update(press(r))
		m = next.(Model)
	}
	return m
}

func step(t *testing.T, m Model, message tea.Msg) Model {
	t.Helper()
	next, _ := m.Update(message)
	return next.(Model)
}

func stepCmd(t *testing.T, m Model, message tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, command := m.Update(message)
	return next.(Model), command
}

func runCommand(t *testing.T, command tea.Cmd) tea.Msg {
	t.Helper()
	if command == nil {
		t.Fatal("expected a command")
	}
	return command()
}

func runCommandMessages(t *testing.T, command tea.Cmd) []tea.Msg {
	t.Helper()
	if command == nil {
		return nil
	}
	message := command()
	if batch, ok := message.(tea.BatchMsg); ok {
		var messages []tea.Msg
		for _, nested := range batch {
			messages = append(messages, runCommandMessages(t, nested)...)
		}
		return messages
	}
	return []tea.Msg{message}
}

func applyCommand(t *testing.T, m Model, command tea.Cmd) Model {
	t.Helper()
	for _, message := range runCommandMessages(t, command) {
		if _, ok := message.(tickMsg); ok {
			continue
		}
		m = step(t, m, message)
	}
	return m
}

func load(t *testing.T, m Model, raw string) Model {
	t.Helper()
	message := inventoryMsg{tab: m.tab, at: m.now, items: mustDecode(t, m.tab, raw)}
	return step(t, m, message)
}

func mustDecode(t *testing.T, tab Tab, raw string) []Item {
	t.Helper()
	items, err := decodeInventory(tab, json.RawMessage(raw), func(text string) string { return Sanitize(nil, text) })
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return items
}

func render(m Model) string { return m.render() }

const nodesJSON = `{"collection":"nodes","items":[
 {"name":"main","roles":["application","builder"],"capabilities":["docker","buildpacks"],"address":"10.77.0.1:22","status":"ready","last_seen_at":"2026-10-20T09:59:50Z","created_at":"2026-10-19T10:00:00Z","updated_at":"2026-10-20T09:59:50Z"},
 {"name":"builder-2","roles":["builder"],"capabilities":["buildpacks"],"address":"10.77.0.2:22","status":"unreachable","created_at":"2026-10-19T10:00:00Z","updated_at":"2026-10-20T09:00:00Z"}
]}`

const appsJSON = `{"collection":"apps","items":[
 {"id":"` + testAppID + `","name":"api","created_at":"2026-10-19T10:00:00Z","updated_at":"2026-10-19T10:00:00Z"},
 {"id":"app_00000000000000000000000000000002","name":"web","created_at":"2026-10-19T10:00:00Z","updated_at":"2026-10-19T10:00:00Z"}
]}`

const envsJSON = `{"collection":"environments","items":[
 {"id":"` + testEnvID + `","app":"api","name":"production","node":"main","host_port":20001,"candidate_port":30001,"container_port":8080,"bind":"127.0.0.1","health":{"type":"http","path":"/healthz","timeout_seconds":30},"env":{"APP_ENV":"production","API_TOKEN":"must-not-render"},"desired_release_id":"` + testReleaseID + `","applied_release_id":"` + testReleaseID + `","serving_release_id":"` + testReleaseID + `","created_at":"2026-10-19T10:00:00Z","updated_at":"2026-10-20T09:59:50Z"}
]}`

func operationJSON(state string) string {
	attempt := 0
	if state == "running" || state == "succeeded" {
		attempt = 1
	}
	return `{"id":"` + testOperationID + `","kind":"platform.app.create","target":"platform","actor":{"kind":"operator","subject":"test"},"state":"` + state + `","attempt":` + fmt.Sprint(attempt) + `,"last_sequence":"1","created_at":"2026-10-20T10:00:00Z","updated_at":"2026-10-20T10:00:00Z"}`
}

func previewJSON(kind, target string) string {
	return `{"kind":"` + kind + `","target":"` + target + `","dry_run":true,"input":{"name":"api"},"summary":"safe preview","steps":["validate"],"warnings":["no-op warning"]}`
}

func TestFleetFreshnessUnreachableAndEmptyStates(t *testing.T) {
	m, api, clock := testArgs(t)
	api.inventory = func(context.Context, string, url.Values) (json.RawMessage, error) {
		return json.RawMessage(nodesJSON), nil
	}
	m = load(t, m, nodesJSON)
	view := render(m)
	for _, want := range []string{"Fleet", "main", "builder-2", "ready", "unreachable", "fresh"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q in:\n%s", want, view)
		}
	}

	// A failed refresh keeps the last known rows and marks them stale.
	api.inventory = func(context.Context, string, url.Values) (json.RawMessage, error) {
		return nil, contract.NewFault(contract.Unavailable, "control plane did not respond", "retry")
	}
	next, command := stepCmd(t, m, press('r'))
	m = applyCommand(t, next, command)
	view = render(m)
	if !strings.Contains(view, "API unreachable") && !strings.Contains(view, "unreachable") {
		t.Fatalf("missing unreachable state:\n%s", view)
	}
	if !strings.Contains(view, "main") {
		t.Fatalf("last known rows disappeared:\n%s", view)
	}
	*clock = clock.Add(2 * time.Minute)
	m = step(t, m, tickMsg(*clock))
	if view = render(m); !strings.Contains(view, "last known") {
		t.Fatalf("missing stale freshness:\n%s", view)
	}

	// Empty is a first-class state, not a fabricated row.
	api.inventory = func(context.Context, string, url.Values) (json.RawMessage, error) {
		return json.RawMessage(`{"collection":"apps","items":[]}`), nil
	}
	next, command = stepCmd(t, m, press('2'))
	m = applyCommand(t, next, command)
	if view = render(m); !strings.Contains(view, "No applications yet") || !strings.Contains(view, "Press a") {
		t.Fatalf("missing empty state:\n%s", view)
	}
}

func TestFilterSearchAndDetails(t *testing.T) {
	m, api, _ := testArgs(t)
	api.inventory = func(context.Context, string, url.Values) (json.RawMessage, error) {
		return json.RawMessage(appsJSON), nil
	}
	next, command := stepCmd(t, m, press('2'))
	m = applyCommand(t, next, command)
	m = load(t, m, appsJSON)
	m = step(t, m, press('/'))
	m = typeText(m, "web")
	m = step(t, m, special(tea.KeyEnter))
	view := render(m)
	if !strings.Contains(view, "web") || strings.Contains(view, "api ") {
		t.Fatalf("filter did not narrow rows:\n%s", view)
	}
	m = step(t, m, special(tea.KeyEnter))
	if view = render(m); !strings.Contains(view, "DETAILS") || !strings.Contains(view, "app_00000000000000000000000000000002") {
		t.Fatalf("details did not open:\n%s", view)
	}
	m = step(t, m, special(tea.KeyEsc))
	m = step(t, m, special(tea.KeyEsc))
	if view = render(m); strings.Contains(view, "/web") {
		t.Fatalf("filter was not cleared:\n%s", view)
	}
}

func TestAppFormPreviewSubmitAndStableKey(t *testing.T) {
	m, api, _ := testArgs(t)
	api.inventory = func(context.Context, string, url.Values) (json.RawMessage, error) {
		return json.RawMessage(appsJSON), nil
	}
	next, command := stepCmd(t, m, press('2'))
	m = applyCommand(t, next, command)
	m = load(t, m, appsJSON)
	m = step(t, m, press('a'))
	m = typeText(m, "api")
	next, command = stepCmd(t, m, special(tea.KeyEnter))
	m = next
	api.submit = func(_ context.Context, kind, target string, input json.RawMessage, key string, dryRun bool) (json.RawMessage, error) {
		return json.RawMessage(previewJSON(kind, target)), nil
	}
	m = applyCommand(t, m, command)
	if len(api.submits) != 1 || !api.submits[0].dryRun || api.submits[0].key != "" {
		t.Fatalf("unexpected dry-run: %+v", api.submits)
	}
	if api.submits[0].kind != "platform.app.create" || api.submits[0].target != "platform" {
		t.Fatalf("wrong action: %+v", api.submits[0])
	}
	view := render(m)
	for _, want := range []string{"DRY-RUN PREVIEW", "nothing has been submitted", "eregion platform submit", "ui:test-key", "s submit"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q in preview:\n%s", want, view)
		}
	}

	// A timed-out submission is ambiguous; the same key must be reused.
	api.submit = func(_ context.Context, _, _ string, _ json.RawMessage, _ string, _ bool) (json.RawMessage, error) {
		return nil, contract.NewFault(contract.Unavailable, "submission did not complete", "reuse the key")
	}
	next, command = stepCmd(t, m, press('s'))
	m = applyCommand(t, next, command)
	if len(api.submits) != 2 || api.submits[1].dryRun || api.submits[1].key != "ui:test-key" {
		t.Fatalf("submission did not reuse the key: %+v", api.submits)
	}
	if view = render(m); !strings.Contains(view, "Retry with the same key") {
		t.Fatalf("missing retry guidance:\n%s", view)
	}

	api.submit = func(_ context.Context, kind, target string, _ json.RawMessage, _ string, _ bool) (json.RawMessage, error) {
		return json.RawMessage(operationJSON("queued")), nil
	}
	next, command = stepCmd(t, m, press('s'))
	m = applyCommand(t, next, command)
	if len(api.submits) != 3 || api.submits[2].key != "ui:test-key" {
		t.Fatalf("retry used a different key: %+v", api.submits)
	}
	view = render(m)
	for _, want := range []string{"ACCEPTED", testOperationID, "operations follow " + testOperationID, "Detaching never cancels"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q after submit:\n%s", want, view)
		}
	}
	if len(m.summary.Operations) != 1 || m.summary.Operations[0] != testOperationID {
		t.Fatalf("summary did not record the operation: %+v", m.summary)
	}
}

func TestGuidedEnvironmentAndImageReleaseForms(t *testing.T) {
	m, api, _ := testArgs(t)
	api.inventory = func(context.Context, string, url.Values) (json.RawMessage, error) {
		return json.RawMessage(appsJSON), nil
	}
	next, command := stepCmd(t, m, press('2'))
	m = applyCommand(t, next, command)
	m = load(t, m, appsJSON)

	// The selected app is prefilled, and every required field is named before
	// the dry run starts.
	m = step(t, m, press('e'))
	m = step(t, m, special(tea.KeyEnter)) // app (prefilled) -> name
	m = typeText(m, "production")
	m = step(t, m, special(tea.KeyEnter)) // name -> node
	m = typeText(m, "main")
	m = step(t, m, special(tea.KeyEnter)) // node -> container port
	m = step(t, m, special(tea.KeyEnter)) // container port
	m = step(t, m, special(tea.KeyEnter)) // host port
	api.submit = func(_ context.Context, kind, target string, input json.RawMessage, _ string, dryRun bool) (json.RawMessage, error) {
		if !dryRun {
			t.Fatal("env form submitted without a preview")
		}
		if kind != "platform.environment.create" || target != "app/api" {
			t.Fatalf("wrong environment action %s %s", kind, target)
		}
		var decoded map[string]any
		if err := json.Unmarshal(input, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["name"] != "production" || decoded["node"] != "main" {
			t.Fatalf("wrong environment input: %s", input)
		}
		return json.RawMessage(previewJSON(kind, target)), nil
	}
	next, command = stepCmd(t, m, special(tea.KeyEnter))
	m = applyCommand(t, next, command)
	if !strings.Contains(render(m), "platform.environment.create") {
		t.Fatalf("environment preview missing:\n%s", render(m))
	}
	m = step(t, m, special(tea.KeyEsc))

	// The image release form only advertises an image source.
	m = step(t, m, press('i'))
	m = step(t, m, special(tea.KeyEnter)) // app (prefilled) -> environment
	m = typeText(m, "production")
	m = step(t, m, special(tea.KeyEnter)) // environment -> image
	m = typeText(m, "ghcr.io/acme/api:2026-10-20")
	api.submit = func(_ context.Context, kind, target string, input json.RawMessage, _ string, dryRun bool) (json.RawMessage, error) {
		if !dryRun || kind != "platform.release.create" || target != "env/api/production" {
			t.Fatalf("wrong release action: %s %s dry=%t", kind, target, dryRun)
		}
		if !strings.Contains(string(input), `"type":"image"`) {
			t.Fatalf("release form advertised another source: %s", input)
		}
		return json.RawMessage(previewJSON(kind, target)), nil
	}
	next, command = stepCmd(t, m, special(tea.KeyEnter))
	m = applyCommand(t, next, command)
	if !strings.Contains(render(m), "platform.release.create") {
		t.Fatalf("release preview missing:\n%s", render(m))
	}
}

func TestDeployRequiresBoundConfirmation(t *testing.T) {
	m, api, _ := testArgs(t)
	api.inventory = func(context.Context, string, url.Values) (json.RawMessage, error) {
		return json.RawMessage(envsJSON), nil
	}
	next, command := stepCmd(t, m, press('3'))
	m = applyCommand(t, next, command)
	m = load(t, m, envsJSON)
	m = step(t, m, press('d'))
	m = step(t, m, special(tea.KeyEnter)) // app -> env
	m = step(t, m, special(tea.KeyEnter)) // env -> release (prefilled from inventory)
	api.submit = func(_ context.Context, kind, target string, input json.RawMessage, _ string, dryRun bool) (json.RawMessage, error) {
		if !dryRun {
			t.Fatal("deploy submitted without a preview")
		}
		if kind != "platform.deploy" || target != "env/api/production" {
			t.Fatalf("wrong deploy action: %s %s", kind, target)
		}
		if !strings.Contains(string(input), testReleaseID) {
			t.Fatalf("deploy did not carry the selected release: %s", input)
		}
		return json.RawMessage(previewJSON(kind, target)), nil
	}
	next, command = stepCmd(t, m, special(tea.KeyEnter))
	m = applyCommand(t, next, command)
	view := render(m)
	if !strings.Contains(view, "Type production to confirm") {
		t.Fatalf("missing bound confirmation:\n%s", view)
	}
	before := len(api.submits)
	m = step(t, m, special(tea.KeyEnter))
	if len(api.submits) != before {
		t.Fatal("deploy submitted without the typed confirmation")
	}
	if !strings.Contains(render(m), "type production") {
		t.Fatalf("missing confirmation error:\n%s", render(m))
	}
	m = typeText(m, "production")
	api.submit = func(_ context.Context, kind, target string, _ json.RawMessage, key string, dryRun bool) (json.RawMessage, error) {
		if dryRun || kind != "platform.deploy" || target != "env/api/production" || key != "ui:test-key" {
			t.Fatalf("unexpected deploy submission: %s %s %q dry=%t", kind, target, key, dryRun)
		}
		var operation contract.Operation
		if err := json.Unmarshal([]byte(operationJSON("queued")), &operation); err != nil {
			t.Fatal(err)
		}
		operation.Kind, operation.Target = kind, target
		raw, err := json.Marshal(operation)
		return raw, err
	}
	next, command = stepCmd(t, m, press('s'))
	m = applyCommand(t, next, command)
	if view = render(m); !strings.Contains(view, "ACCEPTED") {
		t.Fatalf("confirmed deploy was not submitted:\n%s", view)
	}
}

func TestResourceInventoryUnavailableIsHonest(t *testing.T) {
	m, api, _ := testArgs(t)
	api.inventory = func(context.Context, string, url.Values) (json.RawMessage, error) {
		return nil, contract.NewFault(contract.ValidationFailed, "unknown collection", "register a provider")
	}
	next, command := stepCmd(t, m, press('4'))
	m = applyCommand(t, next, command)
	view := render(m)
	for _, want := range []string{"Resources inventory is not available", "No rows are invented", "r to retry"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q:\n%s", want, view)
		}
	}
}

func TestHostileTextIsSanitizedAndSecretRedacted(t *testing.T) {
	registry := &secrets.Registry{}
	registry.Add("super-secret-token")
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	api := &fakeAPI{}
	m := New(Options{
		API: api, Host: "127.0.0.1:8080", Secrets: registry,
		NoColor: true, ASCII: true, InitialTab: Fleet, Width: 44, Height: 30,
		Now: func() time.Time { return now }, Key: func() string { return "ui:test-key" },
	})
	raw := `{"collection":"nodes","items":[{"name":"main","roles":["application"],"capabilities":["docker"],"address":"10.0.0.1:22 \u001b]0;evil\u0007 super-secret-token","status":"ready","created_at":"2026-10-19T10:00:00Z","updated_at":"2026-10-20T09:59:50Z"}]}`
	m = load(t, m, raw)
	m = step(t, m, special(tea.KeyEnter))
	view := render(m)
	if strings.ContainsRune(view, '\x1b') || strings.ContainsRune(view, '\x07') || strings.Contains(view, "super-secret-token") {
		t.Fatalf("unsafe render:\n%q", view)
	}
	if !strings.Contains(view, "\\u001b") {
		t.Fatalf("control character was not visibly escaped:\n%s", view)
	}
	if !strings.Contains(view, "[REDACTED]") && !strings.Contains(view, "redacted") {
		t.Fatalf("secret was not marked redacted:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if len([]rune(line)) > 44 {
			t.Fatalf("line exceeds narrow width: %q", line)
		}
	}
}

func TestGenericResourceSecretFieldsAreRedacted(t *testing.T) {
	raw := `{"collection":"resources","items":[{"id":"res_1","name":"db","status":"running","password":"hunter2","connection":{"token":"abc","host":"db.internal"}}]}`
	items, err := decodeInventory(Resources, json.RawMessage(raw), func(text string) string { return Sanitize(nil, text) })
	if err != nil {
		t.Fatal(err)
	}
	view := ""
	for _, detail := range items[0].Details {
		view += detail.Label + "=" + detail.Value + "\n"
	}
	if strings.Contains(view, "hunter2") || strings.Contains(view, `"abc"`) {
		t.Fatalf("generic resource leaked a secret:\n%s", view)
	}
	if !strings.Contains(view, "[redacted]") {
		t.Fatalf("generic resource did not mark redaction:\n%s", view)
	}
}

func TestOperationFollowAndDetachDoesNotCancel(t *testing.T) {
	m, api, _ := testArgs(t)
	api.inventory = func(context.Context, string, url.Values) (json.RawMessage, error) {
		return json.RawMessage(appsJSON), nil
	}
	next, command := stepCmd(t, m, press('2'))
	m = applyCommand(t, next, command)
	m = load(t, m, appsJSON)
	m = step(t, m, press('a'))
	m = typeText(m, "api")
	next, command = stepCmd(t, m, special(tea.KeyEnter))
	m = next
	api.submit = func(_ context.Context, kind, target string, _ json.RawMessage, _ string, dryRun bool) (json.RawMessage, error) {
		if dryRun {
			return json.RawMessage(previewJSON(kind, target)), nil
		}
		return json.RawMessage(operationJSON("queued")), nil
	}
	m = applyCommand(t, m, command)
	next, command = stepCmd(t, m, press('s'))
	m = applyCommand(t, next, command)

	api.get = func(_ context.Context, id contract.OperationID) (json.RawMessage, error) {
		if id != testOperationID {
			t.Fatalf("wrong operation looked up: %s", id)
		}
		return json.RawMessage(operationJSON("running")), nil
	}
	api.events = func(_ context.Context, cursor contract.Cursor, limit int) (json.RawMessage, error) {
		return json.RawMessage(`{"events":[{"operation_id":"` + testOperationID + `","sequence":"1","at":"2026-10-20T10:00:01Z","kind":"step","attempt":1,"stage":"preflight","message":"candidate ready \u001b[31mraw\u001b[0m"}],"cursor":{"operation_id":"` + testOperationID + `","after":"1"}}`), nil
	}
	next, command = stepCmd(t, m, press('f'))
	m = applyCommand(t, next, command)
	view := render(m)
	for _, want := range []string{testOperationID, "running", "preflight", "candidate ready"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q in operation view:\n%s", want, view)
		}
	}
	if strings.ContainsRune(view, '\x1b') {
		t.Fatalf("event text leaked ANSI:\n%q", view)
	}
	next, command = stepCmd(t, m, press('q'))
	if command == nil {
		t.Fatal("detach did not quit the program")
	}
	m = next
	if !m.summary.Detached || len(m.summary.Operations) != 1 {
		t.Fatalf("unexpected detach summary: %+v", m.summary)
	}
}

func TestNoColorAsciiAndNarrowRender(t *testing.T) {
	m, api, _ := testArgs(t)
	api.inventory = func(context.Context, string, url.Values) (json.RawMessage, error) {
		return json.RawMessage(nodesJSON), nil
	}
	m = load(t, m, nodesJSON)
	m.width, m.height = 24, 10
	view := render(m)
	if strings.ContainsRune(view, '\x1b') {
		t.Fatalf("no-color render contains ANSI:\n%q", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if len([]rune(line)) > 24 {
			t.Fatalf("narrow line too wide: %q", line)
		}
	}
	if !strings.Contains(view, "Fleet") || !strings.Contains(view, "? help") {
		t.Fatalf("compact view missing essentials:\n%s", view)
	}
}
