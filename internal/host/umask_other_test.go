//go:build !unix

package host

func withUmaskTest(_ int, fn func()) { fn() }
