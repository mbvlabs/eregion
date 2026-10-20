//go:build !unix

package engine

import (
	"context"
	"errors"

	"github.com/mbvlabs/eregion/internal/secrets"
)

// ErrInterrupted marks a step stopped by cancellation or shutdown.
var ErrInterrupted = errors.New("interrupted")

// ScriptError is a script that exited non-zero.
type ScriptError struct {
	Step   string
	Status int
	Tail   []string
}

func (e *ScriptError) Error() string { return "script failed: " + e.Step }

// Shell is unavailable outside Unix. Node execution is a Linux feature.
type Shell struct {
	DryRun  bool
	Secrets *secrets.Registry
}

func (Shell) Run(context.Context, string, string, map[string]string, Reporter) error {
	return errors.New("node shell execution requires a Unix host")
}
