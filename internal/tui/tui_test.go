package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/viewer"
)

var testID = contract.OperationID("op_" + strings.Repeat("a", 32))
var testNow = time.Date(2026, 10, 20, 12, 0, 10, 0, time.UTC)

func fixtureModel(t *testing.T, clean viewer.Cleaner) *viewer.Model {
	t.Helper()
	start := testNow.Add(-10 * time.Second)
	model := viewer.New(testID, viewer.Options{Clean: clean})
	model.Snapshot(contract.Operation{
		ID: testID, Kind: "diagnostic.test", Target: "control-plane",
		Actor: contract.Actor{Kind: "ci", Subject: "release"}, State: contract.Running,
		Attempt: 1, LastSequence: 6, CreatedAt: start, UpdatedAt: testNow,
	}, testNow)
	events := []contract.Event{
		{OperationID: testID, Sequence: 1, At: start, Kind: "state", Attempt: 0, State: contract.Queued, Message: "accepted durably"},
		{OperationID: testID, Sequence: 2, At: start.Add(time.Second), Kind: "state", Attempt: 1, State: contract.Running, Message: "worker started"},
		{OperationID: testID, Sequence: 3, At: start.Add(2 * time.Second), Kind: "step", Attempt: 1, Stage: "preflight", Message: "node step started"},
		{OperationID: testID, Sequence: 4, At: start.Add(3 * time.Second), Kind: "step", Attempt: 1, Stage: "build", Message: "node step started"},
		{OperationID: testID, Sequence: 5, At: start.Add(4 * time.Second), Kind: "progress", Attempt: 1, Message: "effect acknowledged durably", Progress: &contract.Progress{Current: 2, Total: 5, Unit: "steps"}},
		{OperationID: testID, Sequence: 6, At: start.Add(5 * time.Second), Kind: "log", Attempt: 1, Stage: "build", Message: "building image"},
	}
	if err := model.Page(events, testNow); err != nil {
		t.Fatal(err)
	}
	return model
}

func testOptions(width, height int) Options {
	return Options{Host: "127.0.0.1:8080", Now: func() time.Time { return testNow }, Width: width, Height: height}
}

func TestLayoutsAreResponsive(t *testing.T) {
	model := newModel(*fixtureModel(t, nil), testOptions(120, 30))
	wide := model.render()
	model.width, model.height = 80, 24
	standard := model.render()
	model.width, model.height = 40, 18
	narrow := model.render()
	model.width, model.height = 20, 6
	tiny := model.render()
	for name, rendered := range map[string]string{"wide": wide, "standard": standard, "narrow": narrow, "tiny": tiny} {
		if rendered == "" {
			t.Fatalf("%s render is empty", name)
		}
		if name != "tiny" && !strings.Contains(rendered, string(testID)) {
			t.Fatalf("%s render lost operation identity: %q", name, rendered)
		}
		if name == "tiny" && !strings.Contains(rendered, "eregion") {
			t.Fatalf("tiny render lost the context title: %q", rendered)
		}
	}
	if !strings.Contains(wide, "EXECUTION STAGES") || !strings.Contains(wide, "EVENTS") {
		t.Fatalf("wide layout missing panes:\n%s", wide)
	}
	if !strings.Contains(standard, "EXECUTION STAGES") || !strings.Contains(standard, "EVENTS") {
		t.Fatalf("standard layout missing panes:\n%s", standard)
	}
	if !strings.Contains(narrow, "EXECUTION STAGES") {
		t.Fatalf("narrow layout missing stages:\n%s", narrow)
	}
}

func TestNoColorAndASCII(t *testing.T) {
	noColor := testOptions(80, 24)
	noColor.NoColor = true
	rendered := newModel(*fixtureModel(t, nil), noColor).render()
	if strings.ContainsRune(rendered, '\x1b') {
		t.Fatalf("NO_COLOR output contains escapes:\n%q", rendered)
	}
	ascii := testOptions(80, 24)
	ascii.ASCII = true
	ascii.NoColor = true
	rendered = newModel(*fixtureModel(t, nil), ascii).render()
	if strings.ContainsAny(rendered, "✓◐●◇■") {
		t.Fatalf("ASCII output contains Unicode glyphs:\n%s", rendered)
	}
	if !strings.Contains(rendered, "[>]") && !strings.Contains(rendered, "[x]") {
		t.Fatalf("ASCII output has no stage glyphs:\n%s", rendered)
	}
}

func TestHostileTextAndRedaction(t *testing.T) {
	clean := func(text string) string {
		return viewer.DefaultClean(strings.ReplaceAll(text, "hunter2", "[REDACTED]"))
	}
	model := fixtureModel(t, clean)
	hostile := "\x1b]52;c;clipboard\a\r\u202e hunter2"
	if err := model.Page([]contract.Event{{
		OperationID: testID, Sequence: 7, At: testNow, Kind: "log", Attempt: 1, Message: hostile,
	}}, testNow); err != nil {
		t.Fatal(err)
	}
	opts := testOptions(80, 24)
	opts.NoColor = true
	rendered := newModel(*model, opts).render()
	if strings.ContainsAny(rendered, "\x1b\a\r\u202e") || strings.Contains(rendered, "hunter2") {
		t.Fatalf("unsafe render: %q", rendered)
	}
	if !strings.Contains(rendered, `\u001b`) {
		t.Fatalf("control characters were not made visible:\n%s", rendered)
	}
}

func TestNavigationHelpPauseAndDetach(t *testing.T) {
	model := newModel(*fixtureModel(t, nil), testOptions(120, 30))
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 26})
	model = updated.(Model)
	if model.width != 100 || model.height != 26 {
		t.Fatalf("resize=%dx%d", model.width, model.height)
	}
	updated, _ = model.Update(keyPress("j"))
	model = updated.(Model)
	if model.stage != 1 {
		t.Fatalf("stage focus did not move: %d", model.stage)
	}
	updated, _ = model.Update(keyPress("tab"))
	model = updated.(Model)
	if model.focus != paneEvents {
		t.Fatalf("tab did not change pane: %v", model.focus)
	}
	updated, _ = model.Update(keyPress("G"))
	model = updated.(Model)
	if !model.events.AtBottom() {
		t.Fatal("G did not reach newest event")
	}
	updated, cmd := model.Update(keyPress("space"))
	model = updated.(Model)
	if !model.paused || cmd != nil {
		t.Fatalf("pause=%v cmd=%v", model.paused, cmd)
	}
	updated, _ = model.Update(keyPress("?"))
	model = updated.(Model)
	if !model.showHelp || !strings.Contains(model.render(), "HELP") {
		t.Fatalf("help did not open:\n%s", model.render())
	}
	updated, cmd = model.Update(keyPress("q"))
	_ = updated
	if cmd == nil || cmd() != tea.Quit() {
		t.Fatalf("q did not quit: %v", cmd)
	}
}

func TestObservationAndCompletion(t *testing.T) {
	model := newModel(*viewer.New(testID, viewer.Options{}), testOptions(80, 24))
	snapshot := contract.Operation{
		ID: testID, Kind: "diagnostic.test", Target: "control-plane",
		Actor: contract.Actor{Kind: "ci", Subject: "release"}, State: contract.Running,
		Attempt: 1, LastSequence: 1, CreatedAt: testNow.Add(-time.Second), UpdatedAt: testNow,
	}
	updated, _ := model.Update(observationMsg{observation: viewer.Observation{Kind: viewer.ObservationSnapshot, At: testNow, Snapshot: &snapshot}})
	model = updated.(Model)
	event := contract.Event{OperationID: testID, Sequence: 1, At: testNow, Kind: "state", Attempt: 1, State: contract.Running, Message: "started"}
	updated, _ = model.Update(observationMsg{observation: viewer.Observation{Kind: viewer.ObservationEvents, At: testNow, Events: []contract.Event{event}}})
	model = updated.(Model)
	if model.view.State != contract.Running || model.view.EventCount != 1 {
		t.Fatalf("reducer not applied: %+v", model.view)
	}
	if !strings.Contains(model.render(), "worker") && !strings.Contains(model.render(), "started") {
		t.Fatalf("event not rendered:\n%s", model.render())
	}
	updated, cmd := model.Update(doneMsg{summary: viewer.Summary{OperationID: testID, State: contract.Succeeded}})
	model = updated.(Model)
	if !model.done || cmd == nil || cmd() != tea.Quit() {
		t.Fatalf("completion did not stop the program: done=%v cmd=%v", model.done, cmd)
	}
}

func keyPress(value string) tea.KeyPressMsg {
	runes := []rune(value)
	if len(runes) == 1 {
		switch runes[0] {
		case ' ':
			return tea.KeyPressMsg{Code: ' ', Text: " "}
		case '?':
			return tea.KeyPressMsg{Code: '?', Text: "?"}
		default:
			code := runes[0]
			if code >= 'A' && code <= 'Z' {
				return tea.KeyPressMsg{Code: code, Text: value}
			}
			return tea.KeyPressMsg{Code: code, Text: value}
		}
	}
	if value == "tab" {
		return tea.KeyPressMsg{Code: tea.KeyTab}
	}
	if value == "space" {
		return tea.KeyPressMsg{Code: ' ', Text: " "}
	}
	panic("unsupported test key: " + value)
}

func TestGoldenRenders(t *testing.T) {
	cases := []struct {
		name   string
		width  int
		height int
		opts   func(Options) Options
		clean  viewer.Cleaner
	}{
		{name: "wide-color", width: 120, height: 30},
		{name: "standard-color", width: 80, height: 24},
		{name: "narrow-color", width: 40, height: 18},
		{name: "tiny-nocolor", width: 28, height: 8, opts: func(o Options) Options { o.NoColor = true; return o }},
		{name: "standard-ascii", width: 80, height: 24, opts: func(o Options) Options { o.ASCII = true; o.NoColor = true; return o }},
		{name: "standard-nocolor", width: 80, height: 24, opts: func(o Options) Options { o.NoColor = true; return o }},
		{name: "hostile-nocolor", width: 80, height: 20, opts: func(o Options) Options { o.NoColor = true; return o }, clean: func(text string) string {
			return viewer.DefaultClean(strings.ReplaceAll(text, "hunter2", "[REDACTED]"))
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			model := fixtureModel(t, test.clean)
			if test.name == "hostile-nocolor" {
				if err := model.Page([]contract.Event{{
					OperationID: testID, Sequence: 7, At: testNow, Kind: "log", Attempt: 1,
					Message: "\x1b]8;;https://example.invalid\a link \x1b]8;;\a hunter2",
				}}, testNow); err != nil {
					t.Fatal(err)
				}
			}
			options := testOptions(test.width, test.height)
			if test.opts != nil {
				options = test.opts(options)
			}
			rendered := newModel(*model, options).render()
			if test.name == "standard-nocolor" || test.name == "tiny-nocolor" || test.name == "standard-ascii" || test.name == "hostile-nocolor" {
				if strings.ContainsRune(rendered, '\x1b') {
					t.Fatalf("no-color golden contains escapes:\n%q", rendered)
				}
			}
			checkGolden(t, test.name, rendered)
		})
	}
}

func checkGolden(t *testing.T, name, rendered string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(rendered), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run UPDATE_GOLDEN=1 go test ./internal/tui): %v", path, err)
	}
	if string(want) != rendered {
		t.Fatalf("golden mismatch for %s:\n--- want ---\n%s\n--- got ---\n%s", name, want, rendered)
	}
}
