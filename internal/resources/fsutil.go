package resources

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func randomHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func ensurePrivateDir(path string) error {
	if path == "" {
		return errors.New("directory is required")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	// Dedicated service directories are private. A shared directory is not a
	// safe destination for credential or database backup files.
	return os.Chmod(path, 0o700)
}

func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return nil
}

func syncDir(dir string) error {
	handle, err := os.Open(dir) //nolint:gosec // G304: a service-owned state/backup directory.
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }()
	return handle.Sync()
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := ensureDir(dir); err != nil {
		return err
	}
	suffix, err := randomHex(6)
	if err != nil {
		return err
	}
	temp := filepath.Join(dir, "."+filepath.Base(path)+"."+suffix+".tmp")
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // G304: path is service-owned.
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(temp)
		}
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	remove = false
	return syncDir(dir)
}

// writeOnce publishes a file without replacing an existing one.
func writeOnce(path string, data []byte, perm os.FileMode) (existed bool, err error) {
	dir := filepath.Dir(path)
	if err := ensureDir(dir); err != nil {
		return false, err
	}
	suffix, err := randomHex(6)
	if err != nil {
		return false, err
	}
	temp := filepath.Join(dir, "."+filepath.Base(path)+"."+suffix+".tmp")
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // G304: path is service-owned.
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(temp) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if err := os.Link(temp, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return true, nil
		}
		return false, err
	}
	return false, syncDir(dir)
}

func readRegular(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > max {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, max)
	}
	return os.ReadFile(path) //nolint:gosec // G304: a validated service-owned file.
}

func fileSHA256(path string) (string, int64, error) {
	file, err := os.Open(path) //nolint:gosec // G304: a validated service-owned file.
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

// withinDir resolves path under root and refuses traversal and symlinks.
func withinDir(root, path string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	pathAbs := path
	if !filepath.IsAbs(pathAbs) {
		pathAbs = filepath.Join(rootAbs, pathAbs)
	}
	pathAbs, err = filepath.Abs(pathAbs)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return "", fmt.Errorf("%s is outside %s", path, rootAbs)
	}
	info, err := os.Lstat(pathAbs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	return pathAbs, nil
}
