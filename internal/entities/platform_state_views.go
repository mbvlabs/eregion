package entities

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
	"github.com/mbvlabs/eregion/internal/platform"
)

type AppStore struct{ q *query.Queries }

func (s *AppStore) GetByName(ctx context.Context, name string) (platform.App, bool, error) {
	row, err := s.q.GetPlatformAppByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.App{}, false, nil
	}
	if err != nil {
		return platform.App{}, false, err
	}
	app, err := appRow(row.ID, row.Name, row.ImageRepository, row.CreatedAt, row.UpdatedAt)
	return app, err == nil, err
}

func (s *AppStore) GetByNameForUpdate(ctx context.Context, name string) (platform.App, bool, error) {
	row, err := s.q.GetPlatformAppByNameForUpdate(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.App{}, false, nil
	}
	if err != nil {
		return platform.App{}, false, err
	}
	app, err := appRow(row.ID, row.Name, row.ImageRepository, row.CreatedAt, row.UpdatedAt)
	return app, err == nil, err
}

func (s *AppStore) IDByName(ctx context.Context, name string) (platform.AppID, bool, error) {
	id, err := s.q.GetPlatformAppIDByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return platform.AppID(id), true, nil
}

func (s *AppStore) NameByID(ctx context.Context, id platform.AppID) (string, bool, error) {
	name, err := s.q.GetPlatformAppNameByID(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return name, err == nil, err
}

func (s *AppStore) Exists(ctx context.Context, name string) (bool, error) {
	return s.q.PlatformAppExists(ctx, name)
}

func (s *AppStore) MissingByID(ctx context.Context, id platform.AppID) (bool, error) {
	return s.q.PlatformAppMissingByID(ctx, string(id))
}

func (s *AppStore) Repository(ctx context.Context, name string) (AppRepository, bool, error) {
	row, err := s.q.GetPlatformAppRepository(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return AppRepository{}, false, nil
	}
	if err != nil {
		return AppRepository{}, false, err
	}
	return AppRepository{ID: platform.AppID(row.ID), ImageRepository: row.ImageRepository}, true, nil
}

func (s *AppStore) UploadIdentity(ctx context.Context, name string) (AppUploadIdentity, bool, error) {
	row, err := s.q.GetPlatformAppUploadIdentity(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return AppUploadIdentity{}, false, nil
	}
	if err != nil {
		return AppUploadIdentity{}, false, err
	}
	return AppUploadIdentity{ID: platform.AppID(row.ID), Name: row.Name, ImageRepository: row.ImageRepository}, true, nil
}

func (s *AppStore) ActionTimestamps(ctx context.Context, name string) (AppActionTimestamps, bool, error) {
	row, err := s.q.GetPlatformAppActionTimestamps(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return AppActionTimestamps{}, false, nil
	}
	if err != nil {
		return AppActionTimestamps{}, false, err
	}
	now, err := timestampAny(row.Now)
	if err != nil {
		return AppActionTimestamps{}, false, err
	}
	updated, err := platformTime(row.UpdatedAt)
	if err != nil {
		return AppActionTimestamps{}, false, err
	}
	return AppActionTimestamps{ID: platform.AppID(row.ID), Now: now, UpdatedAt: updated}, true, nil
}

func (s *AppStore) Insert(ctx context.Context, insert AppInsert) (bool, error) {
	affected, err := s.q.InsertPlatformApp(ctx, query.InsertPlatformAppParams{
		ID: string(insert.ID), Name: insert.Name, ImageRepository: insert.ImageRepository,
	})
	return affected == 1, err
}

func (s *AppStore) List(ctx context.Context, filter AppFilter) ([]platform.App, error) {
	if filter.Limit < 1 {
		return nil, errors.New("entities: app list limit must be positive")
	}
	rows, err := s.q.ListPlatformApps(ctx, query.ListPlatformAppsParams{
		Name: platformOptionalText(filter.Name), MaxRows: int64(filter.Limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]platform.App, 0, len(rows))
	for _, row := range rows {
		app, err := appRow(row.ID, row.Name, row.ImageRepository, row.CreatedAt, row.UpdatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, app)
	}
	return result, nil
}

func (s *AppStore) Delete(ctx context.Context, id platform.AppID) (int64, error) {
	return s.q.DeletePlatformApp(ctx, string(id))
}

func appRow(id, name, repository string, created, updated pgtype.Timestamptz) (platform.App, error) {
	createdAt, err := platformTime(created)
	if err != nil {
		return platform.App{}, err
	}
	updatedAt, err := platformTime(updated)
	if err != nil {
		return platform.App{}, err
	}
	return platform.App{
		ID: platform.AppID(id), Name: name, ImageRepository: repository,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	}, nil
}

type EnvironmentStore struct{ q *query.Queries }

func (s *EnvironmentStore) GetByName(ctx context.Context, app, name string) (EnvironmentObservation, bool, error) {
	row, err := s.q.GetPlatformEnvironmentByName(ctx, query.GetPlatformEnvironmentByNameParams{App: app, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvironmentObservation{}, false, nil
	}
	if err != nil {
		return EnvironmentObservation{}, false, err
	}
	env, err := environmentRow(row)
	return env, err == nil, err
}

func (s *EnvironmentStore) GetByID(ctx context.Context, id platform.EnvironmentID) (EnvironmentObservation, bool, error) {
	row, err := s.q.GetPlatformEnvironmentByID(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvironmentObservation{}, false, nil
	}
	if err != nil {
		return EnvironmentObservation{}, false, err
	}
	env, err := environmentRow(query.GetPlatformEnvironmentByNameRow(row))
	return env, err == nil, err
}

func (s *EnvironmentStore) List(ctx context.Context, filter EnvironmentFilter) ([]EnvironmentObservation, error) {
	if filter.Limit < 1 {
		return nil, errors.New("entities: environment list limit must be positive")
	}
	rows, err := s.q.ListPlatformEnvironments(ctx, query.ListPlatformEnvironmentsParams{
		App: platformOptionalText(filter.App), Environment: platformOptionalText(filter.Environment),
		Node: platformOptionalText(filter.Node), State: platformOptionalText(filter.State),
		MaxRows: int64(filter.Limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]EnvironmentObservation, 0, len(rows))
	for _, row := range rows {
		env, err := environmentRow(query.GetPlatformEnvironmentByNameRow(row))
		if err != nil {
			return nil, err
		}
		result = append(result, env)
	}
	return result, nil
}

func (s *EnvironmentStore) ResolveID(ctx context.Context, app, name string) (EnvironmentIdentity, bool, error) {
	row, err := s.q.ResolvePlatformEnvironmentID(ctx, query.ResolvePlatformEnvironmentIDParams{App: app, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvironmentIdentity{}, false, nil
	}
	if err != nil {
		return EnvironmentIdentity{}, false, err
	}
	return EnvironmentIdentity{ID: platform.EnvironmentID(row.ID), AppID: platform.AppID(row.AppID), App: app, Name: name}, true, nil
}

func (s *EnvironmentStore) AppIDByID(ctx context.Context, id platform.EnvironmentID) (platform.AppID, bool, error) {
	appID, err := s.q.GetPlatformEnvironmentAppID(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return platform.AppID(appID), err == nil, err
}

func (s *EnvironmentStore) IDByAppName(ctx context.Context, appID platform.AppID, name string) (platform.EnvironmentID, bool, error) {
	id, err := s.q.GetPlatformEnvironmentIDByAppName(ctx, query.GetPlatformEnvironmentIDByAppNameParams{AppID: string(appID), Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return platform.EnvironmentID(id), err == nil, err
}

func (s *EnvironmentStore) LockIDForAppName(ctx context.Context, appID platform.AppID, name string) (platform.EnvironmentID, bool, error) {
	id, err := s.q.LockPlatformEnvironmentIDForAppName(ctx, query.LockPlatformEnvironmentIDForAppNameParams{AppID: string(appID), Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return platform.EnvironmentID(id), err == nil, err
}

func (s *EnvironmentStore) LockByIDAppName(ctx context.Context, id platform.EnvironmentID, appID platform.AppID, name string) (bool, error) {
	locked, err := s.q.LockPlatformEnvironmentByIDAppName(ctx, query.LockPlatformEnvironmentByIDAppNameParams{
		ID: string(id), AppID: string(appID), Name: name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return locked == string(id), err
}

func (s *EnvironmentStore) LockByIDApp(ctx context.Context, id platform.EnvironmentID, appID platform.AppID) (bool, error) {
	locked, err := s.q.LockPlatformEnvironmentByIDApp(ctx, query.LockPlatformEnvironmentByIDAppParams{ID: string(id), AppID: string(appID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return locked == string(id), err
}

func (s *EnvironmentStore) LockTargetsForApp(ctx context.Context, app string) ([]platform.EnvironmentID, error) {
	rows, err := s.q.LockPlatformEnvironmentTargetsForApp(ctx, app)
	if err != nil {
		return nil, err
	}
	result := make([]platform.EnvironmentID, 0, len(rows))
	for _, id := range rows {
		result = append(result, platform.EnvironmentID(id))
	}
	return result, nil
}

func (s *EnvironmentStore) Exists(ctx context.Context, app, name string) (bool, error) {
	return s.q.PlatformEnvironmentExists(ctx, query.PlatformEnvironmentExistsParams{App: app, Name: name})
}

func (s *EnvironmentStore) ExistsForApp(ctx context.Context, appID platform.AppID, id platform.EnvironmentID) (bool, error) {
	return s.q.PlatformEnvironmentExistsForApp(ctx, query.PlatformEnvironmentExistsForAppParams{AppID: string(appID), ID: string(id)})
}

func (s *EnvironmentStore) CountForApp(ctx context.Context, appID platform.AppID) (int64, error) {
	return s.q.CountPlatformEnvironmentsForApp(ctx, string(appID))
}

func (s *EnvironmentStore) CurrentRevisionID(ctx context.Context, app, name string) (platform.RevisionID, bool, error) {
	id, err := s.q.GetPlatformEnvironmentCurrentRevision(ctx, query.GetPlatformEnvironmentCurrentRevisionParams{App: app, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return platform.RevisionID(id), err == nil, err
}

func (s *EnvironmentStore) RevisionPointers(ctx context.Context, app, name string) (EnvironmentRevisionPointers, bool, error) {
	row, err := s.q.GetPlatformEnvironmentRevisionPointers(ctx, query.GetPlatformEnvironmentRevisionPointersParams{App: app, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvironmentRevisionPointers{}, false, nil
	}
	if err != nil {
		return EnvironmentRevisionPointers{}, false, err
	}
	return EnvironmentRevisionPointers{
		Current: platform.RevisionID(row.CurrentRevisionID), Serving: platform.RevisionID(row.RevisionID),
	}, true, nil
}

func (s *EnvironmentStore) ListTargets(ctx context.Context, app string) ([]string, error) {
	return s.q.ListPlatformEnvironmentTargets(ctx, app)
}

func (s *EnvironmentStore) RetentionRows(ctx context.Context, node string) ([]EnvironmentRetentionRow, error) {
	rows, err := s.q.ListPlatformEnvironmentRetentionRows(ctx, platformOptionalText(node))
	if err != nil {
		return nil, err
	}
	result := make([]EnvironmentRetentionRow, 0, len(rows))
	for _, row := range rows {
		result = append(result, EnvironmentRetentionRow{
			ID: platform.EnvironmentID(row.ID), App: row.Name, Name: row.Name_2, Node: row.NodeName,
			ServingRelease: platform.ReleaseID(row.ServingReleaseID),
			ServingDeploy:  platform.DeploymentID(row.ServingDeploymentID),
			Retention:      row.Retention,
		})
	}
	return result, nil
}

func (s *EnvironmentStore) RetentionOverride(ctx context.Context, app, name string) (json.RawMessage, bool, error) {
	raw, err := s.q.GetPlatformEnvironmentRetentionOverride(ctx, query.GetPlatformEnvironmentRetentionOverrideParams{App: app, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return raw, err == nil, err
}

func (s *EnvironmentStore) Insert(ctx context.Context, insert EnvironmentInsert) (bool, error) {
	envJSON, err := json.Marshal(insert.Env)
	if err != nil {
		return false, err
	}
	refsJSON, err := json.Marshal(insert.SecretRefs)
	if err != nil {
		return false, err
	}
	affected, err := s.q.InsertPlatformEnvironment(ctx, query.InsertPlatformEnvironmentParams{
		ID: string(insert.ID), AppID: string(insert.AppID), Name: insert.Name, NodeName: insert.Node,
		BuildNode: platformOptionalText(insert.BuildNode), HostPort: int64(insert.HostPort),
		CandidatePort: int64(insert.CandidatePort), ContainerPort: int64(insert.ContainerPort),
		BindAddress: insert.Bind, HealthType: insert.HealthType, HealthPath: insert.HealthPath,
		HealthTimeoutSeconds: int64(insert.HealthTimeoutSeconds), Env: envJSON, SecretRefs: refsJSON,
		Protected: insert.Protected,
	})
	return affected == 1, err
}

func (s *EnvironmentStore) SetCurrentRevision(ctx context.Context, id platform.EnvironmentID, revisionID platform.RevisionID) (int64, error) {
	return s.q.SetPlatformEnvironmentCurrentRevision(ctx, query.SetPlatformEnvironmentCurrentRevisionParams{
		ID: string(id), RevisionID: string(revisionID),
	})
}

func (s *EnvironmentStore) PublishMirror(ctx context.Context, mirror EnvironmentMirror) (int64, error) {
	envJSON, err := json.Marshal(mirror.Env)
	if err != nil {
		return 0, err
	}
	refsJSON, err := json.Marshal(mirror.SecretRefs)
	if err != nil {
		return 0, err
	}
	return s.q.PublishPlatformEnvironmentMirror(ctx, query.PublishPlatformEnvironmentMirrorParams{
		NodeName: mirror.Node, BuildNode: platformOptionalText(mirror.BuildNode),
		ContainerPort: int64(mirror.ContainerPort), HealthType: mirror.HealthType,
		HealthPath: mirror.HealthPath, HealthTimeoutSeconds: int64(mirror.HealthTimeoutSeconds),
		Env: envJSON, SecretRefs: refsJSON, RevisionID: string(mirror.RevisionID), ID: string(mirror.ID),
	})
}

func (s *EnvironmentStore) SetProtected(ctx context.Context, id platform.EnvironmentID, protected bool) (int64, error) {
	return s.q.SetPlatformEnvironmentProtected(ctx, query.SetPlatformEnvironmentProtectedParams{ID: string(id), Protected: protected})
}

func (s *EnvironmentStore) SetDesiredRelease(ctx context.Context, id platform.EnvironmentID, releaseID platform.ReleaseID) (int64, error) {
	return s.q.SetPlatformEnvironmentDesiredRelease(ctx, query.SetPlatformEnvironmentDesiredReleaseParams{
		ID: string(id), ReleaseID: string(releaseID),
	})
}

func (s *EnvironmentStore) SetServing(ctx context.Context, serving EnvironmentServing) (int64, error) {
	return s.q.SetPlatformEnvironmentServing(ctx, query.SetPlatformEnvironmentServingParams{
		ID: string(serving.ID), ReleaseID: string(serving.ReleaseID), DeploymentID: string(serving.DeploymentID),
		HealthState: string(serving.HealthState), HealthDetail: serving.HealthDetail,
		CheckedAt: pgtype.Timestamptz{Time: serving.CheckedAt, Valid: true},
	})
}

func (s *EnvironmentStore) ClearPointers(ctx context.Context, id platform.EnvironmentID) (int64, error) {
	return s.q.ClearPlatformEnvironmentPointers(ctx, string(id))
}

func (s *EnvironmentStore) Delete(ctx context.Context, id platform.EnvironmentID) (int64, error) {
	return s.q.DeletePlatformEnvironment(ctx, string(id))
}

func (s *EnvironmentStore) LockPortAllocation(ctx context.Context, key int64) error {
	return s.q.LockPlatformPortAllocation(ctx, key)
}

func (s *EnvironmentStore) PortInUse(ctx context.Context, port int) (bool, error) {
	return s.q.PlatformPortInUse(ctx, int64(port))
}

func (s *EnvironmentStore) NextHostPort(ctx context.Context) (int, bool, error) {
	port, err := s.q.NextPlatformHostPort(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	value, err := platformInt(port)
	return value, err == nil, err
}

func (s *EnvironmentStore) NextCandidatePort(ctx context.Context, host int) (int, bool, error) {
	port, err := s.q.NextPlatformCandidatePort(ctx, int64(host))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	value, err := platformInt(port)
	return value, err == nil, err
}

func (s *EnvironmentStore) ReferencedByAnother(ctx context.Context, id platform.EnvironmentID) (bool, error) {
	return s.q.PlatformEnvironmentReferencedByAnother(ctx, string(id))
}

func environmentRow(row query.GetPlatformEnvironmentByNameRow) (EnvironmentObservation, error) {
	if !row.Column51 || row.CurrentRevisionID == "" || row.CurrentRevisionID != row.ID_2 {
		return EnvironmentObservation{}, ErrPlatformPointerIntegrity
	}
	host, err := platformInt(row.HostPort)
	if err != nil {
		return EnvironmentObservation{}, err
	}
	candidate, err := platformInt(row.CandidatePort)
	if err != nil {
		return EnvironmentObservation{}, err
	}
	container, err := platformInt(row.ContainerPort)
	if err != nil {
		return EnvironmentObservation{}, err
	}
	timeout, err := platformInt(row.HealthTimeoutSeconds)
	if err != nil {
		return EnvironmentObservation{}, err
	}
	envMap, err := platformJSONMap(row.Env)
	if err != nil {
		return EnvironmentObservation{}, err
	}
	refs, err := platformJSONMap(row.SecretRefs)
	if err != nil {
		return EnvironmentObservation{}, err
	}
	checked := platformTimePtr(row.HealthCheckedAt)
	created, err := platformTime(row.CreatedAt_2)
	if err != nil {
		return EnvironmentObservation{}, err
	}
	updated, err := platformTime(row.UpdatedAt)
	if err != nil {
		return EnvironmentObservation{}, err
	}
	env := platform.Environment{
		ID: platform.EnvironmentID(row.ID), App: row.Name, Name: row.Name_2,
		ImageRepository: row.ImageRepository, Node: row.NodeName, Protected: row.Protected,
		BuildNode: row.BuildNode, HostPort: host, CandidatePort: candidate, ContainerPort: container,
		Bind: row.BindAddress, Health: platform.HealthConfig{Type: row.HealthType, Path: row.HealthPath, TimeoutSeconds: timeout},
		Env: envMap, SecretRefs: refs, SecretRevision: row.SecretRevision,
		DesiredRelease: platform.ReleaseID(row.DesiredReleaseID), AppliedRelease: platform.ReleaseID(row.AppliedReleaseID),
		ServingRelease:      platform.ReleaseID(row.ServingReleaseID),
		ServingDeploymentID: platform.DeploymentID(row.ServingDeploymentID),
		HealthState:         platform.HealthState(row.HealthState), HealthDetail: row.HealthDetail, HealthCheckedAt: checked,
		Domains: row.Column25, PrimaryDomain: row.Column26, PendingCleanup: row.Column28,
		ArtifactReady: row.Exists, RevisionID: platform.RevisionID(row.CurrentRevisionID),
		CreatedAt: created, UpdatedAt: updated,
	}
	if row.ID_2 != "" {
		revision, err := revisionFields(row.ID_2, row.Name, row.Name_2, row.Number, row.OperationID,
			row.ActorKind, row.ActorSubject, row.Operation, row.Summary, row.Source, row.AutoDeploy,
			row.WebhookSecretName, row.Runtime, row.Processes, row.Config, row.SecretNames, row.Resources,
			row.Retention, row.DeployIntent, row.CreatedAt)
		if err != nil {
			return EnvironmentObservation{}, err
		}
		env.Revision = &revision
		env.Processes = revision.Processes
	}
	if env.Processes == nil {
		env.Processes = []platform.ProcessSpec{}
	}
	// Deployability is intentionally not evaluated here. This capability maps
	// raw persisted facts; the application/cutover boundary evaluates the
	// platform policy after it appends resource observations. PrimaryDNS is the
	// exact selected primary-domain DNS fact from this same row.
	return EnvironmentObservation{Environment: env, PrimaryDNS: platform.DNSState(row.Column27)}, nil
}

type RevisionStore struct{ q *query.Queries }

func (s *RevisionStore) GetByID(ctx context.Context, id platform.RevisionID) (platform.EnvironmentRevision, bool, error) {
	row, err := s.q.GetPlatformRevisionByID(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.EnvironmentRevision{}, false, nil
	}
	if err != nil {
		return platform.EnvironmentRevision{}, false, err
	}
	revision, err := revisionRow(row)
	return revision, err == nil, err
}

func (s *RevisionStore) List(ctx context.Context, filter RevisionFilter) ([]platform.EnvironmentRevision, error) {
	if filter.Limit < 1 {
		return nil, errors.New("entities: revision list limit must be positive")
	}
	rows, err := s.q.ListPlatformRevisions(ctx, query.ListPlatformRevisionsParams{
		App: platformOptionalText(filter.App), Environment: platformOptionalText(filter.Environment),
		State: platformOptionalText(filter.State), MaxRows: int64(filter.Limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]platform.EnvironmentRevision, 0, len(rows))
	for _, row := range rows {
		revision, err := revisionRow(query.GetPlatformRevisionByIDRow(row))
		if err != nil {
			return nil, err
		}
		result = append(result, revision)
	}
	return result, nil
}

func (s *RevisionStore) NextNumber(ctx context.Context, environmentID platform.EnvironmentID) (int, error) {
	number, err := s.q.NextPlatformRevisionNumber(ctx, string(environmentID))
	if err != nil {
		return 0, err
	}
	return platformInt(int64(number))
}

func (s *RevisionStore) Insert(ctx context.Context, insert RevisionInsert) (bool, error) {
	source, _ := json.Marshal(insert.State.Source)
	runtime, _ := json.Marshal(insert.State.Runtime)
	processes, _ := json.Marshal(insert.State.Processes)
	config, _ := json.Marshal(insert.State.Config)
	retention, _ := json.Marshal(insert.State.Retention)
	number := insert.Number
	affected, err := s.q.InsertPlatformRevision(ctx, query.InsertPlatformRevisionParams{
		ID: string(insert.ID), EnvironmentID: string(insert.EnvironmentID), Number: int64(number),
		OperationID: platformOptionalText(string(insert.OperationID)), ActorKind: insert.Actor.Kind,
		ActorSubject: insert.Actor.Subject, Operation: insert.Operation, Summary: insert.Summary,
		Source: source, AutoDeploy: insert.State.AutoDeploy, WebhookSecretName: insert.State.WebhookSecretName,
		Runtime: runtime, Processes: processes, Config: config, SecretNames: insert.State.SecretNames,
		Resources: insert.State.Resources, DeployIntent: insert.State.DeployIntent, Retention: retention,
	})
	return affected == 1, err
}

func (s *RevisionStore) Number(ctx context.Context, id platform.RevisionID) (int, bool, error) {
	number, err := s.q.GetPlatformRevisionNumber(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	value, err := platformInt(number)
	return value, err == nil, err
}

func (s *RevisionStore) ExistsForOperation(ctx context.Context, id contract.OperationID) (bool, error) {
	return s.q.PlatformRevisionExistsForOperation(ctx, string(id))
}

func (s *RevisionStore) UsesResource(ctx context.Context, id platform.RevisionID, resource string) (bool, error) {
	return s.q.PlatformRevisionUsesResource(ctx, query.PlatformRevisionUsesResourceParams{ID: string(id), Resource: resource})
}

func (s *RevisionStore) Runtime(ctx context.Context, id platform.RevisionID) (json.RawMessage, bool, error) {
	raw, err := s.q.GetPlatformRevisionRuntime(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return raw, err == nil, err
}

func (s *RevisionStore) FirstDeployOperation(ctx context.Context, operationID contract.OperationID, kind string, actor contract.Actor) (contract.OperationID, bool, error) {
	id, err := s.q.GetPlatformFirstDeployOperation(ctx, query.GetPlatformFirstDeployOperationParams{
		OperationID: string(operationID), Kind: kind, ActorKind: actor.Kind, ActorSubject: actor.Subject,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return contract.OperationID(id), err == nil, err
}

func (s *RevisionStore) FirstDeployActionRows(ctx context.Context, operationID contract.OperationID) ([]FirstDeployActionRow, error) {
	rows, err := s.q.GetPlatformFirstDeployActionRows(ctx, string(operationID))
	if err != nil {
		return nil, err
	}
	result := make([]FirstDeployActionRow, 0, len(rows))
	for _, row := range rows {
		processes, err := platformJSON[[]platform.ProcessSpec](row.Processes)
		if err != nil {
			return nil, err
		}
		config, err := platformJSONMap(row.Config)
		if err != nil {
			return nil, err
		}
		previous, err := platformJSONMap(row.Config_2)
		if err != nil {
			return nil, err
		}
		number, err := platformInt(row.Number)
		if err != nil {
			return nil, err
		}
		result = append(result, FirstDeployActionRow{
			RevisionID: platform.RevisionID(row.ID), EnvironmentID: platform.EnvironmentID(row.EnvironmentID),
			Environment: row.Name, AppID: platform.AppID(row.ID_2), App: row.Name_2, Number: number,
			DeployIntent: row.DeployIntent, DeployOpID: contract.OperationID(coalesceString(row.Coalesce)),
			Processes: processes, Config: config, PreviousConfig: previous,
			HasPrevious: row.Column12.Valid && row.Column12.Bool,
			SecretNames: row.SecretNames, Resources: row.Resources,
		})
	}
	return result, nil
}

func (s *RevisionStore) DeleteForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) (int64, error) {
	return s.q.DeletePlatformRevisionsForEnvironment(ctx, string(environmentID))
}

func revisionFields(id, app, environment string, number int64, operationID, actorKind, actorSubject, operation, summary string,
	source json.RawMessage, autoDeploy bool, webhookSecretName string, runtime, processes, config json.RawMessage,
	secretNames, resources []string, retention json.RawMessage, deployIntent bool, createdAt pgtype.Timestamptz,
) (platform.EnvironmentRevision, error) {
	revision := platform.EnvironmentRevision{
		ID: platform.RevisionID(id), App: app, Environment: environment,
		OperationID: contract.OperationID(operationID),
		Actor:       contract.Actor{Kind: actorKind, Subject: actorSubject}, Operation: operation, Summary: summary,
	}
	numberValue, err := platformInt(number)
	if err != nil {
		return revision, err
	}
	revision.Number = numberValue
	if err := json.Unmarshal(source, &revision.Source); err != nil {
		return revision, ErrPlatformCorrupt
	}
	revision.AutoDeploy = autoDeploy
	revision.WebhookSecretName = webhookSecretName
	if err := json.Unmarshal(runtime, &revision.Runtime); err != nil {
		return revision, ErrPlatformCorrupt
	}
	if err := json.Unmarshal(processes, &revision.Processes); err != nil {
		return revision, ErrPlatformCorrupt
	}
	revision.Config = map[string]string{}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &revision.Config); err != nil {
			return revision, ErrPlatformCorrupt
		}
	}
	revision.SecretNames = secretNames
	if revision.SecretNames == nil {
		revision.SecretNames = []string{}
	}
	revision.Resources = resources
	if revision.Resources == nil {
		revision.Resources = []string{}
	}
	if len(retention) > 0 {
		if err := json.Unmarshal(retention, &revision.Retention); err != nil {
			return revision, ErrPlatformCorrupt
		}
	}
	revision.DeployIntent = deployIntent
	created, err := platformTime(createdAt)
	if err != nil {
		return revision, err
	}
	revision.CreatedAt = created
	if revision.Processes == nil {
		revision.Processes = []platform.ProcessSpec{}
	}
	return revision, nil
}

func revisionRow(row query.GetPlatformRevisionByIDRow) (platform.EnvironmentRevision, error) {
	return revisionFields(row.ID, row.Name, row.Name_2, row.Number, row.OperationID, row.ActorKind,
		row.ActorSubject, row.Operation, row.Summary, row.Source, row.AutoDeploy, row.WebhookSecretName,
		row.Runtime, row.Processes, row.Config, row.SecretNames, row.Resources, row.Retention,
		row.DeployIntent, row.CreatedAt)
}

type ChangeStore struct{ q *query.Queries }

func (s *ChangeStore) NextSequenceForApp(ctx context.Context, appID platform.AppID) (uint64, error) {
	sequence, err := s.q.NextPlatformChangeSequenceForApp(ctx, string(appID))
	if err != nil {
		return 0, err
	}
	return platformUint64FromInt32(sequence)
}

func (s *ChangeStore) NextSequenceForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) (uint64, error) {
	sequence, err := s.q.NextPlatformChangeSequenceForEnvironment(ctx, string(environmentID))
	if err != nil {
		return 0, err
	}
	return platformUint64FromInt32(sequence)
}

func (s *ChangeStore) Insert(ctx context.Context, insert ChangeInsert) (bool, error) {
	sequence, err := platformInt64(insert.Sequence)
	if err != nil {
		return false, err
	}
	affected, err := s.q.InsertPlatformChange(ctx, query.InsertPlatformChangeParams{
		ID: string(insert.ID), AppID: string(insert.AppID), EnvironmentID: platformOptionalText(string(insert.EnvironmentID)),
		Sequence: sequence, Kind: insert.Kind, OperationID: string(insert.OperationID),
		ActorKind: insert.Actor.Kind, ActorSubject: insert.Actor.Subject, Summary: insert.Summary,
		Desired: nullableJSON(insert.Desired), Applied: nullableJSON(insert.Applied),
	})
	return affected == 1, err
}

func (s *ChangeStore) List(ctx context.Context, filter ChangeFilter) ([]platform.Change, error) {
	if filter.Limit < 1 {
		return nil, errors.New("entities: change list limit must be positive")
	}
	rows, err := s.q.ListPlatformChanges(ctx, query.ListPlatformChangesParams{
		App: platformOptionalText(filter.App), Environment: platformOptionalText(filter.Environment),
		State: platformOptionalText(filter.Kind), MaxRows: int64(filter.Limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]platform.Change, 0, len(rows))
	for _, row := range rows {
		sequence, err := platformUint64(row.Sequence)
		if err != nil {
			return nil, err
		}
		created, err := platformTime(row.CreatedAt)
		if err != nil {
			return nil, err
		}
		item := platform.Change{
			ID: platform.ChangeID(row.ID), App: row.Name, Environment: row.Name_2,
			Sequence: sequence, Kind: row.Kind, OperationID: contract.OperationID(row.OperationID),
			Actor: contract.Actor{Kind: row.ActorKind, Subject: row.ActorSubject}, Summary: row.Summary,
			CreatedAt: created,
		}
		if len(derefRaw(row.Desired)) > 0 {
			_ = json.Unmarshal(derefRaw(row.Desired), &item.Desired)
		}
		if len(derefRaw(row.Applied)) > 0 {
			_ = json.Unmarshal(derefRaw(row.Applied), &item.Applied)
		}
		result = append(result, item)
	}
	return result, nil
}

func (s *ChangeStore) DeleteForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) (int64, error) {
	return s.q.DeletePlatformChangesForEnvironment(ctx, string(environmentID))
}

type InventoryStore struct{ q *query.Queries }

func (s *InventoryStore) Summary(ctx context.Context) (map[string]int64, error) {
	rows, err := s.q.PlatformInventorySummary(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string]int64, len(rows))
	for _, row := range rows {
		result[row.Column1] = row.Count
	}
	return result, nil
}

func nullableJSON(raw json.RawMessage) *json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	value := raw
	return &value
}

func derefRaw(raw *json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return *raw
}

func coalesceString(value any) string {
	if typed, ok := value.(string); ok {
		return typed
	}
	return ""
}

func anyBool(value any) bool {
	if typed, ok := value.(bool); ok {
		return typed
	}
	return false
}

// HasForeignReferences reports whether another environment still points at any
// revision, release, build or deployment owned by this environment.
func (s *EnvironmentStore) HasForeignReferences(ctx context.Context, id platform.EnvironmentID) (bool, error) {
	return s.q.PlatformEnvironmentHasForeignReferences(ctx, string(id))
}
