package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestGuideAgentsJSONCarriesThePlainDocument(t *testing.T) {
	code, plain, stderr := invoke(t, "guide", "agents")
	if code != 0 || stderr != "" || plain == "" {
		t.Fatalf("plain guide: code=%d stderr=%q", code, stderr)
	}
	code, stdout, stderr := invoke(t, "guide", "agents", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("json guide: code=%d stderr=%q", code, stderr)
	}
	envelope := decodeOne(t, stdout)
	if !envelope.OK {
		t.Fatalf("json guide is not ok: %+v", envelope)
	}
	var data struct {
		Markdown string `json:"markdown"`
	}
	raw, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if data.Markdown != plain {
		t.Fatal("JSON guide content differs from plain output")
	}
	for _, want := range []string{"eregion commands --json", "operations follow", "--confirm <target>", "input_required"} {
		if !strings.Contains(data.Markdown, want) {
			t.Fatalf("agent guide is missing %q", want)
		}
	}
}

func TestSkillInstallWritesTheGuideIdempotently(t *testing.T) {
	a := newApp()
	cmd, _, err := a.root.Find([]string{"skill", "install"})
	if err != nil {
		t.Fatal(err)
	}

	single := t.TempDir()
	if err := cmd.Flags().Set("dir", single); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("harness", "claude"); err != nil {
		t.Fatal(err)
	}
	res, err := a.skillInstall(cmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	data := res.data.(map[string]any)
	path, _ := data["path"].(string)
	want := filepath.Join(single, ".claude", "skills", "eregion", "SKILL.md")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(first), "---\nname: eregion\n") || !strings.Contains(string(first), "eregion commands --json") {
		t.Fatalf("unexpected skill document:\n%s", first)
	}
	if _, err := a.skillInstall(cmd, nil); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil || string(second) != string(first) {
		t.Fatalf("skill install is not idempotent: %v", err)
	}

	code, stdout, stderr := invoke(t, "skill", "install", "--dir", t.TempDir(), "--json")
	if code != 2 || stderr != "" {
		t.Fatalf("missing harness: code=%d stderr=%q", code, stderr)
	}
	envelope := decodeOne(t, stdout)
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.InputRequired {
		t.Fatalf("missing harness envelope = %+v", envelope)
	}
	for _, want := range []string{"--harness", "claude", "opencode"} {
		if !strings.Contains(envelope.Error.Hint, want) {
			t.Fatalf("missing-harness hint %q does not name %q", envelope.Error.Hint, want)
		}
	}
}

func TestSkillInstallPromptsForHarnesses(t *testing.T) {
	a := newApp()
	cmd, _, err := a.root.Find([]string{"skill", "install"})
	if err != nil {
		t.Fatal(err)
	}
	cmd.SetIn(strings.NewReader("2,3,5\n"))
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	selected, err := promptSkillHarnesses(cmd)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "pi", "crush"}
	if len(selected) != len(want) {
		t.Fatalf("selected = %+v, want %v", selected, want)
	}
	for index, harness := range selected {
		if harness.name != want[index] {
			t.Fatalf("selected[%d] = %q, want %q", index, harness.name, want[index])
		}
	}
	for _, line := range []string{"Select harnesses (comma-separated numbers):", "1) Codex", "2) Claude", "5) Crush", "Selection: "} {
		if !strings.Contains(stderr.String(), line) {
			t.Fatalf("prompt is missing %q:\n%s", line, stderr.String())
		}
	}
}
