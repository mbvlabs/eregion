package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func decodeCommands(t *testing.T, raw string) []Command {
	t.Helper()
	envelope := decodeOne(t, raw)
	data, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Commands []Command `json:"commands"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Commands
}

func TestRootOverviewIsOfflineAndKeepsCatalogData(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "missing", "workspaces.json"))
	t.Setenv("EREGION_WORKSPACE", "missing-workspace")

	code, stdout, stderr := invoke(t)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{"Top-level commands:", "Offline discovery:", "eregion commands --search TEXT", "Accepted work is not completed work", "eregion deploy", "eregion commands"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("overview is missing %q:\n%s", want, stdout)
		}
	}
	if strings.ContainsRune(stdout, '\x1b') {
		t.Fatal("overview contains terminal escapes")
	}

	code, stdout, stderr = invoke(t, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	envelope := decodeOne(t, stdout)
	raw, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"commands", "version", "stage"} {
		if _, ok := data[key]; !ok {
			t.Fatalf("root data is missing %q: %s", key, raw)
		}
	}
	if len(data) != 3 {
		t.Fatalf("root data gained unexpected fields: %s", raw)
	}
	var commands []Command
	if err := json.Unmarshal(data["commands"], &commands); err != nil {
		t.Fatal(err)
	}
	if len(commands) == 0 {
		t.Fatal("root catalog is empty")
	}
	byPath := map[string]Command{}
	for _, command := range commands {
		if strings.HasPrefix(command.Path, "eregion node") {
			t.Fatalf("hidden node command leaked into the root catalog: %s", command.Path)
		}
		byPath[command.Path] = command
	}
	if !byPath["eregion deploy"].DurableSubmission {
		t.Fatal("deploy does not expose its durable operation result")
	}
	if byPath["eregion login"].DurableSubmission || byPath["eregion secret archive"].DurableSubmission {
		t.Fatal("root catalog infers a durable operation from remote mutation")
	}
}

func TestCommandsSearchPlainAndJSON(t *testing.T) {
	code, stdout, stderr := invoke(t, "commands", "--search", "deploy", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	commands := decodeCommands(t, stdout)
	if len(commands) == 0 {
		t.Fatal("search returned no commands")
	}
	foundDeploy := false
	for _, command := range commands {
		if strings.HasPrefix(command.Path, "eregion node") {
			t.Fatalf("hidden node command leaked into search: %s", command.Path)
		}
		haystack := strings.ToLower(command.Path + "\n" + command.Summary + "\n" + strings.Join(command.Examples, "\n"))
		if !strings.Contains(haystack, "deploy") {
			t.Fatalf("search result does not match its fields: %+v", command)
		}
		foundDeploy = foundDeploy || command.Path == "eregion deploy"
	}
	if !foundDeploy {
		t.Fatalf("search did not find eregion deploy: %+v", commands)
	}

	// Results are catalog-ordered and stable across invocations.
	for i := 1; i < len(commands); i++ {
		if commands[i-1].Path >= commands[i].Path {
			t.Fatalf("search order is not deterministic: %s before %s", commands[i-1].Path, commands[i].Path)
		}
	}
	_, again, _ := invoke(t, "commands", "--search", "deploy", "--json")
	if again != stdout {
		t.Fatal("search output changed between identical invocations")
	}

	// Summaries and examples participate, case-insensitively.
	for _, query := range []string{"BLUE/GREEN", "rel_aaaa"} {
		code, stdout, stderr = invoke(t, "commands", "--search", query, "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("%s: code=%d stderr=%q", query, code, stderr)
		}
		if len(decodeCommands(t, stdout)) == 0 {
			t.Fatalf("search %q found no path, summary or example match", query)
		}
	}

	code, stdout, stderr = invoke(t, "commands", "--search", "zzzz-no-such-command", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("no-match: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, `"commands":[]`) {
		t.Fatalf("no-match search is not an empty list: %s", stdout)
	}
	code, stdout, stderr = invoke(t, "commands", "--search", "zzzz-no-such-command")
	if code != 0 || stderr != "" || strings.TrimSpace(stdout) != "" {
		t.Fatalf("plain no-match: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestCommandsSearchRejectsEmptyInput(t *testing.T) {
	for _, args := range [][]string{
		{"commands", "--search=", "--json"},
		{"commands", "--search", "   ", "--json"},
	} {
		code, stdout, stderr := invoke(t, args...)
		if code != 2 || stderr != "" {
			t.Fatalf("%v: code=%d stderr=%q", args, code, stderr)
		}
		envelope := decodeOne(t, stdout)
		if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.InputRequired {
			t.Fatalf("%v: %+v", args, envelope)
		}
	}
}

func TestCommandsDiscoveryFlagsConflict(t *testing.T) {
	for _, args := range [][]string{
		{"commands", "--path", "version", "--search", "deploy", "--json"},
		{"commands", "--path", "version", "--check-meta", "--json"},
		{"commands", "--search", "deploy", "--check-meta", "--json"},
	} {
		code, stdout, stderr := invoke(t, args...)
		if code != 2 || stderr != "" {
			t.Fatalf("%v: code=%d stderr=%q", args, code, stderr)
		}
		envelope := decodeOne(t, stdout)
		if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.Usage || !strings.Contains(envelope.Error.Message, "choose only one") {
			t.Fatalf("%v: %+v", args, envelope)
		}
	}
}

func TestCommandsCheckMeta(t *testing.T) {
	code, stdout, stderr := invoke(t, "commands", "--check-meta", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	envelope := decodeOne(t, stdout)
	raw, _ := json.Marshal(envelope.Data)
	var report metadataCheckReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.Checked == 0 || len(report.Failures) != 0 {
		t.Fatalf("unexpected metadata report: %+v", report)
	}

	code, stdout, stderr = invoke(t, "commands", "--check-meta")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "Command metadata is complete") || strings.ContainsRune(stdout, '\x1b') {
		t.Fatalf("plain check: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	a := newApp()
	version, _, _ := a.root.Find([]string{"version"})
	delete(a.meta, version)
	report = a.meta.check(a.root)
	if report.OK || len(report.Failures) == 0 || !strings.Contains(strings.Join(report.Failures, "\n"), "missing metadata") {
		t.Fatalf("check did not report the broken registry: %+v", report)
	}
}

func TestUnknownCommandSuggestions(t *testing.T) {
	code, stdout, stderr := invoke(t, "versin", "--json")
	if code != 2 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	envelope := decodeOne(t, stdout)
	if envelope.Error == nil || envelope.Error.Code != contract.Usage {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
	suggestions, _ := envelope.Error.Details["suggestions"].([]any)
	if len(suggestions) != 1 || suggestions[0] != "eregion version" {
		t.Fatalf("unexpected suggestions: %+v", envelope.Error.Details)
	}
	_, again, _ := invoke(t, "versin", "--json")
	if again != stdout {
		t.Fatal("suggestions changed between identical invocations")
	}

	code, stdout, stderr = invoke(t, "app", "creat", "--json")
	if code != 2 || stderr != "" {
		t.Fatalf("nested: code=%d stderr=%q", code, stderr)
	}
	envelope = decodeOne(t, stdout)
	suggestions, _ = envelope.Error.Details["suggestions"].([]any)
	if len(suggestions) == 0 || suggestions[0] != "eregion app create" {
		t.Fatalf("unexpected nested suggestions: %+v", envelope.Error.Details)
	}

	// A hidden node command is executable but never suggested or listed.
	code, stdout, _ = invoke(t, "nod", "--json")
	if code != 2 {
		t.Fatal(stdout)
	}
	envelope = decodeOne(t, stdout)
	if strings.Contains(stdout, "eregion node") {
		t.Fatalf("hidden node path leaked into suggestions: %s", stdout)
	}

	code, stdout, stderr = invoke(t, "versin")
	if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "error [usage_error]") ||
		!strings.Contains(stderr, "Did you mean eregion version?") || strings.ContainsRune(stderr, '\x1b') {
		t.Fatalf("plain suggestion: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestDiscoveryDoesNotNeedAWorkspaceOrTerminal(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "missing", "workspaces.json"))
	t.Setenv("EREGION_WORKSPACE", "missing-workspace")
	for _, args := range [][]string{
		{"commands", "--search", "deploy", "--json"},
		{"commands", "--check-meta", "--json"},
	} {
		code, stdout, stderr := invoke(t, args...)
		if code != 0 || stderr != "" {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
		if !decodeOne(t, stdout).OK {
			t.Fatalf("%v: discovery failed", args)
		}
	}
	code, stdout, _ := invoke(t, "--context", "missing-workspace", "commands", "--search", "deploy", "--json")
	if code != 2 {
		t.Fatalf("obsolete --context was not refused: code=%d stdout=%q", code, stdout)
	}
	if envelope := decodeOne(t, stdout); envelope.Error == nil || envelope.Error.Code != contract.Usage {
		t.Fatalf("obsolete --context error = %+v", envelope)
	}
}

func TestHelpExplainsSafetyAndMachineOutput(t *testing.T) {
	code, stdout, stderr := invoke(t, "help", "deploy")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{"When to use:", "Machine output:", "Safety:", "supports --dry-run", "durable operation; follow it to observe completion", "Examples:"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("help is missing %q:\n%s", want, stdout)
		}
	}
	code, stdout, stderr = invoke(t, "help", "version")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "Next:") {
		t.Fatalf("version help has no next step: code=%d stderr=%q\n%s", code, stderr, stdout)
	}
	code, stdout, stderr = invoke(t, "help", "ui")
	if code != 0 || stderr != "" {
		t.Fatalf("ui help: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "Machine output: not supported (requires an interactive terminal)") {
		t.Fatalf("terminal-only help does not say so:\n%s", stdout)
	}
}

func TestHelpDoesNotInventDurableSubmissions(t *testing.T) {
	for _, args := range [][]string{{"login"}, {"secret", "archive"}} {
		code, stdout, stderr := invoke(t, append([]string{"help"}, args...)...)
		if code != 0 || stderr != "" {
			t.Fatalf("%v: code=%d stderr=%q", args, code, stderr)
		}
		if strings.Contains(stdout, "follow it to observe completion") || strings.Contains(stdout, "accepted work is a durable operation") {
			t.Fatalf("%v advertised a durable operation it does not return:\n%s", args, stdout)
		}
		if !strings.Contains(stdout, "Machine output: plain, json") {
			t.Fatalf("%v machine output is not described accurately:\n%s", args, stdout)
		}
	}
}

func TestNodeHelpIsNotHumanOnly(t *testing.T) {
	code, stdout, stderr := invoke(t, "help", "node")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{"Audience: node", "Machine output: plain, json"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("node help is missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "human-only command") || strings.Contains(stdout, "requires an interactive terminal") {
		t.Fatalf("node help is described as human-only:\n%s", stdout)
	}
}

func TestConditionalHumanFactorMetadata(t *testing.T) {
	for _, args := range [][]string{
		{"token", "create"},
		{"app", "delete"},
		{"env", "delete"},
		{"env", "unprotect"},
	} {
		code, stdout, stderr := invoke(t, append([]string{"help"}, args...)...)
		if code != 0 || stderr != "" {
			t.Fatalf("%v: code=%d stderr=%q", args, code, stderr)
		}
		for _, want := range []string{
			"Machine output: plain, json",
			"Human factor:",
			"effectful execution may require",
			"--dry-run remains machine-safe",
		} {
			if !strings.Contains(stdout, want) {
				t.Fatalf("%v help is missing %q:\n%s", args, want, stdout)
			}
		}
		if strings.Contains(stdout, "Machine output: not supported") {
			t.Fatalf("%v lost its machine-safe dry run:\n%s", args, stdout)
		}
	}

	code, stdout, stderr := invoke(t, "help", "env", "protect")
	if code != 0 || stderr != "" {
		t.Fatalf("env protect: code=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, "Human factor:") {
		t.Fatalf("env protect claims a human factor it does not require:\n%s", stdout)
	}

	code, stdout, stderr = invoke(t, "commands", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("catalog: code=%d stderr=%q", code, stderr)
	}
	byPath := map[string]Command{}
	for _, command := range decodeCommands(t, stdout) {
		byPath[command.Path] = command
	}
	for _, path := range []string{
		"eregion token create",
		"eregion app delete",
		"eregion env delete",
		"eregion env unprotect",
	} {
		command := byPath[path]
		if !command.HumanFactor {
			t.Fatalf("%s is not marked with a conditional human factor", path)
		}
		if !command.DryRun || !strings.Contains(strings.Join(command.OutputModes, ","), "json") {
			t.Fatalf("%s must keep its machine-safe dry run: %+v", path, command)
		}
	}
	if byPath["eregion env protect"].HumanFactor {
		t.Fatal("env protect claims a human factor it does not require")
	}
	hasTokenDryRunExample := false
	for _, example := range byPath["eregion token create"].Examples {
		hasTokenDryRunExample = hasTokenDryRunExample || strings.Contains(example, "--dry-run --json")
	}
	if !hasTokenDryRunExample {
		t.Fatal("token create does not show its machine-safe dry run")
	}
}

func TestCatalogRestrictionsAreExplicit(t *testing.T) {
	code, stdout, stderr := invoke(t, "commands", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	commands := decodeCommands(t, stdout)
	byPath := map[string]Command{}
	for _, command := range commands {
		byPath[command.Path] = command
	}
	for _, path := range []string{
		"eregion secret export",
		"eregion secrets export",
		"eregion protect init",
		"eregion ui",
		"eregion domain cloudflare configure",
	} {
		command, ok := byPath[path]
		if !ok {
			t.Fatalf("catalog is missing %s", path)
		}
		if !command.TerminalOnly {
			t.Fatalf("%s is not marked terminal-only", path)
		}
		if len(command.Audience) != 1 || command.Audience[0] != "human" {
			t.Fatalf("%s audience = %v, want human only", path, command.Audience)
		}
		if len(command.OutputModes) != 1 || command.OutputModes[0] != "plain" {
			t.Fatalf("%s output modes = %v, want plain only", path, command.OutputModes)
		}
		for _, example := range command.Examples {
			if strings.Contains(example, "--json") || strings.Contains(example, "--output json") {
				t.Fatalf("%s advertises a machine example: %q", path, example)
			}
		}
	}
	for _, path := range []string{"eregion deploy", "eregion rollback", "eregion restart"} {
		command := byPath[path]
		if !command.DurableSubmission {
			t.Fatalf("%s does not advertise its durable operation result", path)
		}
	}
	if byPath["eregion login"].DurableSubmission || byPath["eregion secret archive"].DurableSubmission {
		t.Fatal("a direct-result command advertises a durable submission")
	}
}
