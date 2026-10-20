package integrations

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
)

// ReadSecretFile reads one root-owned credential file. The action input carries
// only the path; the value never enters operation rows, events, River arguments,
// or logs.
func ReadSecretFile(path string, max int64) ([]byte, error) {
	if err := ValidateSecretRef(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, contract.NewFault(contract.Unavailable, "credential file is unavailable", "Create the referenced root-owned file before submitting the action.")
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > max {
		return nil, contract.NewFault(contract.PreconditionFailed, "credential file is not a safe regular file", "Use a regular 0600 file no larger than the documented bound; symlinks are rejected.")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, contract.NewFault(contract.PreconditionFailed, "credential file is readable by group or others", "Set mode 0600 on the credential file.")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, contract.NewFault(contract.Unavailable, "credential file could not be opened", "Check ownership and permissions.")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(raw)) > max {
		return nil, contract.NewFault(contract.PreconditionFailed, "credential file could not be read safely", "Check the file size and permissions.")
	}
	return raw, nil
}

func ValidateSecretRef(path string) error {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") || strings.Contains(path, "..") {
		return contract.NewFault(contract.ValidationFailed, "a safe absolute credential-file reference is required", "Reference a root-owned 0600 file; never put credential contents in action input.")
	}
	return nil
}
