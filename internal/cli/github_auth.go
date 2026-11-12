package cli

import (
	"bytes"
	"context"
	"os/exec"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
)

// defaultGitReadCredentialName is the vault name used when --github-auth has
// no explicit --git-credential-name and the source does not already name one.
const defaultGitReadCredentialName = "GIT_READ_TOKEN"

// githubAuthRequested reports the explicit opt-in. The local gh token may
// carry broader scopes than a repository read credential, so it is never used
// implicitly or as a guided default.
func githubAuthRequested(cmd *cobra.Command) bool {
	return boolFlag(cmd, "github-auth")
}

// validateGitHubAuthFlags rejects two token sources in one invocation before
// any preview, vault write or gh call.
func validateGitHubAuthFlags(cmd *cobra.Command) error {
	if !githubAuthRequested(cmd) {
		return nil
	}
	if flagChanged(cmd, "git-credential-file") {
		return contract.NewFault(contract.Usage,
			"--github-auth and --git-credential-file choose different credentials",
			"Use --github-auth for the local gh token, or --git-credential-file for an explicit protected token file, not both.")
	}
	if flagChanged(cmd, "git-credential-name") {
		if err := vault.ValidateName(flagString(cmd, "git-credential-name")); err != nil {
			return contract.NewFault(contract.ValidationFailed,
				"--github-auth needs a credential secret name",
				"Pass a valid --git-credential-name, or omit it to use "+defaultGitReadCredentialName+".")
		}
	}
	return nil
}

// applyGitHubAuthToSpecs validates that every git source is a github.com
// repository and records one consistent read-credential name. The token value
// is deliberately not acquired here: a dry run or an idempotent replay must
// never call gh.
func applyGitHubAuthToSpecs(cmd *cobra.Command, ans *setupAnswers, specs []platform.EnvironmentSpec) ([]platform.EnvironmentSpec, error) {
	if !githubAuthRequested(cmd) {
		return specs, nil
	}
	if err := validateGitHubAuthFlags(cmd); err != nil {
		return nil, err
	}
	if ans != nil && len(ans.CredentialValue) > 0 {
		return nil, contract.NewFault(contract.Usage,
			"--github-auth conflicts with another credential value",
			"Use one credential source: gh, a protected token file, or an existing value.")
	}
	requestedName := ""
	if flagChanged(cmd, "git-credential-name") {
		requestedName = flagString(cmd, "git-credential-name")
	}

	names := map[string]bool{}
	gitSpecs := 0
	for _, spec := range specs {
		if spec.Source.URL == "" {
			continue
		}
		gitSpecs++
		if _, ok := platform.GitHubRepository(spec.Source.URL); !ok {
			return nil, contract.NewFault(contract.ValidationFailed,
				"--github-auth needs a GitHub HTTPS git source",
				"Use an https://github.com/owner/repository source; another forge needs --git-credential-file with a token for that host.")
		}
		if spec.Source.CredentialSecretName != "" {
			names[spec.Source.CredentialSecretName] = true
		}
	}
	if gitSpecs == 0 {
		return nil, contract.NewFault(contract.ValidationFailed,
			"--github-auth needs a git source",
			"Set an https://github.com/owner/repository source with --git-url.")
	}
	if requestedName == "" {
		switch len(names) {
		case 0:
			requestedName = defaultGitReadCredentialName
		case 1:
			for name := range names {
				requestedName = name
			}
		default:
			return nil, contract.NewFault(contract.ValidationFailed,
				"the environments name different read credentials",
				"Use one credential name across the environments, then retry.")
		}
	}
	if err := vault.ValidateName(requestedName); err != nil {
		return nil, err
	}
	if ans != nil {
		if _, exists := ans.SecretValues[requestedName]; exists {
			return nil, contract.NewFault(contract.ValidationFailed,
				"secret name conflicts with the git read credential",
				"The read credential is not injected into the app; choose another secret name.")
		}
	}
	for _, spec := range specs {
		if spec.Source.URL == "" {
			continue
		}
		for _, secretName := range spec.SecretNames {
			if secretName == requestedName {
				return nil, contract.NewFault(contract.ValidationFailed,
					"secret name conflicts with the git read credential",
					"The read credential is not injected into the app; choose another secret name.")
			}
		}
	}

	out := append([]platform.EnvironmentSpec(nil), specs...)
	for index := range out {
		if out[index].Source.URL != "" {
			out[index].Source.CredentialSecretName = requestedName
		}
	}
	if ans != nil {
		ans.CredentialName = requestedName
	}
	return out, nil
}

// acquireGitHubAuthToken reads the local gh session token into memory. The
// token never appears in argv, output, errors or an intermediate file.
func acquireGitHubAuthToken(ctx context.Context) ([]byte, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		fault := contract.NewFault(contract.MissingTool,
			"the GitHub CLI (gh) is needed for --github-auth but is not on PATH",
			"Install gh and run `gh auth login --hostname github.com`, or use --git-credential-file.")
		fault.Details = map[string]any{"tool": "gh"}
		return nil, fault
	}
	out, err := runGH(ctx, nil, "auth", "token", "--hostname", "github.com")
	if err != nil {
		return nil, contract.NewFault(contract.PreconditionFailed,
			"gh is not signed in to github.com or its token could not be read",
			"Run `gh auth login --hostname github.com`, or use --git-credential-file.")
	}
	token := bytes.TrimSpace(out)
	if len(token) == 0 {
		clearBytes(out)
		return nil, contract.NewFault(contract.PreconditionFailed,
			"gh returned an empty token for github.com",
			"Run `gh auth login --hostname github.com`, or use --git-credential-file.")
	}
	value := append([]byte(nil), token...)
	clearBytes(out)
	return value, nil
}

// fillGitHubAuthCredential acquires the opted-in token after preview has
// resolved dry-run and replay, and just before the existing vault write.
func (a *app) fillGitHubAuthCredential(cmd *cobra.Command, ans *setupAnswers) error {
	if !githubAuthRequested(cmd) {
		return nil
	}
	if len(ans.CredentialValue) > 0 {
		return contract.NewFault(contract.Internal,
			"two git read credentials were supplied",
			"Report the command and binary version.")
	}
	token, err := acquireGitHubAuthToken(cmd.Context())
	if err != nil {
		return err
	}
	a.registerSecretValue(token)
	ans.CredentialValue = token
	return nil
}
