package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/spf13/cobra"
)

func TestApplyProgressGivesEveryPlanStepALine(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&out)
	r := &serverInit{a: &app{secrets: &secrets.Registry{}}, cmd: cmd}
	p := newApplyProgress(r, []host.PlanEntry{
		{ID: "platform", Description: "Verify release", Complete: true},
		{ID: "packages", Description: "Install packages"},
		{ID: "docker", Description: "Enable Docker"},
		{ID: "api", Description: "Start API"},
		{ID: "ready", Description: "Verify API"},
	})
	_, _ = p.Write([]byte("bootstrap packages: apt\nbootstrap api: unit\nbootstrap host-api-apply: restart\n"))
	p.close(ui.EndOK)
	text := out.String()
	last := -1
	for _, want := range []string{"Verify release: skip already done", "Install packages", "Enable Docker: ok", "Start API", "Verify API: ok"} {
		i := strings.Index(text, want)
		if i < 0 || i < last {
			t.Fatalf("missing or out of order %q in:\n%s", want, text)
		}
		last = i
	}
	if strings.Contains(text, "host-api-apply") {
		t.Fatalf("sub-step got its own line:\n%s", text)
	}
}

func TestSavePendingCarriesPinnedHostKey(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "eregion", "contexts.json"))
	r := &serverInit{
		answers: serverInitAnswers{
			Context: "prod", Host: "203.0.113.10", User: "root", Port: "22",
			Node: "main", Hostname: "app.example.com", AdminUser: "admin",
		},
		pending:    &host.PendingInit{Server: "prod", Phases: map[string]string{}},
		hostKey:    "ssh-ed25519 SHA256:host",
		publicIPv4: "203.0.113.10",
	}
	if err := r.savePending(); err != nil {
		t.Fatal(err)
	}
	loaded, err := host.LoadPending("prod")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.HostKey != r.hostKey || loaded.HostKeyFingerprint != "SHA256:host" {
		t.Fatalf("pinned host key was not saved: %+v", loaded)
	}
}
