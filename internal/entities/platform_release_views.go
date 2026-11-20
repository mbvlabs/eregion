package entities

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

type ReleaseFilter struct {
	App         string
	Environment string
	State       string
	Limit       int
}

type ReleaseInsert struct {
	ID            platform.ReleaseID
	OperationID   contract.OperationID
	AppID         platform.AppID
	EnvironmentID platform.EnvironmentID
	Image         string
	ImageID       string
	Source        json.RawMessage
	Build         json.RawMessage
	Command       json.RawMessage
	Status        platform.ReleaseStatus
	CommitSHA     string
	ErrorCode     contract.Code
	Error         string
	CompletedAt   *time.Time
}

type ReleaseServingEnvironment struct {
	EnvironmentID platform.EnvironmentID
	Serving       platform.ReleaseID
	Desired       platform.ReleaseID
	Applied       platform.ReleaseID
}

type ReleaseRetentionRow struct {
	ID          platform.ReleaseID
	App         string
	Environment string
	Image       string
	ImageID     string
	Status      platform.ReleaseStatus
	CreatedAt   time.Time
	PrunedAt    *time.Time
}

type BuildFilter struct {
	App         string
	Environment string
	State       string
	BuildID     platform.BuildID
	Limit       int
}

type BuildInsert struct {
	ID            platform.BuildID
	OperationID   contract.OperationID
	AppID         platform.AppID
	EnvironmentID platform.EnvironmentID
	RevisionID    platform.RevisionID
	ReleaseID     platform.ReleaseID
	Source        json.RawMessage
	CommitSHA     string
}

type BuildPlacement struct {
	App         string
	Environment string
}

type BuildActionFacts struct {
	Now         time.Time
	ID          platform.BuildID
	OperationID contract.OperationID
	State       platform.BuildState
	CommitSHA   string
	RevisionID  platform.RevisionID
	ReleaseID   platform.ReleaseID
	CompletedAt *time.Time
}

type DeploymentFilter struct {
	App          string
	Environment  string
	State        string
	DeploymentID platform.DeploymentID
	Limit        int
}

type DeploymentInsert struct {
	ID                   platform.DeploymentID
	OperationID          contract.OperationID
	AppID                platform.AppID
	EnvironmentID        platform.EnvironmentID
	Action               platform.DeploymentAction
	ReleaseID            platform.ReleaseID
	PreviousReleaseID    platform.ReleaseID
	PreviousDeploymentID platform.DeploymentID
	RevisionID           platform.RevisionID
	SecretRevisions      json.RawMessage
}

type DeploymentPredecessor struct {
	ReleaseID    platform.ReleaseID
	DeploymentID platform.DeploymentID
}

type DeploymentExecutionFacts struct {
	PreviousReleaseID    platform.ReleaseID
	PreviousDeploymentID platform.DeploymentID
	Now                  time.Time
	EnvironmentUpdatedAt time.Time
	ServingDeploymentID  platform.DeploymentID
	ServingReleaseID     platform.ReleaseID
	DesiredReleaseID     platform.ReleaseID
	AppliedReleaseID     platform.ReleaseID
	HealthState          platform.HealthState
	HealthCheckedAt      *time.Time
	OwnerOperationID     contract.OperationID
	OwnerDeploymentID    platform.DeploymentID
	OwnerPhase           string
	RouteTarget          platform.DeploymentID
	CleanupCount         int64
	PreviousCleanupCount int64
}

type DeploymentFailureFacts struct {
	Action          platform.DeploymentAction
	App             string
	Environment     string
	ReleaseID       platform.ReleaseID
	PreviousRelease platform.ReleaseID
	ServingRelease  platform.ReleaseID
}

type DeploymentRetentionRow struct {
	ID                platform.DeploymentID
	App               string
	Environment       string
	ReleaseID         platform.ReleaseID
	PreviousReleaseID platform.ReleaseID
	State             platform.DeploymentState
	CreatedAt         time.Time
	CompletedAt       *time.Time
}

type DeploymentResourceDependency struct {
	RevisionID        platform.RevisionID
	Resources         []string
	PreviousResources []string
}

type RepairTarget struct {
	App               string
	Environment       string
	ServingDeployment platform.DeploymentID
	Instances         json.RawMessage
	Node              string
}

type BuildDeploymentFacts struct {
	BuildID      platform.BuildID
	BuildState   platform.BuildState
	CommitSHA    string
	DeploymentID platform.DeploymentID
	DeployState  platform.DeploymentState
	RevisionID   platform.RevisionID
	ReleaseID    platform.ReleaseID
	Image        string
	ImageID      string
}

type ResultIdentity struct {
	BuildID      platform.BuildID
	DeploymentID platform.DeploymentID
	RevisionID   platform.RevisionID
	ReleaseID    platform.ReleaseID
	Image        string
	ImageID      string
	CommitSHA    string
}

type ReleaseStore struct{ q *query.Queries }

func (s *ReleaseStore) GetByID(ctx context.Context, id platform.ReleaseID) (platform.Release, bool, error) {
	row, err := s.q.GetPlatformReleaseByID(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.Release{}, false, nil
	}
	if err != nil {
		return platform.Release{}, false, err
	}
	release, err := releaseRow(row)
	return release, err == nil, err
}

func (s *ReleaseStore) GetByOperation(ctx context.Context, id contract.OperationID) (platform.Release, bool, error) {
	releaseID, found, err := s.IDForOperation(ctx, id)
	if err != nil || !found {
		return platform.Release{}, false, err
	}
	return s.GetByID(ctx, releaseID)
}

func (s *ReleaseStore) List(ctx context.Context, filter ReleaseFilter) ([]platform.Release, error) {
	if filter.Limit < 1 {
		return nil, errors.New("entities: release list limit must be positive")
	}
	rows, err := s.q.ListPlatformReleases(ctx, query.ListPlatformReleasesParams{
		App: platformOptionalText(filter.App), Environment: platformOptionalText(filter.Environment),
		State: platformOptionalText(filter.State), MaxRows: int64(filter.Limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]platform.Release, 0, len(rows))
	for _, row := range rows {
		release, err := releaseRow(query.GetPlatformReleaseByIDRow(row))
		if err != nil {
			return nil, err
		}
		result = append(result, release)
	}
	return result, nil
}

func (s *ReleaseStore) IDForOperation(ctx context.Context, id contract.OperationID) (platform.ReleaseID, bool, error) {
	release, err := s.q.GetPlatformReleaseIDForOperation(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return platform.ReleaseID(release), err == nil, err
}

func (s *ReleaseStore) Insert(ctx context.Context, insert ReleaseInsert) (bool, error) {
	var completed pgtype.Timestamptz
	if insert.CompletedAt != nil {
		completed = pgtype.Timestamptz{Time: *insert.CompletedAt, Valid: true}
	}
	affected, err := s.q.InsertPlatformRelease(ctx, query.InsertPlatformReleaseParams{
		ID: string(insert.ID), OperationID: string(insert.OperationID), AppID: string(insert.AppID),
		EnvironmentID: string(insert.EnvironmentID), Image: insert.Image, ImageID: insert.ImageID,
		Source: insert.Source, Build: nullableJSON(insert.Build), Command: nullableJSON(insert.Command),
		Status: string(insert.Status), CommitSha: insert.CommitSHA, ErrorCode: string(insert.ErrorCode),
		Error: insert.Error, CompletedAt: completed,
	})
	return affected == 1, err
}

func (s *ReleaseStore) MarkReadyWithImageID(ctx context.Context, id platform.ReleaseID, imageID string) (int64, error) {
	return s.q.MarkPlatformReleaseReadyWithImageID(ctx, query.MarkPlatformReleaseReadyWithImageIDParams{ID: string(id), ImageID: imageID})
}

func (s *ReleaseStore) MarkOwnedReadyWithImageID(ctx context.Context, id platform.ReleaseID, operationID contract.OperationID, image, imageID string) (int64, error) {
	return s.q.MarkPlatformOwnedReleaseReadyWithImageID(ctx, query.MarkPlatformOwnedReleaseReadyWithImageIDParams{
		ID: string(id), OperationID: string(operationID), Image: image, ImageID: imageID,
	})
}

func (s *ReleaseStore) MarkReadyFromBuild(ctx context.Context, id platform.ReleaseID, image, imageID, commit string) (int64, error) {
	return s.q.MarkPlatformReleaseReadyFromBuild(ctx, query.MarkPlatformReleaseReadyFromBuildParams{
		ID: string(id), Image: image, ImageID: imageID, CommitSha: commit,
	})
}

func (s *ReleaseStore) MarkFailedByID(ctx context.Context, id platform.ReleaseID, code contract.Code, message string) (int64, error) {
	return s.q.MarkPlatformReleaseFailedByID(ctx, query.MarkPlatformReleaseFailedByIDParams{
		ID: string(id), ErrorCode: string(code), Message: message,
	})
}

func (s *ReleaseStore) MarkFailedByOperation(ctx context.Context, operationID contract.OperationID, code contract.Code, message string) (int64, error) {
	return s.q.MarkPlatformReleaseFailedByOperation(ctx, query.MarkPlatformReleaseFailedByOperationParams{
		OperationID: string(operationID), ErrorCode: string(code), Message: message,
	})
}

func (s *ReleaseStore) MarkPruned(ctx context.Context, id platform.ReleaseID) (int64, error) {
	return s.q.MarkPlatformReleasePruned(ctx, string(id))
}

func (s *ReleaseStore) Command(ctx context.Context, id platform.ReleaseID) (json.RawMessage, bool, error) {
	raw, err := s.q.GetPlatformReleaseCommand(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return derefRaw(raw), err == nil, err
}

func (s *ReleaseStore) LockServingEnvironment(ctx context.Context, id platform.ReleaseID) (ReleaseServingEnvironment, bool, error) {
	row, err := s.q.LockPlatformReleaseServingEnvironment(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ReleaseServingEnvironment{}, false, nil
	}
	if err != nil {
		return ReleaseServingEnvironment{}, false, err
	}
	return ReleaseServingEnvironment{
		EnvironmentID: platform.EnvironmentID(row.ID), Serving: platform.ReleaseID(row.ServingReleaseID),
		Desired: platform.ReleaseID(row.DesiredReleaseID), Applied: platform.ReleaseID(row.AppliedReleaseID),
	}, true, nil
}

func (s *ReleaseStore) ImageStillShared(ctx context.Context, imageID string, exclude platform.ReleaseID) (platform.ReleaseID, bool, error) {
	id, err := s.q.PlatformReleaseImageStillShared(ctx, query.PlatformReleaseImageStillSharedParams{
		ImageID: imageID, ExcludeID: string(exclude),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return platform.ReleaseID(id), err == nil, err
}

func (s *ReleaseStore) RetentionRows(ctx context.Context, node string) ([]ReleaseRetentionRow, error) {
	rows, err := s.q.ListPlatformReleaseRetentionRows(ctx, platformOptionalText(node))
	if err != nil {
		return nil, err
	}
	result := make([]ReleaseRetentionRow, 0, len(rows))
	for _, row := range rows {
		created, err := platformTime(row.CreatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, ReleaseRetentionRow{
			ID: platform.ReleaseID(row.ID), App: row.Name, Environment: row.Name_2, Image: row.Image,
			ImageID: row.ImageID, Status: platform.ReleaseStatus(row.Status), CreatedAt: created,
			PrunedAt: platformTimePtr(row.PrunedAt),
		})
	}
	return result, nil
}

func (s *ReleaseStore) ActiveForNode(ctx context.Context, node string) ([]platform.ReleaseID, error) {
	rows, err := s.q.ListPlatformActiveReleaseIDsForNode(ctx, platformOptionalText(node))
	if err != nil {
		return nil, err
	}
	result := make([]platform.ReleaseID, 0, len(rows))
	for _, id := range rows {
		result = append(result, platform.ReleaseID(id))
	}
	return result, nil
}

func (s *ReleaseStore) CountActiveDeployments(ctx context.Context, id platform.ReleaseID) (int64, error) {
	return s.q.CountPlatformActiveDeploymentsForRelease(ctx, string(id))
}

func (s *ReleaseStore) CountActiveOperations(ctx context.Context, id platform.ReleaseID) (int64, error) {
	return s.q.CountPlatformActiveOperationsForRelease(ctx, string(id))
}

func (s *ReleaseStore) DeleteForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) (int64, error) {
	return s.q.DeletePlatformReleasesForEnvironment(ctx, string(environmentID))
}

func releaseRow(row query.GetPlatformReleaseByIDRow) (platform.Release, error) {
	created, err := platformTime(row.CreatedAt)
	if err != nil {
		return platform.Release{}, err
	}
	release := platform.Release{
		ID: platform.ReleaseID(row.ID), OperationID: contract.OperationID(row.OperationID),
		App: row.Name, Environment: row.Name_2, Image: row.Image, ImageID: row.ImageID,
		Status: platform.ReleaseStatus(row.Status), ErrorCode: contract.Code(row.ErrorCode), Error: row.Error,
		CreatedAt: created, CompletedAt: platformTimePtr(row.CompletedAt), BuildID: platform.BuildID(row.BuildID),
		CommitSHA: row.CommitSha, PrunedAt: platformTimePtr(row.PrunedAt),
	}
	if !row.Column18.Valid || !row.Column18.Bool {
		return platform.Release{}, ErrPlatformPointerIntegrity
	}
	if len(row.Source) > 0 {
		if err := json.Unmarshal(row.Source, &release.Source); err != nil {
			return platform.Release{}, ErrPlatformCorrupt
		}
	}
	if row.Build != nil && len(*row.Build) > 0 {
		var build platform.Build
		if err := json.Unmarshal(*row.Build, &build); err != nil {
			return platform.Release{}, ErrPlatformCorrupt
		}
		release.Build = &build
	}
	if row.Command != nil && len(*row.Command) > 0 {
		var command platform.CommandSpec
		if err := json.Unmarshal(*row.Command, &command); err != nil {
			return platform.Release{}, ErrPlatformCorrupt
		}
		release.Command = &command
	}
	return release, nil
}

type BuildStore struct{ q *query.Queries }

func (s *BuildStore) GetByID(ctx context.Context, id platform.BuildID) (platform.BuildRecord, bool, error) {
	row, err := s.q.GetPlatformBuildByID(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.BuildRecord{}, false, nil
	}
	if err != nil {
		return platform.BuildRecord{}, false, err
	}
	build, err := buildRow(row)
	return build, err == nil, err
}

func (s *BuildStore) GetByOperation(ctx context.Context, id contract.OperationID) (platform.BuildRecord, bool, error) {
	row, err := s.q.GetPlatformBuildByOperation(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.BuildRecord{}, false, nil
	}
	if err != nil {
		return platform.BuildRecord{}, false, err
	}
	build, err := buildRow(query.GetPlatformBuildByIDRow(row))
	return build, err == nil, err
}

func (s *BuildStore) List(ctx context.Context, filter BuildFilter) ([]platform.BuildRecord, error) {
	if filter.Limit < 1 {
		return nil, errors.New("entities: build list limit must be positive")
	}
	rows, err := s.q.ListPlatformBuilds(ctx, query.ListPlatformBuildsParams{
		App: platformOptionalText(filter.App), Environment: platformOptionalText(filter.Environment),
		State: platformOptionalText(filter.State), BuildID: platformOptionalText(string(filter.BuildID)),
		MaxRows: int64(filter.Limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]platform.BuildRecord, 0, len(rows))
	for _, row := range rows {
		build, err := buildRow(query.GetPlatformBuildByIDRow(row))
		if err != nil {
			return nil, err
		}
		result = append(result, build)
	}
	return result, nil
}

func (s *BuildStore) Insert(ctx context.Context, insert BuildInsert) (bool, error) {
	affected, err := s.q.InsertPlatformBuild(ctx, query.InsertPlatformBuildParams{
		ID: string(insert.ID), OperationID: string(insert.OperationID), AppID: string(insert.AppID),
		EnvironmentID: string(insert.EnvironmentID), RevisionID: platformOptionalText(string(insert.RevisionID)),
		ReleaseID: platformOptionalText(string(insert.ReleaseID)), Source: insert.Source, CommitSha: insert.CommitSHA,
	})
	return affected == 1, err
}

func (s *BuildStore) Placement(ctx context.Context, id platform.BuildID) (BuildPlacement, bool, error) {
	row, err := s.q.GetPlatformBuildPlacement(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return BuildPlacement{}, false, nil
	}
	return BuildPlacement{App: row.Name, Environment: row.Name_2}, err == nil, err
}

func (s *BuildStore) State(ctx context.Context, id platform.BuildID) (platform.BuildState, bool, error) {
	state, err := s.q.GetPlatformBuildState(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return platform.BuildState(state), err == nil, err
}

func (s *BuildStore) ActiveOperationForApp(ctx context.Context, appID platform.AppID) (bool, error) {
	return s.q.PlatformBuildActiveOperationForApp(ctx, string(appID))
}

func (s *BuildStore) ActiveTargetsForApp(ctx context.Context, appID platform.AppID, target, appPrefix, envPrefix string) ([]contract.Operation, error) {
	rows, err := s.q.ListPlatformBuildActiveTargetsForApp(ctx, query.ListPlatformBuildActiveTargetsForAppParams{
		Target: target, AppTargetPrefix: appPrefix, EnvTargetPrefix: envPrefix, AppID: string(appID),
	})
	if err != nil {
		return nil, err
	}
	result := make([]contract.Operation, 0, len(rows))
	for _, row := range rows {
		result = append(result, contract.Operation{ID: contract.OperationID(row.ID), Kind: row.Kind, Target: row.Target, State: contract.State(row.State)})
	}
	return result, nil
}

func (s *BuildStore) MarkRunningForOperation(ctx context.Context, id platform.BuildID, operationID contract.OperationID, releaseID platform.ReleaseID) (bool, error) {
	n, err := s.q.MarkPlatformBuildRunningForOperation(ctx, query.MarkPlatformBuildRunningForOperationParams{
		ID: string(id), OperationID: string(operationID), ReleaseID: string(releaseID),
	})
	return n == 1, err
}

func (s *BuildStore) MarkSucceeded(ctx context.Context, id platform.BuildID, commit string) (int64, error) {
	return s.q.MarkPlatformBuildSucceeded(ctx, query.MarkPlatformBuildSucceededParams{ID: string(id), CommitSha: commit})
}

func (s *BuildStore) MarkFailed(ctx context.Context, id platform.BuildID, code contract.Code, message string) (int64, error) {
	return s.q.MarkPlatformBuildFailedByID(ctx, query.MarkPlatformBuildFailedByIDParams{ID: string(id), ErrorCode: string(code), Message: message})
}

func (s *BuildStore) MarkCancelled(ctx context.Context, id platform.BuildID, code contract.Code, message string) (int64, error) {
	return s.q.MarkPlatformBuildCancelledByID(ctx, query.MarkPlatformBuildCancelledByIDParams{ID: string(id), ErrorCode: string(code), Message: message})
}

func (s *BuildStore) CancelPending(ctx context.Context, id platform.BuildID, operationID contract.OperationID, code contract.Code, message string) (bool, error) {
	n, err := s.q.CancelPendingPlatformBuild(ctx, query.CancelPendingPlatformBuildParams{
		ID: string(id), OperationID: string(operationID), ErrorCode: string(code), Message: message,
	})
	return n == 1, err
}

func (s *BuildStore) MarkFailedByOperation(ctx context.Context, id contract.OperationID, code contract.Code, message string) (int64, error) {
	return s.q.MarkPlatformBuildFailedByOperation(ctx, query.MarkPlatformBuildFailedByOperationParams{
		OperationID: string(id), ErrorCode: string(code), Message: message,
	})
}

func (s *BuildStore) MarkCancelledByOperation(ctx context.Context, id contract.OperationID, code contract.Code, message string) (int64, error) {
	return s.q.MarkPlatformBuildCancelledByOperation(ctx, query.MarkPlatformBuildCancelledByOperationParams{
		OperationID: string(id), ErrorCode: string(code), Message: message,
	})
}

func (s *BuildStore) ActionFacts(ctx context.Context, id platform.BuildID) (BuildActionFacts, bool, error) {
	row, err := s.q.GetPlatformBuildActionFacts(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return BuildActionFacts{}, false, nil
	}
	if err != nil {
		return BuildActionFacts{}, false, err
	}
	now, err := timestampAny(row.ClockTimestamp)
	if err != nil {
		return BuildActionFacts{}, false, err
	}
	return BuildActionFacts{
		Now: now, ID: platform.BuildID(row.ID), OperationID: contract.OperationID(row.OperationID),
		State: platform.BuildState(row.State), CommitSHA: row.CommitSha,
		RevisionID: platform.RevisionID(row.RevisionID), ReleaseID: platform.ReleaseID(row.ReleaseID),
		CompletedAt: platformTimePtr(row.CompletedAt),
	}, true, nil
}

func (s *BuildStore) DeleteForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) (int64, error) {
	return s.q.DeletePlatformBuildsForEnvironment(ctx, string(environmentID))
}

func buildRow(row query.GetPlatformBuildByIDRow) (platform.BuildRecord, error) {
	created, err := platformTime(row.CreatedAt)
	if err != nil {
		return platform.BuildRecord{}, err
	}
	build := platform.BuildRecord{
		ID: platform.BuildID(row.ID), OperationID: contract.OperationID(row.OperationID),
		App: row.Name, Environment: row.Name_2, RevisionID: platform.RevisionID(row.RevisionID),
		ReleaseID: platform.ReleaseID(row.ReleaseID), CommitSHA: row.CommitSha,
		State: platform.BuildState(row.State), ErrorCode: contract.Code(row.ErrorCode), Error: row.Error,
		CreatedAt: created, StartedAt: platformTimePtr(row.StartedAt), CompletedAt: platformTimePtr(row.CompletedAt),
	}
	if len(row.Source) > 0 {
		if err := json.Unmarshal(row.Source, &build.Source); err != nil {
			return platform.BuildRecord{}, ErrPlatformCorrupt
		}
	}
	return build, nil
}

type DeploymentStore struct{ q *query.Queries }

func (s *DeploymentStore) GetByID(ctx context.Context, id platform.DeploymentID) (platform.Deployment, bool, error) {
	row, err := s.q.GetPlatformDeploymentByID(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.Deployment{}, false, nil
	}
	if err != nil {
		return platform.Deployment{}, false, err
	}
	dep, err := deploymentRow(row)
	return dep, err == nil, err
}

func (s *DeploymentStore) GetByOperation(ctx context.Context, id contract.OperationID) (platform.Deployment, bool, error) {
	row, err := s.q.GetPlatformDeploymentByOperation(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.Deployment{}, false, nil
	}
	if err != nil {
		return platform.Deployment{}, false, err
	}
	dep, err := deploymentRow(query.GetPlatformDeploymentByIDRow(row))
	return dep, err == nil, err
}

func (s *DeploymentStore) List(ctx context.Context, filter DeploymentFilter) ([]platform.Deployment, error) {
	if filter.Limit < 1 {
		return nil, errors.New("entities: deployment list limit must be positive")
	}
	rows, err := s.q.ListPlatformDeployments(ctx, query.ListPlatformDeploymentsParams{
		App: platformOptionalText(filter.App), Environment: platformOptionalText(filter.Environment),
		State: platformOptionalText(filter.State), DeploymentID: platformOptionalText(string(filter.DeploymentID)),
		MaxRows: int64(filter.Limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]platform.Deployment, 0, len(rows))
	for _, row := range rows {
		dep, err := deploymentRow(query.GetPlatformDeploymentByIDRow(row))
		if err != nil {
			return nil, err
		}
		result = append(result, dep)
	}
	return result, nil
}

func (s *DeploymentStore) Insert(ctx context.Context, insert DeploymentInsert) (bool, error) {
	secret := insert.SecretRevisions
	if len(secret) == 0 {
		secret = json.RawMessage(`{}`)
	}
	affected, err := s.q.InsertPlatformDeployment(ctx, query.InsertPlatformDeploymentParams{
		ID: string(insert.ID), OperationID: string(insert.OperationID), AppID: string(insert.AppID),
		EnvironmentID: string(insert.EnvironmentID), Action: string(insert.Action), ReleaseID: string(insert.ReleaseID),
		PreviousReleaseID:    platformOptionalText(string(insert.PreviousReleaseID)),
		PreviousDeploymentID: platformOptionalText(string(insert.PreviousDeploymentID)),
		RevisionID:           platformOptionalText(string(insert.RevisionID)), SecretRevisions: secret,
	})
	return affected == 1, err
}

func (s *DeploymentStore) MarkRunningByOperation(ctx context.Context, operationID contract.OperationID) (int64, error) {
	return s.q.MarkPlatformDeploymentRunningByOperation(ctx, string(operationID))
}

func (s *DeploymentStore) MarkSucceededByOperation(ctx context.Context, operationID contract.OperationID, at time.Time, message string) (int64, error) {
	return s.q.MarkPlatformDeploymentSucceededByOperation(ctx, query.MarkPlatformDeploymentSucceededByOperationParams{
		OperationID: string(operationID), At: pgtype.Timestamptz{Time: at, Valid: true}, Message: message,
	})
}

func (s *DeploymentStore) MarkFailedByOperation(ctx context.Context, operationID contract.OperationID, code contract.Code, message string) (int64, error) {
	return s.q.MarkPlatformDeploymentFailedByOperation(ctx, query.MarkPlatformDeploymentFailedByOperationParams{
		OperationID: string(operationID), ErrorCode: string(code), Message: message,
	})
}

func (s *DeploymentStore) MarkCancelledByOperation(ctx context.Context, operationID contract.OperationID, message string) (int64, error) {
	return s.q.MarkPlatformDeploymentCancelledByOperation(ctx, query.MarkPlatformDeploymentCancelledByOperationParams{
		OperationID: string(operationID), Message: message,
	})
}

func (s *DeploymentStore) SetInstances(ctx context.Context, id platform.DeploymentID, instances json.RawMessage, at time.Time) (int64, error) {
	return s.q.SetPlatformDeploymentInstances(ctx, query.SetPlatformDeploymentInstancesParams{
		ID: string(id), Instances: instances, At: pgtype.Timestamptz{Time: at, Valid: true},
	})
}

func (s *DeploymentStore) SetInstancesByOperation(ctx context.Context, operationID contract.OperationID, instances json.RawMessage) (int64, error) {
	return s.q.SetPlatformDeploymentInstancesByOperation(ctx, query.SetPlatformDeploymentInstancesByOperationParams{
		OperationID: string(operationID), Instances: instances,
	})
}

func (s *DeploymentStore) Predecessor(ctx context.Context, operationID contract.OperationID) (DeploymentPredecessor, bool, error) {
	row, err := s.q.GetPlatformDeploymentPredecessor(ctx, string(operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DeploymentPredecessor{}, false, nil
	}
	return DeploymentPredecessor{ReleaseID: platform.ReleaseID(row.PreviousReleaseID), DeploymentID: platform.DeploymentID(row.PreviousDeploymentID)}, err == nil, err
}

func (s *DeploymentStore) BindPredecessor(ctx context.Context, id platform.DeploymentID, releaseID platform.ReleaseID, deploymentID platform.DeploymentID) (int64, error) {
	return s.q.BindPlatformDeploymentPredecessor(ctx, query.BindPlatformDeploymentPredecessorParams{
		ID: string(id), PreviousReleaseID: platformOptionalText(string(releaseID)),
		PreviousDeploymentID: platformOptionalText(string(deploymentID)),
	})
}

func (s *DeploymentStore) SetRevisionAndSecretSnapshot(ctx context.Context, operationID contract.OperationID, revisionID platform.RevisionID, secret json.RawMessage) (int64, error) {
	if len(secret) == 0 {
		secret = json.RawMessage(`{}`)
	}
	return s.q.SetPlatformDeploymentRevisionAndSecretSnapshot(ctx, query.SetPlatformDeploymentRevisionAndSecretSnapshotParams{
		OperationID: string(operationID), RevisionID: platformOptionalText(string(revisionID)), SecretRevisions: secret,
	})
}

func (s *DeploymentStore) PinCredentialSecret(ctx context.Context, operationID contract.OperationID, name string, ref json.RawMessage) (int64, error) {
	return s.q.PinPlatformDeploymentCredentialSecret(ctx, query.PinPlatformDeploymentCredentialSecretParams{
		OperationID: string(operationID), Name: name, Ref: ref,
	})
}

func (s *DeploymentStore) RollbackSnapshot(ctx context.Context, environmentID platform.EnvironmentID, releaseID platform.ReleaseID) (platform.RevisionID, json.RawMessage, bool, error) {
	row, err := s.q.GetPlatformRollbackSnapshot(ctx, query.GetPlatformRollbackSnapshotParams{
		EnvironmentID: string(environmentID), ReleaseID: string(releaseID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, false, nil
	}
	return platform.RevisionID(row.RevisionID), row.SecretRevisions, err == nil, err
}

func (s *DeploymentStore) PreviousReleaseOfServing(ctx context.Context, environmentID platform.EnvironmentID, releaseID platform.ReleaseID) (platform.ReleaseID, bool, error) {
	id, err := s.q.GetPlatformPreviousReleaseOfServing(ctx, query.GetPlatformPreviousReleaseOfServingParams{
		EnvironmentID: string(environmentID), ReleaseID: string(releaseID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return platform.ReleaseID(id), err == nil, err
}

func (s *DeploymentStore) LatestSucceededReleaseExcluding(ctx context.Context, environmentID platform.EnvironmentID, releaseID platform.ReleaseID) (platform.ReleaseID, bool, error) {
	id, err := s.q.GetPlatformLatestSucceededReleaseExcluding(ctx, query.GetPlatformLatestSucceededReleaseExcludingParams{
		EnvironmentID: string(environmentID), ReleaseID: platformOptionalText(string(releaseID)),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return platform.ReleaseID(id), err == nil, err
}

func (s *DeploymentStore) ActiveReleaseForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) (platform.ReleaseID, bool, error) {
	id, err := s.q.GetPlatformActiveDeploymentReleaseForEnvironment(ctx, string(environmentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return platform.ReleaseID(id), err == nil, err
}

func (s *DeploymentStore) ResourceDependencies(ctx context.Context, environmentID platform.EnvironmentID) ([]DeploymentResourceDependency, error) {
	rows, err := s.q.ListPlatformDeploymentResourceDependencies(ctx, string(environmentID))
	if err != nil {
		return nil, err
	}
	result := make([]DeploymentResourceDependency, 0, len(rows))
	for _, row := range rows {
		result = append(result, DeploymentResourceDependency{
			RevisionID: platform.RevisionID(row.ID), Resources: row.Resources, PreviousResources: row.Resources_2,
		})
	}
	return result, nil
}

func (s *DeploymentStore) ReleaseAndServing(ctx context.Context, operationID contract.OperationID) (platform.ReleaseID, platform.ReleaseID, bool, error) {
	row, err := s.q.GetPlatformDeploymentReleaseAndServing(ctx, string(operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	return platform.ReleaseID(row.ReleaseID), platform.ReleaseID(row.ServingReleaseID), err == nil, err
}

func (s *DeploymentStore) FailureFacts(ctx context.Context, operationID contract.OperationID) (DeploymentFailureFacts, bool, error) {
	row, err := s.q.GetPlatformDeploymentFailureFacts(ctx, string(operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DeploymentFailureFacts{}, false, nil
	}
	return DeploymentFailureFacts{
		Action: platform.DeploymentAction(row.Action), App: row.Name, Environment: row.Name_2,
		ReleaseID: platform.ReleaseID(row.ReleaseID), PreviousRelease: platform.ReleaseID(row.PreviousReleaseID),
		ServingRelease: platform.ReleaseID(row.ServingReleaseID),
	}, err == nil, err
}

func (s *DeploymentStore) ExecutionFacts(ctx context.Context, id platform.DeploymentID, operationID contract.OperationID) (DeploymentExecutionFacts, bool, error) {
	row, err := s.q.GetPlatformDeploymentExecutionFacts(ctx, query.GetPlatformDeploymentExecutionFactsParams{
		ID: string(id), OperationID: string(operationID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DeploymentExecutionFacts{}, false, nil
	}
	if err != nil {
		return DeploymentExecutionFacts{}, false, err
	}
	now, err := timestampAny(row.ClockTimestamp)
	if err != nil {
		return DeploymentExecutionFacts{}, false, err
	}
	updated, err := platformTime(row.UpdatedAt)
	if err != nil {
		return DeploymentExecutionFacts{}, false, err
	}
	return DeploymentExecutionFacts{
		PreviousReleaseID: platform.ReleaseID(row.PreviousReleaseID), PreviousDeploymentID: platform.DeploymentID(row.PreviousDeploymentID),
		Now: now, EnvironmentUpdatedAt: updated, ServingDeploymentID: platform.DeploymentID(row.ServingDeploymentID),
		ServingReleaseID: platform.ReleaseID(row.ServingReleaseID), DesiredReleaseID: platform.ReleaseID(row.DesiredReleaseID),
		AppliedReleaseID: platform.ReleaseID(row.AppliedReleaseID), HealthState: platform.HealthState(row.HealthState),
		HealthCheckedAt: platformTimePtr(row.HealthCheckedAt), OwnerOperationID: contract.OperationID(row.OperationID),
		OwnerDeploymentID: platform.DeploymentID(row.DeploymentID), OwnerPhase: row.Phase,
		RouteTarget: platform.DeploymentID(row.RouteTargetDeploymentID), CleanupCount: row.Count, PreviousCleanupCount: row.Count_2,
	}, true, nil
}

func (s *DeploymentStore) RetentionRows(ctx context.Context, node string) ([]DeploymentRetentionRow, error) {
	rows, err := s.q.ListPlatformDeploymentRetentionRows(ctx, platformOptionalText(node))
	if err != nil {
		return nil, err
	}
	result := make([]DeploymentRetentionRow, 0, len(rows))
	for _, row := range rows {
		created, err := platformTime(row.CreatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, DeploymentRetentionRow{
			ID: platform.DeploymentID(row.ID), App: row.Name, Environment: row.Name_2,
			ReleaseID: platform.ReleaseID(row.ReleaseID), PreviousReleaseID: platform.ReleaseID(row.PreviousReleaseID),
			State: platform.DeploymentState(row.State), CreatedAt: created, CompletedAt: platformTimePtr(row.CompletedAt),
		})
	}
	return result, nil
}

func (s *DeploymentStore) CountActiveForRelease(ctx context.Context, releaseID platform.ReleaseID) (int64, error) {
	return s.q.CountPlatformActiveDeploymentsForRelease(ctx, string(releaseID))
}

func (s *DeploymentStore) WorkloadNodes(ctx context.Context, environmentID platform.EnvironmentID) ([]string, error) {
	return s.q.ListPlatformDeploymentWorkloadNodes(ctx, string(environmentID))
}

func (s *DeploymentStore) RepairTargets(ctx context.Context) ([]RepairTarget, error) {
	rows, err := s.q.ListPlatformRepairTargets(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]RepairTarget, 0, len(rows))
	for _, row := range rows {
		result = append(result, RepairTarget{
			App: row.Name, Environment: row.Name_2, ServingDeployment: platform.DeploymentID(row.ServingDeploymentID),
			Instances: row.Instances, Node: row.NodeName,
		})
	}
	return result, nil
}

func (s *DeploymentStore) DeleteForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) (int64, error) {
	return s.q.DeletePlatformDeploymentsForEnvironment(ctx, string(environmentID))
}

func (s *DeploymentStore) BuildDeploymentFacts(ctx context.Context, operationID contract.OperationID) (BuildDeploymentFacts, bool, error) {
	row, err := s.q.GetPlatformBuildDeploymentFacts(ctx, string(operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return BuildDeploymentFacts{}, false, nil
	}
	return BuildDeploymentFacts{
		BuildID: platform.BuildID(row.ID), BuildState: platform.BuildState(row.State), CommitSHA: row.CommitSha,
		DeploymentID: platform.DeploymentID(row.ID_2), DeployState: platform.DeploymentState(row.State_2),
		RevisionID: platform.RevisionID(row.RevisionID), ReleaseID: platform.ReleaseID(row.ReleaseID),
		Image: row.Image, ImageID: row.ImageID,
	}, err == nil, err
}

func (s *DeploymentStore) ResultIdentity(ctx context.Context, operationID contract.OperationID) (ResultIdentity, error) {
	row, err := s.q.GetPlatformResultIdentity(ctx, string(operationID))
	if err != nil {
		return ResultIdentity{}, err
	}
	return ResultIdentity{
		BuildID: platform.BuildID(coalesceString(row.Coalesce)), DeploymentID: platform.DeploymentID(coalesceString(row.Coalesce_2)),
		RevisionID: platform.RevisionID(coalesceString(row.Coalesce_3)), ReleaseID: platform.ReleaseID(coalesceString(row.Coalesce_4)),
		Image: coalesceString(row.Coalesce_5), ImageID: coalesceString(row.Coalesce_6), CommitSHA: coalesceString(row.Coalesce_7),
	}, nil
}

func deploymentRow(row query.GetPlatformDeploymentByIDRow) (platform.Deployment, error) {
	created, err := platformTime(row.CreatedAt)
	if err != nil {
		return platform.Deployment{}, err
	}
	updated, err := platformTime(row.UpdatedAt)
	if err != nil {
		return platform.Deployment{}, err
	}
	secret, err := platformJSON[map[string]platform.SecretRevisionRef](row.SecretRevisions)
	if err != nil {
		return platform.Deployment{}, err
	}
	instances, err := platformJSON[[]platform.DeploymentInstance](row.Instances)
	if err != nil {
		return platform.Deployment{}, err
	}
	if secret == nil {
		return platform.Deployment{}, ErrPlatformPointerIntegrity
	}
	for name, ref := range secret {
		if vault.ValidateName(name) != nil || !vault.ValidRevisionIdentity(ref.Revision, ref.Hash, ref.KeyID) {
			return platform.Deployment{}, ErrPlatformPointerIntegrity
		}
	}
	if instances == nil {
		instances = []platform.DeploymentInstance{}
	}
	return platform.Deployment{
		ID: platform.DeploymentID(row.ID), OperationID: contract.OperationID(row.OperationID),
		App: row.Name, Environment: row.Name_2, Action: platform.DeploymentAction(row.Action),
		ReleaseID: platform.ReleaseID(row.ReleaseID), PreviousReleaseID: platform.ReleaseID(row.PreviousReleaseID),
		PreviousDeploymentID: platform.DeploymentID(row.PreviousDeploymentID), RevisionID: platform.RevisionID(row.RevisionID),
		SecretRevisions: secret, Instances: instances, State: platform.DeploymentState(row.State),
		ErrorCode: contract.Code(row.ErrorCode), Message: row.Message, CreatedAt: created, UpdatedAt: updated,
		CompletedAt: platformTimePtr(row.CompletedAt),
	}, nil
}
