package controlplane

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

type deletedEnvironmentResult struct {
	ID   platform.EnvironmentID `json:"id"`
	Name string                 `json:"name"`
}

// An optional evidence write must not veto the existing deletion transaction.
// Work on a copy so a rolled-back savepoint cannot advance the event cursor.
func (s *Service) tryDeletionOutputTx(ctx context.Context, tx pgx.Tx, op *contract.Operation, data json.RawMessage) {
	nested, err := tx.Begin(ctx)
	if err != nil {
		op.PendingResult = &contract.ActionResult{DataStatus: "unavailable"}
		return
	}
	copy := *op
	if err := s.appendActionOutputTx(ctx, nested, &copy, data); err != nil {
		_ = nested.Rollback(ctx)
		op.PendingResult = &contract.ActionResult{DataStatus: "unavailable"}
		return
	}
	if err := nested.Commit(ctx); err != nil {
		op.PendingResult = &contract.ActionResult{DataStatus: "unavailable"}
		return
	}
	*op = copy
}

func domainRemovalActionDataTx(ctx context.Context, tx pgx.Tx, app, environment, domain string) (json.RawMessage, error) {
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
	err := tx.QueryRow(ctx, `SELECT d.id,e.id,d.name,d.is_primary,
		EXISTS(SELECT 1 FROM eregion_platform_domain_dns x WHERE x.domain_id=d.id),
		coalesce((SELECT state FROM eregion_platform_domain_dns x WHERE x.domain_id=d.id),'not_required')
		FROM eregion_platform_domains d JOIN eregion_platform_environments e ON e.id=d.environment_id
		JOIN eregion_platform_apps a ON a.id=e.app_id
		WHERE a.name=$1 AND e.name=$2 AND d.name=$3 FOR UPDATE OF d`,
		app, environment, domain).Scan(&result.DomainID, &result.EnvironmentID, &result.Domain,
		&result.WasPrimary, &result.ManagedDNS, &result.ProviderCleanup)
	if err != nil {
		return nil, err
	}
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
