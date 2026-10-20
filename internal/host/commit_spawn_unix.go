//go:build unix

package host

import (
	"os/exec"
	"syscall"
)

// spawnDetached starts a process in its own session so it survives the CLI
// process and the SSH connection that started the apply. The watchdog only
// reads its manifest and writes markers under the bootstrap state directory.
func spawnDetached(path string) error {
	command := exec.Command("/bin/sh", path) //nolint:gosec // fixed interpreter and a generated script under the private state directory.
	command.Stdin, command.Stdout, command.Stderr = nil, nil, nil
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return command.Start()
}
