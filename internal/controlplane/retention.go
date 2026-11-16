package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
)

// ServerRetentionPolicy reads the server-level default retention policy. An
// absent row means the D30 defaults; a stored block is layered over them so a
// field added by a later release inherits its default instead of reading zero.
func (s *Service) ServerRetentionPolicy(ctx context.Context) (retention.Policy, error) {
	policy := retention.DefaultPolicy()
	settings, found, err := s.entities.Retention().GetServerSettings(ctx)
	if err != nil {
		return retention.Policy{}, unavailable(err)
	}
	if !found {
		return policy, nil
	}
	if len(settings.Retention) > 0 {
		if err := json.Unmarshal(settings.Retention, &policy); err != nil {
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
	if err := s.entities.Retention().SetServerPolicy(ctx, raw); err != nil {
		return unavailable(err)
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
	raw, found, err := s.entities.Platform().Environments().RetentionOverride(ctx, app, environment)
	if err != nil {
		return retention.Policy{}, unavailable(err)
	}
	if !found {
		return retention.Policy{}, contract.NewFault(contract.NotFound, "environment not found", "Create the environment under this app first.")
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
