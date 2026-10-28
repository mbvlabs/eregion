package platform

import (
	"crypto/subtle"

	"github.com/mbvlabs/eregion/internal/contract"
)

// targetConfirmedKinds are the destructive actions whose D12 confirmation is a
// token naming the exact target, not a digest of a dry-run plan.
var targetConfirmedKinds = map[string]bool{
	KindRollback:          true,
	KindAppDelete:         true,
	KindEnvironmentDelete: true,
}

// RequiresTargetConfirmation reports whether kind is guarded by a target-name
// confirmation token.
func RequiresTargetConfirmation(kind string) bool { return targetConfirmedKinds[kind] }

// factorGatedKinds are the platform action kinds the D7 human factor gates
// inside the performing request. Their dry run is factor-free. Token creation
// and the exports are node RPC methods, not platform action kinds.
var factorGatedKinds = map[string]bool{
	KindAppDelete:         true,
	KindEnvironmentDelete: true,
}

// RequiresHumanFactor reports whether kind needs the D7 factor in the same
// request that performs it.
func RequiresHumanFactor(kind string) bool { return factorGatedKinds[kind] }

// ConfirmationToken is the D12 token for one target: <app> for an application
// and <app>/<env> for an environment.
func ConfirmationToken(target Target) (string, error) {
	switch target.Kind {
	case TargetApp:
		return target.App, nil
	case TargetEnvironment:
		return target.App + "/" + target.Environment, nil
	default:
		return "", contract.NewFault(contract.ValidationFailed, "this target does not take a target confirmation", "Use app/<app> or env/<app>/<env>.")
	}
}

// ValidateTargetConfirmation compares a token with the target names loaded from
// current state. It is the one validator used by rollback, env delete and app
// delete; callers must resolve the authoritative target first.
func ValidateTargetConfirmation(target Target, confirm string) error {
	want, err := ConfirmationToken(target)
	if err != nil {
		return err
	}
	if confirm == "" {
		return contract.NewFault(contract.ConfirmationRequired, "confirmation is required for this destructive action", "Pass --confirm "+want+".")
	}
	if subtle.ConstantTimeCompare([]byte(confirm), []byte(want)) != 1 {
		return contract.NewFault(contract.PreconditionFailed, "confirmation token does not name this target", "Pass --confirm "+want+" for the exact target.")
	}
	return nil
}

// ValidateConfirmation checks a destructive action's operator-supplied token.
// The caller supplies the canonical target; this function only proves that the
// token names it exactly.
func ValidateConfirmation(kind, targetText, confirm string) error {
	if !RequiresTargetConfirmation(kind) {
		return nil
	}
	target, err := ParseTarget(targetText)
	if err != nil {
		return err
	}
	return ValidateTargetConfirmation(target, confirm)
}
