//go:build unix

package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// acquireLock takes an exclusive flock on path. The kernel releases it when the
// process dies, so an interrupted upgrade cannot leave a live lock behind.
func acquireLock(ctx context.Context, path string, timeout time.Duration) (func(), error) {
	if err := ensureStateDir(filepath.Dir(path)); err != nil {
		return nil, contract.NewFault(contract.Unavailable, "upgrade state directory is not usable", "Check state-directory ownership and permissions.")
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: state-directory path.
	if err != nil {
		return nil, contract.NewFault(contract.Unavailable, "upgrade lock could not be opened", "Check state-directory ownership and permissions.")
	}
	fd := int(file.Fd()) //nolint:gosec // G115: file descriptors fit in int.
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(fd, syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = file.Close()
			return nil, contract.NewFault(contract.Unavailable, "upgrade lock could not be acquired", "Check the upgrade state directory filesystem.")
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, contract.NewFault(contract.Conflict, "another upgrade or rollback holds the lock", "Wait for the other process to finish; never run two replacements concurrently.")
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
