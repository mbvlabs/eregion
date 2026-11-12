package cli

import (
	"slices"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// validateGitCredentialRole rejects a source read-credential name that is also
// a runtime secret or the push-to-deploy webhook secret. The same vault name is
// one value, so writing a Git token there would inject it into app processes or
// replace another credential.
func validateGitCredentialRole(name string, runtimeSecretNames []string, webhookSecretName string) error {
	if name == "" {
		return nil
	}
	if slices.Contains(runtimeSecretNames, name) {
		return contract.NewFault(contract.ValidationFailed,
			"the git read credential name "+name+" is also a runtime secret",
			"Choose a different --git-credential-name; a read credential must not be injected into app processes.")
	}
	if webhookSecretName != "" && name == webhookSecretName {
		return contract.NewFault(contract.ValidationFailed,
			"the git read credential name "+name+" is also the push-to-deploy webhook secret",
			"Choose a different --git-credential-name; one vault value cannot serve two credential roles.")
	}
	return nil
}

// validateEnvironmentGitCredentialRoles applies the role check to every git
// source in a create recipe; image environments carry no source credential and
// are skipped.
func validateEnvironmentGitCredentialRoles(specs []platform.EnvironmentSpec) error {
	for _, spec := range specs {
		if spec.Source.URL == "" {
			continue
		}
		if err := validateGitCredentialRole(spec.Source.CredentialSecretName, spec.SecretNames, spec.WebhookSecretName); err != nil {
			return err
		}
	}
	return nil
}

// revisionCredentialRoles returns the credential roles a source read token
// must not overwrite on an existing environment.
func revisionCredentialRoles(current platform.Environment) ([]string, string) {
	if current.Revision == nil {
		return nil, ""
	}
	return current.Revision.SecretNames, current.Revision.WebhookSecretName
}
