package platform

import (
	"crypto/subtle"

	"github.com/mbvlabs/eregion/internal/contract"
)

// ValidateDomainConfirmation checks the target-naming token for a domain
// removal against the hostname loaded from current state (MASTER D12).
func ValidateDomainConfirmation(domain, confirm string) error {
	if confirm == "" {
		return contract.NewFault(contract.ConfirmationRequired, "removing a domain needs a confirmation token",
			"Run with --dry-run to inspect the route, then pass --confirm "+domain+".")
	}
	if subtle.ConstantTimeCompare([]byte(confirm), []byte(domain)) != 1 {
		return contract.NewFault(contract.PreconditionFailed, "confirmation token does not match the current domain",
			"Pass --confirm "+domain+" for this exact hostname.")
	}
	return nil
}
