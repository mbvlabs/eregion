package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// MaxArtifactBytes bounds one downloaded archive and one extracted binary.
const MaxArtifactBytes int64 = 1 << 30

// MaxManifestBytes bounds a signed manifest fetched from disk or HTTPS.
const MaxManifestBytes int64 = 1 << 20

func ensureStateDir(path string) error {
	if path == "" {
		return fmt.Errorf("upgrade state directory is empty")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("upgrade state directory %s is not a real directory", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("could not restrict upgrade state directory permissions: %w", err)
		}
	}
	return nil
}

// writeFileAtomic writes through a temp file in the same directory, fsyncs,
// renames, and fsyncs the directory. Sensitive files use 0600.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	remove = false
	return syncDir(dir)
}

// copyFileAtomic copies src to dst through a temp file in dst's directory and
// returns the SHA256 of the copied bytes. The source must be a regular file; a
// symlink is never followed.
func copyFileAtomic(dst, src string, perm os.FileMode, limit int64) (string, int64, error) {
	info, err := os.Lstat(src)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("source %s is not a regular file", src)
	}
	if limit > 0 && info.Size() > limit {
		return "", 0, fmt.Errorf("source %s exceeds the size bound", src)
	}
	in, err := os.Open(src) //nolint:gosec // G304: caller-selected binary/backup inside the upgrade state.
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = in.Close() }()
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tmpName)
		}
	}()
	hash := sha256.New()
	writer := io.MultiWriter(tmp, hash)
	var written int64
	if limit > 0 {
		written, err = io.Copy(writer, io.LimitReader(in, limit+1))
		if err == nil && written > limit {
			err = fmt.Errorf("source %s exceeds the size bound", src)
		}
	} else {
		written, err = io.Copy(writer, in)
	}
	if err != nil {
		_ = tmp.Close()
		return "", 0, err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return "", 0, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", 0, err
	}
	remove = false
	if err := syncDir(dir); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), written, nil
}

// fileSHA256 hashes a regular file and returns its size and permission bits.
func fileSHA256(path string, limit int64) (string, int64, os.FileMode, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", 0, 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, 0, fmt.Errorf("%s is not a regular file", path)
	}
	if limit > 0 && info.Size() > limit {
		return "", 0, 0, fmt.Errorf("%s exceeds the size bound", path)
	}
	file, err := os.Open(path) //nolint:gosec // G304: caller-selected installed binary.
	if err != nil {
		return "", 0, 0, err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", 0, 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), info.Size(), info.Mode().Perm(), nil
}

func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := file.Sync(); err != nil {
		if errors.Is(err, os.ErrInvalid) || runtime.GOOS == "windows" {
			return nil
		}
		return err
	}
	return nil
}
