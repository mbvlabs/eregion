package vault

import "github.com/mbvlabs/eregion/internal/contract"

func contractFault(message, hint string) *contract.FaultError {
	return contract.NewFault(contract.Internal, message, hint)
}
