package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/mbvlabs/eregion/internal/cli"
	"github.com/mbvlabs/eregion/internal/containerresolver"
)

func main() {
	os.Exit(run())
}

func run() int {
	if containerresolver.IsInvocation(os.Args[1:]) {
		return containerresolver.Run(os.Args[2:], os.Stderr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}
