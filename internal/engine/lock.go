//go:build unix

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrLockHeld means another local process holds the lock. It is the only
// portable way to distinguish contention from an I/O failure.
var ErrLockHeld = errors.New("node lock is held")

const lockPoll = 50 * time.Millisecond

// LockMode is exclusive or shared.
type LockMode int

const (
	LockExclusive LockMode = iota
	LockShared
)

// Release drops a lock acquired by Acquire.
type Release func()

// Acquire takes an exclusive or shared flock on path. A zero timeout tries
// once. The lock is node-local: the kernel releases it when the process dies,
// so it cannot coordinate different hosts or control-plane instances.
func Acquire(ctx context.Context, path string, mode LockMode, timeout time.Duration) (Release, error) {
	if err := EnsureDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: fixed node state path.
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if mode == LockShared {
		how = syscall.LOCK_SH
	}
	fd := int(f.Fd()) //nolint:gosec // G115: file descriptors fit in int.
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(fd, how|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, err
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, ErrLockHeld
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// WithFileLock runs fn holding an exclusive flock on path. It is for short
// read-modify-write cycles of node files, not for host mutations.
func WithFileLock(path string, fn func() error) error {
	if err := EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: fixed node state path.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fd := int(f.Fd()) //nolint:gosec // G115: file descriptors fit in int.
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(fd, syscall.LOCK_UN) }()
	return fn()
}
