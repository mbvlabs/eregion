package controlplane

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

type deletedEnvironmentResult struct {
	ID   platform.EnvironmentID `json:"id"`
	Name string                 `json:"name"`
}

// An optional evidence write must not veto the existing deletion transaction.
// Work on a copy so a rolled-back savepoint cannot advance the event cursor.
func (s *Service) tryDeletionOutputTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, data json.RawMessage) {
	if tx == nil {
		op.PendingResult = &contract.ActionResult{DataStatus: "unavailable"}
		return
	}
	copy := *op
	if err := tx.WithinSavepoint(ctx, func(nested *entities.StoreTx) error {
		return s.appendActionOutputTx(ctx, nested, &copy, data)
	}); err != nil {
		op.PendingResult = &contract.ActionResult{DataStatus: "unavailable"}
		return
	}
	*op = copy
}

func domainRemovalActionDataTx(ctx context.Context, tx *entities.StoreTx, app, environment, domain string) (json.RawMessage, error) {
	var result struct {
		DomainID              platform.DomainID      `json:"domain_id"`
		EnvironmentID         platform.EnvironmentID `json:"environment_id"`
		App                   string                 `json:"app"`
		Environment           string                 `json:"environment"`
		Domain                string                 `json:"domain"`
		WasPrimary            bool                   `json:"was_primary"`
		ManagedDNS            bool                   `json:"managed_dns"`
		ProviderCleanup       string                 `json:"provider_cleanup"`
		DesiredBindingRemoved bool                   `json:"desired_binding_removed"`
		RouteSync             string                 `json:"route_sync"`
		ObservedAt            time.Time              `json:"observed_at"`
	}
	facts, found, err := tx.Platform().Domains().RemovalData(ctx, app, environment, domain)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, contract.NewFault(contract.NotFound, "domain binding was not found", "Reload the binding before recording removal evidence.")
	}
	result.DomainID, result.EnvironmentID, result.Domain = facts.DomainID, facts.EnvironmentID, facts.Domain
	result.WasPrimary, result.ManagedDNS, result.ProviderCleanup = facts.Primary, facts.ManagedDNS, facts.DNSState
	result.App, result.Environment = app, environment
	result.DesiredBindingRemoved, result.RouteSync, result.ObservedAt = true, "not_observed", time.Now().UTC()
	return json.Marshal(result)
}

// deletionActionData uses identities retained by the deletion worker, not joins
// against rows that have already been removed. Call only in its final transaction
// after workload/route cleanup and the guarded metadata deletion have succeeded.
func deletionActionData(app platform.App, environments []platform.Environment, appDeleted bool) (json.RawMessage, error) {
	result := struct {
		AppID            platform.AppID             `json:"app_id"`
		App              string                     `json:"app"`
		AppDeleted       bool                       `json:"app_deleted"`
		Environments     []deletedEnvironmentResult `json:"deleted_environments"`
		WorkloadCleanup  string                     `json:"workload_cleanup"`
		RouteCleanup     string                     `json:"route_cleanup"`
		OperationHistory string                     `json:"operation_history"`
		ResourceVolumes  string                     `json:"resource_volumes"`
	}{
		AppID: app.ID, App: app.Name, AppDeleted: appDeleted,
		Environments:    make([]deletedEnvironmentResult, 0, len(environments)),
		WorkloadCleanup: "completed", RouteCleanup: "completed",
		OperationHistory: "retained", ResourceVolumes: "unchanged",
	}
	for _, environment := range environments {
		result.Environments = append(result.Environments, deletedEnvironmentResult{
			ID: environment.ID, Name: environment.Name,
		})
	}
	return json.Marshal(result)
}
