package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

// PushWebhookResolver resolves one environment's current revision and its
// push-to-deploy HMAC secret. It reads the secret from the vault per delivery,
// so a rotation takes effect without restarting the API.
type PushWebhookResolver struct {
	pool    *pgxpool.Pool
	secrets *vault.EnvironmentResolver
}

var _ api.PushWebhookResolver = (*PushWebhookResolver)(nil)

// NewPushWebhookResolver builds the resolver for the API process.
func NewPushWebhookResolver(pool *pgxpool.Pool, service *vault.Service) *PushWebhookResolver {
	return &PushWebhookResolver{pool: pool, secrets: vault.NewEnvironmentResolver(service)}
}

// ResolvePushWebhook implements api.PushWebhookResolver.
func (r *PushWebhookResolver) ResolvePushWebhook(ctx context.Context, app, environment string) (api.PushWebhookTarget, bool, error) {
	if r == nil || r.pool == nil {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.Unavailable, "push to deploy is not configured", "Start the API with the control-plane database.")
	}
	var autoDeploy bool
	var secretName string
	var sourceRaw []byte
	err := r.pool.QueryRow(ctx, `SELECT coalesce(rev.auto_deploy,false), coalesce(rev.webhook_secret_name,''), coalesce(rev.source,'{}'::jsonb)
        FROM eregion_platform_apps a
        JOIN eregion_platform_environments e ON e.app_id = a.id
        LEFT JOIN eregion_platform_environment_revisions rev ON rev.id = e.current_revision_id
        WHERE a.name=$1 AND e.name=$2`, app, environment).Scan(&autoDeploy, &secretName, &sourceRaw)
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
	values, _, err := r.secrets.ResolveEnvironmentSnapshot(ctx, app, environment, []string{secretName}, nil)
	if err != nil {
		return api.PushWebhookTarget{}, false, err
	}
	secret := []byte(values[secretName])
	if len(secret) < 16 || len(secret) > 512 {
		return api.PushWebhookTarget{}, false, contract.NewFault(contract.PreconditionFailed, "the environment webhook secret is unusable", "Rotate it with eregion env update "+app+" "+environment+" --rotate-webhook-secret.")
	}
	return api.PushWebhookTarget{
		App: app, Environment: environment, Repository: repository,
		Branch: strings.TrimPrefix(strings.TrimSpace(source.Ref), "refs/heads/"), Secret: secret,
	}, true, nil
}
