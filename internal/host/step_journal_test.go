package host

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/engine"
)

type journalTestExec struct {
	output string
	err    error
	script string
	env    map[string]string
}

func (e *journalTestExec) Output(context.Context, string) (string, error) { return e.output, e.err }

func (e *journalTestExec) Run(_ context.Context, _ string, script string, env map[string]string, _ engine.Reporter) error {
	e.script, e.env = script, env
	return e.err
}

func TestJournalAccessStepConvergesTheAdminUser(t *testing.T) {
	step := &journalAccessStep{cfg: Config{AdminUser: "operator"}, exec: &journalTestExec{output: "member:ok"}}
	if step.ID() != "host-journal-access-v1" {
		t.Fatalf("step ID = %q", step.ID())
	}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(inputs), `"user":"operator"`) || !strings.Contains(string(inputs), `"group":"systemd-journal"`) {
		t.Fatalf("inputs = %s", inputs)
	}
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || !check.Complete {
		t.Fatalf("member check = %+v, %v", check, err)
	}
}

func TestJournalAccessStepIsIncompleteUntilMembership(t *testing.T) {
	exec := &journalTestExec{output: "member:missing"}
	step := &journalAccessStep{cfg: Config{AdminUser: "operator"}, exec: exec}
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || check.Complete {
		t.Fatalf("missing membership check = %+v, %v", check, err)
	}
	if !strings.Contains(check.Detail, "systemd-journal") {
		t.Fatalf("detail = %q", check.Detail)
	}
	result, err := step.Apply(context.Background(), engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(exec.script, "usermod -aG systemd-journal") || !strings.Contains(exec.script, "operator") {
		t.Fatalf("apply script does not add the group membership:\n%s", exec.script)
	}
	if result.Result == nil {
		t.Fatal("apply result is empty")
	}
	applied, ok := result.Result.(map[string]any)
	if !ok || applied["user"] != "operator" || applied["group"] != "systemd-journal" {
		t.Fatalf("apply result = %+v", result.Result)
	}
	diff, err := step.Diff(context.Background(), engine.Runtime{})
	if err != nil || len(diff) != 1 || !strings.Contains(diff[0], "systemd-journal") {
		t.Fatalf("diff = %v, %v", diff, err)
	}
}

func TestJournalAccessStepSkipsAMissingAdminAccount(t *testing.T) {
	step := &journalAccessStep{cfg: Config{AdminUser: "operator"}, exec: &journalTestExec{output: "user:missing"}}
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || !check.Complete {
		t.Fatalf("missing account check = %+v, %v", check, err)
	}
}

func TestJournalAccessStepUsesTheDefaultAdminWhenTheIntentPredatesIt(t *testing.T) {
	step := &journalAccessStep{cfg: Config{}, exec: &journalTestExec{output: "member:ok"}}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		User  string `json:"user"`
		Group string `json:"group"`
	}
	if err := json.Unmarshal(inputs, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.User != "admin" || decoded.Group != "systemd-journal" {
		t.Fatalf("inputs = %+v", decoded)
	}
}
