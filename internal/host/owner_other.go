//go:build !unix

package host

import "os"

func ownershipMatches(os.FileInfo, string, string) (bool, error) { return true, nil }
