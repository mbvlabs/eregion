// Package noded implements the node side of the node protocol: it answers
// requests from a control plane, runs detached node tasks, and owns the node's
// private state directory. It never talks to a database and never holds a
// credential for the control plane.
package noded

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// DefaultConfigPath is where a node service keeps its configuration.
	DefaultConfigPath = "/etc/eregion/node.json"
	// EnvConfigPath overrides the configuration path for a test fixture.
	EnvConfigPath = "EREGION_NODE_CONFIG"
	// TaskRequestFD is the file descriptor a detached task reads its request
	// from. The request travels through a pipe, so a transient secret never
	// reaches the node's disk.
	TaskRequestFD = 3
)

// Config is the node's own configuration. It never contains a credential: the
// node authenticates the control plane with an SSH host key, and the control
// plane authenticates itself with a key the node trusts.
type Config struct {
	// Node is the node's name in the node journal.
	Node string `json:"node"`
	// StateDir is the node's private state directory. It is shared by every
	// node process on this host.
	StateDir string `json:"state_dir"`
	// Path is where the configuration was read from.
	Path string `json:"-"`
	// TaskCommand starts a detached node task. It defaults to this binary's
	// own "task" subcommand. A test fixture replaces it.
	TaskCommand []string `json:"-"`
}

// LoadConfig reads the node configuration. An explicit path wins over the
// environment, which wins over the default path.
func LoadConfig(path string) (Config, error) {
	if path == "" {
		path = os.Getenv(EnvConfigPath)
	}
	if path == "" {
		path = DefaultConfigPath
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: operator-owned node configuration.
	if err != nil {
		return Config{}, fmt.Errorf("read the node configuration %s: %w", path, err)
	}
	var config Config
	if err := json.Unmarshal(raw, &config); err != nil {
		return Config{}, fmt.Errorf("parse the node configuration %s: %w", path, err)
	}
	config.Path = path
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// Validate rejects a configuration that would make the node write outside its
// own state directory or record an unsafe node name.
func (c Config) Validate() error {
	if c.Node == "" {
		return errors.New("the node configuration needs a node name")
	}
	if c.StateDir == "" {
		return errors.New("the node configuration needs a state directory")
	}
	if !filepath.IsAbs(c.StateDir) {
		return errors.New("the node state directory must be an absolute path")
	}
	if strings.ContainsAny(c.StateDir, "\n\r") {
		return errors.New("the node state directory must not contain control characters")
	}
	return nil
}

// taskCommand returns the command that starts a detached node task.
func (c Config) taskCommand() []string {
	if len(c.TaskCommand) > 0 {
		return c.TaskCommand
	}
	return []string{os.Args[0], "task", "--config", c.Path}
}
