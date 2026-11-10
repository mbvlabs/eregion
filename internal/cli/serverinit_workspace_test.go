package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/spf13/cobra"
)

func validServerInitAnswers() serverInitAnswers {
	return serverInitAnswers{
		Host: "203.0.113.10", User: "root", Port: "22", Node: "main", Context: "prod",
		Hostname: "app.example.com", AdminUser: "admin", Workspace: "thelab",
		DatabaseURL: "postgres://user:pass@db.example.com/app?sslmode=verify-full",
	}
}

func TestServerInitValidateRequiresAndValidatesAWorkspace(t *testing.T) {
	valid := validServerInitAnswers()
	r := &serverInit{a: &app{secrets: &secrets.Registry{}}, cmd: &cobra.Command{}, answers: valid}
	if err := r.validate(); err != nil {
		t.Fatalf("valid answers: %v", err)
	}

	missing := valid
	missing.Workspace = ""
	r = &serverInit{a: &app{secrets: &secrets.Registry{}}, cmd: &cobra.Command{}, answers: missing}
	if err := r.validate(); contract.Classify(err).Code != contract.InputRequired {
		t.Fatalf("missing workspace = %v, want input_required", err)
	}

	invalid := valid
	invalid.Workspace = "The Lab"
	r = &serverInit{a: &app{secrets: &secrets.Registry{}}, cmd: &cobra.Command{}, answers: invalid}
	if err := r.validate(); contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("invalid workspace = %v, want validation_failed", err)
	}
}

func TestSavePendingCarriesWorkspaceWithoutInventingAnID(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "eregion", "contexts.json"))
	r := &serverInit{
		answers: serverInitAnswers{
			Context: "prod", Host: "203.0.113.10", User: "root", Port: "22",
			Node: "main", Workspace: "thelab", Hostname: "app.example.com", AdminUser: "admin",
		},
		pending: &host.PendingInit{Server: "prod", Phases: map[string]string{}},
	}
	if err := r.savePending(); err != nil {
		t.Fatal(err)
	}
	loaded, err := host.LoadPending("prod")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace != "thelab" || loaded.WorkspaceID != "" {
		t.Fatalf("pending workspace = %q/%q, want thelab with no generated ID", loaded.Workspace, loaded.WorkspaceID)
	}

	r.workspace = controlplane.WorkspaceIdentity{Configured: true, ID: "ws_0123456789abcdef01234567", Name: "thelab"}
	if err := r.savePending(); err != nil {
		t.Fatal(err)
	}
	loaded, err = host.LoadPending("prod")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace != "thelab" || loaded.WorkspaceID != "ws_0123456789abcdef01234567" {
		t.Fatalf("authoritative workspace was not persisted: %+v", loaded)
	}
}

func serverInitResumeCommand(t *testing.T, workspace string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	flags := cmd.Flags()
	flags.Bool("resume", true, "")
	flags.Bool("dry-run", false, "")
	flags.String("workspace-name", "", "")

	flags.String("identity", "", "")
	flags.Bool("password-stdin", false, "")
	flags.String("host-key-fingerprint", "", "")
	flags.String("secrets-out", "", "")
	if err := flags.Set("resume", "true"); err != nil {
		t.Fatal(err)
	}
	if workspace != "" {
		if err := flags.Set("workspace-name", workspace); err != nil {
			t.Fatal(err)
		}
	}
	return cmd
}

func TestResumePersistsChosenWorkspaceBeforeAnyPlanOrApply(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	t.Setenv("EREGION_DATABASE_URL", "postgres://user:password@127.0.0.1:5432/db")
	pending := &host.PendingInit{
		Server: "a", Host: "203.0.113.10", User: "root", Port: 22,
		Node: "main", Context: "a", Hostname: "app.example.com", AdminUser: "admin",
		Phases: map[string]string{phaseContact: "done", phaseSecrets: "done"},
	}
	if err := pending.Save(); err != nil {
		t.Fatal(err)
	}
	first := &serverInit{a: &app{secrets: &secrets.Registry{}}, cmd: serverInitResumeCommand(t, "thelab")}
	if err := first.prepare(); err != nil {
		t.Fatal(err)
	}
	stored, err := host.LoadPending("a")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Workspace != "thelab" {
		t.Fatalf("prepare did not persist the chosen workspace before plan/apply: %+v", stored)
	}

	// A later flagless resume after an interruption must retain it even though
	// contact and secrets were already complete.
	second := &serverInit{a: &app{secrets: &secrets.Registry{}}, cmd: serverInitResumeCommand(t, "")}
	if err := second.prepare(); err != nil {
		t.Fatal(err)
	}
	if second.answers.Workspace != "thelab" {
		t.Fatalf("flagless resume workspace = %q, want thelab", second.answers.Workspace)
	}
}

func TestResumeDryRunDoesNotPersistAWorkspaceChoice(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	t.Setenv("EREGION_DATABASE_URL", "postgres://user:password@127.0.0.1:5432/db")
	pending := &host.PendingInit{
		Server: "a", Host: "203.0.113.10", User: "root", Port: 22,
		Node: "main", Context: "a", Hostname: "app.example.com", AdminUser: "admin",
		Phases: map[string]string{phaseContact: "done", phaseSecrets: "done"},
	}
	if err := pending.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := host.LoadPendingFile("a", host.PendingInitFile)
	if err != nil {
		t.Fatal(err)
	}
	cmd := serverInitResumeCommand(t, "thelab")
	if err := cmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatal(err)
	}
	preview := &serverInit{a: &app{secrets: &secrets.Registry{}}, cmd: cmd}
	if err := preview.prepare(); err != nil {
		t.Fatal(err)
	}
	if preview.answers.Workspace != "thelab" {
		t.Fatal("the preview did not use the requested workspace in memory")
	}
	after, err := host.LoadPendingFile("a", host.PendingInitFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("dry-run preparation changed the durable pending record")
	}
	resumed := &serverInit{a: &app{secrets: &secrets.Registry{}}, cmd: serverInitResumeCommand(t, "")}
	if err := resumed.prepare(); err != nil {
		t.Fatal(err)
	}
	if resumed.answers.Workspace != "" || !resumed.workspaceOptional {
		t.Fatal("a later flagless resume adopted the preview-only workspace")
	}
}

func TestResumeAllowsTheSameWorkspaceAfterTheHostPhase(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	t.Setenv("EREGION_DATABASE_URL", "postgres://user:password@127.0.0.1:5432/db")
	pending := &host.PendingInit{
		Server: "a", Host: "203.0.113.10", User: "root", Port: 22,
		Node: "main", Workspace: "thelab", Context: "a", Hostname: "app.example.com", AdminUser: "admin",
		Phases: map[string]string{phaseContact: "done", phaseSecrets: "done", phaseHost: "done"},
	}
	if err := pending.Save(); err != nil {
		t.Fatal(err)
	}
	r := &serverInit{a: &app{secrets: &secrets.Registry{}}, cmd: serverInitResumeCommand(t, "thelab")}
	if err := r.prepare(); err != nil {
		t.Fatalf("same persisted workspace after host phase: %v", err)
	}
}

func TestResumeRefusesIntroducingAWorkspaceAfterTheHostPhase(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	t.Setenv("EREGION_DATABASE_URL", "postgres://user:password@127.0.0.1:5432/db")
	pending := &host.PendingInit{
		Server: "a", Host: "203.0.113.10", User: "root", Port: 22,
		Node: "main", Context: "a", Hostname: "app.example.com", AdminUser: "admin",
		Phases: map[string]string{phaseContact: "done", phaseSecrets: "done", phaseHost: "done"},
	}
	if err := pending.Save(); err != nil {
		t.Fatal(err)
	}
	r := &serverInit{a: &app{secrets: &secrets.Registry{}}, cmd: serverInitResumeCommand(t, "thelab")}
	err := r.prepare()
	if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("introducing a workspace after host apply = %v, want precondition_failed", err)
	}
	stored, loadErr := host.LoadPending("a")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if stored.Workspace != "" {
		t.Fatalf("refused workspace was persisted: %+v", stored)
	}
}

func TestServerInitThreadsOneExplicitStoreThroughInitPaths(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "env", "contexts.json")
	flagPath := filepath.Join(t.TempDir(), "flag", "contexts.json")
	t.Setenv("EREGION_WORKSPACE_FILE", envPath)
	cmd := &cobra.Command{}
	cmd.Flags().String("workspace-file", "", "")
	if err := cmd.Flags().Set("workspace-file", flagPath); err != nil {
		t.Fatal(err)
	}
	store, err := loadWorkspaceStore(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if store.Path != flagPath {
		t.Fatalf("resolved store = %q, want flag path %q", store.Path, flagPath)
	}
	r := &serverInit{a: &app{secrets: &secrets.Registry{}}, cmd: cmd, storePath: store.Path}
	dir, err := r.identityDir("thelab")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(flagPath), "servers", "thelab"); dir != want {
		t.Fatalf("identity dir = %q, want %q", dir, want)
	}
	pending := &host.PendingInit{Server: "thelab", Node: "main", Context: "thelab", Phases: map[string]string{}}
	if err := r.savePendingRecord(pending); err != nil {
		t.Fatal(err)
	}
	if !host.PendingExistsAt(flagPath, "thelab") {
		t.Fatal("pending init was not stored under the explicit flag store")
	}
	if host.PendingExistsAt(envPath, "thelab") {
		t.Fatal("pending init leaked into the environment-selected store")
	}
}

func TestServerInitRejectsSharedStoreDirectories(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one.json", "two.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := rejectSharedInitStoreRoot(filepath.Join(dir, "one.json")); contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("shared store directory = %v, want validation_failed", err)
	}
	isolated := filepath.Join(t.TempDir(), "one.json")
	if err := rejectSharedInitStoreRoot(isolated); err != nil {
		t.Fatalf("isolated store directory refused: %v", err)
	}
}

func TestResumeRefusesAConflictingWorkspaceBeforeContact(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	t.Setenv("EREGION_DATABASE_URL", "")
	pending := &host.PendingInit{
		Server: "a", Host: "203.0.113.10", User: "root", Port: 22,
		Node: "main", Workspace: "thelab", Context: "a", Hostname: "app.example.com", AdminUser: "admin",
		Phases: map[string]string{},
	}
	if err := pending.Save(); err != nil {
		t.Fatal(err)
	}
	a := newApp()
	a.root.SetArgs([]string{"server", "init", "--resume", "--workspace-name", "other", "--json"})
	err := a.root.Execute()
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("conflicting resume = %v, want conflict before contact", err)
	}
	if got, err := host.LoadPending("a"); err != nil || got.Workspace != "thelab" {
		t.Fatalf("pending workspace changed before refusal: %+v, %v", got, err)
	}
}

func TestStatusTextShowsWorkspaceAndNode(t *testing.T) {
	text := statusText(host.NodeStatus{
		Node: "main", Role: "main", NodeVersion: "dev",
		Workspace: controlplane.WorkspaceIdentity{Configured: true, ID: "ws_0123456789abcdef01234567", Name: "thelab"},
	})
	for _, want := range []string{"node", "main", "workspace", "thelab", "ws_0123456789abcdef01234567"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status lacks %q:\n%s", want, text)
		}
	}
	unconfigured := statusText(host.NodeStatus{Node: "main", Role: "main", NodeVersion: "dev"})
	if !strings.Contains(unconfigured, "not configured") {
		t.Fatalf("unconfigured status is not explicit:\n%s", unconfigured)
	}
}

func TestVerifyWorkspaceIdentity(t *testing.T) {
	configured := controlplane.WorkspaceIdentity{Configured: true, ID: "ws_0123456789abcdef01234567", Name: "thelab"}
	if err := verifyWorkspaceIdentity("", controlplane.WorkspaceIdentity{}); err != nil {
		t.Fatalf("unconfigured legacy handoff: %v", err)
	}
	if err := verifyWorkspaceIdentity("thelab", configured); err != nil {
		t.Fatalf("matching handoff: %v", err)
	}
	if err := verifyWorkspaceIdentity("thelab", controlplane.WorkspaceIdentity{}); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("requested workspace on an unconfigured node = %v", err)
	}
	if err := verifyWorkspaceIdentity("other", configured); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("conflicting handoff = %v", err)
	}
}
