// Package retention defines the disk-retention policy and run records from
// MASTER D30. It has no database, HTTP, or River dependency so the control
// plane, maintenance workers, operator CLI and node effects share one
// vocabulary.
package retention

import (
	"fmt"

	"github.com/mbvlabs/eregion/internal/contract"
)

// D30's numbers are authoritative: keep the newest 10 releases per
// environment plus any release newer than 14 days; remove registry artifacts no
// release references after 14 days; prune Buildpacks caches above 10 GiB or when
// filesystem utilization is above 80%. The serving release and its rollback
// target are always kept, which is an invariant of the planner, not a
// configurable field.
const (
	DefaultKeepReleases             = 10
	DefaultKeepNewerThanDays        = 14
	DefaultArtifactGraceDays        = 14
	DefaultBuildpackCacheMaxBytes   = int64(10) << 30
	DefaultBuildpackCacheMaxPercent = 80
)

// Limits bound a policy so a mistaken value cannot plan an empty or
// effectively infinite retention set. Values outside them are rejected.
const (
	MinKeepReleases             = 1
	MaxKeepReleases             = 1000
	MinKeepNewerThanDays        = 1
	MaxKeepNewerThanDays        = 3650
	MinArtifactGraceDays        = 1
	MaxArtifactGraceDays        = 3650
	MinBuildpackCacheMaxBytes   = int64(1) << 30
	MaxBuildpackCacheMaxBytes   = int64(1) << 50
	MinBuildpackCacheMaxPercent = 1
	MaxBuildpackCacheMaxPercent = 95
)

// Policy is a complete, effective retention policy. Environments inherit it
// from the server default and may override the release-scoped fields.
type Policy struct {
	// KeepReleases is the count of newest releases kept per environment even
	// when they are older than KeepNewerThanDays.
	KeepReleases int `json:"keep_releases"`
	// KeepNewerThanDays keeps every release created within that many days,
	// regardless of count.
	KeepNewerThanDays int `json:"keep_newer_than_days"`
	// ArtifactGraceDays removes a registry artifact no release references after
	// that many days.
	ArtifactGraceDays int `json:"artifact_grace_days"`
	// BuildpackCacheMaxBytes and BuildpackCacheMaxPercent are the two D30
	// Buildpacks-cache limits. Exceeding either one makes the cache eligible
	// for pruning.
	BuildpackCacheMaxBytes   int64 `json:"buildpack_cache_max_bytes"`
	BuildpackCacheMaxPercent int   `json:"buildpack_cache_max_percent"`
}

// DefaultPolicy returns the D30 server defaults.
func DefaultPolicy() Policy {
	return Policy{
		KeepReleases:             DefaultKeepReleases,
		KeepNewerThanDays:        DefaultKeepNewerThanDays,
		ArtifactGraceDays:        DefaultArtifactGraceDays,
		BuildpackCacheMaxBytes:   DefaultBuildpackCacheMaxBytes,
		BuildpackCacheMaxPercent: DefaultBuildpackCacheMaxPercent,
	}
}

// Override is the environment-scoped part of the policy. A nil field inherits
// the server default, so a revision stores only what the environment changes.
// The Buildpacks cache is node-wide, so it is not overridable per environment.
type Override struct {
	KeepReleases      *int `json:"keep_releases,omitempty"`
	KeepNewerThanDays *int `json:"keep_newer_than_days,omitempty"`
	ArtifactGraceDays *int `json:"artifact_grace_days,omitempty"`
}

// IsZero reports whether the environment inherits the whole server default.
func (o Override) IsZero() bool {
	return o.KeepReleases == nil && o.KeepNewerThanDays == nil && o.ArtifactGraceDays == nil
}

// Clone copies the optional fields so a new revision never aliases the
// previous revision's override.
func (o Override) Clone() Override {
	out := o
	if o.KeepReleases != nil {
		value := *o.KeepReleases
		out.KeepReleases = &value
	}
	if o.KeepNewerThanDays != nil {
		value := *o.KeepNewerThanDays
		out.KeepNewerThanDays = &value
	}
	if o.ArtifactGraceDays != nil {
		value := *o.ArtifactGraceDays
		out.ArtifactGraceDays = &value
	}
	return out
}

// Apply replaces the server policy's fields with the override's set fields. It
// does not validate; use Merge at a trust boundary.
func (o Override) Apply(policy Policy) Policy {
	if o.KeepReleases != nil {
		policy.KeepReleases = *o.KeepReleases
	}
	if o.KeepNewerThanDays != nil {
		policy.KeepNewerThanDays = *o.KeepNewerThanDays
	}
	if o.ArtifactGraceDays != nil {
		policy.ArtifactGraceDays = *o.ArtifactGraceDays
	}
	return policy
}

// Validate checks the fields the override sets. Nil means inherit.
func (o Override) Validate() error {
	if o.KeepReleases != nil {
		if err := validateRange("keep_releases", int64(*o.KeepReleases), MinKeepReleases, MaxKeepReleases); err != nil {
			return err
		}
	}
	if o.KeepNewerThanDays != nil {
		if err := validateRange("keep_newer_than_days", int64(*o.KeepNewerThanDays), MinKeepNewerThanDays, MaxKeepNewerThanDays); err != nil {
			return err
		}
	}
	if o.ArtifactGraceDays != nil {
		if err := validateRange("artifact_grace_days", int64(*o.ArtifactGraceDays), MinArtifactGraceDays, MaxArtifactGraceDays); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks every field of a complete policy.
func (p Policy) Validate() error {
	if err := validateRange("keep_releases", int64(p.KeepReleases), MinKeepReleases, MaxKeepReleases); err != nil {
		return err
	}
	if err := validateRange("keep_newer_than_days", int64(p.KeepNewerThanDays), MinKeepNewerThanDays, MaxKeepNewerThanDays); err != nil {
		return err
	}
	if err := validateRange("artifact_grace_days", int64(p.ArtifactGraceDays), MinArtifactGraceDays, MaxArtifactGraceDays); err != nil {
		return err
	}
	if err := validateRange("buildpack_cache_max_bytes", p.BuildpackCacheMaxBytes, MinBuildpackCacheMaxBytes, MaxBuildpackCacheMaxBytes); err != nil {
		return err
	}
	return validateRange("buildpack_cache_max_percent", int64(p.BuildpackCacheMaxPercent), MinBuildpackCacheMaxPercent, MaxBuildpackCacheMaxPercent)
}

// Merge validates the server policy and the override, then returns the
// environment's effective policy. Environment fields replace the server fields
// one by one; unset fields stay inherited.
func Merge(server Policy, override Override) (Policy, error) {
	if err := server.Validate(); err != nil {
		return Policy{}, err
	}
	if err := override.Validate(); err != nil {
		return Policy{}, err
	}
	return override.Apply(server), nil
}

// BuildpackCacheExceeded reports whether a node needs its Buildpacks cache
// pruned. D30's two limits are independent: a cache larger than the absolute
// cap or a filesystem already more than the configured percentage used both
// trigger pruning. filesystemUsedBytes is total filesystem use (the cache is
// part of it), not free or non-cache use. A non-positive filesystemTotalBytes
// means the filesystem size is unknown, so only the absolute limit applies.
func (p Policy) BuildpackCacheExceeded(cacheBytes, filesystemUsedBytes, filesystemTotalBytes int64) bool {
	if cacheBytes > p.BuildpackCacheMaxBytes {
		return true
	}
	if filesystemTotalBytes <= 0 {
		return false
	}
	if filesystemUsedBytes < 0 {
		filesystemUsedBytes = 0
	}
	return float64(filesystemUsedBytes) > float64(filesystemTotalBytes)*float64(p.BuildpackCacheMaxPercent)/100
}

func validateRange(field string, value, minimum, maximum int64) error {
	if value < minimum || value > maximum {
		return contract.NewFault(contract.ValidationFailed,
			fmt.Sprintf("invalid retention %s", field),
			fmt.Sprintf("Use %d–%d for retention %s.", minimum, maximum, field))
	}
	return nil
}
