package controlplane

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

// activateWebhookSecretTx seals a transient webhook secret in the same
// transaction that records the winning operation and its desired state. A
// concurrent loser never reaches this helper because its operation INSERT
// conflicts first. The plaintext is passed in from the request transport, not
// from operation input, its hash, events or River arguments.
func (s *Service) activateWebhookSecretTx(ctx context.Context, tx pgx.Tx, actor contract.Actor, input *platform.EnvironmentUpdateInput, state platform.RevisionState, secret []byte) (bool, error) {
	if len(secret) == 0 {
		if input.RotateWebhookSecret {
			return false, contract.NewFault(contract.InputRequired,
				"webhook rotation requires a new transient secret",
				"Rerun through the interactive terminal or --github-webhook path so a new secret can be generated and committed with the operation.")
		}
		if input.AutoDeploy != nil && *input.AutoDeploy {
			// An explicit enable may repeat an already-enabled environment. It
			// must still name an active sealed secret; otherwise the webhook
			// would be enabled with no value to verify.
			if !state.AutoDeploy {
				return false, contract.NewFault(contract.PreconditionFailed,
					"push to deploy is not enabled in the effective desired state",
					"Enable it with --auto-deploy and a GitHub source.")
			}
			if err := platform.ValidatePushToDeploy(state); err != nil {
				return false, err
			}
			if s.vault == nil {
				return false, contract.NewFault(contract.Unavailable,
					"the vault is not configured on this control plane",
					"Set EREGION_VAULT_KEY_FILE on the node before enabling push to deploy.")
			}
			namespace, err := vault.NewNamespace(input.App, input.Environment, state.WebhookSecretName)
			if err != nil {
				return false, err
			}
			metadata, err := s.vault.Store().GetTx(ctx, tx, namespace)
			if err != nil {
				if contract.Classify(err).Code == contract.NotFound {
					return false, contract.NewFault(contract.PreconditionFailed,
						"push to deploy needs an active webhook secret",
						"Enable it through the CLI with --auto-deploy so the secret is generated and committed, or create the named secret first.")
				}
				return false, err
			}
			if metadata.State != vault.StateActive {
				return false, contract.NewFault(contract.PreconditionFailed,
					"the named webhook secret is archived",
					"Enable it with a new secret through the CLI.")
			}
		}
		return false, nil
	}
	if !input.RotateWebhookSecret && (input.AutoDeploy == nil || !*input.AutoDeploy) {
		return false, contract.NewFault(contract.ValidationFailed,
			"a webhook secret can only be activated by an enable or rotation request",
			"Pass --auto-deploy or --rotate-webhook-secret together with the secret.")
	}
	if !state.AutoDeploy {
		return false, contract.NewFault(contract.PreconditionFailed,
			"push to deploy is not enabled, so there is no webhook secret to activate",
			"Enable it with --auto-deploy first.")
	}
	if err := platform.ValidatePushToDeploy(state); err != nil {
		return false, err
	}
	if s.vault == nil {
		return false, contract.NewFault(contract.Unavailable,
			"the vault is not configured on this control plane",
			"Set EREGION_VAULT_KEY_FILE on the node before enabling or rotating push to deploy.")
	}
	namespace, err := vault.NewNamespace(input.App, input.Environment, state.WebhookSecretName)
	if err != nil {
		return false, err
	}
	expected := uint64(0)
	exists := false
	metadata, err := s.vault.Store().GetTx(ctx, tx, namespace)
	switch {
	case err == nil:
		expected, exists = metadata.LatestRevision, true
	case contract.Classify(err).Code == contract.NotFound:
		// First activation creates revision 1 under the same transaction.
	default:
		return false, err
	}
	if input.RotateWebhookSecret && !exists {
		return false, contract.NewFault(contract.PreconditionFailed,
			"push to deploy is not enabled, so there is no secret to rotate",
			"Enable it with --auto-deploy before rotating.")
	}
	if _, err := s.vault.Store().Set(ctx, tx, namespace, secret, expected, actor); err != nil {
		return false, err
	}
	return true, nil
}
