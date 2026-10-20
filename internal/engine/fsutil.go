package engine

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data through a temp file in the same directory:
// write, chmod, fsync, close, rename, then fsync the directory. Setting the
// mode before fsync makes the mode durable with the content.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, f, err := createTemp(dir, filepath.Base(path), perm)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	remove = false
	return syncDir(dir)
}

// WriteFileOnce writes path only if it does not exist. The publish step is a
// hard link from the fsynced temp file, which cannot replace an existing file.
// It reports existed=true and leaves the original untouched when path is
// already present. This is how a test effect makes duplicate application
// observable rather than silently overwriting the first one.
func WriteFileOnce(path string, data []byte, perm os.FileMode) (existed bool, err error) {
	dir := filepath.Dir(path)
	tmp, f, err := createTemp(dir, filepath.Base(path), perm)
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(tmp) }()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return false, err
	}
	if err = f.Chmod(perm); err != nil {
		_ = f.Close()
		return false, err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return false, err
	}
	if err = f.Close(); err != nil {
		return false, err
	}
	if err = os.Link(tmp, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return true, nil
		}
		return false, err
	}
	return false, syncDir(dir)
}

func createTemp(dir, name string, perm os.FileMode) (string, *os.File, error) {
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", nil, err
	}
	tmp := filepath.Join(dir, "."+name+"."+hex.EncodeToString(suffix)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // G304: temp file beside the caller's node path.
	if err != nil {
		return "", nil, err
	}
	return tmp, f, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: directory of a caller's node path.
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// EnsureDir creates a private node directory.
func EnsureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700) //nolint:gosec // G302: node state must not be group/world writable.
}

// ValidateNodeName keeps journal identities from escaping the state directory.
func ValidateNodeName(name string) error {
	if name == "" || len(name) > 64 {
		return fmt.Errorf("node name must be 1-64 bytes")
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return fmt.Errorf("node name has unsafe characters")
		}
	}
	return nil
}
