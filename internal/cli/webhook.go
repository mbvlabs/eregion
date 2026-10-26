package cli

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
)

// defaultWebhookSecretName is the vault name used when a revision does not
// record one. The name, never the value, is part of the revision.
const defaultWebhookSecretName = "EREGION_WEBHOOK_SECRET"

// webhookUpdate is the push-to-deploy part of env update. The secret is kept
// out of the revision and shown once at a terminal.
type webhookUpdate struct {
	// Enabled is nil when --auto-deploy was not passed.
	Enabled        *bool
	SecretName     string
	Secret         []byte
	URL            string
	Branch         string
	Repository     string
	AlreadyEnabled bool
	Rotated        bool
}

// readWebhookUpdate validates --auto-deploy/--rotate-webhook-secret against
// the current revision and the effective source after the other update flags.
// It generates a secret only for a real (non-dry-run) enable or rotation.
func (a *app) readWebhookUpdate(cmd *cobra.Command, appName, environment string, current platform.Environment, patch platform.EnvironmentUpdateInput) (webhookUpdate, error) {
	var update webhookUpdate
	autoSet := flagChanged(cmd, "auto-deploy")
	rotate, _ := cmd.Flags().GetBool("rotate-webhook-secret")
	if !autoSet && !rotate {
		return update, nil
	}
	enabled := true
	if autoSet {
		enabled, _ = cmd.Flags().GetBool("auto-deploy")
	}
	if rotate && autoSet && !enabled {
		return update, contract.NewFault(contract.Usage, "choose --auto-deploy or --rotate-webhook-secret, not both", "Rotation needs auto-deploy to stay on.")
	}
	if autoSet {
		update.Enabled = &enabled
	}
	if autoSet && !enabled {
		// Disabling clears the recorded name; the vault revision remains.
		empty := ""
		update.SecretName = empty
		return update, nil
	}

	currentEnabled := current.Revision != nil && current.Revision.AutoDeploy
	currentName := ""
	source := platform.SourceBinding{}
	if current.Revision != nil {
		currentName = current.Revision.WebhookSecretName
		source = current.Revision.Source
	}
	if patch.Source != nil {
		source = *patch.Source
	}
	if rotate && (!currentEnabled || currentName == "") {
		return update, contract.NewFault(contract.PreconditionFailed, "push to deploy is not enabled, so there is no secret to rotate", "Enable it with eregion env update "+appName+" "+environment+" --auto-deploy.")
	}
	if autoSet && enabled && !rotate && currentEnabled && currentName != "" {
		update.Enabled = nil
		update.AlreadyEnabled = true
		update.SecretName = currentName
		update.Branch = strings.TrimPrefix(strings.TrimSpace(source.Ref), "refs/heads/")
		update.Repository, _ = platform.GitHubRepository(source.URL)
		url, err := a.webhookURL(cmd, appName, environment)
		if err != nil {
			return update, err
		}
		update.URL = url
		return update, nil
	}
	if !dryRun(cmd) && !humanInteractive(cmd) {
		return update, contract.NewFault(contract.InputRequired, "enabling or rotating push to deploy needs a human at a terminal",
			"Run it yourself in a terminal, without --json, --yes, or a pipe: the generated secret is shown once and cannot be recovered.")
	}
	if source.Kind != "git" {
		return update, contract.NewFault(contract.PreconditionFailed, "push to deploy needs a git source", "Set a git source with --git-url and a branch ref first.")
	}
	repository, ok := platform.GitHubRepository(source.URL)
	if !ok {
		return update, contract.NewFault(contract.PreconditionFailed, "push to deploy needs a GitHub HTTPS git source", "Use an https://github.com/owner/repository URL.")
	}
	branch := strings.TrimPrefix(strings.TrimSpace(source.Ref), "refs/heads/")
	if branch == "" {
		return update, contract.NewFault(contract.PreconditionFailed, "push to deploy needs a bound branch", "Set the source ref with --git-ref.")
	}
	url, err := a.webhookURL(cmd, appName, environment)
	if err != nil {
		return update, err
	}
	update.URL = url
	update.SecretName = currentName
	if update.SecretName == "" {
		update.SecretName = defaultWebhookSecretName
	}
	update.Branch, update.Repository = branch, repository
	update.Rotated = rotate
	if !dryRun(cmd) {
		secret, err := newWebhookSecret()
		if err != nil {
			return update, err
		}
		update.Secret = secret
	}
	return update, nil
}

// applyWebhookPatch puts the push-to-deploy fields into the revision patch.
func applyWebhookPatch(patch *platform.EnvironmentUpdateInput, update webhookUpdate) {
	if update.Enabled == nil {
		return
	}
	patch.AutoDeploy = update.Enabled
	name := update.SecretName
	patch.WebhookSecretName = &name
}

// writeWebhookSecret stores the generated secret before the revision names it.
func (a *app) writeWebhookSecret(cmd *cobra.Command, appName, environment string, update webhookUpdate) error {
	if len(update.Secret) == 0 {
		return nil
	}
	namespace, err := vault.NewNamespace(appName, environment, update.SecretName)
	if err != nil {
		return err
	}
	a.registerSecretValue(update.Secret)
	_, err = a.putSecret(cmd, namespace, update.Secret)
	return err
}

// printWebhookOnce shows the URL and secret on stderr. The result envelope
// never contains the secret.
func printWebhookOnce(cmd *cobra.Command, update webhookUpdate) {
	if len(update.Secret) == 0 {
		return
	}
	action := "Enabling push to deploy"
	tail := "The webhook accepts deliveries once the environment update is current."
	if update.Rotated {
		action = "The push-to-deploy secret was rotated"
		tail = "The new value is active immediately."
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"\n%s for %s.\nWebhook URL: %s\nWebhook secret: %s\n\n%s\nPaste both into the repository's GitHub webhook settings (content type application/json, push events only). The secret is not shown again; only its encrypted vault revision is stored.\n\n",
		action, update.Repository+", branch "+update.Branch, update.URL, string(update.Secret), tail)
}

func (a *app) webhookURL(cmd *cobra.Command, appName, environment string) (string, error) {
	client, err := a.client(cmd)
	if err != nil {
		return "", err
	}
	info, err := client.NodeInfo(cmd.Context())
	if err != nil {
		return "", err
	}
	return "https://" + strings.ToLower(strings.TrimSuffix(info.Hostname, ".")) + "/v1/webhooks/github/" + appName + "/" + environment, nil
}

func newWebhookSecret() ([]byte, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, contract.NewFault(contract.Internal, "could not generate a webhook secret", "Retry the command.")
	}
	return []byte(base64.RawURLEncoding.EncodeToString(raw[:])), nil
}
