package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
)

// ServerRetentionPolicy reads the server-level default retention policy. An
// absent row means the D30 defaults; a stored block is layered over them so a
// field added by a later release inherits its default instead of reading zero.
func (s *Service) ServerRetentionPolicy(ctx context.Context) (retention.Policy, error) {
	policy := retention.DefaultPolicy()
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT retention FROM eregion_server_settings WHERE singleton`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy, nil
	}
	if err != nil {
		return retention.Policy{}, unavailable()
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &policy); err != nil {
			return retention.Policy{}, unavailable(err)
		}
	}
	if err := policy.Validate(); err != nil {
		return retention.Policy{}, unavailable(err)
	}
	return policy, nil
}

// SetServerRetentionPolicy validates and stores the server-level default. The
// one settings row is upserted so the default has a single durable value.
func (s *Service) SetServerRetentionPolicy(ctx context.Context, policy retention.Policy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return unavailable(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO eregion_server_settings (singleton,retention,retention_gc_pending,updated_at)
        VALUES (true,$1,false,clock_timestamp())
        ON CONFLICT (singleton) DO UPDATE SET retention=EXCLUDED.retention, updated_at=clock_timestamp()`, raw); err != nil {
		return unavailable()
	}
	return nil
}

// EffectiveRetentionPolicy returns the policy a planner applies to one
// environment: the server default with the current revision's override fields
// replacing it one by one.
func (s *Service) EffectiveRetentionPolicy(ctx context.Context, app, environment string) (retention.Policy, error) {
	server, err := s.ServerRetentionPolicy(ctx)
	if err != nil {
		return retention.Policy{}, err
	}
	var raw []byte
	err = s.pool.QueryRow(ctx, `SELECT coalesce(r.retention,'{}'::jsonb)
        FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id
        LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id
        WHERE a.name=$1 AND e.name=$2`, app, environment).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return retention.Policy{}, contract.NewFault(contract.NotFound, "environment not found", "Create the environment under this app first.")
	}
	if err != nil {
		return retention.Policy{}, unavailable()
	}
	override := retention.Override{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &override); err != nil {
			return retention.Policy{}, unavailable(err)
		}
	}
	return retention.Merge(server, override)
}

// refusePrunedRelease refuses a deploy or rollback to a release whose artifact
// disk retention removed. The release row stays for history (D30).
func refusePrunedRelease(release platform.Release) error {
	if release.PrunedAt == nil {
		return nil
	}
	return contract.NewFault(contract.PreconditionFailed,
		fmt.Sprintf("release %s for %s/%s was pruned at %s", release.ID, release.App, release.Environment,
			release.PrunedAt.UTC().Format(time.RFC3339)),
		"Deploy a new build instead; the pruned release's artifact is gone.")
}
