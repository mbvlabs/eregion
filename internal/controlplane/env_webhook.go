package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

// PushWebhookResolver resolves one environment's current revision and its
// push-to-deploy HMAC secret. It reads the environment, revision, secret
// metadata and the latest ciphertext through one typed persistence read, then
// decrypts through the vault package so cryptographic validation stays in one
// place. It reads the secret per delivery, so a rotation takes effect without
// restarting the API and no TTL cache can accept an old secret.
type PushWebhookResolver struct {
	store   *entities.Store
	secrets *vault.Service
}

var _ api.PushWebhookResolver = (*PushWebhookResolver)(nil)

// NewPushWebhookResolver builds the resolver for the API process.
func NewPushWebhookResolver(store *entities.Store, service *vault.Service) *PushWebhookResolver {
	return &PushWebhookResolver{store: store, secrets: service}
}

// ResolvePushWebhook implements api.PushWebhookResolver.
func (r *PushWebhookResolver) ResolvePushWebhook(ctx context.Context, app, environment string) (api.PushWebhookTarget, bool, error) {
	if r == nil || r.store == nil {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.Unavailable, "push to deploy is not configured", "Start the API with the control-plane database.")
	}
	binding, found, err := r.store.Platform().Outcomes().PushWebhookBinding(ctx, app, environment)
	if err != nil {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.Unavailable, "the environment could not be read", "Check the control-plane database.")
	}
	if !found {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.NotFound, "environment not found", "Check the app and environment in the webhook URL.")
	}
	if !binding.AutoDeploy || binding.WebhookSecretName == "" {
		return api.PushWebhookTarget{}, false, nil
	}
	var source platform.SourceBinding
	if err := json.Unmarshal(binding.Source, &source); err != nil {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.PreconditionFailed, "the environment source does not parse", "Re-save the environment source.")
	}
	repository, ok := platform.GitHubRepository(source.URL)
	if !ok || source.URL == "" {
		return api.PushWebhookTarget{}, false, nil
	}
	if r.secrets == nil || r.secrets.Store() == nil || r.secrets.Store().Ring() == nil {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.Unavailable, "secret storage is not configured", "Set EREGION_VAULT_KEY_FILE on the API process.")
	}
	if binding.SecretState == "" || binding.LatestRevision == 0 {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.NotFound, "the environment webhook secret was not found", "Rotate it with eregion env update "+app+" "+environment+" --rotate-webhook-secret.")
	}
	if uint64(binding.Revision) != uint64(binding.LatestRevision) {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.PreconditionFailed, "the environment webhook secret is unusable", "Rotate it with eregion env update "+app+" "+environment+" --rotate-webhook-secret.")
	}
	namespace, err := vault.NewNamespace(binding.App, binding.Environment, binding.WebhookSecretName)
	if err != nil {
		return api.PushWebhookTarget{}, false, err
	}
	createdAt := time.Time{}
	if binding.RevisionCreatedAt != nil {
		createdAt = binding.RevisionCreatedAt.UTC()
	}
	secret, ref, err := vault.OpenRevision(r.secrets.Store().Ring(), vault.EncryptedRevision{
		Namespace: namespace, State: vault.State(binding.SecretState), Revision: uint64(binding.Revision),
		KeyID: binding.KeyID, Ciphertext: binding.Ciphertext, CiphertextSHA256: binding.CiphertextSHA256,
		RevisionHash: binding.RevisionHash, CreatedAt: createdAt,
	})
	if err != nil {
		return api.PushWebhookTarget{}, false, err
	}
	if len(secret) < 16 || len(secret) > 512 {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.PreconditionFailed, "the environment webhook secret is unusable", "Rotate it with eregion env update "+app+" "+environment+" --rotate-webhook-secret.")
	}
	return api.PushWebhookTarget{
		App: app, Environment: environment,
		EnvironmentID: string(binding.EnvironmentID), RevisionID: string(binding.CurrentRevisionID),
		Repository:     repository,
		Branch:         strings.TrimPrefix(strings.TrimSpace(source.Ref), "refs/heads/"),
		Secret:         secret,
		SecretRevision: ref.Revision, SecretRevisionHash: ref.Hash,
	}, true, nil
}
