package entities

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
	"github.com/mbvlabs/eregion/internal/platform"
)

// DomainConflictGuard carries the exact kind/target identity used by domain
// admission. Policy remains with the caller.
type DomainConflictGuard struct {
	ExcludeID             contract.OperationID
	EnvironmentTarget     string
	AppTarget             string
	DomainAddKind         string
	DomainRemoveKind      string
	EnvironmentDeleteKind string
	AppDeleteKind         string
}

// DeleteTargetGuard is the app-wide deletion guard shape.
type DeleteTargetGuard struct {
	ExcludeID          contract.OperationID
	AppTarget          string
	EnvironmentPattern string
}

// EnvironmentDeleteTargetGuard adds the exact environment target and the
// sibling environment-delete serialization rule.
type EnvironmentDeleteTargetGuard struct {
	ExcludeID             contract.OperationID
	AppTarget             string
	EnvironmentTarget     string
	EnvironmentDeleteKind string
	EnvironmentPattern    string
}

// ParentDeleteGuard covers resource changes racing app/environment deletion.
type ParentDeleteGuard struct {
	ExcludeID             contract.OperationID
	AppTarget             string
	EnvironmentTarget     string
	AppDeleteKind         string
	EnvironmentDeleteKind string
}

// ActiveAppEnvironmentOperation is one queued/running/cancelling operation for
// a target pair.
type ActiveAppEnvironmentOperation struct {
	Kind  string
	State contract.State
}

// DefinitionActiveOperation is one active operation whose target belongs to
// the definition's app, environment, resource or build ownership set.
type DefinitionActiveOperation struct {
	ID     contract.OperationID
	Kind   string
	Target string
	State  contract.State
}

// ActiveDeploymentOperation is one active deployment operation and its
// deployment id.
type ActiveDeploymentOperation struct {
	ID           contract.OperationID
	Attempt      uint32
	State        contract.State
	DeploymentID platform.DeploymentID
}

// ReleaseCommandLive is the release-command run plus whether its operation is
// still live.
type ReleaseCommandLive struct {
	State       string
	OperationID contract.OperationID
	Live        bool
}

func activeDomainConflicts(ctx context.Context, q *query.Queries, guard DomainConflictGuard) (int64, error) {
	return q.CountActiveDomainConflicts(ctx, query.CountActiveDomainConflictsParams{
		ExcludeID: string(guard.ExcludeID), DomainAddKind: guard.DomainAddKind,
		DomainRemoveKind: guard.DomainRemoveKind, EnvironmentTarget: guard.EnvironmentTarget,
		EnvironmentDeleteKind: guard.EnvironmentDeleteKind, AppDeleteKind: guard.AppDeleteKind,
		AppTarget: guard.AppTarget,
	})
}

func (o *OperationStore) CountActiveDomainConflicts(ctx context.Context, guard DomainConflictGuard) (int64, error) {
	return activeDomainConflicts(ctx, o.q, guard)
}

func (o *OperationTx) CountActiveDomainConflicts(ctx context.Context, guard DomainConflictGuard) (int64, error) {
	return activeDomainConflicts(ctx, o.owner.q, guard)
}

func latestDomainOperation(ctx context.Context, q *query.Queries, target, addKind, removeKind string) (contract.OperationID, bool, error) {
	id, err := q.LatestDomainOperation(ctx, query.LatestDomainOperationParams{
		Target: target, DomainAddKind: addKind, DomainRemoveKind: removeKind,
	})
	if err != nil {
		return "", false, err
	}
	return contract.OperationID(id), id != "", nil
}

func (o *OperationStore) LatestDomainOperation(ctx context.Context, target, addKind, removeKind string) (contract.OperationID, bool, error) {
	return latestDomainOperation(ctx, o.q, target, addKind, removeKind)
}

func (o *OperationTx) LatestDomainOperation(ctx context.Context, target, addKind, removeKind string) (contract.OperationID, bool, error) {
	return latestDomainOperation(ctx, o.owner.q, target, addKind, removeKind)
}

func countActiveDeleteTargetsForApp(ctx context.Context, q *query.Queries, guard DeleteTargetGuard) (int64, error) {
	return q.CountActiveDeleteTargetsForApp(ctx, query.CountActiveDeleteTargetsForAppParams{
		ExcludeID: string(guard.ExcludeID), AppTarget: guard.AppTarget,
		EnvironmentPattern: guard.EnvironmentPattern,
	})
}

func (o *OperationStore) CountActiveDeleteTargetsForApp(ctx context.Context, guard DeleteTargetGuard) (int64, error) {
	return countActiveDeleteTargetsForApp(ctx, o.q, guard)
}

func (o *OperationTx) CountActiveDeleteTargetsForApp(ctx context.Context, guard DeleteTargetGuard) (int64, error) {
	return countActiveDeleteTargetsForApp(ctx, o.owner.q, guard)
}

func countActiveDeleteTargetsForEnvironment(ctx context.Context, q *query.Queries, guard EnvironmentDeleteTargetGuard) (int64, error) {
	return q.CountActiveDeleteTargetsForEnvironment(ctx, query.CountActiveDeleteTargetsForEnvironmentParams{
		ExcludeID: string(guard.ExcludeID), AppTarget: guard.AppTarget,
		EnvironmentTarget: guard.EnvironmentTarget, EnvironmentDeleteKind: guard.EnvironmentDeleteKind,
		EnvironmentPattern: guard.EnvironmentPattern,
	})
}

func (o *OperationStore) CountActiveDeleteTargetsForEnvironment(ctx context.Context, guard EnvironmentDeleteTargetGuard) (int64, error) {
	return countActiveDeleteTargetsForEnvironment(ctx, o.q, guard)
}

func (o *OperationTx) CountActiveDeleteTargetsForEnvironment(ctx context.Context, guard EnvironmentDeleteTargetGuard) (int64, error) {
	return countActiveDeleteTargetsForEnvironment(ctx, o.owner.q, guard)
}

func countActiveParentDeletes(ctx context.Context, q *query.Queries, guard ParentDeleteGuard) (int64, error) {
	return q.CountActiveParentDeletes(ctx, query.CountActiveParentDeletesParams{
		ExcludeID: string(guard.ExcludeID), AppTarget: guard.AppTarget,
		EnvironmentTarget: guard.EnvironmentTarget, AppDeleteKind: guard.AppDeleteKind,
		EnvironmentDeleteKind: guard.EnvironmentDeleteKind,
	})
}

func (o *OperationStore) CountActiveParentDeletes(ctx context.Context, guard ParentDeleteGuard) (int64, error) {
	return countActiveParentDeletes(ctx, o.q, guard)
}

func (o *OperationTx) CountActiveParentDeletes(ctx context.Context, guard ParentDeleteGuard) (int64, error) {
	return countActiveParentDeletes(ctx, o.owner.q, guard)
}

func countActiveResourceOperations(ctx context.Context, q *query.Queries, exclude contract.OperationID, target string) (int64, error) {
	return q.CountActiveResourceOperations(ctx, query.CountActiveResourceOperationsParams{
		ExcludeID: string(exclude), Target: target,
	})
}

func (o *OperationStore) CountActiveResourceOperations(ctx context.Context, exclude contract.OperationID, target string) (int64, error) {
	return countActiveResourceOperations(ctx, o.q, exclude, target)
}

func (o *OperationTx) CountActiveResourceOperations(ctx context.Context, exclude contract.OperationID, target string) (int64, error) {
	return countActiveResourceOperations(ctx, o.owner.q, exclude, target)
}

func listActiveAppEnvironmentOperations(ctx context.Context, q *query.Queries, exclude contract.OperationID, environmentTarget, appTarget string) ([]ActiveAppEnvironmentOperation, error) {
	rows, err := q.ListActiveAppEnvironmentOperations(ctx, query.ListActiveAppEnvironmentOperationsParams{
		ExcludeID: string(exclude), EnvironmentTarget: environmentTarget, AppTarget: appTarget,
	})
	if err != nil {
		return nil, err
	}
	result := make([]ActiveAppEnvironmentOperation, 0, len(rows))
	for _, row := range rows {
		result = append(result, ActiveAppEnvironmentOperation{Kind: row.Kind, State: contract.State(row.State)})
	}
	return result, nil
}

func (o *OperationStore) ListActiveAppEnvironmentOperations(ctx context.Context, exclude contract.OperationID, environmentTarget, appTarget string) ([]ActiveAppEnvironmentOperation, error) {
	return listActiveAppEnvironmentOperations(ctx, o.q, exclude, environmentTarget, appTarget)
}

func (o *OperationTx) ListActiveAppEnvironmentOperations(ctx context.Context, exclude contract.OperationID, environmentTarget, appTarget string) ([]ActiveAppEnvironmentOperation, error) {
	return listActiveAppEnvironmentOperations(ctx, o.owner.q, exclude, environmentTarget, appTarget)
}

func definitionActiveOperations(ctx context.Context, q *query.Queries, appID platform.AppID, appTarget, envPrefix, resourcePrefix string) ([]DefinitionActiveOperation, error) {
	rows, err := q.ListPlatformDefinitionActiveOperations(ctx, query.ListPlatformDefinitionActiveOperationsParams{
		AppTarget: appTarget, EnvPrefix: envPrefix, ResourcePrefix: resourcePrefix, AppID: string(appID),
	})
	if err != nil {
		return nil, err
	}
	result := make([]DefinitionActiveOperation, 0, len(rows))
	for _, row := range rows {
		result = append(result, DefinitionActiveOperation{
			ID: contract.OperationID(row.ID), Kind: row.Kind, Target: row.Target, State: contract.State(row.State),
		})
	}
	return result, nil
}

// DefinitionActiveOperations returns every active operation that can change
// this app's definition-relevant targets, including app-owned builds.
func (o *OperationStore) DefinitionActiveOperations(ctx context.Context, appID platform.AppID, appTarget, envPrefix, resourcePrefix string) ([]DefinitionActiveOperation, error) {
	return definitionActiveOperations(ctx, o.q, appID, appTarget, envPrefix, resourcePrefix)
}

// DefinitionActiveOperations reads the exact caller transaction.
func (o *OperationTx) DefinitionActiveOperations(ctx context.Context, appID platform.AppID, appTarget, envPrefix, resourcePrefix string) ([]DefinitionActiveOperation, error) {
	return definitionActiveOperations(ctx, o.owner.q, appID, appTarget, envPrefix, resourcePrefix)
}

// ListActiveDeploymentsForEnvironment returns the operations used by legacy
// ambiguous-owner settlement, ordered by operation id.
func (o *OperationStore) ListActiveDeploymentsForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) ([]ActiveDeploymentOperation, error) {
	rows, err := o.q.ListActiveDeploymentsForEnvironment(ctx, string(environmentID))
	if err != nil {
		return nil, err
	}
	result := make([]ActiveDeploymentOperation, 0, len(rows))
	for _, row := range rows {
		attempt, err := platformUint32(row.Attempt)
		if err != nil {
			return nil, err
		}
		result = append(result, ActiveDeploymentOperation{
			ID: contract.OperationID(row.ID), Attempt: attempt,
			State: contract.State(row.State), DeploymentID: platform.DeploymentID(row.ID_2),
		})
	}
	return result, nil
}

// CountActiveDeploymentsForEnvironment is the settlement recheck count.
func (o *OperationStore) CountActiveDeploymentsForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) (int64, error) {
	return o.q.CountActiveDeploymentsForEnvironment(ctx, string(environmentID))
}

// GetWithLive reads the release-command run and its operation liveness in one
// statement.
func (s *ReleaseCommandStore) GetWithLive(ctx context.Context, releaseID platform.ReleaseID, digest string) (ReleaseCommandLive, bool, error) {
	row, err := s.q.GetPlatformReleaseCommandRunLive(ctx, query.GetPlatformReleaseCommandRunLiveParams{
		ReleaseID: string(releaseID), CommandDigest: digest,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ReleaseCommandLive{}, false, nil
	}
	if err != nil {
		return ReleaseCommandLive{}, false, err
	}
	return ReleaseCommandLive{
		State: row.State, OperationID: contract.OperationID(row.OperationID), Live: row.Exists,
	}, true, nil
}

// ActiveDeploymentRelease returns the active deployment release used by
// promotion admission. A missing row is (zero,false,nil).
func (o *OperationStore) ActiveDeploymentRelease(ctx context.Context, environmentID platform.EnvironmentID) (platform.ReleaseID, bool, error) {
	releaseID, err := o.q.GetActiveDeploymentReleaseForEnvironment(ctx, string(environmentID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return platform.ReleaseID(releaseID), releaseID != "", nil
}
