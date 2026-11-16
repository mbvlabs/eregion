//go:build !unix

package host

import (
	"context"
	"errors"
)

func spawnDetached(context.Context, string) error {
	return errors.New("the commit-confirm watchdog requires a Unix host")
}
