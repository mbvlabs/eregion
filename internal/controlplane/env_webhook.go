package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

// PushWebhookResolver resolves one environment's current revision and its
// push-to-deploy HMAC secret. It reads the environment, revision, secret
// metadata and latest ciphertext in one joined query, then decrypts through the
// vault package so cryptographic validation stays in one place. It reads the
// secret per delivery, so a rotation takes effect without restarting the API
// and no TTL cache can accept an old secret.
type PushWebhookResolver struct {
	pool    *pgxpool.Pool
	secrets *vault.Service
}

var _ api.PushWebhookResolver = (*PushWebhookResolver)(nil)

// NewPushWebhookResolver builds the resolver for the API process.
func NewPushWebhookResolver(pool *pgxpool.Pool, service *vault.Service) *PushWebhookResolver {
	return &PushWebhookResolver{pool: pool, secrets: service}
}

const pushWebhookQuery = `SELECT e.id, a.name, e.name,
    coalesce(e.current_revision_id,''),
    coalesce(r.auto_deploy,false),
    coalesce(r.webhook_secret_name,''),
    coalesce(r.source,'{}'::jsonb),
    coalesce(s.state,''),
    coalesce(s.latest_revision,0),
    coalesce(sr.revision,0),
    coalesce(sr.key_id,''),
    sr.ciphertext,
    coalesce(sr.ciphertext_sha256,''),
    coalesce(sr.revision_hash,''),
    sr.created_at
    FROM eregion_platform_apps a
    JOIN eregion_platform_environments e ON e.app_id = a.id
    LEFT JOIN eregion_platform_environment_revisions r ON r.id = e.current_revision_id
    LEFT JOIN eregion_secrets s
      ON s.app = a.name AND s.environment = e.name AND s.name = r.webhook_secret_name
    LEFT JOIN eregion_secret_revisions sr
      ON sr.app = s.app AND sr.environment = s.environment
     AND sr.name = s.name AND sr.revision = s.latest_revision
    WHERE a.name=$1 AND e.name=$2`

// ResolvePushWebhook implements api.PushWebhookResolver.
func (r *PushWebhookResolver) ResolvePushWebhook(ctx context.Context, app, environment string) (api.PushWebhookTarget, bool, error) {
	if r == nil || r.pool == nil {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.Unavailable, "push to deploy is not configured", "Start the API with the control-plane database.")
	}
	var (
		environmentID, appName, envName, revisionID string
		autoDeploy                                  bool
		secretName, secretState                     string
		secretRevision, encryptedRevision           uint64
		keyID, ciphertextHash, revisionHash         string
		sourceRaw, ciphertext                       []byte
		revisionCreated                             *time.Time
	)
	err := r.pool.QueryRow(ctx, pushWebhookQuery, app, environment).Scan(
		&environmentID, &appName, &envName, &revisionID, &autoDeploy, &secretName, &sourceRaw,
		&secretState, &secretRevision, &encryptedRevision, &keyID, &ciphertext,
		&ciphertextHash, &revisionHash, &revisionCreated)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.NotFound, "environment not found", "Check the app and environment in the webhook URL.")
	}
	if err != nil {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.Unavailable, "the environment could not be read", "Check the control-plane database.")
	}
	if !autoDeploy || secretName == "" {
		return api.PushWebhookTarget{}, false, nil
	}
	var source platform.SourceBinding
	if err := json.Unmarshal(sourceRaw, &source); err != nil {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.PreconditionFailed, "the environment source does not parse", "Re-save the environment source.")
	}
	repository, ok := platform.GitHubRepository(source.URL)
	if !ok || source.Kind != "git" {
		return api.PushWebhookTarget{}, false, nil
	}
	if r.secrets == nil || r.secrets.Store() == nil || r.secrets.Store().Ring() == nil {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.Unavailable, "secret storage is not configured", "Set EREGION_VAULT_KEY_FILE on the API process.")
	}
	if secretState == "" || secretRevision == 0 {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.NotFound, "the environment webhook secret was not found", "Rotate it with eregion env update "+app+" "+environment+" --rotate-webhook-secret.")
	}
	if encryptedRevision != secretRevision {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.PreconditionFailed, "the environment webhook secret is unusable", "Rotate it with eregion env update "+app+" "+environment+" --rotate-webhook-secret.")
	}
	namespace, err := vault.NewNamespace(appName, envName, secretName)
	if err != nil {
		return api.PushWebhookTarget{}, false, err
	}
	createdAt := time.Time{}
	if revisionCreated != nil {
		createdAt = revisionCreated.UTC()
	}
	secret, ref, err := vault.OpenRevision(r.secrets.Store().Ring(), vault.EncryptedRevision{
		Namespace: namespace, State: vault.State(secretState), Revision: encryptedRevision,
		KeyID: keyID, Ciphertext: ciphertext, CiphertextSHA256: ciphertextHash,
		RevisionHash: revisionHash, CreatedAt: createdAt,
	})
	if err != nil {
		return api.PushWebhookTarget{}, false, err
	}
	if len(secret) < 16 || len(secret) > 512 {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.PreconditionFailed, "the environment webhook secret is unusable", "Rotate it with eregion env update "+app+" "+environment+" --rotate-webhook-secret.")
	}
	return api.PushWebhookTarget{
		App: app, Environment: environment,
		EnvironmentID: environmentID, RevisionID: revisionID,
		Repository:     repository,
		Branch:         strings.TrimPrefix(strings.TrimSpace(source.Ref), "refs/heads/"),
		Secret:         secret,
		SecretRevision: ref.Revision, SecretRevisionHash: ref.Hash,
	}, true, nil
}
