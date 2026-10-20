package host

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mbvlabs/eregion/internal/engine"
)

func ensureDir(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a real directory", path)
	}
	return os.Chmod(path, mode) //nolint:gosec // G302: the caller chooses a private mode for host state.
}

// writeFile writes atomically and applies ownership when bootstrap runs as
// root. The mode is always explicit; a zero mode is a programming error.
func writeFile(path string, data []byte, mode os.FileMode, owner, group string) error {
	if mode == 0 {
		return fmt.Errorf("refusing to write %s with mode 0000", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if err := engine.WriteFileAtomic(path, data, mode); err != nil {
		return err
	}
	return chownIfRoot(path, owner, group)
}

func chownIfRoot(path, owner, group string) error {
	if owner == "" && group == "" {
		return nil
	}
	if os.Geteuid() != 0 {
		return nil
	}
	uid, gid, err := resolveIDs(owner, group)
	if err != nil {
		return err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return err
	}
	return nil
}

func resolveIDs(owner, group string) (int, int, error) {
	uid, gid := -1, -1
	if owner != "" {
		account, err := user.Lookup(owner)
		if err != nil {
			return -1, -1, fmt.Errorf("look up user %s: %w", owner, err)
		}
		uid, err = strconv.Atoi(account.Uid)
		if err != nil {
			return -1, -1, fmt.Errorf("user %s has an invalid uid", owner)
		}
	}
	if group != "" {
		entry, err := user.LookupGroup(group)
		if err != nil {
			return -1, -1, fmt.Errorf("look up group %s: %w", group, err)
		}
		gid, err = strconv.Atoi(entry.Gid)
		if err != nil {
			return -1, -1, fmt.Errorf("group %s has an invalid gid", group)
		}
	}
	return uid, gid, nil
}

// fileState reports content and mode without following a symlink outside the
// intended path. A missing file is (nil, 0, false, nil).
func fileState(path string) ([]byte, os.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, false, nil
		}
		return nil, 0, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf("%s is not a regular file", path)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed host configuration path.
	if err != nil {
		return nil, 0, false, err
	}
	return raw, info.Mode().Perm(), true, nil
}

func contentSHA256(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func sameFileContent(path string, want []byte) (bool, os.FileMode, error) {
	raw, mode, exists, err := fileState(path)
	if err != nil || !exists {
		return false, mode, err
	}
	return bytes.Equal(raw, want), mode, nil
}

// writeSecretOnce writes a credential export only if it does not already exist
// with different content. It never overwrites an existing token.
func writeSecretOnce(path string, data []byte, owner, group string) (bool, error) {
	raw, mode, exists, err := fileState(path)
	if err != nil {
		return false, err
	}
	if exists {
		if !bytes.Equal(raw, data) {
			return false, fmt.Errorf("%s already contains a different credential; remove it deliberately or choose another output path", path)
		}
		if mode&0o077 != 0 {
			if err := os.Chmod(path, 0o600); err != nil {
				return false, err
			}
		}
		return false, chownIfRoot(path, owner, group)
	}
	return true, writeFile(path, data, 0o600, owner, group)
}

// writeReplacingSecret is used only for service configuration that must track
// the selected intent. The file is still atomic and owner-only.
func writeReplacingSecret(path string, data []byte, owner, group string) error {
	return writeFile(path, data, 0o600, owner, group)
}

func readPrivateFile(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must not be readable by group or others", path)
	}
	if info.Size() > max {
		return nil, fmt.Errorf("%s is too large", path)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: operator-provided private input file.
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// ReadPrivateInput reads bounded, owner-only bootstrap input without following symlinks.
func ReadPrivateInput(path string) ([]byte, error) { return readPrivateFile(path, 64<<10) }

func writePublicFile(path string, data []byte, owner, group string) error {
	return writeFile(path, data, 0o644, owner, group)
}

// removeIfEmpty removes a file only when it exists and is empty. It is used to
// roll back a partially handed-off credential file.
func removeIfEmpty(path string) {
	info, err := os.Stat(path)
	if err == nil && info.Mode().IsRegular() && info.Size() == 0 {
		_ = os.Remove(path)
	}
}

func trimTrailingNewline(raw []byte) []byte {
	return []byte(strings.TrimSuffix(string(raw), "\n"))
}
