//go:build !unix

package host

import "os"

func ownershipMatches(os.FileInfo, string, string) (bool, error) { return true, nil }

func fileUIDGID(os.FileInfo) (uint32, uint32, bool) { return 0, 0, false }
