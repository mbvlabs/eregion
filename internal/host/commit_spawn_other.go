//go:build !unix

package host

import "errors"

func spawnDetached(string) error {
	return errors.New("the commit-confirm watchdog requires a Unix host")
}
