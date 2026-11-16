package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

const (
	domainDNSStatePending         = "pending"
	domainDNSStateApplied         = "applied"
	domainDNSStateFailed          = "failed"
	domainDNSStateCleanupPending  = "cleanup_pending"
	domainDNSStateCleanupVerified = "cleanup_verified"
	domainDNSStateCleanupFailed   = "cleanup_failed"
	definitionCleanupUnknown      = "definition cleanup outcome unknown"
)

type domainDNSLedger struct {
	DomainID          platform.DomainID
	Domain            string
	App               string
	Environment       string
	Provider          string
	ZoneID            string
	ZoneName          string
	Ownership         string
	Generation        int64
	DesiredOrigins    []string
	DesiredProxied    bool
	State             string
	LastOperationID   contract.OperationID
	LastError         string
	AppliedGeneration int64
	CleanupError      string
}

func domainDNSLedgerFromEntities(ledger entities.DomainDNSLedger) domainDNSLedger {
	return domainDNSLedger{
		DomainID: ledger.DomainID, Domain: ledger.Domain, App: ledger.App, Environment: ledger.Environment,
		Provider: ledger.Provider, ZoneID: ledger.ZoneID, ZoneName: ledger.ZoneName,
		Ownership: ledger.OwnershipMarker, Generation: ledger.Generation,
		DesiredOrigins: ledger.DesiredOrigins, DesiredProxied: ledger.DesiredProxied,
		State: ledger.State, LastOperationID: ledger.LastOperationID, LastError: ledger.LastError,
		AppliedGeneration: ledger.AppliedGeneration, CleanupError: ledger.CleanupError,
	}
}

func (s *Service) loadDomainDNSLedger(ctx context.Context, app, environment, domain string) (*domainDNSLedger, error) {
	ledger, found, err := s.entities.Platform().DomainDNS().GetByAppEnvironmentDomain(ctx, app, environment, domain)
	if err != nil {
		return nil, unavailable(err)
	}
	if !found {
		return nil, nil
	}
	mapped := domainDNSLedgerFromEntities(ledger)
	return &mapped, nil
}

func loadDomainDNSLedgerByDomainIDTx(ctx context.Context, tx *entities.StoreTx, domainID platform.DomainID) (*domainDNSLedger, error) {
	ledger, found, err := tx.Platform().DomainDNS().GetByDomainID(ctx, domainID)
	if err != nil {
		return nil, unavailable(err)
	}
	if !found {
		return nil, nil
	}
	mapped := domainDNSLedgerFromEntities(ledger)
	return &mapped, nil
}

func (s *Service) loadDomainDNSLedgersForEnvironments(ctx context.Context, environmentIDs []platform.EnvironmentID) ([]domainDNSLedger, error) {
	if len(environmentIDs) == 0 {
		return nil, nil
	}
	rows, err := s.entities.Platform().DomainDNS().ListForEnvironments(ctx, environmentIDs)
	if err != nil {
		return nil, unavailable(err)
	}
	ledgers := make([]domainDNSLedger, 0, len(rows))
	for _, row := range rows {
		ledgers = append(ledgers, domainDNSLedgerFromEntities(row))
	}
	return ledgers, nil
}

func managedDNSBinding(ledger domainDNSLedger) platform.ManagedDNSBinding {
	return platform.ManagedDNSBinding{
		Provider: ledger.Provider, ZoneID: ledger.ZoneID, ZoneName: ledger.ZoneName,
		Origins: append([]string(nil), ledger.DesiredOrigins...), Proxied: ledger.DesiredProxied,
		State: ledger.State, Generation: ledger.Generation, AppliedGeneration: ledger.AppliedGeneration,
	}
}

// checkDomainDNSBoundary fences a worker attempt and checks cancellation at a
// boundary where a provider effect or durable deletion is about to happen. A
// stale attempt and a cancellation both stop before the effect.
func dnsBoundaryStop(err error) bool {
	return errors.Is(err, errPlatformCancelled) || errors.Is(err, errNotOwned) || errors.Is(err, errOperationTerminal)
}

func (s *Service) checkDomainDNSBoundary(ctx context.Context, id contract.OperationID, attempt uint32) error {
	state, current, err := s.operationState(ctx, id)
	if err != nil {
		return err
	}
	if current != attempt {
		return errNotOwned
	}
	if state == contract.Cancelling {
		return errPlatformCancelled
	}
	if state.Terminal() {
		return errOperationTerminal
	}
	return nil
}

func (s *Service) authorizeDomainDNSWrite(ctx context.Context, actor contract.Actor) error {
	if s.domainDNS == nil {
		return contract.NewFault(contract.InputRequired, "Cloudflare DNS is not configured on this control plane", "Configure a provider credential with eregion dns configure --provider cloudflare; retry.")
	}
	return s.domainDNS.AuthorizeWrite(ctx, actor)
}

func (s *Service) resolveDomainDNSZone(ctx context.Context, ref string) (integrations.DNSZone, error) {
	if s.domainDNS == nil {
		return integrations.DNSZone{}, contract.NewFault(contract.InputRequired, "Cloudflare DNS is not configured on this control plane", "Configure a provider credential with eregion dns configure --provider cloudflare; manual DNS remains the default.")
	}
	zone, err := s.domainDNS.ResolveZone(ctx, ref)
	if err != nil {
		return integrations.DNSZone{}, err
	}
	return zone, nil
}

// resolveDomainDNSZoneTx resolves the same exact zone reference as
// CloudflareCredentialStore.DNSZone from the admission transaction's provider
// metadata. An open snapshot must never borrow a second pool connection.
func (s *Service) resolveDomainDNSZoneTx(ctx context.Context, tx *entities.StoreTx, ref string) (integrations.DNSZone, error) {
	if s.domainDNS == nil {
		return integrations.DNSZone{}, contract.NewFault(contract.InputRequired, "Cloudflare DNS is not configured on this control plane", "Configure a provider credential with eregion dns configure --provider cloudflare; manual DNS remains the default.")
	}
	ref = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(ref), "."))
	metadata, err := tx.Providers().ListProviderMetadata(ctx, vault.ProviderCloudflare, vault.MaxProviderZones)
	if err != nil {
		return integrations.DNSZone{}, unavailable(err)
	}
	for _, zone := range metadata {
		if zone.ZoneID == ref || strings.EqualFold(zone.ZoneName, ref) {
			return integrations.DNSZone{ZoneID: zone.ZoneID, ZoneName: zone.ZoneName}, nil
		}
	}
	return integrations.DNSZone{}, contract.NewFault(contract.InputRequired, "Cloudflare zone is not configured on this control plane", "Configure it with eregion dns configure --provider cloudflare; manual DNS remains the default.")
}

func (s *Service) validateDomainDNSCleanupRecovery(ledger domainDNSLedger) error {
	zone := integrations.DNSZone{ZoneID: ledger.ZoneID, ZoneName: ledger.ZoneName}
	if err := integrations.ValidateDomainDNSHost(zone, ledger.Domain); err != nil {
		if ledger.AppliedGeneration == 0 && ledger.State != domainDNSStateApplied {
			// A pending/failed intent never had an applied provider effect. It
			// can be discarded without touching Cloudflare.
			return nil
		}
		return contract.NewFault(contract.PreconditionFailed,
			"managed DNS ledger zone does not contain the binding hostname",
			"The invalid applied intent is retained; no provider cleanup was attempted. Inspect the ledger and repair the binding deliberately.")
	}
	return nil
}

func (s *Service) ensureDomainDNSCleanupSafeTx(ctx context.Context, tx *entities.StoreTx, actor contract.Actor, app, environment string) error {
	rows, err := tx.Platform().DomainDNS().CleanupViolations(ctx, app, environment)
	if err != nil {
		return unavailable(err)
	}
	found := false
	for _, row := range rows {
		found = true
		ledger := domainDNSLedger{
			Domain: row.Domain, ZoneID: row.ZoneID, ZoneName: row.ZoneName,
			State: row.State, AppliedGeneration: row.AppliedGeneration,
		}
		if err := s.validateDomainDNSCleanupRecovery(ledger); err != nil {
			return err
		}
	}
	if found {
		return s.authorizeDomainDNSWrite(ctx, actor)
	}
	return nil
}

func upsertDomainDNSIntentTx(ctx context.Context, tx *entities.StoreTx, domainID platform.DomainID, zone integrations.DNSZone, origins []string, proxied bool, opID contract.OperationID) error {
	marker := integrations.DomainDNSOwnership(string(domainID), zone.ZoneID)
	existingZone, found, err := tx.Platform().DomainDNS().LockLedger(ctx, domainID)
	if err != nil {
		return unavailable(err)
	}
	if found && existingZone != zone.ZoneID {
		return contract.NewFault(contract.Conflict,
			"managed DNS zone cannot change on an existing binding",
			"Remove the binding and its managed records first, then add it with the new zone.")
	}
	if _, err := tx.Platform().DomainDNS().UpsertIntent(ctx, domainID, zone.ZoneID, zone.ZoneName, marker, origins, proxied, opID); err != nil {
		return unavailable(err)
	}
	return nil
}

// fenceDomainDNSOperationTx makes a non-provider domain add (for example a
// primary promotion or check-path update) the newest operation for a managed
// binding without changing its desired provider state.
func fenceDomainDNSOperationTx(ctx context.Context, tx *entities.StoreTx, domainID platform.DomainID, opID contract.OperationID) error {
	if _, err := tx.Platform().DomainDNS().FenceOperation(ctx, domainID, opID); err != nil {
		return unavailable(err)
	}
	return nil
}

func insertDomainDNSCleanupIntentTx(ctx context.Context, tx *entities.StoreTx, domainID platform.DomainID, zone integrations.DNSZone, opID contract.OperationID) error {
	marker := integrations.DomainDNSOwnership(string(domainID), zone.ZoneID)
	if _, err := tx.Platform().DomainDNS().InsertCleanupIntent(ctx, domainID, zone.ZoneID, zone.ZoneName, marker, opID); err != nil {
		return unavailable(err)
	}
	return nil
}

func markDomainDNSCleanupTx(ctx context.Context, tx *entities.StoreTx, domainID platform.DomainID, opID contract.OperationID) error {
	if _, err := tx.Platform().DomainDNS().MarkCleanupPending(ctx, domainID, opID); err != nil {
		return unavailable(err)
	}
	return nil
}

func markDomainDNSCleanupForEnvironmentsTx(ctx context.Context, tx *entities.StoreTx, app, environment string, opID contract.OperationID) error {
	if _, err := tx.Platform().DomainDNS().MarkCleanupPendingForEnvironments(ctx, app, environment, opID); err != nil {
		return unavailable(err)
	}
	return nil
}

func (s *Service) markDomainDNSCleanupVerified(ctx context.Context, ledger domainDNSLedger, opID contract.OperationID) (bool, error) {
	rows, err := s.entities.Platform().DomainDNS().MarkCleanupVerified(ctx, ledger.DomainID, opID)
	if err != nil {
		return false, unavailable(err)
	}
	return rows == 1, nil
}

func markDomainDNSCleanupFailed(ctx context.Context, tx *entities.StoreTx, domainID platform.DomainID, opID contract.OperationID, message string) error {
	if _, err := tx.Platform().DomainDNS().MarkCleanupFailed(ctx, domainID, opID, boundedRouteError(message)); err != nil {
		return unavailable(err)
	}
	return nil
}

func markDomainDNSApplied(ctx context.Context, tx *entities.StoreTx, ledger domainDNSLedger, opID contract.OperationID) (bool, error) {
	rows, err := tx.Platform().DomainDNS().MarkApplied(ctx, ledger.DomainID, opID)
	if err != nil {
		return false, unavailable(err)
	}
	return rows == 1, nil
}

func markDomainDNSFailed(ctx context.Context, tx *entities.StoreTx, ledger domainDNSLedger, opID contract.OperationID, message string) error {
	rows, err := tx.Platform().DomainDNS().MarkFailed(ctx, ledger.DomainID, opID, ledger.Generation, boundedRouteError(message))
	if err != nil {
		return unavailable(err)
	}
	if rows != 1 {
		return definitionDNSConflict()
	}
	return nil
}

func (s *Service) finishDomainOperation(ctx context.Context, id contract.OperationID, attempt uint32, finalize func(*entities.StoreTx, *contract.Operation) error) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		if op.State == contract.Cancelling {
			return errPlatformCancelled
		}
		if err := finalize(tx, op); err != nil {
			return err
		}
		return transition(ctx, tx, op, contract.Succeeded, "", "Platform operation succeeded; durable effects were retained.")
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func (s *Service) applyDomainDNSLedger(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger) error {
	if s.domainDNS == nil {
		return contract.NewFault(contract.InputRequired, "Cloudflare DNS is not configured on this control plane", "Configure the missing provider credential with eregion dns configure --provider cloudflare; the durable binding is retained.")
	}
	if ledger.LastOperationID != op.ID {
		return contract.NewFault(contract.Conflict, "a newer domain operation owns this managed binding", "This operation is stale and did not apply DNS; retry the current intent.")
	}
	if err := s.checkDomainDNSBoundary(ctx, op.ID, attempt); err != nil {
		return err
	}
	request := integrations.DomainDNSRequest{
		ZoneID: ledger.ZoneID, ZoneName: ledger.ZoneName, DomainID: string(ledger.DomainID),
		Ownership: ledger.Ownership, Host: ledger.Domain, Addresses: ledger.DesiredOrigins,
		Proxied: ledger.DesiredProxied,
	}
	guard := func(mutationCtx context.Context) error {
		return s.checkDomainDNSBoundary(mutationCtx, op.ID, attempt)
	}
	applied, unchanged, store, err := s.executeDomainDNSLedgerEffect(ctx, op, attempt, ledger, request, guard)
	if err != nil {
		if !dnsBoundaryStop(err) {
			if recordErr := s.recordDomainDNSFailure(ctx, op, attempt, ledger, err); recordErr != nil {
				return recordErr
			}
		}
		return err
	}
	if err := s.checkDomainDNSBoundary(ctx, op.ID, attempt); err != nil {
		return err
	}
	if store != nil {
		if err := store.Guard(ctx); err != nil {
			return err
		}
	}
	var marked bool
	_, owned, err := s.mutateOwned(ctx, op.ID, attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		if current.State == contract.Cancelling {
			return errPlatformCancelled
		}
		var err error
		marked, err = markDomainDNSApplied(ctx, tx, ledger, op.ID)
		return err
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	if !marked {
		return contract.NewFault(contract.Conflict, "managed DNS ledger changed after the provider effect", "A newer binding operation owns the ledger; reconcile the current intent before trusting this result.")
	}
	s.appendWorkerEvent(ctx, op.ID, attempt, "dns", "Managed DNS applied generation "+fmt.Sprint(ledger.Generation)+" for "+ledger.Domain+" ("+fmt.Sprint(applied)+" changed, "+fmt.Sprint(unchanged)+" unchanged); provider state is separate from public readiness.")
	return nil
}

func (s *Service) executeDomainDNSLedgerEffect(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger, request integrations.DomainDNSRequest, guard func(context.Context) error) (int, int, *definitionDNSProgressStore, error) {
	step, err := s.definitionDNSChild(ctx, op.ID)
	if err != nil {
		return 0, 0, nil, err
	}
	if step == nil {
		plan, err := s.domainDNS.ApplyWithGuard(ctx, op.Actor, op.ID, request, guard)
		if err != nil {
			return 0, 0, nil, err
		}
		return plan.Applied, plan.Unchanged, nil, nil
	}
	if step.Plan == nil {
		return 0, 0, nil, definitionDNSConflict()
	}
	store := newDefinitionDNSProgressStore(s, op, attempt, ledger, step)
	applied, err := s.domainDNS.ExecuteApprovedDefinitionDNS(ctx, op.Actor, op.ID, request, *step.Plan, store)
	if err != nil {
		return 0, 0, store, err
	}
	return applied, 0, store, nil
}

// preflightDomainDNSLedger performs every read-only check and ownership plan
// for one managed binding before the deletion worker removes any serving
// workload or mutates any provider record. A conflict, authorization failure or
// unsafe ledger therefore stops the whole deletion before its first DNS write.
func (s *Service) preflightDomainDNSLedger(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger) error {
	if s.domainDNS == nil {
		return contract.NewFault(contract.InputRequired, "Cloudflare DNS is not configured on this control plane", "Configure the missing provider credential with eregion dns configure --provider cloudflare; no workload was removed.")
	}
	if ledger.LastOperationID != op.ID {
		return contract.NewFault(contract.Conflict, "a newer operation owns this managed binding cleanup", "No provider mutation was attempted; retry the current deletion.")
	}
	if err := s.checkDomainDNSBoundary(ctx, op.ID, attempt); err != nil {
		return err
	}
	if err := s.domainDNS.AuthorizeWrite(ctx, op.Actor); err != nil {
		return err
	}
	if err := s.validateDomainDNSCleanupRecovery(ledger); err != nil {
		return err
	}
	zone := integrations.DNSZone{ZoneID: ledger.ZoneID, ZoneName: ledger.ZoneName}
	if err := integrations.ValidateDomainDNSHost(zone, ledger.Domain); err != nil {
		// The cleanup path can discard an invalid pending/failed intent without
		// touching the provider because no effect could have been applied in
		// this zone. Preflight permits that safe recovery.
		return nil
	}
	plan, err := s.domainDNS.PlanRemove(ctx, integrations.DomainDNSRequest{
		ZoneID: ledger.ZoneID, ZoneName: ledger.ZoneName, DomainID: string(ledger.DomainID),
		Ownership: ledger.Ownership, Host: ledger.Domain,
	})
	if err != nil {
		return err
	}
	if len(plan.Conflicts) > 0 {
		fault := contract.NewFault(contract.PreconditionFailed,
			"Cloudflare DNS cleanup preflight found a conflicting record",
			"No provider record was mutated and no workload was removed; resolve the conflicting record and retry the deletion.")
		fault.Details = map[string]any{"zone": ledger.ZoneName, "host": ledger.Domain, "conflicts": plan.Conflicts}
		return fault
	}
	return nil
}

func (s *Service) cleanupDomainDNSLedger(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger) error {
	if s.domainDNS == nil {
		return contract.NewFault(contract.InputRequired, "Cloudflare DNS is not configured on this control plane", "Configure the missing provider credential with eregion dns configure --provider cloudflare; the binding and cleanup ledger are retained.")
	}
	if ledger.LastOperationID != op.ID {
		return contract.NewFault(contract.Conflict, "a newer operation owns this managed binding cleanup", "This deletion is stale; retry the current deletion after the newer operation finishes.")
	}
	if err := s.checkDomainDNSBoundary(ctx, op.ID, attempt); err != nil {
		return err
	}
	step, err := s.definitionDNSChild(ctx, op.ID)
	if err != nil {
		return err
	}
	if step != nil && step.Cleanup == nil {
		return definitionDNSConflict()
	}
	if err := s.domainDNS.AuthorizeWrite(ctx, op.Actor); err != nil {
		return err
	}
	if err := s.validateDomainDNSCleanupRecovery(ledger); err != nil {
		return err
	}
	zone := integrations.DNSZone{ZoneID: ledger.ZoneID, ZoneName: ledger.ZoneName}
	if err := integrations.ValidateDomainDNSHost(zone, ledger.Domain); err != nil {
		return s.cleanupInvalidDomainDNSIntent(ctx, op, attempt, ledger, step)
	}
	request := integrations.DomainDNSRequest{
		ZoneID: ledger.ZoneID, ZoneName: ledger.ZoneName, DomainID: string(ledger.DomainID),
		Ownership: ledger.Ownership, Host: ledger.Domain,
	}
	guard := func(mutationCtx context.Context) error {
		if err := s.checkDomainDNSBoundary(mutationCtx, op.ID, attempt); err != nil {
			return err
		}
		if step != nil {
			return s.guardDefinitionDNSCleanup(mutationCtx, op, attempt, ledger, step, request)
		}
		return nil
	}
	if step != nil {
		if err := s.fenceDefinitionDNSCleanup(ctx, op, attempt, ledger, step, guard); err != nil {
			return err
		}
	}
	plan, err := s.domainDNS.RemoveWithGuard(ctx, op.Actor, op.ID, request, guard)
	if err != nil {
		if !dnsBoundaryStop(err) {
			s.markCleanupFailure(ctx, op, attempt, ledger, err)
		}
		return err
	}
	return s.finishDomainDNSCleanup(ctx, op, attempt, ledger, step, plan, guard)
}

func (s *Service) cleanupInvalidDomainDNSIntent(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger, step *definitionDNSChild) error {
	if step != nil {
		return definitionDNSConflict()
	}
	// A pending/failed intent that never applied cannot own a provider record
	// in this zone. Record verification without touching Cloudflare.
	marked, err := s.markDomainDNSCleanupVerified(ctx, ledger, op.ID)
	if err != nil {
		return err
	}
	if !marked {
		return contract.NewFault(contract.Conflict, "managed DNS cleanup ledger changed before recovery was recorded", "A newer operation owns the binding; retry the current intent.")
	}
	s.appendWorkerEvent(ctx, op.ID, attempt, "dns", "Discarded invalid pending managed-DNS intent for "+ledger.Domain+" without provider access; the zone does not contain the hostname.")
	return nil
}

func (s *Service) fenceDefinitionDNSCleanup(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger, step *definitionDNSChild, guard func(context.Context) error) error {
	if err := guard(ctx); err != nil {
		return err
	}
	if len(step.Cleanup.Changes) == 0 {
		return nil
	}
	// Persist a conservative in-flight fence before any DELETE. A restarted
	// attempt must read back, never redispatch the cleanup.
	_, owned, err := s.mutateOwned(ctx, op.ID, attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		if current.State != contract.Running {
			return errPlatformCancelled
		}
		rows, err := tx.Platform().DomainDNS().SetCleanupError(ctx, ledger.DomainID, op.ID, ledger.Generation, definitionCleanupUnknown)
		if err != nil {
			return unavailable(err)
		}
		if rows != 1 {
			return definitionDNSConflict()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func (s *Service) finishDomainDNSCleanup(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger, step *definitionDNSChild, plan integrations.DomainDNSPlan, guard func(context.Context) error) error {
	if err := s.checkDomainDNSBoundary(ctx, op.ID, attempt); err != nil {
		return err
	}
	if step != nil {
		if err := guard(ctx); err != nil {
			return err
		}
	}
	marked, err := s.markDomainDNSCleanupVerified(ctx, ledger, op.ID)
	if err != nil {
		return err
	}
	if !marked {
		return contract.NewFault(contract.Conflict, "managed DNS cleanup ledger changed before verification was recorded", "A newer operation owns the binding; reconcile the current intent before deleting it.")
	}
	s.appendWorkerEvent(ctx, op.ID, attempt, "dns", "Verified Cloudflare cleanup for "+ledger.Domain+" ("+fmt.Sprint(plan.Applied)+" records removed, "+fmt.Sprint(plan.Unchanged)+" already absent); unrelated records were preserved.")
	return nil
}

func (s *Service) markCleanupFailure(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger, cause error) {
	// Failure bookkeeping is itself a ledger mutation. Recheck the worker's
	// DNS authority before writing it, so a revoked actor leaves the cleanup
	// state exactly as the authorized work left it. An action-specific denial
	// is also not recorded as a provider failure.
	var providerFailure *integrations.ProviderFailure
	isProviderFailure := errors.As(cause, &providerFailure)
	if contract.Classify(cause).Code == contract.Forbidden && !isProviderFailure {
		return
	}
	if err := s.authorizeDomainDNSWrite(context.WithoutCancel(ctx), op.Actor); err != nil {
		return
	}
	message := ""
	if cause != nil {
		message = s.redact(cause.Error())
		if providerFailure != nil && providerFailure.DefinitiveWrite && providerFailure.DefinitiveRecordDigest != "" {
			message = "definitive cleanup rejection: " + providerFailure.DefinitiveRecordDigest + " " + message
		}
	}
	_, _, _ = s.mutateOwned(context.WithoutCancel(ctx), op.ID, attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		return markDomainDNSCleanupFailed(context.WithoutCancel(ctx), tx, ledger.DomainID, op.ID, message)
	})
}

func (s *Service) recordDomainDNSFailure(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger, cause error) error {
	// An unauthorized worker must not turn the admitted intent into a failure
	// record, and a missing authorization configuration fails closed without
	// mutating the ledger. An action-specific denial is not a provider failure.
	var providerFailure *integrations.ProviderFailure
	isProviderFailure := errors.As(cause, &providerFailure)
	if contract.Classify(cause).Code == contract.Forbidden && !isProviderFailure {
		return nil
	}
	if err := s.authorizeDomainDNSWrite(context.WithoutCancel(ctx), op.Actor); err != nil {
		return err
	}
	message := ""
	if cause != nil {
		message = s.redact(cause.Error())
		if providerFailure != nil && providerFailure.DefinitiveWrite {
			message = "definitive write rejection: " + message
		}
	}
	_, owned, err := s.mutateOwned(context.WithoutCancel(ctx), op.ID, attempt, func(tx *entities.StoreTx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		return markDomainDNSFailed(context.WithoutCancel(ctx), tx, ledger, op.ID, message)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}
