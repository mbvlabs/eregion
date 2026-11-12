package platform

import (
	"fmt"
	"os"
)

// IsolatedDockerEnvironment creates a private, empty Docker CLI configuration
// directory and returns an explicit child environment that cannot inherit the
// node user's HOME, DOCKER_CONFIG, DOCKER_CONTEXT, credential helpers or
// ambient registry auth. The child process receives only these values; callers
// must remove the directory after the helper and all of its children exit.
func IsolatedDockerEnvironment() ([]string, func(), error) {
	dir, err := os.MkdirTemp("", "eregion-docker-config-")
	if err != nil {
		return nil, nil, fmt.Errorf("create isolated Docker configuration: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, fmt.Errorf("protect isolated Docker configuration: %w", err)
	}
	env := []string{
		"DOCKER_CONFIG=" + dir,
		"HOME=" + dir,
		"XDG_CONFIG_HOME=" + dir,
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	return env, cleanup, nil
}
