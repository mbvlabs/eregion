//go:build darwin

package api

import (
	"os"
	"syscall"
)

// authOwnership reports the metadata that changes when an owner-only file is
// chowned or chmodded after publish. ctime catches access changes that leave
// size, mode and mtime untouched.
func authOwnership(info os.FileInfo) (uid, gid uint32, ctimeSec, ctimeNsec int64, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, 0, false
	}
	return stat.Uid, stat.Gid, stat.Ctimespec.Sec, stat.Ctimespec.Nsec, true
}
