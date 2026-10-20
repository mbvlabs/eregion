//go:build !unix

package host

import "github.com/mbvlabs/eregion/internal/contract"

func rootCheck() error {
	return contract.NewFault(contract.ValidationFailed, "host bootstrap requires a Unix host", "Run bootstrap on a supported Debian or Ubuntu host.")
}
