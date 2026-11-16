//go:build unix

package host

import (
	"context"
	"os/exec"
	"syscall"
)

// spawnDetached starts a process in its own session so it survives the CLI
// process and the SSH connection that started the apply. The watchdog only
// reads its manifest and writes markers under the bootstrap state directory.
func spawnDetached(ctx context.Context, path string) error {
	// The watchdog must survive the CLI process and its canceled context; only
	// the generated script's own deadline ends it.
	command := exec.CommandContext(context.WithoutCancel(ctx), "/bin/sh", path)
	command.Stdin, command.Stdout, command.Stderr = nil, nil, nil
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return command.Start()
}
