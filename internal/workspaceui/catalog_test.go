package workspaceui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/cli"
	"github.com/mbvlabs/eregion/internal/workspaceui"
)

// TestEveryDocumentedActionResolvesToTheCLICatalog builds the real command
// catalog through the exported CLI entry point and proves that every
// noninteractive equivalent in the workspace mapping names an implemented
// command. It does not execute any action.
func TestEveryDocumentedActionResolvesToTheCLICatalog(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"commands", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("commands --json exited %d: %s %s", code, stdout.String(), stderr.String())
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Commands []struct {
				Path string `json:"path"`
			} `json:"commands"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode catalog: %v\n%s", err, stdout.String())
	}
	if !envelope.OK {
		t.Fatalf("catalog failed: %s", stdout.String())
	}
	paths := make(map[string]bool, len(envelope.Data.Commands))
	for _, command := range envelope.Data.Commands {
		paths[command.Path] = true
	}
	actions := workspaceui.CommandActions()
	if len(actions) == 0 {
		t.Fatal("the command mapping is empty")
	}
	for _, action := range actions {
		if strings.TrimSpace(action.Name) == "" || strings.TrimSpace(action.CLI) == "" {
			t.Fatalf("incomplete action: %+v", action)
		}
		if !strings.HasPrefix(action.CLI, "eregion ") {
			t.Fatalf("%s does not show a full noninteractive command: %q", action.ID, action.CLI)
		}
		if action.CommandPath == "" || !strings.HasPrefix(action.CommandPath, "eregion ") {
			t.Fatalf("%s has an invalid command path %q", action.ID, action.CommandPath)
		}
		if !paths[action.CommandPath] {
			t.Fatalf("%s resolves to %q, which is not in the CLI catalog", action.ID, action.CommandPath)
		}
		if !strings.Contains(action.CLI, strings.TrimPrefix(action.CommandPath, "eregion ")) {
			t.Fatalf("%s CLI %q does not contain its command path %q", action.ID, action.CLI, action.CommandPath)
		}
	}
}
