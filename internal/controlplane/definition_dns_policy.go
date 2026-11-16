package controlplane

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// DefinitionDNSPolicyStore keeps non-secret policy in the control database
// through the typed entities boundary.
type DefinitionDNSPolicyStore struct{ entities *entities.Store }

// definitionPolicyLock serializes policy writes with definition admission. It
// must be acquired before application or policy row locks in either path.
const definitionPolicyLock = entities.DefinitionPolicyLockKey

func lockDefinitionPolicyTx(ctx context.Context, tx *entities.StoreTx) error {
	return tx.DefinitionDNSPolicy().LockWriters(ctx)
}

func NewDefinitionDNSPolicyStore(store *entities.Store) *DefinitionDNSPolicyStore {
	return &DefinitionDNSPolicyStore{entities: store}
}

func defaultDefinitionDNSPolicy() api.DefinitionDNSPolicy {
	return api.DefinitionDNSPolicy{Origins: []string{}, ProxyMode: "dns-only"}
}

func lockedDefinitionDNSPolicy(ctx context.Context, tx *entities.StoreTx) (api.DefinitionDNSPolicy, error) {
	record, found, err := tx.DefinitionDNSPolicy().LockShared(ctx)
	if err != nil {
		return api.DefinitionDNSPolicy{}, definitionPolicyReadFault()
	}
	if !found {
		return defaultDefinitionDNSPolicy(), nil
	}
	return definitionPolicyFromRecord(record)
}

func readDefinitionDNSPolicy(ctx context.Context, tx *entities.StoreTx) (api.DefinitionDNSPolicy, error) {
	record, found, err := tx.DefinitionDNSPolicy().Policy(ctx)
	if err != nil {
		return api.DefinitionDNSPolicy{}, definitionPolicyReadFault()
	}
	if !found {
		return defaultDefinitionDNSPolicy(), nil
	}
	return definitionPolicyFromRecord(record)
}

// definitionPolicyFromRecord validates the persisted non-secret policy and
// maps it to the transport-neutral API model.
func definitionPolicyFromRecord(record entities.DefinitionDNSPolicyRecord) (api.DefinitionDNSPolicy, error) {
	policy := api.DefinitionDNSPolicy{
		Revision: record.Revision, Enabled: record.Enabled,
		ZoneID: record.ZoneID, ZoneName: record.ZoneName,
		Origins: append([]string{}, record.Origins...), ProxyMode: record.ProxyMode,
	}
	if record.ProviderSet != record.ZoneIDSet ||
		(record.ProviderSet && (record.Provider != "cloudflare" || record.ZoneID == "" || !record.ZoneNameSet || record.ZoneName == "")) ||
		(policy.ProxyMode != "dns-only" && policy.ProxyMode != "proxied") ||
		(policy.Enabled && (!record.ZoneIDSet || len(policy.Origins) == 0)) {
		return api.DefinitionDNSPolicy{}, definitionPolicyReadFault()
	}
	if _, err := platform.NormalizeDNSOrigins(policy.Origins); err != nil {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "definition DNS policy is invalid", "Repair the stored policy before using definition DNS.")
	}
	return policy, nil
}

func (s *DefinitionDNSPolicyStore) GetDefinitionDNSPolicy(ctx context.Context, actor contract.Actor) (api.DefinitionDNSPolicy, error) {
	if err := requireCloudflareOperator(actor); err != nil {
		return api.DefinitionDNSPolicy{}, err
	}
	if s == nil || s.entities == nil {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "definition DNS policy storage is unavailable", "Check database connectivity.")
	}
	record, found, err := s.entities.DefinitionDNSPolicy().Get(ctx)
	if err != nil {
		return api.DefinitionDNSPolicy{}, definitionPolicyReadFault()
	}
	if !found {
		return defaultDefinitionDNSPolicy(), nil
	}
	return definitionPolicyFromRecord(record)
}

// SetDefinitionDNSPolicy compares the exact current revision under the shared
// advisory lock and row lock. Disable retains prior settings; enabling replaces
// all settings explicitly. The policy update and its audit entry commit in one
// entities transaction.
func (s *DefinitionDNSPolicyStore) SetDefinitionDNSPolicy(ctx context.Context, actor contract.Actor, input api.DefinitionDNSPolicyInput) (api.DefinitionDNSPolicy, error) {
	if err := requireCloudflareOperator(actor); err != nil {
		return api.DefinitionDNSPolicy{}, err
	}
	origins, err := validateDefinitionDNSPolicyInput(input)
	if err != nil {
		return api.DefinitionDNSPolicy{}, err
	}
	if s == nil || s.entities == nil {
		return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "definition DNS policy storage is unavailable", "Check database connectivity.")
	}
	var policy api.DefinitionDNSPolicy
	err = s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		return setDefinitionDNSPolicyTx(ctx, tx, actor, input, origins, &policy)
	})
	if err != nil {
		switch {
		case entities.IsBeginFailure(err):
			return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "definition DNS policy storage is unavailable", "Check database connectivity.")
		case entities.IsCommitFailure(err):
			return api.DefinitionDNSPolicy{}, contract.NewFault(contract.Unavailable, "definition DNS policy commit outcome is unknown", "Read cloudflare status and inspect the revision before retrying.")
		default:
			return api.DefinitionDNSPolicy{}, err
		}
	}
	return policy, nil
}

func setDefinitionDNSPolicyTx(ctx context.Context, tx *entities.StoreTx, actor contract.Actor, input api.DefinitionDNSPolicyInput, origins []string, policy *api.DefinitionDNSPolicy) error {
	policyTx := tx.DefinitionDNSPolicy()
	if err := lockDefinitionDNSPolicyWriteTx(ctx, policyTx, input); err != nil {
		return err
	}
	if err := writeDefinitionDNSPolicySettings(ctx, policyTx, input, origins); err != nil {
		return err
	}
	updated, found, err := policyTx.Policy(ctx)
	if err != nil {
		return definitionPolicyReadFault()
	}
	if !found {
		return definitionPolicyReadFault()
	}
	value, err := definitionPolicyFromRecord(updated)
	if err != nil {
		return err
	}
	*policy = value
	if err := tx.Audit().Record(ctx, integrations.AuditEntry{At: time.Now().UTC(), Actor: actor.Subject, Action: "cloudflare.definition-dns-policy", Target: "installation", Outcome: "succeeded"}); err != nil {
		return contract.NewFault(contract.Unavailable, "definition DNS policy audit is unavailable", "The policy was not changed; repair database connectivity and retry.")
	}
	return nil
}

func lockDefinitionDNSPolicyWriteTx(ctx context.Context, policyTx *entities.DefinitionDNSPolicyTx, input api.DefinitionDNSPolicyInput) error {
	if err := policyTx.LockWriters(ctx); err != nil {
		return definitionPolicyFailure(err, "definition DNS policy could not be locked", "Check database connectivity.")
	}
	active, err := policyTx.ActiveDefinitionRun(ctx)
	if err != nil {
		return contract.NewFault(contract.Unavailable, "active definition runs could not be checked", "Check database connectivity and schema.")
	}
	if active {
		return contract.NewFault(contract.Conflict, "definition DNS policy is reserved by an active definition run", "Wait for definition work to finish before changing policy.")
	}
	// The advisory lock makes insertion and revision comparison one
	// serialized operation, including when no singleton row exists yet.
	if err := policyTx.EnsureSingleton(ctx, []string{}, "dns-only", time.Now().UTC()); err != nil {
		return definitionPolicyFailure(err, "definition DNS policy could not be locked", "Check database connectivity and schema.")
	}
	current, found, err := policyTx.Lock(ctx)
	if err != nil || !found {
		return definitionPolicyReadFault()
	}
	if _, err := definitionPolicyFromRecord(current); err != nil {
		return err
	}
	if current.Revision != input.ExpectedRevision {
		return contract.NewFault(contract.Conflict, "definition DNS policy revision changed", "Read cloudflare status and retry with its current policy revision.")
	}
	return nil
}

func writeDefinitionDNSPolicySettings(ctx context.Context, policyTx *entities.DefinitionDNSPolicyTx, input api.DefinitionDNSPolicyInput, origins []string) *contract.FaultError {
	if !input.Enabled {
		if err := policyTx.Disable(ctx); err != nil {
			return definitionPolicyFailure(err, "definition DNS policy could not be saved", "Check database connectivity; retry after reading status.")
		}
		return nil
	}
	ref := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(input.Zone), "."))
	zones, err := policyTx.LockProviderZones(ctx, ref)
	if err != nil {
		return contract.NewFault(contract.Unavailable, "Cloudflare zone metadata could not be read", "Check database connectivity.")
	}
	if len(zones) != 1 {
		return contract.NewFault(contract.PreconditionFailed, "Cloudflare zone is missing or ambiguous", "Configure a credential first and select its exact zone ID.")
	}
	if err := policyTx.Enable(ctx, zones[0], origins, input.ProxyMode); err != nil {
		return definitionPolicyFailure(err, "definition DNS policy could not be saved", "Check database connectivity; retry after reading status.")
	}
	return nil
}

func definitionPolicyReadFault() *contract.FaultError {
	return contract.NewFault(contract.Unavailable, "definition DNS policy could not be read", "Check database connectivity and schema; do not infer that DNS is disabled.")
}

func definitionPolicyFailure(err error, message, hint string) *contract.FaultError {
	var fault *contract.FaultError
	if errors.As(err, &fault) {
		return fault
	}
	return contract.NewFault(contract.Unavailable, message, hint)
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
