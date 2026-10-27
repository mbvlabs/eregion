//go:build !unix

package cli

import (
	"context"
	"net"

	"github.com/mbvlabs/eregion/internal/host"
)

// The shared tunnel daemon is unix-only. Other platforms keep one in-process
// userspace tunnel per route; a non-unix operator cannot run the node-mode
// commands that need it in practice, but the fallback keeps one binary.
func (a *app) tunnelSpecs() []spec { return nil }

func wireGuardDial(wg *host.ContextWireGuard) (func(context.Context, string, string) (net.Conn, error), error) {
	return inProcessWireGuardDial(wg)
}

func closeWireGuardRoute(route *host.ContextSSH) { closeInProcessWireGuardRoute(route) }
