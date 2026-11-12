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

func TestImagePushRejectsInvalidAppIdentityBeforeUpload(t *testing.T) {
	_, err := pushNodeImage(context.Background(), rpcImagePushParams{Repository: "Not-An-App", Tag: "v1"}, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "invalid application name") {
		t.Fatalf("invalid app identity = %v", err)
	}
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
