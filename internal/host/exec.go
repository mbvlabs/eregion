package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// Exec is the command seam for host steps. Apply scripts always run through
// engine.Shell, which redacts output and terminates the process group on
// cancellation. Check scripts are read-only and return a small observation.
type Exec interface {
	Run(context.Context, string, string, map[string]string, engine.Reporter) error
	Output(context.Context, string) (string, error)
}

// ShellExec is the production implementation. DryRun makes Run refuse, so a
// preview can never accidentally execute an apply script.
type ShellExec struct {
	DryRun  bool
	Secrets *secrets.Registry
}

func (s ShellExec) Run(ctx context.Context, stepID, script string, env map[string]string, report engine.Reporter) error {
	return engine.Shell{DryRun: s.DryRun, Secrets: s.Secrets}.Run(ctx, stepID, script, env, report)
}

func (s ShellExec) Output(ctx context.Context, script string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/bash", "-c", script) //nolint:gosec // G204: fixed interpreter and scripts from Go source.
	command.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "HOME=/root", "DEBIAN_FRONTEND=noninteractive"}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	text := output.String()
	if s.Secrets != nil {
		text = s.Secrets.Redact(text)
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return text, fmt.Errorf("host check timed out")
		}
		return text, fmt.Errorf("host check failed: %s", strings.TrimSpace(tail(text, 4)))
	}
	return text, nil
}

func tail(text string, lines int) string {
	fields := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(fields) > lines {
		fields = fields[len(fields)-lines:]
	}
	return strings.Join(fields, " | ")
}
