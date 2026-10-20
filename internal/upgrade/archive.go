package upgrade

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"

	"github.com/mbvlabs/eregion/internal/contract"
)

// extractBinary streams one regular file out of a tar.gz archive into a staged
// file beside the destination binary. The archive itself was already
// SHA256-verified. Path traversal, links, devices, duplicate matches, and
// oversized content are refused.
func extractBinary(archivePath, entry, destinationDir string, target Target) (string, error) {
	if err := validEntry(entry); err != nil {
		return "", err
	}
	file, err := os.Open(archivePath) //nolint:gosec // G304: verified artifact in the upgrade state directory.
	if err != nil {
		return "", contract.NewFault(contract.Unavailable, "verified artifact could not be opened", "Retry the download; no binary was replaced.")
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return "", contract.NewFault(contract.PreconditionFailed, "artifact is not a valid gzip archive", "Refuse the artifact; it does not match the signed release format.")
	}
	defer func() { _ = gzipReader.Close() }()
	reader := tar.NewReader(gzipReader)
	found := false
	for entries := 0; ; entries++ {
		if entries > 1000 {
			return "", contract.NewFault(contract.PreconditionFailed, "artifact contains too many entries", "Refuse the artifact; release archives contain one binary.")
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", contract.NewFault(contract.PreconditionFailed, "artifact tar header is invalid", "Refuse the artifact; it was tampered with or corrupted.")
		}
		name := path.Clean(header.Name)
		if path.IsAbs(header.Name) || name == ".." || len(name) >= 3 && name[:3] == "../" {
			return "", contract.NewFault(contract.PreconditionFailed, "artifact contains an unsafe path", "Refuse the artifact; release archives contain one relative binary path.")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			if header.Typeflag == tar.TypeDir {
				continue
			}
			return "", contract.NewFault(contract.PreconditionFailed, "artifact contains a link or special file", "Refuse the artifact; release archives contain one regular binary.")
		}
		if name != entry {
			if _, err := io.Copy(io.Discard, io.LimitReader(reader, MaxArtifactBytes+1)); err != nil {
				return "", contract.NewFault(contract.PreconditionFailed, "artifact entry could not be read", "Refuse the artifact; it was corrupted.")
			}
			continue
		}
		if found {
			return "", contract.NewFault(contract.PreconditionFailed, "artifact contains the binary more than once", "Refuse the artifact; the signed archive is ambiguous.")
		}
		found = true
		staged, err := stageReader(reader, destinationDir, target)
		if err != nil {
			return "", err
		}
		return staged, nil
	}
	return "", contract.NewFault(contract.PreconditionFailed, "artifact does not contain the declared binary", "Refuse the artifact; the signed manifest names a missing entry.")
}

func stageReader(reader io.Reader, directory string, target Target) (string, error) {
	temp, err := os.CreateTemp(directory, ".eregion-"+string(target)+"-upgrade-*")
	if err != nil {
		return "", contract.NewFault(contract.Unavailable, "staged binary could not be created beside the destination", "Check directory permissions and free space; no binary was replaced.")
	}
	name := temp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(name)
		}
	}()
	written, err := io.Copy(temp, io.LimitReader(reader, MaxArtifactBytes+1))
	if err != nil {
		_ = temp.Close()
		return "", contract.NewFault(contract.Unavailable, "staged binary could not be written", "Check free space; no binary was replaced.")
	}
	if written <= 0 || written > MaxArtifactBytes {
		_ = temp.Close()
		return "", contract.NewFault(contract.PreconditionFailed, "artifact binary size is outside the allowed range", "Refuse the artifact; no binary was replaced.")
	}
	if err := temp.Chmod(0o755); err != nil {
		_ = temp.Close()
		return "", contract.NewFault(contract.Unavailable, "staged binary permissions could not be set", "Check directory permissions; no binary was replaced.")
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return "", contract.NewFault(contract.Unavailable, "staged binary could not be synced", "Check the filesystem; no binary was replaced.")
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	remove = false
	return name, nil
}

// ensureReplaceable rejects symlinks, special files, and binaries writable by
// group or others. The destination must already exist as a regular file.
func ensureReplaceable(path string) (string, int64, os.FileMode, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", 0, 0, contract.NewFault(contract.NotFound, "target binary does not exist", "Install the binary first or pass --binary with the current operator/node path.")
	}
	if !info.Mode().IsRegular() {
		return "", 0, 0, contract.NewFault(contract.PreconditionFailed, "target binary is not a regular file", "Refuse symlinks and special files; replace the real installed binary path.")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		return "", 0, 0, contract.NewFault(contract.PreconditionFailed, "target binary is group- or world-writable", "chmod 0755 or stricter before upgrading; a writable binary is not a trusted baseline.")
	}
	sha, size, mode, err := fileSHA256(path, MaxArtifactBytes)
	if err != nil {
		return "", 0, 0, err
	}
	return sha, size, mode, nil
}

func backupDir(stateDir string, target Target) string {
	return filepath.Join(stateDir, string(target), "backups")
}

func downloadDir(stateDir string, target Target) string {
	return filepath.Join(stateDir, string(target), "downloads")
}

func formatBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}
