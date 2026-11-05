//go:build !linux && !darwin

package api

import "os"

// authOwnership is unavailable on this platform; the portable size, mode,
// mtime and file-identity checks still apply.
func authOwnership(os.FileInfo) (uid, gid uint32, ctimeSec, ctimeNsec int64, ok bool) {
	return 0, 0, 0, 0, false
}
