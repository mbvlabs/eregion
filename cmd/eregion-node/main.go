// Command eregion-node is the node-side binary. It owns the node's private
// state directory and answers node requests; it never talks to a database and
// never holds a credential for the control plane.
//
// The control plane reaches it over SSH by running "eregion-node serve" and
// exchanging the node protocol over the channel's standard input and output.
// The request never appears in a command line, so nothing an operator or an
// operation supplies is ever interpreted by a shell.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mbvlabs/eregion/internal/buildinfo"
	"github.com/mbvlabs/eregion/internal/noded"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: eregion-node <serve|task|compact|identity|version>")
		return 2
	}
	switch args[0] {
	case "serve":
		return report(stderr, serveCommand(args[1:], stdin, stdout))
	case "task":
		return report(stderr, taskCommand(args[1:]))
	case "compact":
		return report(stderr, compactCommand(args[1:], stdout))
	case "identity":
		return report(stderr, identityCommand(args[1:], stdout))
	case "version":
		return report(stderr, versionCommand(args[1:], stdout))
	default:
		fmt.Fprintf(stderr, "unknown node command %q\n", args[0])
		return 2
	}
}

func report(stderr io.Writer, err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "eregion-node: %v\n", err)
	return 1
}

// versionCommand reports the build metadata injected by the release workflow.
// The JSON form is the upgrade health probe's primary identity check, so a
// signed node artifact proves the exact semantic version it was built from.
func versionCommand(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("version", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "Print build information as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("version takes no arguments")
	}
	info := buildinfo.Current()
	if *jsonOutput {
		raw, err := json.Marshal(info)
		if err != nil {
			return fmt.Errorf("build information could not be rendered: %w", err)
		}
		fmt.Fprintln(stdout, string(raw))
		return nil
	}
	fmt.Fprintf(stdout, "eregion-node %s\ncommit: %s\nplatform: %s/%s\ngo: %s\ndirty: %t\n",
		info.Version, info.Commit, info.OS, info.Arch, info.Go, info.Dirty)
	return nil
}

func serveCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "", "node configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	config, err := noded.LoadConfig(*path)
	if err != nil {
		return err
	}
	return noded.Serve(config, stdin, stdout)
}

func taskCommand(args []string) error {
	flags := flag.NewFlagSet("task", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "", "node configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	config, err := noded.LoadConfig(*path)
	if err != nil {
		return err
	}
	return noded.RunTask(config)
}

func compactCommand(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("compact", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "", "node configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	config, err := noded.LoadConfig(*path)
	if err != nil {
		return err
	}
	removed, err := noded.Compact(config)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "removed %d journal tasks\n", removed)
	return nil
}

func identityCommand(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("identity", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "", "node configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	config, err := noded.LoadConfig(*path)
	if err != nil {
		return err
	}
	fingerprint, err := noded.Identity(config)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, fingerprint)
	return nil
}
