package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// interleavingWorkspaceDatabase lets the test hold the apply lock while the
// second apply has already passed its advisory workspace read. Releasing the
// lock after changing the authoritative name proves the in-lock re-read stops
// the stale apply before state, journal, credentials or host steps.
type failHostExec struct{}

func (failHostExec) Run(context.Context, string, string, map[string]string, engine.Reporter) error {
	return errors.New("unexpected host mutation")
}
func (failHostExec) Output(context.Context, string) (string, error) {
	return "", errors.New("unexpected host observation")
}

type interleavingWorkspaceDatabase struct {
	early    chan struct{}
	conflict chan struct{}
	once     sync.Once
	reads    atomic.Int32
}

func (d *interleavingWorkspaceDatabase) Inspect(context.Context, string) (DatabaseState, error) {
	return DatabaseState{}, nil
}
func (d *interleavingWorkspaceDatabase) Migrate(context.Context, string) (DatabaseState, error) {
	return DatabaseState{}, nil
}
func (d *interleavingWorkspaceDatabase) AssignWorkspace(context.Context, string, string, controlplane.RegistryHostIdentity) (controlplane.WorkspaceIdentity, error) {
	return controlplane.WorkspaceIdentity{}, errors.New("unexpected assignment")
}
func (d *interleavingWorkspaceDatabase) ReadWorkspace(context.Context, string) (controlplane.WorkspaceIdentity, error) {
	identity := controlplane.WorkspaceIdentity{Configured: true, ID: "ws_0123456789abcdef01234567", Name: "thelab", RegistryHost: testWorkspaceRegistryHostPtr()}
	select {
	case <-d.conflict:
		identity = controlplane.WorkspaceIdentity{Configured: true, ID: "ws_aaaaaaaaaaaaaaaaaaaaaaaa", Name: "otherlab", RegistryHost: testWorkspaceRegistryHostPtr()}
	default:
	}
	// Capture the first result before allowing the test to change authority.
	// Otherwise the advisory read itself could conflict and hide a missing
	// second read under the apply lock.
	d.reads.Add(1)
	d.once.Do(func() { close(d.early) })
	return identity, nil
}

func TestApplyRechecksWorkspaceAuthorityUnderTheApplyLock(t *testing.T) {
	cfg := fixture(t, RoleMain)
	cfg.Workspace = "thelab"
	now := time.Unix(1800000000, 0).UTC()
	runner := newRunner(cfg, &secrets.Registry{})
	runner.SkipRootCheck = true
	runner.Exec = failHostExec{}
	runner.Now = func() time.Time { return now }
	database := &interleavingWorkspaceDatabase{early: make(chan struct{}), conflict: make(chan struct{})}
	runner.Database = database
	runner.RegistryHostIdentity = testWorkspaceRegistryHostPtr()
	if err := ensureState(cfg); err != nil {
		t.Fatal(err)
	}

	token, err := func() (Confirmation, error) {
		configHash, err := cfg.ConfigHash()
		if err != nil {
			return Confirmation{}, err
		}
		projected, err := projectState(cfg, configHash, now)
		if err != nil {
			return Confirmation{}, err
		}
		preview, err := LoadCredentials(cfg, false)
		if err != nil {
			return Confirmation{}, err
		}
		changes, err := AccessChanges(cfg, preview)
		if err != nil {
			return Confirmation{}, err
		}
		risks := append(append([]string(nil), cfg.AccessRisks()...), changeStrings(changes)...)
		return issueConfirmation("host.apply", cfg.Node+"/"+string(cfg.Role), configHash, planInputs(configHash, projected.Generation, risks), now)
	}()
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	// Simulate an active first apply holding the whole-apply lock.
	release, err := engine.Acquire(ctx, filepath.Join(cfg.Layout.StateDir, "apply.lock"), engine.LockExclusive, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		err error
	}
	result := make(chan outcome, 1)
	go func() {
		_, err := runner.Apply(ctx, token.Token, false)
		result <- outcome{err: err}
	}()
	select {
	case <-database.early:
	case <-time.After(5 * time.Second):
		release()
		t.Fatal("the second apply did not reach its advisory workspace read")
	}
	// The active apply finishes with a different authoritative workspace name,
	// then releases the lock. The waiting apply must re-read it.
	close(database.conflict)
	release()
	select {
	case got := <-result:
		if contract.Classify(got.err).Code != contract.Conflict {
			t.Fatalf("interleaved apply = %v, want conflict", got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second apply did not finish after the lock was released")
	}
	if got := database.reads.Load(); got != 2 {
		t.Fatalf("workspace authority was read %d times, want advisory and locked reads", got)
	}

	if _, err := os.Stat(statePath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a conflicting apply wrote bootstrap state: %v", err)
	}
	if _, err := os.Stat(JournalPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a conflicting apply wrote the journal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Layout.StateDir, "effects")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a conflicting apply wrote effect records: %v", err)
	}
}
