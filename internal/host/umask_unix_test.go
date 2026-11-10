//go:build unix

package host

import "syscall"

func withUmaskTest(mask int, fn func()) {
	previous := syscall.Umask(mask)
	defer syscall.Umask(previous)
	fn()
}
