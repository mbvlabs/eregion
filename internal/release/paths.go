package release

import (
	"os"
	"path/filepath"
)

func cacheHome() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "eregion-cache")
	}
	return filepath.Join(home, ".cache")
}

func releaseCacheDir(version, goos, goarch string) string {
	return filepath.Join(cacheHome(), "eregion", "releases", version, goos+"-"+goarch)
}

func tufCacheDir() string {
	return filepath.Join(cacheHome(), "eregion", "sigstore-tuf")
}
