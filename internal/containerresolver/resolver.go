// Package containerresolver resolves and executes a bare container command
// with the target runtime's own cwd, PATH and permissions. It is invoked from
// the same static eregion binary bind-mounted read-only into the container.
package containerresolver

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
)

const (
	// Invocation is the hidden argv marker used inside the container.
	Invocation = "__eregion_container_exec"
	// TargetPath is the fixed absolute mount point inside the container.
	TargetPath = "/run/eregion/eregion"
)

// ErrNotFound is returned when no executable can be resolved.
var ErrNotFound = errors.New("executable file not found in PATH or working directory")

// IsInvocation reports whether args is the hidden resolver invocation.
func IsInvocation(args []string) bool {
	return len(args) > 0 && args[0] == Invocation
}

// Run resolves command from the current environment and replaces this process.
// commandArgs excludes the hidden invocation marker. On success it never
// returns.
func Run(commandArgs []string, stderr io.Writer) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "eregion container resolver: working directory: %v\n", err)
		return 126
	}
	return runWith(commandArgs, cwd, os.Getenv("PATH"), os.Environ(), syscall.Exec, stderr)
}

func runWith(commandArgs []string, cwd, pathEnv string, env []string, execFn func(string, []string, []string) error, stderr io.Writer) int {
	if len(commandArgs) == 0 || commandArgs[0] == "" {
		fmt.Fprintln(stderr, "eregion container resolver: command is required")
		return 2
	}
	command := commandArgs[0]
	argv := append([]string(nil), commandArgs...)
	target := command
	if !strings.Contains(command, "/") {
		var err error
		target, err = Resolve(command, pathEnv, cwd)
		if err != nil {
			fmt.Fprintf(stderr, "eregion container resolver: %s: %v\n", command, err)
			return 127
		}
	}
	if err := execFn(target, argv, env); err != nil {
		fmt.Fprintf(stderr, "eregion container resolver: %s: %v\n", command, err)
		return 126
	}
	return 0
}

// Resolve searches the effective PATH in order, including relative and empty
// entries, then falls back to the working directory. Candidate paths are built
// by string concatenation rather than path cleaning so symlink and ".."
// resolution is left to the target runtime and filesystem. It never invokes a
// shell or changes the environment.
func Resolve(command, pathEnv, cwd string) (string, error) {
	if command == "" {
		return "", ErrNotFound
	}
	if strings.Contains(command, "/") {
		if isExecutable(command) {
			return command, nil
		}
		return "", ErrNotFound
	}

	candidates := make([]string, 0, len(strings.Split(pathEnv, ":"))+1)
	for _, entry := range strings.Split(pathEnv, ":") {
		candidates = append(candidates, candidateFor(entry, cwd, command))
	}
	candidates = append(candidates, joinRaw(cwd, command))
	for _, candidate := range candidates {
		if isExecutable(candidate) {
			return candidate, nil
		}
	}
	return "", ErrNotFound
}

func candidateFor(entry, cwd, command string) string {
	switch {
	case entry == "":
		return joinRaw(cwd, command)
	case path.IsAbs(entry):
		return joinRaw(entry, command)
	default:
		return joinRaw(joinRaw(cwd, entry), command)
	}
}

func joinRaw(dir, name string) string {
	if dir == "" {
		return name
	}
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

func isExecutable(candidate string) bool {
	info, err := os.Stat(candidate)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return false
	}
	// Access checks the resolver's real uid/gid, which are the target process's
	// uid/gid because the resolver is exec-replaced before the command runs.
	return syscall.Access(candidate, 1) == nil
}
