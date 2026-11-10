package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/spf13/cobra"
)

func TestInitPublishesOnlyVerifiedWorkspaceAndResumesIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.json")
	t.Setenv("EREGION_WORKSPACE_FILE", path)
	r := &serverInit{cmd: &cobra.Command{}, answers: serverInitAnswers{AccessKey: "thelab", Workspace: "thelab"},
		workspace: controlplane.WorkspaceIdentity{Configured: true, ID: "ws_0123456789abcdef01234567", Name: "thelab"}}
	route := &host.WorkspaceSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: "/example/known_hosts"}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unexpected store before verification: %v", err)
	}
	if err := r.saveInitializedWorkspace(route); err != nil {
		t.Fatal(err)
	}
	if err := r.saveInitializedWorkspace(route); err != nil {
		t.Fatalf("resume publication: %v", err)
	}
	store, err := host.LoadWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	access, ok := store.AccessByName("thelab")
	if !ok || access.Workspace == nil || access.Workspace.ID != r.workspace.ID || !host.SameWorkspaceRoute(access.SSH, route) {
		t.Fatalf("incorrect saved access: %+v", access)
	}
	changed := *route
	changed.Address = "10.78.0.2:22"
	if err := r.saveInitializedWorkspace(&changed); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("changed route publication = %v, want conflict", err)
	}
}

func TestFreshInitDoesNotAdoptOrDiscardOldCredentials(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("EREGION_WORKSPACE_FILE", "")
	t.Setenv("EREGION_CONTEXT", "")
	t.Setenv("EREGION_CONTEXT_FILE", "")
	t.Setenv("EREGION_WORKSPACE", "")
	t.Setenv("EREGION_DATABASE_URL", "postgres://user:password@db.example.com/db?sslmode=verify-full")
	storePath, err := host.DefaultWorkspacePath()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(storePath)
	credential := filepath.Join(base, "servers", "old", "ssh_ed25519")
	pending := filepath.Join(base, "pending", "old", host.PendingInitFile)
	for _, path := range []string{credential, pending} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{credential: "old-private-key\n", pending: `{"schema":1,"server":"old","context":"old"}`, filepath.Join(base, "contexts.json"): `{"schema":1,"contexts":[]}`}
	for path, contents := range files {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	makeInit := func(name string) *serverInit {
		a := &app{secrets: &secrets.Registry{}}
		cmd := &cobra.Command{}
		a.serverSpecs()[0].children[0].flags(cmd)
		for flag, value := range map[string]string{"host": "root@203.0.113.10", "hostname": "app.example.com", "workspace-name": name} {
			if err := cmd.Flags().Set(flag, value); err != nil {
				t.Fatal(err)
			}
		}
		return &serverInit{a: a, cmd: cmd}
	}
	if err := makeInit("old").prepare(); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("old pending accepted: %v", err)
	}
	// No pending file: the occupied credential directory independently blocks adoption.
	if err := os.Rename(pending, pending+".held"); err != nil {
		t.Fatal(err)
	}
	if err := makeInit("old").prepare(); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("old credentials accepted: %v", err)
	}
	if err := os.Rename(pending+".held", pending); err != nil {
		t.Fatal(err)
	}
	if err := makeInit("new").prepare(); err != nil {
		t.Fatalf("unused fresh name: %v", err)
	}
	discard := makeInit("old")
	if err := discard.cmd.Flags().Set("confirm", "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := discard.discard(); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unsupported discard = %v", err)
	}
	for path, want := range files {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("legacy fixture changed: %s: %q, %v", path, got, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(base, "servers", "new")); !os.IsNotExist(err) {
		t.Fatalf("fresh preparation created credentials: %v", err)
	}
}

func TestInitClientNeverFallsBackToAmbientWorkspace(t *testing.T) {
	t.Setenv("EREGION_WORKSPACE", "other")
	r := &serverInit{cmd: &cobra.Command{}}
	if _, err := r.initNodeClient(); err == nil {
		t.Fatal("missing pending route used ambient workspace")
	}
}

func TestFreshInitRefusesSymlinkedCredentialDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "missing-target")
	link := filepath.Join(root, "credentials")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := refuseOccupiedInitPath(link); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("symlink was not refused: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("refusal modified symlink: %v", err)
	}
}

func TestInitVerifiesAndChecksReadyBeforePublicationOnResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.json")
	t.Setenv("EREGION_WORKSPACE_FILE", path)
	identity := controlplane.WorkspaceIdentity{Configured: true, ID: "ws_0123456789abcdef01234567", Name: "thelab"}
	transport := &initWorkspaceTransport{name: "thelab", workspace: identity, failReady: true}
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	r := &serverInit{cmd: cmd, answers: serverInitAnswers{AccessKey: "thelab", Workspace: "thelab", Node: "main"},
		pending:    &host.PendingInit{Server: "thelab", Phases: map[string]string{}},
		initClient: &nodeClient{remote: transport, operator: "admin", stderr: io.Discard}}
	route := &host.WorkspaceSSH{Address: "10.78.0.1:22", User: "admin", KnownHosts: "/example/known_hosts"}
	if err := r.publishReadyWorkspace(context.Background(), route); err == nil {
		t.Fatal("failed readiness accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("published before readiness: %v", err)
	}
	if r.pending.WorkspaceID != identity.ID || r.pending.Done(phaseWorkspace) {
		t.Fatalf("wrong pending checkpoint: %+v", r.pending)
	}
	reloaded, err := host.LoadPendingAt(path, "thelab")
	if err != nil {
		t.Fatal(err)
	}
	r = &serverInit{cmd: cmd, answers: answersFromPending(reloaded), pending: reloaded,
		initClient: &nodeClient{remote: transport, operator: "admin", stderr: io.Discard}}
	transport.failReady = false
	if err := r.publishReadyWorkspace(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	if !r.pending.Done(phaseWorkspace) {
		t.Fatal("publication phase not saved")
	}
	if err := r.publishReadyWorkspace(context.Background(), route); err != nil {
		t.Fatalf("idempotent resume: %v", err)
	}
	transport.workspace.ID = "ws_aaaaaaaaaaaaaaaaaaaaaaaa"
	if err := r.publishReadyWorkspace(context.Background(), route); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("changed identity = %v", err)
	}
}
