package cli

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
)

func TestImagePushTakesTheNodeHostLock(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("EREGION_NODE_STATE_DIR", stateDir)
	lockPath := filepath.Join(stateDir, "host.lock")
	if got := nodeHostLockPath(); got != lockPath {
		t.Fatalf("host lock path = %q, want %q", got, lockPath)
	}
	release, err := engine.Acquire(context.Background(), lockPath, engine.LockExclusive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, err = pushNodeImage(context.Background(), rpcImagePushParams{Repository: "andurel", Tag: "v1"}, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "another host mutation") {
		t.Fatalf("an image push ignored the host lock: %v", err)
	}
	release()
}

func TestImagePushLockUsesTheNodeStateDirectory(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("EREGION_NODE_STATE_DIR", stateDir)
	release, err := acquireNodeHostLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := nodeHostLockPath(); got != filepath.Join(stateDir, "host.lock") {
		t.Fatalf("host lock path = %q", got)
	}
	release()
	// The lock must be free again after the release.
	second, err := engine.Acquire(context.Background(), filepath.Join(stateDir, "host.lock"), engine.LockExclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second()
}
