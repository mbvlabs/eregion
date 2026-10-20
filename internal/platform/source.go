package platform

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
)

// DefaultBuildSourceRoots is the only default directory from which a directory
// source build may read. A deployment may configure additional roots explicitly;
// there is no fallback to the server's whole filesystem.
const DefaultBuildSourceRoot = "/srv/eregion/sources"

// BuildSourceRoots returns the configured directory-build allowlist. The value
// is a path list, so a launcher can add a repository example directory without
// widening the default to /home or /etc.
func BuildSourceRoots() []string {
	raw := os.Getenv("EREGION_BUILD_SOURCE_ROOTS")
	if strings.TrimSpace(raw) == "" {
		return []string{DefaultBuildSourceRoot}
	}
	var roots []string
	for _, value := range filepath.SplitList(raw) {
		value = strings.TrimSpace(value)
		if value != "" && filepath.IsAbs(value) {
			roots = append(roots, value)
		}
	}
	if len(roots) == 0 {
		return []string{DefaultBuildSourceRoot}
	}
	return roots
}

// ResolveBuildSource resolves symlinks and requires the real path to be at or
// below one of the real configured roots. It rejects path traversal, symlink
// escapes, relative paths, and non-directories. Call it at submission and again
// immediately before the node step, because the filesystem can change.
func ResolveBuildSource(path string, roots []string) (string, error) {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) || strings.ContainsAny(path, "\r\n") {
		return "", contract.NewFault(contract.ValidationFailed, "directory source path must be absolute", "Use an absolute directory beneath a configured build source root.")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", contract.NewFault(contract.ValidationFailed, "directory source path is not readable", "Use an existing directory beneath a configured build source root.")
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", contract.NewFault(contract.ValidationFailed, "directory source path is not a directory", "Point source.path at a readable directory.")
	}
	if len(roots) == 0 {
		roots = BuildSourceRoots()
	}
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			continue
		}
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		realRoot = filepath.Clean(realRoot)
		if real == realRoot || strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
			return real, nil
		}
	}
	return "", contract.NewFault(contract.Forbidden, "directory source is outside the configured build roots", "Set EREGION_BUILD_SOURCE_ROOTS to a deliberate, narrow source directory; arbitrary server paths are not buildable.")
}
