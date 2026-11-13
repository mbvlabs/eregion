package controlplane

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// DefinitionDNSPolicyStore keeps non-secret policy in the control database.
type DefinitionDNSPolicyStore struct{ pool *pgxpool.Pool }

// definitionPolicyLock serializes policy writes with definition admission. It
// must be acquired before application or policy row locks in either path.
const definitionPolicyLock int64 = 0x657265646e73706c

func lockDefinitionPolicyTx(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, definitionPolicyLock)
	return err
}

func NewDefinitionDNSPolicyStore(pool *pgxpool.Pool) *DefinitionDNSPolicyStore {
	return &DefinitionDNSPolicyStore{pool: pool}
}

const definitionDNSPolicySelect = `SELECT p.revision, p.enabled, p.provider, p.zone_id, c.zone_name, p.origins, p.proxy_mode
 FROM eregion_definition_dns_policy p LEFT JOIN eregion_provider_credentials c ON c.provider=p.provider AND c.zone_id=p.zone_id WHERE p.singleton=true`

func defaultDefinitionDNSPolicy() api.DefinitionDNSPolicy {
	return api.DefinitionDNSPolicy{Origins: []string{}, ProxyMode: "dns-only"}
}

func scanDefinitionDNSPolicy(row pgx.Row) (api.DefinitionDNSPolicy, error) {
	var policy api.DefinitionDNSPolicy
	var revision int64
	var provider, zone, zoneName *string
	err := row.Scan(&revision, &policy.Enabled, &provider, &zone, &zoneName, &policy.Origins, &policy.ProxyMode)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultDefinitionDNSPolicy(), nil
	}
	if err != nil || revision < 0 || (provider == nil) != (zone == nil) ||
		(provider != nil && (*provider != "cloudflare" || zone == nil || *zone == "" || zoneName == nil || *zoneName == "")) ||
		(policy.ProxyMode != "dns-only" && policy.ProxyMode != "proxied") ||
		(policy.Enabled && (zone == nil || len(policy.Origins) == 0)) {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "definition DNS policy could not be read", "Check database connectivity and schema; do not infer that DNS is disabled.")
	}
	if _, err := platform.NormalizeDNSOrigins(policy.Origins); err != nil {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "definition DNS policy is invalid", "Repair the stored policy before using definition DNS.")
	}
	policy.Revision = uint64(revision)
	if zone != nil {
		policy.ZoneID, policy.ZoneName = *zone, *zoneName
	}
	return policy, nil
}

func (s *DefinitionDNSPolicyStore) GetDefinitionDNSPolicy(ctx context.Context, actor contract.Actor) (api.DefinitionDNSPolicy, error) {
	if err := requireCloudflareOperator(actor); err != nil {
		return api.DefinitionDNSPolicy{}, err
	}
	if s == nil || s.pool == nil {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "definition DNS policy storage is unavailable", "Check database connectivity.")
	}
	return scanDefinitionDNSPolicy(s.pool.QueryRow(ctx, definitionDNSPolicySelect))
}

// SetDefinitionDNSPolicy compares the exact current revision under a row lock.
// Disable retains prior settings; enabling replaces all settings explicitly.
func (s *DefinitionDNSPolicyStore) SetDefinitionDNSPolicy(ctx context.Context, actor contract.Actor, input api.DefinitionDNSPolicyInput) (api.DefinitionDNSPolicy, error) {
	if err := requireCloudflareOperator(actor); err != nil {
		return api.DefinitionDNSPolicy{}, err
	}
	origins, err := validateDefinitionDNSPolicyInput(input)
	if err != nil {
		return api.DefinitionDNSPolicy{}, err
	}
	if s == nil || s.pool == nil {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "definition DNS policy storage is unavailable", "Check database connectivity.")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "definition DNS policy storage is unavailable", "Check database connectivity.")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := lockDefinitionPolicyTx(ctx, tx); err != nil {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "definition DNS policy could not be locked", "Check database connectivity.")
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM eregion_definition_runs WHERE finished_at IS NULL)`).Scan(&active); err != nil {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "active definition runs could not be checked", "Check database connectivity and schema.")
	}
	if active {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Conflict, "definition DNS policy is reserved by an active definition run", "Wait for definition work to finish before changing policy.")
	}
	// The advisory lock makes insertion and revision comparison one serialized
	// operation, including when no singleton row exists yet.
	_, err = tx.Exec(ctx, `INSERT INTO eregion_definition_dns_policy
		(singleton, revision, enabled, provider, zone_id, origins, proxy_mode, updated_at)
		VALUES (true, 0, false, NULL, NULL, $1, $2, $3) ON CONFLICT (singleton) DO NOTHING`,
		[]string{}, "dns-only", time.Now().UTC())
	if err != nil {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "definition DNS policy could not be locked", "Check database connectivity and schema.")
	}
	current, err := scanDefinitionDNSPolicy(tx.QueryRow(ctx, definitionDNSPolicySelect+` FOR UPDATE OF p`))
	if err != nil {
		return api.DefinitionDNSPolicy{}, err
	}
	if current.Revision != input.ExpectedRevision {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Conflict, "definition DNS policy revision changed", "Read cloudflare status and retry with its current policy revision.")
	}
	if input.Enabled {
		ref := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(input.Zone), "."))
		rows, err := tx.Query(ctx, `SELECT zone_id FROM eregion_provider_credentials WHERE provider='cloudflare' AND latest_revision>0 AND (zone_id=$1 OR zone_name=$1) FOR SHARE`, ref)
		if err != nil {
			return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "Cloudflare zone metadata could not be read", "Check database connectivity.")
		}
		var zones []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "Cloudflare zone metadata could not be read", "Check database connectivity.")
			}
			zones = append(zones, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "Cloudflare zone metadata could not be read", "Check database connectivity.")
		}
		if len(zones) != 1 {
			return api.DefinitionDNSPolicy{}, contract.NewFault(contract.PreconditionFailed, "Cloudflare zone is missing or ambiguous", "Configure a credential first and select its exact zone ID.")
		}
		_, err = tx.Exec(ctx, `UPDATE eregion_definition_dns_policy SET revision=revision+1, enabled=true, provider='cloudflare', zone_id=$1, origins=$2, proxy_mode=$3, updated_at=clock_timestamp() WHERE singleton=true`, zones[0], origins, input.ProxyMode)
	} else {
		_, err = tx.Exec(ctx, `UPDATE eregion_definition_dns_policy SET revision=revision+1, enabled=false, updated_at=clock_timestamp() WHERE singleton=true`)
	}
	if err != nil {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "definition DNS policy could not be saved", "Check database connectivity; retry after reading status.")
	}
	policy, err := scanDefinitionDNSPolicy(tx.QueryRow(ctx, definitionDNSPolicySelect))
	if err != nil {
		return api.DefinitionDNSPolicy{}, err
	}
	if err := recordAuditTx(ctx, tx, integrations.AuditEntry{At: time.Now().UTC(), Actor: actor.Subject, Action: "cloudflare.definition-dns-policy", Target: "installation", Outcome: "succeeded"}); err != nil {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "definition DNS policy audit is unavailable", "The policy was not changed; repair database connectivity and retry.")
	}
	if err := tx.Commit(ctx); err != nil {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "definition DNS policy commit outcome is unknown", "Read cloudflare status and inspect the revision before retrying.")
	}
	return policy, nil
}

func validateDefinitionDNSPolicyInput(input api.DefinitionDNSPolicyInput) ([]string, error) {
	var origins []string
	if input.Enabled {
		var err error
		origins, err = platform.NormalizeDNSOrigins(input.Origins)
		if err != nil {
			return nil, err
		}
		if len(origins) == 0 || strings.TrimSpace(input.Zone) == "" || (input.ProxyMode != "proxied" && input.ProxyMode != "dns-only") {
			return nil, contract.NewFault(contract.ValidationFailed, "enabled definition DNS requires a zone, public origins and proxy mode", "Supply --zone, --origin and --proxy explicitly.")
		}
		sort.Strings(origins)
	} else if input.Zone != "" || len(input.Origins) != 0 || input.ProxyMode != "" {
		return nil, contract.NewFault(contract.ValidationFailed, "disable does not change policy settings", "Omit zone, origins and proxy when disabling.")
	}
	return origins, nil
}
