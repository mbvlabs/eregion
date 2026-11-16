package controlplane

import (
	"context"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

// activateWebhookSecretTx seals a transient webhook secret in the same
// transaction that records the winning operation and its desired state. A
// concurrent loser never reaches this helper because its operation INSERT
// conflicts first. The plaintext is passed in from the request transport, not
// from operation input, its hash, events or River arguments.
func (s *Service) activateWebhookSecretTx(ctx context.Context, tx *entities.StoreTx, actor contract.Actor, input *platform.EnvironmentUpdateInput, state platform.RevisionState, secret []byte) (bool, error) {
	if len(secret) == 0 {
		return s.validateExistingWebhookSecret(ctx, tx, input, state)
	}
	return s.sealWebhookSecret(ctx, tx, actor, input, state, secret)
}

func (s *Service) validateExistingWebhookSecret(ctx context.Context, tx *entities.StoreTx, input *platform.EnvironmentUpdateInput, state platform.RevisionState) (bool, error) {
	if input.RotateWebhookSecret {
		return false, contract.NewFault(contract.InputRequired,
			"webhook rotation requires a new transient secret",
			"Rerun through the interactive terminal or --github-webhook path so a new secret can be generated and committed with the operation.")
	}
	explicitEnable := (input.AutoDeploy != nil && *input.AutoDeploy) ||
		(input.WebhookSecretName != nil && *input.WebhookSecretName != "")
	if !explicitEnable {
		return false, nil
	}
	// Explicit enable or a new name must refer to an active sealed secret.
	// This also checks names recorded while automation is disabled.
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
	// Hold the same namespace-row lock used by Archive until the desired
	// revision and operation commit.
	metadata, err := s.vault.Store().GetForUpdateTx(ctx, tx.Secrets(), namespace)
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
	return false, nil
}

func (s *Service) sealWebhookSecret(ctx context.Context, tx *entities.StoreTx, actor contract.Actor, input *platform.EnvironmentUpdateInput, state platform.RevisionState, secret []byte) (bool, error) {
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
	expected, exists, err := s.webhookSecretRevision(ctx, tx, namespace)
	if err != nil {
		return false, err
	}
	if input.RotateWebhookSecret && !exists {
		return false, contract.NewFault(contract.PreconditionFailed,
			"push to deploy is not enabled, so there is no secret to rotate",
			"Enable it with --auto-deploy before rotating.")
	}
	if _, err := s.vault.Store().Set(ctx, tx.Secrets(), namespace, secret, expected, actor); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) webhookSecretRevision(ctx context.Context, tx *entities.StoreTx, namespace vault.Namespace) (uint64, bool, error) {
	metadata, err := s.vault.Store().GetTx(ctx, tx.Secrets(), namespace)
	switch {
	case err == nil:
		return metadata.LatestRevision, true, nil
	case contract.Classify(err).Code == contract.NotFound:
		// First activation creates revision 1 under the same transaction.
		return 0, false, nil
	default:
		return 0, false, err
	}
}
