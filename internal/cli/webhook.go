package cli

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

// webhookUpdate is the push-to-deploy part of env update. The candidate secret
// is transient request data until a fresh admission seals it; it is shown once
// only on the manual human-terminal path.
type webhookUpdate struct {
	// Enabled is nil when --auto-deploy was not passed.
	Enabled    *bool
	SecretName string
	Secret     []byte
	URL        string
	Branch     string
	Repository string
	Rotated    bool
	// NeedsSecret reports that a fresh enable or rotation needs a candidate
	// secret. Generation waits for replay resolution and gh preflight.
	NeedsSecret bool
	// Replay marks an authoritative idempotent admission: the original operation
	// already installed every effect, so no secret is generated, printed or
	// installed and the hook is not retried or verified.
	Replay bool
	// GitHub opts in to creating or updating the repository webhook with an
	// authenticated gh CLI. The secret still travels only on stdin.
	GitHub bool
	// GitHubAccess is the preflight result reused by the post-admission
	// installer so it does not repeat an unverified access decision.
	GitHubAccess githubWebhookAccess
}

// readWebhookUpdate validates --auto-deploy/--rotate-webhook-secret against
// the current revision and the effective source after the other update flags.
// It records that an effectful enable or rotation needs a secret but never
// generates one; replay detection and gh preflight must resolve first.
func (a *app) readWebhookUpdate(cmd *cobra.Command, appName, environment string, current platform.Environment, patch platform.EnvironmentUpdateInput) (webhookUpdate, error) {
	update := webhookUpdate{GitHub: boolFlag(cmd, "github-webhook")}
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
	needsSecret := rotate || (autoSet && enabled && (!currentEnabled || currentName == ""))
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
		update.SecretName = platform.DefaultWebhookSecretName
	}
	update.Branch, update.Repository = branch, repository
	update.Rotated = rotate
	update.NeedsSecret = needsSecret
	// A dry run describes the effect but must not generate, preflight or store
	// a secret; prepare handles that after authoritative replay resolution.
	return update, nil
}

// generateWebhookSecret is a test seam for proving that replay and dry-run
// paths never generate a candidate.
var generateWebhookSecret = newWebhookSecret

// prepareWebhookUpdate resolves replay, preflights gh when the operator chose
// it, and only then generates the candidate secret. A preflight failure
// therefore leaves generation, the vault, desired state and repository
// untouched. On a known replay it performs none of those interactions.
func (a *app) prepareWebhookUpdate(cmd *cobra.Command, update *webhookUpdate, replay bool) error {
	if update == nil {
		return nil
	}
	update.Replay = replay
	if replay || dryRun(cmd) {
		return nil
	}
	if !update.GitHub && update.NeedsSecret && webhookHumanInteractive(cmd) {
		if err := a.offerGitHubWebhook(cmd, update); err != nil {
			return err
		}
	}
	if err := a.preflightWebhookUpdate(cmd, update); err != nil {
		return err
	}
	if !update.NeedsSecret {
		return nil
	}
	if !update.GitHub && !webhookHumanInteractive(cmd) {
		// readWebhookUpdate already refused this; keep the guard at the point
		// where a secret would be created in case a caller reaches it another way.
		return contract.NewFault(contract.InputRequired, "generating a push-to-deploy secret needs a human at a terminal or an explicit --github-webhook",
			"Run it yourself in a terminal, or add --github-webhook with an authenticated gh.")
	}
	secret, err := generateWebhookSecret()
	if err != nil {
		return err
	}
	update.Secret = secret
	return nil
}

// preflightWebhookUpdate verifies the exact repository's admin permission
// before an effectful run. Callers that already know a replay may skip it.
func (a *app) preflightWebhookUpdate(cmd *cobra.Command, update *webhookUpdate) error {
	if update == nil || !update.GitHub || update.GitHubAccess.OK || dryRun(cmd) || update.Replay {
		return nil
	}
	access := inspectGitHubWebhookAccess(cmd.Context(), update.Repository)
	if !access.OK {
		return githubWebhookPreflightFault(access)
	}
	update.GitHubAccess = access
	return nil
}

// applyWebhookPatch puts the push-to-deploy fields into the revision patch.
func applyWebhookPatch(patch *platform.EnvironmentUpdateInput, update webhookUpdate) {
	if update.Rotated {
		patch.RotateWebhookSecret = true
	}
	if update.Enabled == nil {
		return
	}
	patch.AutoDeploy = update.Enabled
	name := update.SecretName
	patch.WebhookSecretName = &name
}

// submitWebhookUpdate submits the canonical environment intent and a transient
// candidate secret through the protected node request. The returned receipt is
// authoritative: only a fresh admission may print a secret or run gh. The CLI
// generates the candidate only after a non-replay preview; an unknown
// concurrent loser may have generated one, but it cannot activate or install it
// and the replay receipt reports that the candidate was discarded. Known
// replays never generate. This is the accepted alternative to returning a
// server-generated secret through the transport.
func (a *app) submitWebhookUpdate(cmd *cobra.Command, appName, environment string, patch platform.EnvironmentUpdateInput, update webhookUpdate) (contract.Operation, submissionReceipt, error) {
	var zero contract.Operation
	client, err := a.client(cmd)
	if err != nil {
		return zero, submissionReceipt{}, err
	}
	submitter, ok := client.(webhookSubmitter)
	if !ok {
		return zero, submissionReceipt{}, contract.NewFault(contract.Unavailable,
			"this client cannot submit a webhook secret atomically",
			"Use a matching released CLI and node binary; do not retry through a preview-only path.")
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return zero, submissionReceipt{}, contract.NewFault(contract.Internal, "could not encode the environment update", "Report the command and binary version.")
	}
	secrets := map[string]string{}
	if len(update.Secret) > 0 {
		a.registerSecretValue(update.Secret)
		secrets[rpcWebhookSecretKey] = base64Encode(update.Secret)
	}
	key := flagString(cmd, "idempotency-key")
	if key == "" {
		key = "cli:" + strings.TrimPrefix(platform.KindEnvironmentUpdate, "platform.") + ":" + randomHex(8)
	}
	if err := contract.ValidateIdempotencyKey(key); err != nil {
		return zero, submissionReceipt{}, err
	}
	data, receipt, err := submitter.SubmitWebhookAction(cmd.Context(), platform.KindEnvironmentUpdate,
		"env/"+appName+"/"+environment, raw, key, "", false, secrets)
	if err != nil {
		return zero, submissionReceipt{}, err
	}
	var op contract.Operation
	if json.Unmarshal(data, &op) != nil || op.Validate() != nil ||
		op.Kind != platform.KindEnvironmentUpdate || op.Target != "env/"+appName+"/"+environment {
		return zero, submissionReceipt{}, invalidNodeData("accepted operation")
	}
	return op, receipt, nil
}

// printWebhookOnce shows the URL and secret on stderr only for the manual
// human-terminal path. The result envelope and machine/gh paths never contain
// the secret, and a gh failure never falls back to printing it.
func printWebhookOnce(cmd *cobra.Command, update webhookUpdate) {
	if update.Replay || update.GitHub || !webhookHumanInteractive(cmd) || len(update.Secret) == 0 {
		return
	}
	action := "Enabling push to deploy"
	tail := "The webhook accepts deliveries once the environment update is current."
	if update.Rotated {
		action = "The push-to-deploy secret was rotated"
		tail = "The new value is active immediately."
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"\n%s for %s.\nWebhook URL: %s\nWebhook secret: %s\n\n%s\nPaste both into the repository's GitHub webhook settings (content type application/json, push events only). The secret is not shown again; only its encrypted vault revision is stored.\nWith gh installed, signed in, and authorized to administer the repository, add --github-webhook when enabling or rotating to create or update this hook for you.\n\n",
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
