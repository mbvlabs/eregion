//go:build unix

package host

import (
	"os"
	"syscall"
)

func ownershipMatches(info os.FileInfo, owner, group string) (bool, error) {
	uid, gid, err := resolveIDs(owner, group)
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (uid < 0 || int(stat.Uid) == uid) && (gid < 0 || int(stat.Gid) == gid), nil
}
