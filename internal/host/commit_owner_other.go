//go:build !unix

package host

func fileOwner(string) (int, int, error) { return -1, -1, nil }
