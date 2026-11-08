package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// previewDomainDNS reads the real provider state for a dry run. It never
// mutates Cloudflare and never needs the control database. If no durable ledger
// exists yet, a preview identity is used: no existing provider record can carry
// the future admission marker, so any record at the hostname is shown as a
// conflict rather than silently owned.
func (s *Service) previewDomainDNS(ctx context.Context, actor contract.Actor, input *platform.DomainInput, kind string) (integrations.DomainDNSPlan, error) {
	if input.DNSMode != integrations.DomainDNSModeCloudflare {
		return integrations.DomainDNSPlan{}, contract.NewFault(contract.ValidationFailed, "Cloudflare DNS is not selected", "Use --dns cloudflare with --dns-zone and --dns-origin, or omit DNS flags for manual DNS.")
	}
	if s.domainDNS == nil {
		return integrations.DomainDNSPlan{}, contract.NewFault(contract.InputRequired, "Cloudflare DNS is not configured on this control plane", "Set EREGION_CLOUDFLARE_ZONES and EREGION_CLOUDFLARE_TOKEN_FILE, then retry; manual DNS remains the default.")
	}
	if err := s.domainDNS.AuthorizeRead(ctx, actor); err != nil {
		return integrations.DomainDNSPlan{}, err
	}
	var ledger *domainDNSLedger
	var err error
	if s.pool != nil {
		ledger, err = s.loadDomainDNSLedger(ctx, input.App, input.Environment, input.Domain)
		if err != nil {
			return integrations.DomainDNSPlan{}, err
		}
	}
	var zone integrations.DNSZone
	var domainID platform.DomainID
	ownership := ""
	if ledger != nil {
		zone = integrations.DNSZone{ZoneID: ledger.ZoneID, ZoneName: ledger.ZoneName}
		domainID = ledger.DomainID
		ownership = ledger.Ownership
		if input.DNSZone != "" {
			requested, err := s.resolveDomainDNSZone(input.DNSZone)
			if err != nil {
				return integrations.DomainDNSPlan{}, err
			}
			if requested.ZoneID != ledger.ZoneID {
				return integrations.DomainDNSPlan{}, contract.NewFault(contract.Conflict, "managed DNS zone does not match the binding ledger", "Omit --dns-zone to use the persisted zone, or correct the flag.")
			}
		}
	} else {
		zone, err = s.resolveDomainDNSZone(input.DNSZone)
		if err != nil {
			return integrations.DomainDNSPlan{}, err
		}
		id, idErr := platform.NewDomainID()
		if idErr != nil {
			return integrations.DomainDNSPlan{}, contract.NewFault(contract.Internal, "could not allocate a preview domain identity", "Report the command and binary version.")
		}
		domainID = id
		ownership = integrations.DomainDNSOwnership(string(domainID), zone.ZoneID)
	}
	request := integrations.DomainDNSRequest{
		ZoneID: zone.ZoneID, ZoneName: zone.ZoneName, DomainID: string(domainID),
		Ownership: ownership, Host: input.Domain,
	}
	if kind != platform.KindDomainRemove {
		request.Addresses = append([]string(nil), input.DNSOrigins...)
		if input.DNSProxied != nil {
			request.Proxied = *input.DNSProxied
		}
	}
	var plan integrations.DomainDNSPlan
	if kind == platform.KindDomainRemove {
		plan, err = s.domainDNS.PlanRemove(ctx, request)
	} else {
		plan, err = s.domainDNS.PlanApply(ctx, request)
	}
	if err != nil {
		return plan, err
	}
	if ledger == nil {
		plan.Warnings = append(plan.Warnings, "No durable DNS ledger exists yet; admission will allocate the immutable binding identity. Existing records at this hostname are treated as foreign and never adopted.")
	}
	return plan, nil
}

// applyDomainDNSEffect performs the durable provider effect for one accepted
// domain operation using the persisted ledger, never operation-only intent.
func (s *Service) applyDomainDNSEffect(ctx context.Context, op contract.Operation, attempt uint32, input *platform.DomainInput, remove bool) error {
	ledger, err := s.loadDomainDNSLedger(ctx, input.App, input.Environment, input.Domain)
	if err != nil {
		return err
	}
	if remove {
		if ledger == nil {
			if input.DNSMode == integrations.DomainDNSModeCloudflare {
				exists, err := s.domainBindingExists(ctx, input.App, input.Environment, input.Domain)
				if err != nil {
					return err
				}
				if exists {
					return contract.NewFault(contract.PreconditionFailed, "managed DNS ledger is missing for this binding", "Do not delete a provider-managed binding without its durable cleanup ledger; inspect domain status and retry.")
				}
			}
			// Already cleaned, or a manual binding that never had managed DNS.
			return nil
		}
		return s.cleanupDomainDNSLedger(ctx, op, attempt, *ledger)
	}
	if input.DNSMode != integrations.DomainDNSModeCloudflare {
		return nil
	}
	if ledger == nil {
		return contract.NewFault(contract.PreconditionFailed, "managed DNS ledger is missing for this binding", "Do not apply provider changes outside a persisted binding intent; re-add the binding explicitly.")
	}
	return s.applyDomainDNSLedger(ctx, op, attempt, *ledger)
}

// deleteDomainBindingRowOwned removes the desired binding after provider
// cleanup. The ledger and binding row are deleted in one transaction under the
// operation row lock, so a retry either still has the ledger (and repeats
// verified cleanup) or has neither, and a cancellation accepted before this
// boundary prevents the delete.
func (s *Service) deleteDomainBindingRowOwned(ctx context.Context, id contract.OperationID, attempt uint32, app, environment, domain string) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State == contract.Cancelling {
			return errPlatformCancelled
		}
		return deleteDomainBindingRowTx(ctx, tx, app, environment, domain)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func deleteDomainBindingRowTx(ctx context.Context, tx pgx.Tx, app, environment, domain string) error {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if _, err := tx.Exec(ctx, `DELETE FROM eregion_platform_domain_dns x
        USING eregion_platform_domains d, eregion_platform_environments e, eregion_platform_apps a
        WHERE x.domain_id=d.id AND d.environment_id=e.id AND e.app_id=a.id
          AND d.name=$1 AND a.name=$2 AND e.name=$3`, domain, app, environment); err != nil {
		return unavailable()
	}
	if _, err := tx.Exec(ctx, `DELETE FROM eregion_platform_domains d
        USING eregion_platform_environments e, eregion_platform_apps a
        WHERE d.environment_id=e.id AND e.app_id=a.id
          AND d.name=$1 AND a.name=$2 AND e.name=$3`, domain, app, environment); err != nil {
		return unavailable()
	}
	return nil
}

func (s *Service) domainBindingExists(ctx context.Context, app, environment, domain string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
        SELECT 1 FROM eregion_platform_domains d
        JOIN eregion_platform_environments e ON e.id=d.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE d.name=$1 AND a.name=$2 AND e.name=$3)`,
		strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), ".")), app, environment).Scan(&exists)
	if err != nil {
		return false, unavailable()
	}
	return exists, nil
}

// cleanupDomainDNSForEnvironments verifies provider cleanup before domain rows
// can be deleted. The ledger rows are deleted atomically with the domain rows
// later, so any failure leaves the deletion retryable.
func (s *Service) cleanupDomainDNSForEnvironments(ctx context.Context, op contract.Operation, attempt uint32, environmentIDs []platform.EnvironmentID) error {
	ledgers, err := s.loadDomainDNSLedgersForEnvironments(ctx, environmentIDs)
	if err != nil {
		return err
	}
	// Provider effects are not atomic. Preflight every managed binding first so
	// an obvious conflict, authorization refusal, unsafe ledger or read failure
	// stops the deletion before the first record is changed.
	for _, ledger := range ledgers {
		if err := s.preflightDomainDNSLedger(ctx, op, attempt, ledger); err != nil {
			switch {
			case errors.Is(err, errPlatformCancelled):
				s.appendWorkerEvent(ctx, op.ID, attempt, "dns", "Cancellation acknowledged before any provider mutation in this pass; no workload was removed.")
			case errors.Is(err, errNotOwned), errors.Is(err, errOperationTerminal):
				// A newer attempt or a terminal operation owns the history.
			default:
				s.appendWorkerEvent(ctx, op.ID, attempt, "dns", "Managed DNS cleanup preflight refused before any provider mutation in this pass; no workload was removed. Verify the ledger and retry the deletion.")
			}
			return err
		}
	}
	processed := 0
	for _, ledger := range ledgers {
		if err := s.cleanupDomainDNSLedger(ctx, op, attempt, ledger); err != nil {
			// Do not claim records were preserved: some earlier bindings may
			// already be cleaned, and this binding may have partially changed.
			s.appendWorkerEvent(ctx, op.ID, attempt, "dns", fmt.Sprintf("Managed DNS cleanup stopped after %d of %d binding(s). Provider effects are not atomic; already-cleaned bindings stay cleaned and the remaining bindings may be unchanged or partially changed. The ledger and workload rows are retained; retry the deletion.", processed, len(ledgers)))
			return err
		}
		processed++
	}
	s.appendWorkerEvent(ctx, op.ID, attempt, "dns", fmt.Sprintf("Verified cleanup for %d managed DNS binding(s) before deletion.", len(ledgers)))
	return nil
}

// domainOperationInput returns the canonical, non-secret domain input for an
// operation. Recovery uses it to converge the same provider effect and row
// deletion without trusting a message or inventing progress.
func (s *Service) domainOperationInput(ctx context.Context, id contract.OperationID) (contract.Operation, *platform.DomainInput, platform.Normalized, error) {
	op, raw, err := s.operationSnapshot(ctx, id)
	if err != nil {
		return op, nil, platform.Normalized{}, err
	}
	normalized, err := platform.Default().Normalize(op.Kind, op.Target, raw)
	if err != nil {
		return op, nil, platform.Normalized{}, err
	}
	input, ok := normalized.Input.(*platform.DomainInput)
	if !ok {
		return op, nil, platform.Normalized{}, contract.NewFault(contract.Internal, "domain operation input is unavailable", "Do not assume the operation applied; inspect binary compatibility.")
	}
	return op, input, normalized, nil
}
