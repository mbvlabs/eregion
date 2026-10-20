//go:build !unix

package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// acquireLock uses an exclusive create on platforms without flock. A killed
// process can leave the lock file behind; the operator removes it after
// confirming no upgrade is running. The journal remains the recovery record.
func acquireLock(ctx context.Context, path string, timeout time.Duration) (func(), error) {
	if err := ensureStateDir(filepath.Dir(path)); err != nil {
		return nil, contract.NewFault(contract.Unavailable, "upgrade state directory is not usable", "Check state-directory ownership and permissions.")
	}
	deadline := time.Now().Add(timeout)
	for {
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: state-directory path.
		if err == nil {
			_ = file.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, contract.NewFault(contract.Unavailable, "upgrade lock could not be acquired", "Check the upgrade state directory filesystem.")
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			return nil, contract.NewFault(contract.Conflict, "another upgrade or rollback holds the lock", "Wait for the other process to finish; on this platform remove a stale lock file only after confirming no upgrade is running.")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
