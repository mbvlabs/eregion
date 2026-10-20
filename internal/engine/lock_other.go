//go:build !unix

package engine

import (
	"context"
	"errors"
	"time"
)

// ErrLockHeld means another local process holds the lock.
var ErrLockHeld = errors.New("node lock is held")

type LockMode int

const (
	LockExclusive LockMode = iota
	LockShared
)

type Release func()

// Acquire is unavailable outside Unix. Node execution is a Linux feature; the
// portable CLI still builds and returns this error rather than silently
// running without host coordination.
func Acquire(context.Context, string, LockMode, time.Duration) (Release, error) {
	return nil, errors.New("node locks require a Unix host")
}

func WithFileLock(string, func() error) error {
	return errors.New("node locks require a Unix host")
}
