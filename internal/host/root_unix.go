//go:build unix

package host

import (
	"os"

	"github.com/mbvlabs/eregion/internal/contract"
)

func rootCheck() error {
	if os.Geteuid() != 0 {
		return contract.NewFault(contract.Forbidden, "host apply must run as root", "Run it through sudo on the target host; previews do not require root.")
	}
	return nil
}
