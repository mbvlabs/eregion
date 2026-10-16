// Package cli adapts declarative command contracts to Cobra. It never owns durable
// operation execution and does not depend on a renderer, database, or queue.
package cli

import (
	"context"
	"errors"
	"io"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// Run writes exactly one envelope in JSON mode, including parse/help failures.
// All current human output is append-only plain text; no TTY probing is needed.
func Run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) int {
	return run(ctx, args, in, out, stderr, &secrets.Registry{})
}

func run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer, registry *secrets.Registry) int {
	mode, err := output.OptionsFromArgs(args, "path")
	writer := output.Writer{Out: out, Err: stderr, Mode: mode, Secrets: registry}
	command := "eregion"
	a := newApp()
	if err == nil {
		err = a.meta.validate(a.root)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		a.root.SetIn(in)
		a.root.SetOut(io.Discard)
		a.root.SetErr(io.Discard)
		a.root.SetArgs(args)
		cmd, executeErr := a.root.ExecuteContextC(ctx)
		if cmd != nil {
			command = cmd.CommandPath()
		}
		err = executeErr
		var fault *contract.Fault
		if err != nil && !a.started && !errors.As(err, &fault) {
			err = contract.NewFault(contract.Usage, err.Error(), "Discover available commands with eregion commands --json.")
		}
	}
	var envelope contract.Envelope
	if err != nil {
		envelope = contract.Failure(command, err)
	} else {
		envelope = contract.Success(a.result.command, a.result.data, a.result.next)
	}
	if writeErr := writer.Write(envelope, a.result.plain); writeErr != nil {
		// Do not append a second JSON envelope after a possibly partial write.
		// Never expose arbitrary I/O/serialization errors on an unredacted path.
		_, _ = io.WriteString(stderr, "error [internal_error]: could not write command output\n")
		return 70
	}
	return contract.ExitCode(err)
}
