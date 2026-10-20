package vault

import "github.com/mbvlabs/eregion/internal/contract"

func contractFault(message, hint string) *contract.Fault {
	return contract.NewFault(contract.Internal, message, hint)
}
