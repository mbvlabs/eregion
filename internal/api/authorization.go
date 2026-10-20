package api

import (
	"regexp"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// ActionGrant binds one exact action kind to an explicit canonical target.
// Only an environment's final segment may be wildcarded, e.g. env/my-app/*.
// Omitting grants preserves administrative operator/system credentials; CI and
// webhook principals never gain platform-wide access from operations:submit.
type ActionGrant struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

var grantKindPattern = regexp.MustCompile(`^platform\.[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)*$`)

func validateGrants(grants []ActionGrant) error {
	if len(grants) > 128 {
		return contract.NewFault(contract.ValidationFailed, "too many action grants", "Configure at most 128 explicit action/target grants per credential.")
	}
	seen := make(map[ActionGrant]bool)
	for _, grant := range grants {
		valid := grantKindPattern.MatchString(grant.Kind)
		if grant.Kind == "diagnostic.test" {
			valid = grant.Target == "control-plane"
		} else if strings.HasSuffix(grant.Target, "/*") {
			parts := strings.Split(grant.Target, "/")
			valid = valid && len(parts) == 3 && parts[0] == "env" && platform.ValidateName(parts[1]) == nil
		} else {
			_, err := platform.ParseTarget(grant.Target)
			valid = valid && err == nil
		}
		if !valid || seen[grant] {
			return contract.NewFault(contract.ValidationFailed, "invalid or duplicate action grant", "Use one exact action kind and canonical target; only env/<app>/* permits a wildcard.")
		}
		seen[grant] = true
	}
	return nil
}

func administrative(principal Principal) bool {
	return len(principal.ActionGrants) == 0 && (principal.Actor.Kind == "operator" || principal.Actor.Kind == "system")
}

func targetMatches(pattern, target string) bool {
	if pattern == target {
		return true
	}
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "*")
		if strings.HasPrefix(target, prefix) {
			return platform.ValidateName(strings.TrimPrefix(target, prefix)) == nil
		}
	}
	return false
}

func authorizePrincipalAction(principal Principal, kind, target string) error {
	if administrative(principal) {
		return nil
	}
	// Existing diagnostic-only automation remains compatible and cannot acquire
	// deployment, enrollment, resource, or secret permissions by implication.
	if len(principal.ActionGrants) == 0 && kind == "diagnostic.test" && target == "control-plane" {
		return nil
	}
	for _, grant := range principal.ActionGrants {
		if grant.Kind == kind && targetMatches(grant.Target, target) {
			return nil
		}
	}
	return contract.NewFault(contract.Forbidden, "credential is not authorized for this action and target", "Ask an operator for an explicit action_grants entry; operations scope alone is not platform authorization.")
}

// authorizeAction is called after normalization, for previews as well as writes.
func (a *Auth) authorizeAction(header, kind, target string) error {
	principal, err := a.resolvePrincipal(header)
	if err != nil {
		return err
	}
	return authorizePrincipalAction(principal, kind, target)
}

// authorizeInventory fails closed for extension providers that cannot yet honor
// per-principal grants. Scoped deployment credentials can read one explicit
// authorized environment, never global fleet/secret/resource collections.
func (a *Auth) authorizeInventory(header string, query platform.InventoryQuery) error {
	principal, err := a.resolvePrincipal(header)
	if err != nil {
		return err
	}
	if administrative(principal) {
		return nil
	}
	switch query.Collection {
	case "environments", "releases", "deployments", "changes":
		if platform.ValidateName(query.App) == nil && platform.ValidateName(query.Environment) == nil {
			target := "env/" + query.App + "/" + query.Environment
			for _, grant := range principal.ActionGrants {
				if targetMatches(grant.Target, target) {
					return nil
				}
			}
		}
	}
	return contract.NewFault(contract.Forbidden, "credential cannot read this inventory scope", "Scoped credentials must specify their authorized app and environment; global and extension inventory require an operator credential.")
}
