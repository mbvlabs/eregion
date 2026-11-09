package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
)

const (
	domainDNSStatePending         = "pending"
	domainDNSStateApplied         = "applied"
	domainDNSStateFailed          = "failed"
	domainDNSStateCleanupPending  = "cleanup_pending"
	domainDNSStateCleanupVerified = "cleanup_verified"
	domainDNSStateCleanupFailed   = "cleanup_failed"
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

const domainDNSLedgerSelect = `SELECT x.domain_id,d.name,a.name,e.name,x.provider,x.zone_id,x.zone_name,
    x.ownership_marker,x.generation,x.desired_origins,x.desired_proxied,x.state,x.last_operation_id,
    x.last_error,x.applied_generation,x.cleanup_error
    FROM eregion_platform_domain_dns x
    JOIN eregion_platform_domains d ON d.id=x.domain_id
    JOIN eregion_platform_environments e ON e.id=d.environment_id
    JOIN eregion_platform_apps a ON a.id=e.app_id`

func scanDomainDNSLedger(row pgx.Row) (domainDNSLedger, error) {
	var ledger domainDNSLedger
	err := row.Scan(&ledger.DomainID, &ledger.Domain, &ledger.App, &ledger.Environment, &ledger.Provider,
		&ledger.ZoneID, &ledger.ZoneName, &ledger.Ownership, &ledger.Generation, &ledger.DesiredOrigins,
		&ledger.DesiredProxied, &ledger.State, &ledger.LastOperationID, &ledger.LastError,
		&ledger.AppliedGeneration, &ledger.CleanupError)
	return ledger, err
}

func (s *Service) loadDomainDNSLedger(ctx context.Context, app, environment, domain string) (*domainDNSLedger, error) {
	ledger, err := scanDomainDNSLedger(s.pool.QueryRow(ctx, domainDNSLedgerSelect+`
        WHERE a.name=$1 AND e.name=$2 AND d.name=$3`, app, environment, domain))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailable()
	}
	return &ledger, nil
}

func loadDomainDNSLedgerByDomainIDTx(ctx context.Context, tx pgx.Tx, domainID platform.DomainID) (*domainDNSLedger, error) {
	ledger, err := scanDomainDNSLedger(tx.QueryRow(ctx, domainDNSLedgerSelect+`
        WHERE x.domain_id=$1`, domainID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailable()
	}
	return &ledger, nil
}

func (s *Service) loadDomainDNSLedgersForEnvironments(ctx context.Context, environmentIDs []platform.EnvironmentID) ([]domainDNSLedger, error) {
	if len(environmentIDs) == 0 {
		return nil, nil
	}
	ids := make([]string, len(environmentIDs))
	for index, id := range environmentIDs {
		ids[index] = string(id)
	}
	rows, err := s.pool.Query(ctx, domainDNSLedgerSelect+`
        WHERE e.id = ANY($1)
        ORDER BY a.name,e.name,d.name`, ids)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	var ledgers []domainDNSLedger
	for rows.Next() {
		ledger, err := scanDomainDNSLedger(rows)
		if err != nil {
			return nil, unavailable()
		}
		ledgers = append(ledgers, ledger)
	}
	return ledgers, rows.Err()
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
		return contract.NewFault(contract.InputRequired, "Cloudflare DNS is not configured on this control plane", "Configure a provider credential with eregion domain cloudflare configure; retry.")
	}
	return s.domainDNS.AuthorizeWrite(ctx, actor)
}

func (s *Service) resolveDomainDNSZone(ctx context.Context, ref string) (integrations.DNSZone, error) {
	if s.domainDNS == nil {
		return integrations.DNSZone{}, contract.NewFault(contract.InputRequired, "Cloudflare DNS is not configured on this control plane", "Configure a provider credential with eregion domain cloudflare configure; manual DNS remains the default.")
	}
	zone, err := s.domainDNS.ResolveZone(ctx, ref)
	if err != nil {
		return integrations.DNSZone{}, err
	}
	return zone, nil
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

func (s *Service) ensureDomainDNSCleanupSafeTx(ctx context.Context, tx pgx.Tx, actor contract.Actor, app, environment string) error {
	rows, err := tx.Query(ctx, `SELECT d.name,x.zone_id,x.zone_name,x.state,x.applied_generation
        FROM eregion_platform_domain_dns x
        JOIN eregion_platform_domains d ON d.id=x.domain_id
        JOIN eregion_platform_environments e ON e.id=d.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name=$1 AND ($2='' OR e.name=$2)`, app, environment)
	if err != nil {
		return unavailable()
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		found = true
		var ledger domainDNSLedger
		if err := rows.Scan(&ledger.Domain, &ledger.ZoneID, &ledger.ZoneName, &ledger.State, &ledger.AppliedGeneration); err != nil {
			return unavailable()
		}
		if err := s.validateDomainDNSCleanupRecovery(ledger); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return unavailable()
	}
	if found {
		return s.authorizeDomainDNSWrite(ctx, actor)
	}
	return nil
}

func upsertDomainDNSIntentTx(ctx context.Context, tx pgx.Tx, domainID platform.DomainID, zone integrations.DNSZone, origins []string, proxied bool, opID contract.OperationID) error {
	marker := integrations.DomainDNSOwnership(string(domainID), zone.ZoneID)
	var existingZone string
	err := tx.QueryRow(ctx, `SELECT zone_id FROM eregion_platform_domain_dns WHERE domain_id=$1 FOR UPDATE`, domainID).Scan(&existingZone)
	switch {
	case err == nil && existingZone != zone.ZoneID:
		return contract.NewFault(contract.Conflict,
			"managed DNS zone cannot change on an existing binding",
			"Remove the binding and its managed records first, then add it with the new zone.")
	case err == nil:
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domain_dns
            SET generation=generation+1,desired_origins=$2,desired_proxied=$3,
                state='pending',last_operation_id=$4,last_error='',
                cleanup_requested_at=NULL,cleanup_verified_at=NULL,cleanup_error='',updated_at=clock_timestamp()
            WHERE domain_id=$1`, domainID, origins, proxied, opID); err != nil {
			return unavailable()
		}
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_domain_dns
            (domain_id,provider,zone_id,zone_name,ownership_marker,generation,desired_origins,desired_proxied,state,last_operation_id)
            VALUES($1,'cloudflare',$2,$3,$4,1,$5,$6,'pending',$7)`,
			domainID, zone.ZoneID, zone.ZoneName, marker, origins, proxied, opID); err != nil {
			return unavailable()
		}
	default:
		return unavailable()
	}
	return nil
}

// fenceDomainDNSOperationTx makes a non-provider domain add (for example a
// primary promotion or check-path update) the newest operation for a managed
// binding without changing its desired provider state.
func fenceDomainDNSOperationTx(ctx context.Context, tx pgx.Tx, domainID platform.DomainID, opID contract.OperationID) error {
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domain_dns
        SET last_operation_id=$2,updated_at=clock_timestamp() WHERE domain_id=$1`, domainID, opID); err != nil {
		return unavailable()
	}
	return nil
}

func insertDomainDNSCleanupIntentTx(ctx context.Context, tx pgx.Tx, domainID platform.DomainID, zone integrations.DNSZone, opID contract.OperationID) error {
	marker := integrations.DomainDNSOwnership(string(domainID), zone.ZoneID)
	if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_domain_dns
        (domain_id,provider,zone_id,zone_name,ownership_marker,generation,desired_origins,desired_proxied,
         state,last_operation_id,cleanup_requested_at)
        VALUES($1,'cloudflare',$2,$3,$4,1,'{}',false,'cleanup_pending',$5,clock_timestamp())`,
		domainID, zone.ZoneID, zone.ZoneName, marker, opID); err != nil {
		return unavailable()
	}
	return nil
}

func markDomainDNSCleanupTx(ctx context.Context, tx pgx.Tx, domainID platform.DomainID, opID contract.OperationID) error {
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domain_dns
        SET state='cleanup_pending',last_operation_id=$2,cleanup_requested_at=clock_timestamp(),cleanup_verified_at=NULL,
            last_error='',cleanup_error='',updated_at=clock_timestamp()
        WHERE domain_id=$1`, domainID, opID); err != nil {
		return unavailable()
	}
	return nil
}

func markDomainDNSCleanupForEnvironmentsTx(ctx context.Context, tx pgx.Tx, app, environment string, opID contract.OperationID) error {
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domain_dns x
        SET state='cleanup_pending',last_operation_id=$3,cleanup_requested_at=clock_timestamp(),cleanup_verified_at=NULL,
            last_error='',cleanup_error='',updated_at=clock_timestamp()
        FROM eregion_platform_domains d
        JOIN eregion_platform_environments e ON e.id=d.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE d.id=x.domain_id AND a.name=$1 AND ($2='' OR e.name=$2)`, app, environment, opID); err != nil {
		return unavailable()
	}
	return nil
}

func (s *Service) markDomainDNSCleanupVerified(ctx context.Context, ledger domainDNSLedger, opID contract.OperationID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE eregion_platform_domain_dns
        SET state='cleanup_verified',last_operation_id=$2,cleanup_verified_at=clock_timestamp(),
            cleanup_error='',updated_at=clock_timestamp()
        WHERE domain_id=$1 AND last_operation_id=$2`, ledger.DomainID, opID)
	if err != nil {
		return false, unavailable()
	}
	return tag.RowsAffected() == 1, nil
}

func markDomainDNSCleanupFailed(ctx context.Context, tx pgx.Tx, domainID platform.DomainID, opID contract.OperationID, message string) error {
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domain_dns
        SET state='cleanup_failed',last_operation_id=$2,cleanup_error=$3,updated_at=clock_timestamp()
        WHERE domain_id=$1 AND last_operation_id=$2`, domainID, opID, boundedRouteError(message)); err != nil {
		return unavailable()
	}
	return nil
}

func markDomainDNSApplied(ctx context.Context, tx pgx.Tx, ledger domainDNSLedger, opID contract.OperationID) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE eregion_platform_domain_dns
        SET state='applied',applied_generation=generation,applied_at=clock_timestamp(),
            last_error='',updated_at=clock_timestamp()
        WHERE domain_id=$1 AND last_operation_id=$2`, ledger.DomainID, opID)
	if err != nil {
		return false, unavailable()
	}
	return tag.RowsAffected() == 1, nil
}

func markDomainDNSFailed(ctx context.Context, tx pgx.Tx, ledger domainDNSLedger, opID contract.OperationID, message string) error {
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_domain_dns
        SET state='failed',last_error=$2,updated_at=clock_timestamp()
        WHERE domain_id=$1 AND last_operation_id=$3`, ledger.DomainID, boundedRouteError(message), opID); err != nil {
		return unavailable()
	}
	return nil
}

func (s *Service) finishDomainOperation(ctx context.Context, id contract.OperationID, attempt uint32, finalize func(pgx.Tx, *contract.Operation) error) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
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
		return contract.NewFault(contract.InputRequired, "Cloudflare DNS is not configured on this control plane", "Configure the missing provider credential with eregion domain cloudflare configure; the durable binding is retained.")
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
	plan, err := s.domainDNS.ApplyWithGuard(ctx, op.Actor, op.ID, request, guard)
	if err != nil {
		if !dnsBoundaryStop(err) {
			s.recordDomainDNSFailure(ctx, op, attempt, ledger, err)
		}
		return err
	}
	if err := s.checkDomainDNSBoundary(ctx, op.ID, attempt); err != nil {
		return err
	}
	var marked bool
	_, owned, err := s.mutateOwned(ctx, op.ID, attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
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
	s.appendWorkerEvent(ctx, op.ID, attempt, "dns", "Managed DNS applied generation "+fmt.Sprint(ledger.Generation)+" for "+ledger.Domain+" ("+fmt.Sprint(plan.Applied)+" changed, "+fmt.Sprint(plan.Unchanged)+" unchanged); provider state is separate from public readiness.")
	return nil
}

// preflightDomainDNSLedger performs every read-only check and ownership plan
// for one managed binding before the deletion worker removes any serving
// workload or mutates any provider record. A conflict, authorization failure or
// unsafe ledger therefore stops the whole deletion before its first DNS write.
func (s *Service) preflightDomainDNSLedger(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger) error {
	if s.domainDNS == nil {
		return contract.NewFault(contract.InputRequired, "Cloudflare DNS is not configured on this control plane", "Configure the missing provider credential with eregion domain cloudflare configure; no workload was removed.")
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
		return contract.NewFault(contract.InputRequired, "Cloudflare DNS is not configured on this control plane", "Configure the missing provider credential with eregion domain cloudflare configure; the binding and cleanup ledger are retained.")
	}
	if ledger.LastOperationID != op.ID {
		return contract.NewFault(contract.Conflict, "a newer operation owns this managed binding cleanup", "This deletion is stale; retry the current deletion after the newer operation finishes.")
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
		// A pending/failed intent that never applied cannot own a provider
		// record in this zone. Record verification without touching Cloudflare.
		if marked, markErr := s.markDomainDNSCleanupVerified(ctx, ledger, op.ID); markErr != nil {
			return markErr
		} else if !marked {
			return contract.NewFault(contract.Conflict, "managed DNS cleanup ledger changed before recovery was recorded", "A newer operation owns the binding; retry the current intent.")
		}
		s.appendWorkerEvent(ctx, op.ID, attempt, "dns", "Discarded invalid pending managed-DNS intent for "+ledger.Domain+" without provider access; the zone does not contain the hostname.")
		return nil
	}
	request := integrations.DomainDNSRequest{
		ZoneID: ledger.ZoneID, ZoneName: ledger.ZoneName, DomainID: string(ledger.DomainID),
		Ownership: ledger.Ownership, Host: ledger.Domain,
	}
	guard := func(mutationCtx context.Context) error {
		return s.checkDomainDNSBoundary(mutationCtx, op.ID, attempt)
	}
	plan, err := s.domainDNS.RemoveWithGuard(ctx, op.Actor, op.ID, request, guard)
	if err != nil {
		if !dnsBoundaryStop(err) {
			s.markCleanupFailure(ctx, op, attempt, ledger, err)
		}
		return err
	}
	if err := s.checkDomainDNSBoundary(ctx, op.ID, attempt); err != nil {
		return err
	}
	if marked, err := s.markDomainDNSCleanupVerified(ctx, ledger, op.ID); err != nil {
		return err
	} else if !marked {
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
	if contract.Classify(cause).Code == contract.Forbidden {
		return
	}
	if err := s.authorizeDomainDNSWrite(context.WithoutCancel(ctx), op.Actor); err != nil {
		return
	}
	message := ""
	if cause != nil {
		message = s.redact(cause.Error())
	}
	_, _, _ = s.mutateOwned(context.WithoutCancel(ctx), op.ID, attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		return markDomainDNSCleanupFailed(context.WithoutCancel(ctx), tx, ledger.DomainID, op.ID, message)
	})
}

func (s *Service) recordDomainDNSFailure(ctx context.Context, op contract.Operation, attempt uint32, ledger domainDNSLedger, cause error) {
	// An unauthorized worker must not turn the admitted intent into a failure
	// record, and a missing authorization configuration fails closed without
	// mutating the ledger. An action-specific denial is not a provider failure.
	if contract.Classify(cause).Code == contract.Forbidden {
		return
	}
	if err := s.authorizeDomainDNSWrite(context.WithoutCancel(ctx), op.Actor); err != nil {
		return
	}
	message := ""
	if cause != nil {
		message = s.redact(cause.Error())
	}
	_, _, _ = s.mutateOwned(context.WithoutCancel(ctx), op.ID, attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		return markDomainDNSFailed(context.WithoutCancel(ctx), tx, ledger, op.ID, message)
	})
}
