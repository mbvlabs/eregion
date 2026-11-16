package entities

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
	"github.com/mbvlabs/eregion/internal/platform"
)

type ChildPlacement struct {
	Kind          string
	ID            string
	AppID         platform.AppID
	EnvironmentID platform.EnvironmentID
}

type BuildOwnership struct {
	AppID         platform.AppID
	EnvironmentID platform.EnvironmentID
	ReleaseID     platform.ReleaseID
}

// BuildReleasePointer is the exact persisted build-to-release reciprocal. The
// caller compares ReleaseID with its expected release; entities does not decide
// whether a mismatch is permitted.
type BuildReleasePointer struct {
	AppID         platform.AppID
	EnvironmentID platform.EnvironmentID
	ReleaseID     platform.ReleaseID
}

type EnvironmentPointers struct {
	AppID             platform.AppID
	CurrentRevision   platform.RevisionID
	DesiredRelease    platform.ReleaseID
	AppliedRelease    platform.ReleaseID
	ServingRelease    platform.ReleaseID
	ServingDeployment platform.DeploymentID
}

type ReleasePointer struct {
	AppID         platform.AppID
	EnvironmentID platform.EnvironmentID
	Pruned        bool
	BuildID       platform.BuildID
}

type PointerStore struct{ q *query.Queries }

func (s *PointerStore) OperationChildren(ctx context.Context, operationID contract.OperationID) ([]ChildPlacement, error) {
	rows, err := s.q.ListPlatformOperationChildPlacements(ctx, string(operationID))
	if err != nil {
		return nil, err
	}
	result := make([]ChildPlacement, 0, len(rows))
	for _, row := range rows {
		result = append(result, ChildPlacement{Kind: row.Kind, ID: row.ID, AppID: platform.AppID(row.AppID), EnvironmentID: platform.EnvironmentID(row.EnvironmentID)})
	}
	return result, nil
}

func (s *PointerStore) DeploymentMatchesOperation(ctx context.Context, operationID contract.OperationID, deploymentID platform.DeploymentID, environmentID platform.EnvironmentID, releaseID platform.ReleaseID) (bool, error) {
	return s.q.PlatformDeploymentMatchesOperation(ctx, query.PlatformDeploymentMatchesOperationParams{
		OperationID: string(operationID), DeploymentID: string(deploymentID),
		EnvironmentID: string(environmentID), ReleaseID: string(releaseID),
	})
}

func (s *PointerStore) BuildOwnership(ctx context.Context, id platform.BuildID) (BuildOwnership, bool, error) {
	row, err := s.q.GetPlatformBuildOwnership(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return BuildOwnership{}, false, nil
	}
	return BuildOwnership{AppID: platform.AppID(row.AppID), EnvironmentID: platform.EnvironmentID(row.EnvironmentID), ReleaseID: platform.ReleaseID(row.ReleaseID)}, err == nil, err
}

func (s *PointerStore) ReleaseBuildID(ctx context.Context, id platform.ReleaseID) (platform.BuildID, bool, error) {
	build, err := s.q.GetPlatformReleaseBuildID(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if !build.Valid {
		return "", true, nil
	}
	return platform.BuildID(build.String), true, nil
}

func (s *PointerStore) LockAppKeyShare(ctx context.Context, appID platform.AppID) (bool, error) {
	id, err := s.q.LockPlatformAppKeyShare(ctx, string(appID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return id == string(appID), err
}

func (s *PointerStore) LockEnvironmentPointers(ctx context.Context, environmentID platform.EnvironmentID) (EnvironmentPointers, bool, error) {
	row, err := s.q.LockPlatformEnvironmentPointers(ctx, string(environmentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvironmentPointers{}, false, nil
	}
	if err != nil {
		return EnvironmentPointers{}, false, err
	}
	return EnvironmentPointers{
		AppID: platform.AppID(row.AppID), CurrentRevision: platform.RevisionID(platformText(row.CurrentRevisionID)),
		DesiredRelease: platform.ReleaseID(platformText(row.DesiredReleaseID)), AppliedRelease: platform.ReleaseID(platformText(row.AppliedReleaseID)),
		ServingRelease:    platform.ReleaseID(platformText(row.ServingReleaseID)),
		ServingDeployment: platform.DeploymentID(platformText(row.ServingDeploymentID)),
	}, true, nil
}

func (s *PointerStore) LockRevisionPointer(ctx context.Context, revisionID platform.RevisionID, environmentID platform.EnvironmentID) (bool, error) {
	owner, err := s.q.LockPlatformRevisionPointer(ctx, query.LockPlatformRevisionPointerParams{ID: string(revisionID), EnvironmentID: string(environmentID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return owner == string(environmentID), err
}

func (s *PointerStore) LockOwnedRelease(ctx context.Context, releaseID platform.ReleaseID, appID platform.AppID, environmentID platform.EnvironmentID) (ReleasePointer, bool, error) {
	row, err := s.q.LockPlatformOwnedRelease(ctx, query.LockPlatformOwnedReleaseParams{
		ID: string(releaseID), AppID: string(appID), EnvironmentID: string(environmentID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ReleasePointer{}, false, nil
	}
	var buildID platform.BuildID
	if row.BuildID.Valid {
		buildID = platform.BuildID(row.BuildID.String)
	}
	return ReleasePointer{AppID: platform.AppID(row.AppID), EnvironmentID: platform.EnvironmentID(row.EnvironmentID), Pruned: anyBool(row.Column3), BuildID: buildID}, err == nil, err
}

func (s *PointerStore) LockOwnedBuildForRelease(ctx context.Context, buildID platform.BuildID, appID platform.AppID, environmentID platform.EnvironmentID) (BuildReleasePointer, bool, error) {
	row, err := s.q.LockPlatformOwnedBuildForRelease(ctx, query.LockPlatformOwnedBuildForReleaseParams{
		ID: string(buildID), AppID: string(appID), EnvironmentID: string(environmentID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return BuildReleasePointer{}, false, nil
	}
	if err != nil {
		return BuildReleasePointer{}, false, err
	}
	return BuildReleasePointer{
		AppID: platform.AppID(row.AppID), EnvironmentID: platform.EnvironmentID(row.EnvironmentID),
		ReleaseID: platform.ReleaseID(row.ReleaseID),
	}, true, nil
}

func (s *PointerStore) LockOwnedDeployment(ctx context.Context, deploymentID platform.DeploymentID, appID platform.AppID, environmentID platform.EnvironmentID, releaseID platform.ReleaseID) (bool, error) {
	row, err := s.q.LockPlatformOwnedDeployment(ctx, query.LockPlatformOwnedDeploymentParams{
		ID: string(deploymentID), AppID: string(appID), EnvironmentID: string(environmentID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return row.ReleaseID == string(releaseID), err
}

func (s *PointerStore) SetReleaseBuild(ctx context.Context, releaseID platform.ReleaseID, buildID platform.BuildID) (int64, error) {
	return s.q.SetPlatformReleaseBuildPointer(ctx, query.SetPlatformReleaseBuildPointerParams{ID: string(releaseID), BuildID: string(buildID)})
}

// LockStore owns advisory-lock primitives. Transaction locks use the caller's
// transaction; session leases retain one borrowed connection until Release.
type LockStore struct {
	store *PlatformStore
	tx    *PlatformTx
}

func (l *LockStore) TryTargetXact(ctx context.Context, key int64) (bool, error) {
	q := l.q()
	return q.TryPlatformTargetXactLock(ctx, key)
}

func (l *LockStore) q() *query.Queries {
	if l.store != nil {
		return l.store.q
	}
	return l.tx.q
}

// SessionLease owns one borrowed connection and the session advisory locks
// acquired on it. Release never returns a connection whose unlock is uncertain
// to the pool.
type SessionLease struct {
	ctx        context.Context
	conn       *pgxpool.Conn
	keys       []int64
	once       sync.Once
	released   bool
	releaseErr error
}

func (l *SessionLease) Release() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if l.conn == nil {
			return
		}
		l.releaseErr = releaseSessionKeys(l.ctx, l.conn, l.keys)
		l.keys = nil
		l.conn = nil
		l.released = true
	})
	return l.releaseErr
}

func releaseSessionKeys(ctx context.Context, conn *pgxpool.Conn, keys []int64) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	q := query.New(conn)
	for index := len(keys) - 1; index >= 0; index-- {
		unlocked, err := q.ReleasePlatformSessionLock(ctx, keys[index])
		if err != nil || !unlocked {
			discardConn(ctx, conn)
			return fmt.Errorf("%w: session unlock failed", ErrPlatformLeaseUncertain)
		}
	}
	conn.Release()
	return nil
}

func discardConn(ctx context.Context, conn *pgxpool.Conn) {
	raw := conn.Hijack()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = raw.Close(ctx)
}

func rollbackLeaseTx(ctx context.Context, tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func (l *LockStore) AcquireTargets(ctx context.Context, keys []int64) (*SessionLease, error) {
	if l.store == nil {
		return nil, errors.New("entities: session locks require a Store")
	}
	conn, err := l.store.store.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	q := query.New(conn)
	held := make([]int64, 0, len(keys))
	for _, key := range uniqueKeys(keys) {
		if err := q.AcquirePlatformSessionLock(ctx, key); err != nil {
			discardConn(ctx, conn)
			return nil, fmt.Errorf("%w: session acquisition failed", ErrPlatformLeaseUncertain)
		}
		held = append(held, key)
	}
	return &SessionLease{ctx: ctx, conn: conn, keys: held}, nil
}

func (l *LockStore) TryTargets(ctx context.Context, keys []int64) (*SessionLease, bool, error) {
	if l.store == nil {
		return nil, false, errors.New("entities: session locks require a Store")
	}
	conn, err := l.store.store.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	q := query.New(conn)
	held := make([]int64, 0, len(keys))
	for _, key := range uniqueKeys(keys) {
		acquired, err := q.TryPlatformSessionLock(ctx, key)
		if err != nil {
			discardConn(ctx, conn)
			return nil, false, fmt.Errorf("%w: session acquisition failed", ErrPlatformLeaseUncertain)
		}
		if !acquired {
			if err := releaseSessionKeys(ctx, conn, held); err != nil {
				return nil, false, err
			}
			return nil, false, nil
		}
		held = append(held, key)
	}
	return &SessionLease{ctx: ctx, conn: conn, keys: held}, true, nil
}

func (l *LockStore) AcquireImageUpload(ctx context.Context, key int64) (*SessionLease, bool, error) {
	return l.TryTargets(ctx, []int64{key})
}

func uniqueKeys(keys []int64) []int64 {
	if len(keys) == 0 {
		return nil
	}
	result := append([]int64(nil), keys...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	unique := result[:0]
	for _, key := range result {
		if len(unique) == 0 || unique[len(unique)-1] != key {
			unique = append(unique, key)
		}
	}
	return unique
}

// ResourceStore exposes raw managed-resource persistence facts. It does not
// validate stored state, health, normalized spec or owned-volume identity; the
// application/cutover boundary must call validateStoredResource after every
// read, exactly as the existing raw scanner does.
type ResourceStore struct{ q *query.Queries }

type ResourceFilter struct {
	App         string
	Environment string
	State       string
	Node        string
	Limit       int
}

type ResourceInsert struct {
	ID            platform.ResourceID
	AppID         platform.AppID
	EnvironmentID platform.EnvironmentID
	Name          string
	Node          string
	Spec          platform.ResourceSpec
	Image         string
	OperationID   contract.OperationID
}

type ResourceReset struct {
	ID          platform.ResourceID
	Node        string
	Spec        platform.ResourceSpec
	Image       string
	OperationID contract.OperationID
}

type ResourceReady struct {
	ID            platform.ResourceID
	Image         string
	ImageID       string
	ImageDigest   string
	ContainerName string
	NetworkName   string
	Volumes       []platform.ResourceVolumeStatus
	OperationID   contract.OperationID
}

type ResourceDestroyed struct {
	ID          platform.ResourceID
	OperationID contract.OperationID
	Volumes     []platform.ResourceVolumeStatus
	Detail      string
}

type ResourceContainer struct {
	ID            platform.ResourceID
	OperationID   contract.OperationID
	Image         string
	ImageID       string
	ImageDigest   string
	ContainerName string
	NetworkName   string
	Volumes       []platform.ResourceVolumeStatus
}

type ResourceFailure struct {
	App         string
	Environment string
	Name        string
	OperationID contract.OperationID
	Code        contract.Code
	Message     string
}

type ResourceBlocker struct {
	Environment string
	Name        string
	State       string
}

type ResourceDeployability struct {
	State  platform.ResourceState
	Health platform.HealthState
}

func (s *ResourceStore) GetByName(ctx context.Context, app, environment, name string, forUpdate bool) (platform.Resource, bool, error) {
	if forUpdate {
		row, err := s.q.GetPlatformResourceByNameForUpdate(ctx, query.GetPlatformResourceByNameForUpdateParams{App: app, Environment: environment, Name: name})
		if errors.Is(err, pgx.ErrNoRows) {
			return platform.Resource{}, false, nil
		}
		if err != nil {
			return platform.Resource{}, false, err
		}
		resource, err := resourceRow(query.GetPlatformResourceByNameRow(row))
		return resource, err == nil, err
	}
	row, err := s.q.GetPlatformResourceByName(ctx, query.GetPlatformResourceByNameParams{App: app, Environment: environment, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.Resource{}, false, nil
	}
	if err != nil {
		return platform.Resource{}, false, err
	}
	resource, err := resourceRow(row)
	return resource, err == nil, err
}

func (s *ResourceStore) List(ctx context.Context, filter ResourceFilter) ([]platform.Resource, error) {
	if filter.Limit < 1 {
		return nil, errors.New("entities: resource list limit must be positive")
	}
	rows, err := s.q.ListPlatformResources(ctx, query.ListPlatformResourcesParams{
		App: platformOptionalText(filter.App), Environment: platformOptionalText(filter.Environment),
		State: platformOptionalText(filter.State), Node: platformOptionalText(filter.Node), MaxRows: int64(filter.Limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]platform.Resource, 0, len(rows))
	for _, row := range rows {
		resource, err := resourceRow(query.GetPlatformResourceByNameRow(row))
		if err != nil {
			return nil, err
		}
		result = append(result, resource)
	}
	return result, nil
}

func (s *ResourceStore) LockState(ctx context.Context, environmentID platform.EnvironmentID, name string) (platform.ResourceState, bool, error) {
	state, err := s.q.LockPlatformResourceState(ctx, query.LockPlatformResourceStateParams{EnvironmentID: string(environmentID), Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return platform.ResourceState(state), err == nil, err
}

func (s *ResourceStore) Insert(ctx context.Context, insert ResourceInsert) (bool, error) {
	spec, err := json.Marshal(insert.Spec)
	if err != nil {
		return false, err
	}
	affected, err := s.q.InsertPlatformResource(ctx, query.InsertPlatformResourceParams{
		ID: string(insert.ID), AppID: string(insert.AppID), EnvironmentID: string(insert.EnvironmentID),
		Name: insert.Name, NodeName: insert.Node, Spec: spec, Image: insert.Image, OperationID: string(insert.OperationID),
	})
	return affected == 1, err
}

func (s *ResourceStore) ResetForApply(ctx context.Context, reset ResourceReset) (int64, error) {
	spec, err := json.Marshal(reset.Spec)
	if err != nil {
		return 0, err
	}
	return s.q.ResetPlatformResourceForApply(ctx, query.ResetPlatformResourceForApplyParams{
		ID: string(reset.ID), NodeName: reset.Node, Spec: spec, Image: reset.Image, OperationID: string(reset.OperationID),
	})
}

func (s *ResourceStore) MarkDestroying(ctx context.Context, id platform.ResourceID, operationID contract.OperationID) (int64, error) {
	return s.q.MarkPlatformResourceDestroying(ctx, query.MarkPlatformResourceDestroyingParams{ID: string(id), OperationID: string(operationID)})
}

func (s *ResourceStore) MarkReady(ctx context.Context, ready ResourceReady) (int64, error) {
	volumes, err := json.Marshal(ready.Volumes)
	if err != nil {
		return 0, err
	}
	return s.q.MarkPlatformResourceReady(ctx, query.MarkPlatformResourceReadyParams{
		ID: string(ready.ID), OperationID: string(ready.OperationID), Image: ready.Image, ImageID: ready.ImageID,
		ImageDigest: ready.ImageDigest, ContainerName: ready.ContainerName, NetworkName: ready.NetworkName, Volumes: volumes,
	})
}

func (s *ResourceStore) MarkDestroyed(ctx context.Context, destroyed ResourceDestroyed) (int64, error) {
	volumes, err := json.Marshal(destroyed.Volumes)
	if err != nil {
		return 0, err
	}
	return s.q.MarkPlatformResourceDestroyed(ctx, query.MarkPlatformResourceDestroyedParams{
		ID: string(destroyed.ID), OperationID: string(destroyed.OperationID), Volumes: volumes, Detail: destroyed.Detail,
	})
}

func (s *ResourceStore) RecordContainer(ctx context.Context, record ResourceContainer) (int64, error) {
	volumes, err := json.Marshal(record.Volumes)
	if err != nil {
		return 0, err
	}
	return s.q.RecordPlatformResourceContainer(ctx, query.RecordPlatformResourceContainerParams{
		ID: string(record.ID), OperationID: string(record.OperationID), Image: record.Image, ImageID: record.ImageID,
		ImageDigest: record.ImageDigest, ContainerName: record.ContainerName, NetworkName: record.NetworkName, Volumes: volumes,
	})
}

func (s *ResourceStore) LockVolumes(ctx context.Context, id platform.ResourceID, operationID contract.OperationID) (json.RawMessage, bool, error) {
	raw, err := s.q.LockPlatformResourceVolumes(ctx, query.LockPlatformResourceVolumesParams{ID: string(id), OperationID: string(operationID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return raw, err == nil, err
}

func (s *ResourceStore) SetVolumes(ctx context.Context, id platform.ResourceID, operationID contract.OperationID, volumes json.RawMessage) (int64, error) {
	return s.q.SetPlatformResourceVolumes(ctx, query.SetPlatformResourceVolumesParams{ID: string(id), OperationID: string(operationID), Volumes: volumes})
}

func (s *ResourceStore) SettleTerminal(ctx context.Context, operationID contract.OperationID, code contract.Code, detail string) (int64, error) {
	return s.q.SettlePlatformResourceForTerminalOperation(ctx, query.SettlePlatformResourceForTerminalOperationParams{
		OperationID: string(operationID), ErrorCode: string(code), Detail: detail,
	})
}

func (s *ResourceStore) ReconcileTerminal(ctx context.Context) (int64, error) {
	return s.q.ReconcileTerminalPlatformResources(ctx)
}

func (s *ResourceStore) RecordFailure(ctx context.Context, failure ResourceFailure) (int64, error) {
	return s.q.RecordPlatformResourceFailure(ctx, query.RecordPlatformResourceFailureParams{
		App: failure.App, Environment: failure.Environment, Name: failure.Name,
		OperationID: string(failure.OperationID), ErrorCode: string(failure.Code), Message: failure.Message,
	})
}

func (s *ResourceStore) SetStateForTarget(ctx context.Context, operationID contract.OperationID, target string, state platform.ResourceState) (int64, error) {
	return s.q.SetPlatformResourceStateForTarget(ctx, query.SetPlatformResourceStateForTargetParams{
		OperationID: string(operationID), Target: target, State: string(state),
	})
}

func (s *ResourceStore) State(ctx context.Context, environmentID platform.EnvironmentID, name string) (ResourceDeployability, bool, error) {
	row, err := s.q.GetPlatformResourceDeployability(ctx, query.GetPlatformResourceDeployabilityParams{EnvironmentID: string(environmentID), Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return ResourceDeployability{}, false, nil
	}
	return ResourceDeployability{State: platform.ResourceState(row.State), Health: platform.HealthState(row.HealthState)}, err == nil, err
}

func (s *ResourceStore) Targets(ctx context.Context, app, environment string) ([]string, error) {
	return s.q.ListPlatformResourceTargets(ctx, query.ListPlatformResourceTargetsParams{App: app, Environment: platformOptionalText(environment)})
}

func (s *ResourceStore) LiveForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) ([]ResourceBlocker, error) {
	rows, err := s.q.ListPlatformLiveResourcesForEnvironment(ctx, string(environmentID))
	if err != nil {
		return nil, err
	}
	result := make([]ResourceBlocker, 0, len(rows))
	for _, row := range rows {
		result = append(result, ResourceBlocker{Environment: row.Name, Name: row.Name_2, State: row.State})
	}
	return result, nil
}

func (s *ResourceStore) LiveForApp(ctx context.Context, appID platform.AppID) ([]ResourceBlocker, error) {
	rows, err := s.q.ListPlatformLiveResourcesForApp(ctx, string(appID))
	if err != nil {
		return nil, err
	}
	result := make([]ResourceBlocker, 0, len(rows))
	for _, row := range rows {
		result = append(result, ResourceBlocker{Environment: row.Name, Name: row.Name_2, State: row.State})
	}
	return result, nil
}

func (s *ResourceStore) DefinitionSnapshot(ctx context.Context, environmentID platform.EnvironmentID) ([]ResourceDefinitionSnapshot, error) {
	rows, err := s.q.GetPlatformResourceDefinitionSnapshot(ctx, string(environmentID))
	if err != nil {
		return nil, err
	}
	result := make([]ResourceDefinitionSnapshot, 0, len(rows))
	for _, row := range rows {
		result = append(result, ResourceDefinitionSnapshot{
			ID: platform.ResourceID(row.ID), Name: row.Name, Node: row.NodeName, Spec: row.Spec,
			State: platform.ResourceState(row.State), OperationID: contract.OperationID(row.OperationID),
		})
	}
	return result, nil
}

func (s *ResourceStore) ActionFacts(ctx context.Context, app, environment, name string) (ResourceActionFacts, bool, error) {
	row, err := s.q.GetPlatformResourceActionFacts(ctx, query.GetPlatformResourceActionFactsParams{App: app, Environment: environment, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return ResourceActionFacts{}, false, nil
	}
	if err != nil {
		return ResourceActionFacts{}, false, err
	}
	now, err := timestampAny(row.ClockTimestamp)
	if err != nil {
		return ResourceActionFacts{}, false, err
	}
	updated, err := platformTime(row.UpdatedAt)
	if err != nil {
		return ResourceActionFacts{}, false, err
	}
	return ResourceActionFacts{
		Now: now, UpdatedAt: updated, ID: platform.ResourceID(row.ID), EnvironmentID: platform.EnvironmentID(row.EnvironmentID),
		Node: row.NodeName, State: platform.ResourceState(row.State), Image: row.Image, ImageID: row.ImageID,
		ContainerName: row.ContainerName, NetworkName: row.NetworkName, Volumes: row.Volumes,
		Spec: row.Spec, HealthState: platform.HealthState(row.HealthState), HealthCheckedAt: platformTimePtr(row.HealthCheckedAt),
		OperationID: contract.OperationID(row.OperationID), DestroyedAt: platformTimePtr(row.DestroyedAt),
	}, true, nil
}

func (s *ResourceStore) DeleteForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) (int64, error) {
	return s.q.DeletePlatformResourcesForEnvironment(ctx, string(environmentID))
}

type ResourceDefinitionSnapshot struct {
	ID          platform.ResourceID
	Name        string
	Node        string
	Spec        json.RawMessage
	State       platform.ResourceState
	OperationID contract.OperationID
}

type ResourceActionFacts struct {
	Now             time.Time
	UpdatedAt       time.Time
	ID              platform.ResourceID
	EnvironmentID   platform.EnvironmentID
	Node            string
	State           platform.ResourceState
	Image           string
	ImageID         string
	ContainerName   string
	NetworkName     string
	Volumes         json.RawMessage
	Spec            json.RawMessage
	HealthState     platform.HealthState
	HealthCheckedAt *time.Time
	OperationID     contract.OperationID
	DestroyedAt     *time.Time
}

// resourceRow maps raw persisted resource facts. Semantic validation
// (validateStoredResource) intentionally stays at the application boundary.
func resourceRow(row query.GetPlatformResourceByNameRow) (platform.Resource, error) {
	created, err := platformTime(row.CreatedAt)
	if err != nil {
		return platform.Resource{}, err
	}
	updated, err := platformTime(row.UpdatedAt)
	if err != nil {
		return platform.Resource{}, err
	}
	spec, err := platformJSON[platform.ResourceSpec](row.Spec)
	if err != nil {
		return platform.Resource{}, err
	}
	volumes, err := platformJSON[[]platform.ResourceVolumeStatus](row.Volumes)
	if err != nil {
		return platform.Resource{}, err
	}
	if volumes == nil {
		return platform.Resource{}, ErrPlatformCorrupt
	}
	return platform.Resource{
		ID: platform.ResourceID(row.ID), App: row.Name, Environment: row.Name_2, Name: row.Name_3,
		Node: row.NodeName, Spec: spec, State: platform.ResourceState(row.State), Image: row.Image,
		ImageID: row.ImageID, ImageDigest: row.ImageDigest, ContainerName: row.ContainerName, NetworkName: row.NetworkName,
		Volumes: volumes, HealthState: platform.HealthState(row.HealthState), HealthDetail: row.HealthDetail,
		HealthChecked: platformTimePtr(row.HealthCheckedAt), OperationID: contract.OperationID(row.OperationID),
		ErrorCode: contract.Code(row.ErrorCode), Error: row.Error, CreatedAt: created, UpdatedAt: updated,
		DestroyedAt: platformTimePtr(row.DestroyedAt),
	}, nil
}

type NodeFilter struct {
	Name  string
	State string
	Limit int
}

type NodeConnection struct {
	Name               string
	Roles              []string
	Enrollment         platform.EnrollmentState
	Transport          string
	SSHAddress         string
	SSHUser            string
	HostKey            string
	ExpectedIdentityFP string
	IdentityKey        string
	IdentityFP         string
	BuildConcurrency   int
	BuildUser          string
	WGEnabled          bool
	WGAddress          string
	WGEndpoint         string
	WGSSHPort          int
	WGPublicKey        string
	ControlWGPublicKey string
	WGState            platform.WireGuardState
}

type NodePeer struct {
	PublicKey string
	Address   string
	Endpoint  string
}

type NodeEnrollment struct {
	Name                string
	Roles               []string
	Capabilities        []string
	Address             string
	State               platform.EnrollmentState
	User                string
	HostKey             string
	HostKeyFingerprint  string
	IdentityFingerprint string
	BuildConcurrency    int
	BuildUser           string
	WGEnabled           bool
	WGAddress           string
	WGEndpoint          string
	WGSSHPort           int
	OperationID         contract.OperationID
}

type NodeEnrollmentResult struct {
	Name                string
	IdentityPublicKey   string
	IdentityFingerprint string
	Capabilities        []string
	OSID                string
	OSVersion           string
	Arch                string
	Probe               json.RawMessage
	Detail              string
}

type NodeCheckResult struct {
	Name         string
	Capabilities []string
	OSID         string
	OSVersion    string
	Arch         string
	Probe        json.RawMessage
	Detail       string
}

type BuildConcurrencyState struct {
	BuildConcurrency int
	Roles            []string
	Enrollment       platform.EnrollmentState
	Transport        string
}

type NodeStore struct {
	store *Store
	q     *query.Queries
}

func (s *NodeStore) Get(ctx context.Context, name string) (platform.Node, bool, error) {
	row, err := s.q.GetPlatformNode(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.Node{}, false, nil
	}
	if err != nil {
		return platform.Node{}, false, err
	}
	node, err := nodeRow(row)
	return node, err == nil, err
}

func (s *NodeStore) List(ctx context.Context, filter NodeFilter) ([]platform.Node, error) {
	if filter.Limit < 1 {
		return nil, errors.New("entities: node list limit must be positive")
	}
	rows, err := s.q.ListPlatformNodes(ctx, query.ListPlatformNodesParams{
		Name: platformOptionalText(filter.Name), State: platformOptionalText(filter.State), MaxRows: int64(filter.Limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]platform.Node, 0, len(rows))
	for _, row := range rows {
		node, err := nodeRow(query.GetPlatformNodeRow(row))
		if err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, nil
}

func (s *NodeStore) Connection(ctx context.Context, name string) (NodeConnection, bool, error) {
	row, err := s.q.GetPlatformNodeConnection(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return NodeConnection{}, false, nil
	}
	if err != nil {
		return NodeConnection{}, false, err
	}
	concurrency, err := platformInt(row.BuildConcurrency)
	if err != nil {
		return NodeConnection{}, false, err
	}
	port, err := platformInt(row.WgSshPort)
	if err != nil {
		return NodeConnection{}, false, err
	}
	return NodeConnection{
		Name: row.Name, Roles: row.Roles, Enrollment: platform.EnrollmentState(row.EnrollmentState),
		Transport: row.Transport, SSHAddress: row.SshAddress, SSHUser: row.SshUser, HostKey: row.SshHostKey,
		ExpectedIdentityFP: row.ExpectedIdentityFingerprint, IdentityKey: row.IdentityPublicKey,
		IdentityFP: row.IdentityFingerprint, BuildConcurrency: concurrency, BuildUser: row.BuildUser,
		WGEnabled: row.WgEnabled, WGAddress: row.WgAddress, WGEndpoint: row.WgEndpoint, WGSSHPort: port,
		WGPublicKey: row.WgPublicKey, ControlWGPublicKey: row.ControlWgPublicKey,
		WGState: platform.WireGuardState(row.WgState),
	}, true, nil
}

func (s *NodeStore) Exists(ctx context.Context, name string) (bool, error) {
	return s.q.PlatformNodeExists(ctx, name)
}

func (s *NodeStore) InsertRegistration(ctx context.Context, name string, roles, capabilities []string, address string) (bool, error) {
	affected, err := s.q.InsertPlatformNodeRegistration(ctx, query.InsertPlatformNodeRegistrationParams{
		Name: name, Roles: roles, Capabilities: capabilities, Address: address,
	})
	return affected == 1, err
}

func (s *NodeStore) EnsureLocal(ctx context.Context, name string, roles, capabilities []string) (int64, error) {
	return s.q.EnsurePlatformLocalNode(ctx, query.EnsurePlatformLocalNodeParams{Name: name, Roles: roles, Capabilities: capabilities})
}

func (s *NodeStore) ResetEnrollment(ctx context.Context, name string, operationID contract.OperationID) (int64, error) {
	return s.q.ResetPlatformNodeEnrollment(ctx, query.ResetPlatformNodeEnrollmentParams{Name: name, OperationID: string(operationID)})
}

func (s *NodeStore) UpsertEnrollment(ctx context.Context, enrollment NodeEnrollment) (bool, error) {
	affected, err := s.q.UpsertPlatformNodeEnrollment(ctx, query.UpsertPlatformNodeEnrollmentParams{
		Name: enrollment.Name, Roles: enrollment.Roles, Capabilities: enrollment.Capabilities,
		Address: enrollment.Address, EnrollmentState: string(enrollment.State), SshUser: enrollment.User,
		HostKey: enrollment.HostKey, HostKeyFingerprint: enrollment.HostKeyFingerprint,
		IdentityFingerprint: enrollment.IdentityFingerprint, BuildConcurrency: int64(enrollment.BuildConcurrency),
		BuildUser: enrollment.BuildUser, WgEnabled: enrollment.WGEnabled, WgAddress: enrollment.WGAddress,
		WgEndpoint: enrollment.WGEndpoint, WgSshPort: int64(enrollment.WGSSHPort), OperationID: string(enrollment.OperationID),
	})
	return affected == 1, err
}

func (s *NodeStore) RecordEnrolled(ctx context.Context, result NodeEnrollmentResult) (int64, error) {
	return s.q.RecordPlatformNodeEnrolled(ctx, query.RecordPlatformNodeEnrolledParams{
		Name: result.Name, IdentityPublicKey: result.IdentityPublicKey, IdentityFingerprint: result.IdentityFingerprint,
		Capabilities: result.Capabilities, OsID: result.OSID, OsVersion: result.OSVersion, Arch: result.Arch,
		Probe: result.Probe, Detail: result.Detail,
	})
}

func (s *NodeStore) RecordEnrollmentFailure(ctx context.Context, name, message, checkState string, terminal bool) (int64, error) {
	return s.q.RecordPlatformNodeEnrollmentFailure(ctx, query.RecordPlatformNodeEnrollmentFailureParams{
		Name: name, Message: message, CheckState: checkState, Terminal: terminal,
	})
}

func (s *NodeStore) RecordCheckSuccess(ctx context.Context, result NodeCheckResult) (int64, error) {
	return s.q.RecordPlatformNodeCheckSuccess(ctx, query.RecordPlatformNodeCheckSuccessParams{
		Name: result.Name, Capabilities: result.Capabilities, OsID: result.OSID, OsVersion: result.OSVersion,
		Arch: result.Arch, Probe: result.Probe, Detail: result.Detail,
	})
}

func (s *NodeStore) RecordCheckStatus(ctx context.Context, name, status, state, detail string) (int64, error) {
	return s.q.RecordPlatformNodeCheckStatus(ctx, query.RecordPlatformNodeCheckStatusParams{Name: name, Status: status, State: state, Detail: detail})
}

func (s *NodeStore) RecordCheckReady(ctx context.Context, name string) (int64, error) {
	return s.q.RecordPlatformNodeCheckReady(ctx, name)
}

func (s *NodeStore) EnrollmentForTarget(ctx context.Context, target string) (platform.EnrollmentState, contract.OperationID, bool, error) {
	row, err := s.q.GetPlatformNodeEnrollmentForTarget(ctx, target)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	return platform.EnrollmentState(row.EnrollmentState), contract.OperationID(platformText(row.EnrollOperationID)), err == nil, err
}

func (s *NodeStore) LockBuildConcurrency(ctx context.Context, name string) (BuildConcurrencyState, bool, error) {
	row, err := s.q.LockPlatformNodeBuildConcurrency(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return BuildConcurrencyState{}, false, nil
	}
	if err != nil {
		return BuildConcurrencyState{}, false, err
	}
	concurrency, err := platformInt(row.BuildConcurrency)
	return BuildConcurrencyState{BuildConcurrency: concurrency, Roles: row.Roles, Enrollment: platform.EnrollmentState(row.EnrollmentState), Transport: row.Transport}, err == nil, err
}

func (s *NodeStore) SetBuildConcurrency(ctx context.Context, name string, value int) (int64, error) {
	return s.q.SetPlatformNodeBuildConcurrency(ctx, query.SetPlatformNodeBuildConcurrencyParams{Name: name, BuildConcurrency: int64(value)})
}

// BuildSlotProbe is one capacity observation under the node row lock. Lease is
// non-nil only when a slot was admitted; the caller owns its Release.
type BuildSlotProbe struct {
	Node  string
	Limit int
	Busy  int
	Lease *SessionLease
}

func (p *BuildSlotProbe) Release() error {
	if p == nil || p.Lease == nil {
		return nil
	}
	return p.Lease.Release()
}

// ProbeBuildSlots performs the existing bounded build-slot admission probe:
// lock the node row, read its limit, try every slot, unlock every non-chosen
// slot, and retain the chosen session lock on one borrowed connection.
func (s *NodeStore) ProbeBuildSlots(ctx context.Context, name string, max int) (BuildSlotProbe, error) {
	if s.store == nil {
		return BuildSlotProbe{}, errors.New("entities: build-slot leases require a Store")
	}
	conn, err := s.store.pool.Acquire(ctx)
	if err != nil {
		return BuildSlotProbe{}, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		return BuildSlotProbe{}, err
	}
	txq := query.New(tx)
	row, err := txq.LockPlatformNodeBuildConcurrency(ctx, name)
	if err != nil {
		rollbackLeaseTx(ctx, tx)
		conn.Release()
		if errors.Is(err, pgx.ErrNoRows) {
			return BuildSlotProbe{}, ErrPlatformPointerIntegrity
		}
		return BuildSlotProbe{}, err
	}
	limit, err := platformInt(row.BuildConcurrency)
	if err != nil {
		rollbackLeaseTx(ctx, tx)
		conn.Release()
		return BuildSlotProbe{}, err
	}
	held, busy, err := tryBuildSlotLocks(ctx, txq, name, max)
	if err != nil {
		rollbackLeaseTx(ctx, tx)
		discardConn(ctx, conn)
		return BuildSlotProbe{}, err
	}
	var chosen int64
	admitted := busy < limit && len(held) > 0
	if admitted {
		chosen = held[0]
		held = held[1:]
	}
	if err := releaseBuildSlotLocks(ctx, txq, held); err != nil {
		rollbackLeaseTx(ctx, tx)
		discardConn(ctx, conn)
		return BuildSlotProbe{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		discardConn(ctx, conn)
		return BuildSlotProbe{}, fmt.Errorf("%w: build-slot commit failed", ErrPlatformLeaseUncertain)
	}
	probe := BuildSlotProbe{Node: name, Limit: limit, Busy: busy}
	if admitted {
		probe.Lease = &SessionLease{ctx: ctx, conn: conn, keys: []int64{chosen}}
	} else {
		conn.Release()
	}
	return probe, nil
}

func tryBuildSlotLocks(ctx context.Context, q *query.Queries, node string, max int) ([]int64, int, error) {
	held := make([]int64, 0, max)
	busy := 0
	for slot := 0; slot < max; slot++ {
		key := buildSlotKey(node, slot)
		acquired, err := q.TryPlatformSessionLock(ctx, key)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: build-slot acquisition failed", ErrPlatformLeaseUncertain)
		}
		if acquired {
			held = append(held, key)
		} else {
			busy++
		}
	}
	return held, busy, nil
}

func releaseBuildSlotLocks(ctx context.Context, q *query.Queries, keys []int64) error {
	for _, key := range keys {
		unlocked, err := q.ReleasePlatformSessionLock(ctx, key)
		if err != nil || !unlocked {
			return fmt.Errorf("%w: build-slot unlock failed", ErrPlatformLeaseUncertain)
		}
	}
	return nil
}

func buildSlotKey(node string, slot int) int64 {
	sum := sha256.Sum256([]byte("eregion-build-slot\x00" + node + "\x00" + strconv.Itoa(slot)))
	// The full hash is an intentional two's-complement PostgreSQL advisory-lock
	// key, not a bounded numeric quantity. Masking a bit would change the lock
	// identity for half of all nodes and break coordination with processes
	// still using the original key.
	// #nosec G115 -- intentional full-width hash-to-advisory-key conversion.
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func (s *NodeStore) LockWireGuardAllocation(ctx context.Context, key int64) error {
	return s.q.LockPlatformWireGuardAllocation(ctx, key)
}

func (s *NodeStore) WireGuardAddresses(ctx context.Context) ([]string, error) {
	return s.q.ListPlatformNodeWireGuardAddresses(ctx)
}

func (s *NodeStore) WireGuardPeers(ctx context.Context, exclude string) ([]NodePeer, error) {
	rows, err := s.q.ListPlatformNodeWireGuardPeers(ctx, exclude)
	if err != nil {
		return nil, err
	}
	result := make([]NodePeer, 0, len(rows))
	for _, row := range rows {
		result = append(result, NodePeer{PublicKey: row.WgPublicKey, Address: row.WgAddress, Endpoint: row.WgEndpoint})
	}
	return result, nil
}

func (s *NodeStore) SetWireGuardKeys(ctx context.Context, name, public, control string) (int64, error) {
	return s.q.SetPlatformNodeWireGuardKeys(ctx, query.SetPlatformNodeWireGuardKeysParams{Name: name, WgPublicKey: public, ControlWgPublicKey: control})
}

func (s *NodeStore) SetWireGuardFailed(ctx context.Context, name string) (int64, error) {
	return s.q.SetPlatformNodeWireGuardFailed(ctx, name)
}

func (s *NodeStore) SetWireGuardState(ctx context.Context, name string, state platform.WireGuardState) (int64, error) {
	return s.q.SetPlatformNodeWireGuardState(ctx, query.SetPlatformNodeWireGuardStateParams{Name: name, WgState: string(state)})
}

func (s *NodeStore) LockExistingNames(ctx context.Context, names []string) ([]string, error) {
	return s.q.LockPlatformExistingNodes(ctx, names)
}

func (s *NodeStore) DefinitionSnapshot(ctx context.Context, names []string) ([]NodeDefinitionSnapshot, error) {
	rows, err := s.q.GetPlatformNodeDefinitionSnapshot(ctx, names)
	if err != nil {
		return nil, err
	}
	result := make([]NodeDefinitionSnapshot, 0, len(rows))
	for _, row := range rows {
		result = append(result, NodeDefinitionSnapshot{
			Name: row.Name, Roles: row.Roles, Capabilities: row.Capabilities, Status: platform.NodeStatus(row.Status),
			Enrollment: platform.EnrollmentState(row.EnrollmentState), Transport: row.Transport, LastCheckState: row.LastCheckState,
		})
	}
	return result, nil
}

func (s *NodeStore) ActionFacts(ctx context.Context, name string) (NodeActionFacts, bool, error) {
	row, err := s.q.GetPlatformNodeActionFacts(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return NodeActionFacts{}, false, nil
	}
	if err != nil {
		return NodeActionFacts{}, false, err
	}
	now, err := timestampAny(row.ClockTimestamp)
	if err != nil {
		return NodeActionFacts{}, false, err
	}
	updated, err := platformTime(row.UpdatedAt)
	if err != nil {
		return NodeActionFacts{}, false, err
	}
	return NodeActionFacts{
		Now: now, UpdatedAt: updated, Roles: row.Roles, Capabilities: row.Capabilities,
		Status: platform.NodeStatus(row.Status), Enrollment: platform.EnrollmentState(row.EnrollmentState),
		EnrollmentOperation: contract.OperationID(row.EnrollOperationID), EnrolledAt: platformTimePtr(row.EnrolledAt),
		IdentityFingerprint: row.IdentityFingerprint, LastCheckState: row.LastCheckState,
		LastCheckAt: platformTimePtr(row.LastCheckAt), WGState: platform.WireGuardState(row.WgState),
	}, true, nil
}

type NodeDefinitionSnapshot struct {
	Name           string
	Roles          []string
	Capabilities   []string
	Status         platform.NodeStatus
	Enrollment     platform.EnrollmentState
	Transport      string
	LastCheckState string
}

type NodeActionFacts struct {
	Now                 time.Time
	UpdatedAt           time.Time
	Roles               []string
	Capabilities        []string
	Status              platform.NodeStatus
	Enrollment          platform.EnrollmentState
	EnrollmentOperation contract.OperationID
	EnrolledAt          *time.Time
	IdentityFingerprint string
	LastCheckState      string
	LastCheckAt         *time.Time
	WGState             platform.WireGuardState
}

func nodeRow(row query.GetPlatformNodeRow) (platform.Node, error) {
	created, err := platformTime(row.CreatedAt)
	if err != nil {
		return platform.Node{}, err
	}
	updated, err := platformTime(row.UpdatedAt)
	if err != nil {
		return platform.Node{}, err
	}
	concurrency, err := platformInt(row.BuildConcurrency)
	if err != nil {
		return platform.Node{}, err
	}
	node := platform.Node{
		Name: row.Name, Roles: row.Roles, Capabilities: row.Capabilities, Address: row.Address,
		Status: platform.NodeStatus(row.Status), LastSeenAt: platformTimePtr(row.LastSeenAt),
		CreatedAt: created, UpdatedAt: updated, EnrollmentState: platform.EnrollmentState(row.EnrollmentState),
		EnrollmentError: row.EnrollmentError, Transport: row.Transport, SSHUser: row.SshUser,
		HostKeyFingerprint: row.SshHostKeyFingerprint, IdentityFingerprint: row.IdentityFingerprint,
		Arch: row.Arch, BuildConcurrency: concurrency, EnrolledAt: platformTimePtr(row.EnrolledAt),
		LastCheckAt: platformTimePtr(row.LastCheckAt), LastCheckState: row.LastCheckState,
		LastCheckDetail: row.LastCheckDetail,
	}
	if row.OsID != "" {
		node.OS = row.OsID + " " + row.OsVersion
	}
	wgState := platform.WireGuardState(row.WgState)
	if row.WgEnabled || wgState != platform.WireGuardNone {
		node.WireGuard = &platform.NodeWireGuard{
			Enabled: row.WgEnabled, Address: row.WgAddress, Endpoint: row.WgEndpoint,
			PublicKey: row.WgPublicKey, State: wgState, ReconciledAt: platformTimePtr(row.WgReconciledAt),
		}
	}
	return node, nil
}
