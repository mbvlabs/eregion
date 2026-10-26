// Package cli adapts declarative command contracts to Cobra. It never owns durable
// operation execution; presenters only observe work the control plane owns.
package cli

import (
	"context"
	"errors"
	"io"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/nodeagent"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// Run writes exactly one envelope in JSON mode, including parse/help failures,
// except for follow commands whose presenter writes its own terminal protocol.
func Run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) int {
	// The node agent speaks the node protocol on stdin/stdout, so it bypasses
	// the result envelope entirely. Node RPC speaks its own NDJSON protocol.
	if len(args) == 2 && args[0] == "node" && args[1] == "rpc" {
		return serveNodeRPC(ctx, in, out, stderr)
	}
	if len(args) > 0 && args[0] == "node-agent" {
		return nodeagent.Run(args[1:], in, out, stderr)
	}
	return run(ctx, args, in, out, stderr, &secrets.Registry{})
}

func run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer, registry *secrets.Registry) int {
	registerEnvironmentSecrets(registry)
	a := newApp()
	a.secrets = registry
	selection := followSelectionFromArgs(args, a.valueFlags())
	mode, err := output.OptionsFromArgs(args, a.valueFlags()...)
	followMode := string(output.Plain)
	if selection.NDJSON {
		if selection.JSON {
			err = contract.NewFault(contract.InvalidOutputMode, "conflicting output modes", "Use --output ndjson or --json, not both.")
		} else {
			var fault *contract.Fault
			if errors.As(err, &fault) && fault.Code == contract.InvalidOutputMode {
				// NDJSON is a streaming mode available only to follow commands.
				err, mode = nil, output.Plain
			}
		}
	}
	if selection.NDJSON && !selection.JSON {
		followMode = "ndjson"
	} else if mode == output.JSON {
		followMode = string(output.JSON)
	}
	ctx = context.WithValue(ctx, followIOKey{}, &followIO{in: in, out: out, err: stderr, mode: followMode})
	writer := output.Writer{Out: out, Err: stderr, Mode: mode, Secrets: registry}
	command := "eregion"
	if err == nil {
		err = a.meta.validate(a.root)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		a.root.SetIn(in)
		a.root.SetOut(io.Discard)
		a.root.SetErr(stderr)
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
	if a.result.streamed {
		// The follow presenter already wrote its protocol (NDJSON result,
		// stable plain summary, or restored TUI plus summary).
		return contract.ExitCode(err)
	}
	if selection.Active && followMode == "ndjson" {
		if finishErr := output.NewStream(out, registry).Finish(envelope); finishErr != nil {
			// Do not append another record after a possibly partial write.
			_, _ = io.WriteString(stderr, "error [internal_error]: could not write command output\n")
			return 70
		}
		return contract.ExitCode(err)
	}
	if writeErr := writer.Write(envelope, a.result.plain); writeErr != nil {
		// Do not append a second JSON envelope after a possibly partial write.
		// Never expose arbitrary I/O/serialization errors on an unredacted path.
		_, _ = io.WriteString(stderr, "error [internal_error]: could not write command output\n")
		return 70
	}
	return contract.ExitCode(err)
}
