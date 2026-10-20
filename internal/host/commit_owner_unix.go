//go:build unix

package host

import (
	"os"
	"syscall"
)

// fileOwner returns the numeric owner of path. A missing file is -1,-1.
func fileOwner(path string) (int, int, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return -1, -1, nil
		}
		return -1, -1, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1, nil
	}
	return int(stat.Uid), int(stat.Gid), nil
}
