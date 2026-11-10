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
		Host: "203.0.113.10", User: "root", Port: "22", Node: "main", AccessKey: "prod",
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
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "eregion", "workspaces.json"))
	r := &serverInit{
		answers: serverInitAnswers{
			AccessKey: "prod", Host: "203.0.113.10", User: "root", Port: "22",
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

func TestResumeRetainsPersistedWorkspaceBeforeAnyPlanOrApply(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "workspaces.json"))
	t.Setenv("EREGION_DATABASE_URL", "postgres://user:password@127.0.0.1:5432/db")
	pending := &host.PendingInit{
		Server: "a", Host: "203.0.113.10", User: "root", Port: 22,
		Node: "main", Workspace: "thelab", AccessKey: "a", Hostname: "app.example.com", AdminUser: "admin",
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

func TestResumeDryRunDoesNotAlterPendingWorkspace(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "workspaces.json"))
	t.Setenv("EREGION_DATABASE_URL", "postgres://user:password@127.0.0.1:5432/db")
	pending := &host.PendingInit{
		Server: "a", Host: "203.0.113.10", User: "root", Port: 22,
		Node: "main", Workspace: "thelab", AccessKey: "a", Hostname: "app.example.com", AdminUser: "admin",
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
	if resumed.answers.Workspace != "thelab" {
		t.Fatal("a later flagless resume lost the persisted workspace")
	}
}

func TestResumeAllowsTheSameWorkspaceAfterTheHostPhase(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "workspaces.json"))
	t.Setenv("EREGION_DATABASE_URL", "postgres://user:password@127.0.0.1:5432/db")
	pending := &host.PendingInit{
		Server: "a", Host: "203.0.113.10", User: "root", Port: 22,
		Node: "main", Workspace: "thelab", AccessKey: "a", Hostname: "app.example.com", AdminUser: "admin",
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

func TestResumeRefusesPendingWithoutWorkspaceAfterTheHostPhase(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "workspaces.json"))
	t.Setenv("EREGION_DATABASE_URL", "postgres://user:password@127.0.0.1:5432/db")
	pending := &host.PendingInit{
		Server: "a", Host: "203.0.113.10", User: "root", Port: 22,
		Node: "main", AccessKey: "a", Hostname: "app.example.com", AdminUser: "admin",
		Phases: map[string]string{phaseContact: "done", phaseSecrets: "done", phaseHost: "done"},
	}
	if err := pending.Save(); err != nil {
		t.Fatal(err)
	}
	r := &serverInit{a: &app{secrets: &secrets.Registry{}}, cmd: serverInitResumeCommand(t, "")}
	err := r.prepare()
	if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("introducing a workspace after host apply = %v, want precondition_failed", err)
	}
}

func TestServerInitThreadsOneExplicitStoreThroughInitPaths(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "env", "workspaces.json")
	flagPath := filepath.Join(t.TempDir(), "flag", "workspaces.json")
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
	pending := &host.PendingInit{Server: "thelab", Node: "main", Workspace: "thelab", AccessKey: "thelab", Phases: map[string]string{}}
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
	t.Setenv("EREGION_WORKSPACE_FILE", filepath.Join(t.TempDir(), "workspaces.json"))
	t.Setenv("EREGION_DATABASE_URL", "")
	pending := &host.PendingInit{
		Server: "a", Host: "203.0.113.10", User: "root", Port: 22,
		Node: "main", Workspace: "thelab", AccessKey: "a", Hostname: "app.example.com", AdminUser: "admin",
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

func TestServerInitAnswerRowsUseWorkspaceNotPrivateAccessKey(t *testing.T) {
	const privateKey = "historical-private"
	rows := serverInitAnswerRows(serverInitAnswers{
		User: "root", Host: "203.0.113.10", Port: "22",
		Node: "main", Workspace: "thelab", AccessKey: privateKey,
		Hostname: "app.example.com", AdminUser: "admin",
	})
	workspaceRows := 0
	for _, row := range rows {
		if row[0] == "Workspace" {
			workspaceRows++
			if row[1] != "thelab" {
				t.Fatalf("workspace row = %+v", row)
			}
		}
		if row[0] == "Context" || row[1] == privateKey {
			t.Fatalf("private access key reached the human answer rows: %+v", rows)
		}
	}
	if workspaceRows != 1 {
		t.Fatalf("workspace rows = %d, want exactly one: %+v", workspaceRows, rows)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		seen[row[0]] = true
	}
	for _, label := range []string{"Server", "Node", "Workspace", "Public hostname", "Admin user"} {
		if !seen[label] {
			t.Fatalf("answer rows are missing %s: %+v", label, rows)
		}
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
	if strings.Contains(unconfigured, "--assign-workspace") || strings.Contains(unconfigured, "run eregion apply") {
		t.Fatalf("status recommends a removed initialization path:\n%s", unconfigured)
	}
}

func TestVerifyWorkspaceIdentity(t *testing.T) {
	configured := controlplane.WorkspaceIdentity{Configured: true, ID: "ws_0123456789abcdef01234567", Name: "thelab"}
	if err := verifyWorkspaceIdentity("", controlplane.WorkspaceIdentity{}); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("missing requested workspace = %v", err)
	}
	if err := verifyWorkspaceIdentity("thelab", configured); err != nil {
		t.Fatalf("matching handoff: %v", err)
	}
	if err := verifyWorkspaceIdentity("thelab", controlplane.WorkspaceIdentity{}); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("requested workspace on an unconfigured node = %v", err)
	}
	err := verifyWorkspaceIdentity("other", configured)
	if contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("conflicting handoff = %v", err)
	}
	fault := contract.Classify(err)
	if !strings.Contains(fault.Hint, "eregion server init --resume --workspace-name thelab") {
		t.Fatalf("conflicting handoff hint lacks the actionable creation command: %q", fault.Hint)
	}
	if strings.Contains(fault.Hint, "Resume with --workspace ") {
		t.Fatalf("conflicting handoff hint still uses the rejected global selector: %q", fault.Hint)
	}
}
