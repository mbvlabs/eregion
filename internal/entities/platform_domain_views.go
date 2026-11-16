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
)

type DomainFilter struct {
	App         string
	Environment string
	Limit       int
}

type DomainInsert struct {
	ID            platform.DomainID
	Domain        string
	EnvironmentID platform.EnvironmentID
	Primary       bool
	Addresses     []string
	CheckPath     string
}

type DomainBindingRef struct {
	ID      platform.DomainID
	Primary bool
}

type DomainDNSLedger struct {
	DomainID          platform.DomainID
	Domain            string
	App               string
	Environment       string
	Provider          string
	ZoneID            string
	ZoneName          string
	OwnershipMarker   string
	Generation        int64
	DesiredOrigins    []string
	DesiredProxied    bool
	State             string
	LastOperationID   contract.OperationID
	LastError         string
	AppliedGeneration int64
	CleanupError      string
}

type DomainObservationTarget struct {
	Domain            string
	ExpectedAddresses []string
	CheckPath         string
}

type DomainRemovalData struct {
	DomainID      platform.DomainID
	EnvironmentID platform.EnvironmentID
	Domain        string
	Primary       bool
	ManagedDNS    bool
	DNSState      string
}

type DomainActionFacts struct {
	Now                   time.Time
	ID                    platform.DomainID
	EnvironmentID         platform.EnvironmentID
	Primary               bool
	DesiredRevision       int64
	AppliedRevision       int64
	RouteState            platform.RouteState
	RouteObservedAt       *time.Time
	RouteAppliedAt        *time.Time
	DNSState              platform.DNSState
	DNSObservedAt         *time.Time
	CertificateState      platform.CertificateState
	CertificateObservedAt *time.Time
	PublicState           platform.PublicState
	PublicObservedAt      *time.Time
	Provider              string
	ZoneName              string
	OwnershipMarker       string
	Generation            int64
	AppliedGeneration     int64
	DesiredOrigins        []string
	DesiredProxied        bool
	LedgerState           string
	LastOperationID       contract.OperationID
	LedgerUpdatedAt       *time.Time
	CleanupRequestedAt    *time.Time
	CleanupVerifiedAt     *time.Time
}

type DomainBindingSummary struct {
	App         string
	Environment string
	Primary     bool
	CreatedAt   time.Time
}

type DomainCleanupViolation struct {
	Domain            string
	ZoneID            string
	ZoneName          string
	State             string
	AppliedGeneration int64
}

type DomainDefinitionSnapshot struct {
	ID                platform.DomainID
	Domain            string
	Primary           bool
	CheckPath         string
	DesiredRevision   int64
	ZoneID            string
	Generation        int64
	Provider          string
	ZoneName          string
	OwnershipMarker   string
	DesiredOrigins    []string
	DesiredProxied    bool
	State             string
	AppliedGeneration int64
	LastOperationID   contract.OperationID
}

type RouteTarget struct {
	Domain        string
	EnvironmentID platform.EnvironmentID
	Primary       bool
	Instances     json.RawMessage
}

type DomainDNSState struct {
	State           string
	LastError       string
	CleanupError    string
	LastOperationID contract.OperationID
}

type Owner struct {
	EnvironmentID     platform.EnvironmentID
	OperationID       contract.OperationID
	DeploymentID      platform.DeploymentID
	Attempt           uint32
	Phase             string
	RouteTarget       platform.DeploymentID
	RestoreRelease    platform.ReleaseID
	RestoreDeployment platform.DeploymentID
	App               string
	Environment       string
}

type OwnerInsert struct {
	EnvironmentID     platform.EnvironmentID
	OperationID       contract.OperationID
	DeploymentID      platform.DeploymentID
	Attempt           uint32
	Phase             string
	RouteTarget       platform.DeploymentID
	RestoreRelease    platform.ReleaseID
	RestoreDeployment platform.DeploymentID
}

type CleanupRow struct {
	ContainerName string
	EnvironmentID platform.EnvironmentID
	App           string
	Environment   string
	Node          string
	DeploymentID  platform.DeploymentID
	Reason        string
	Attempts      int64
	CreatedAt     time.Time
}

type CleanupRecord struct {
	ContainerName string
	EnvironmentID platform.EnvironmentID
	Node          string
	DeploymentID  platform.DeploymentID
	Reason        string
	LastError     string
}

type ReleaseCommandRun struct {
	State       string
	OperationID contract.OperationID
	Active      bool
}

type CleanupProtectedFacts struct {
	ServingDeployment platform.DeploymentID
	OwnerPhase        string
	OwnerDeployment   platform.DeploymentID
	RouteTarget       platform.DeploymentID
	RestoreDeployment platform.DeploymentID
	HasOwner          bool
}

type DomainStore struct{ q *query.Queries }

func (s *DomainStore) GetByName(ctx context.Context, name string) (platform.DomainID, platform.EnvironmentID, bool, error) {
	row, err := s.q.GetPlatformDomainByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	return platform.DomainID(row.ID), platform.EnvironmentID(row.EnvironmentID), err == nil, err
}

func (s *DomainStore) LockByNameAndEnvironment(ctx context.Context, name string, environmentID platform.EnvironmentID) (platform.DomainID, bool, error) {
	id, err := s.q.LockPlatformDomainByNameAndEnvironment(ctx, query.LockPlatformDomainByNameAndEnvironmentParams{
		Name: name, EnvironmentID: string(environmentID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return platform.DomainID(id), err == nil, err
}

func (s *DomainStore) BindingByNameAndEnvironment(ctx context.Context, name string, environmentID platform.EnvironmentID) (DomainBindingRef, bool, error) {
	row, err := s.q.GetPlatformDomainBindingByNameAndEnvironment(ctx, query.GetPlatformDomainBindingByNameAndEnvironmentParams{
		Name: name, EnvironmentID: string(environmentID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DomainBindingRef{}, false, nil
	}
	return DomainBindingRef{ID: platform.DomainID(row.ID), Primary: row.IsPrimary}, err == nil, err
}

func (s *DomainStore) EnvironmentIDByName(ctx context.Context, name string) (platform.EnvironmentID, bool, error) {
	id, err := s.q.GetPlatformDomainEnvironmentIDByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return platform.EnvironmentID(id), err == nil, err
}

func (s *DomainStore) EnvironmentIDByID(ctx context.Context, id platform.DomainID) (platform.EnvironmentID, bool, error) {
	envID, err := s.q.GetPlatformDomainEnvironmentIDByID(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return platform.EnvironmentID(envID), err == nil, err
}

func (s *DomainStore) LockByIDAndEnvironment(ctx context.Context, id platform.DomainID, environmentID platform.EnvironmentID) (bool, error) {
	locked, err := s.q.LockPlatformDomainByIDAndEnvironment(ctx, query.LockPlatformDomainByIDAndEnvironmentParams{
		ID: string(id), EnvironmentID: string(environmentID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return locked == string(id), err
}

func (s *DomainStore) ExistsByID(ctx context.Context, id platform.DomainID) (bool, error) {
	return s.q.PlatformDomainExistsByID(ctx, string(id))
}

func (s *DomainStore) BindingExists(ctx context.Context, domain, app, environment string) (bool, error) {
	return s.q.PlatformDomainBindingExists(ctx, query.PlatformDomainBindingExistsParams{
		Domain: domain, App: app, Environment: environment,
	})
}

func (s *DomainStore) HasPrimary(ctx context.Context, environmentID platform.EnvironmentID) (bool, error) {
	return s.q.PlatformDomainHasPrimary(ctx, string(environmentID))
}

func (s *DomainStore) PrimaryDNSState(ctx context.Context, environmentID platform.EnvironmentID) (string, bool, error) {
	state, err := s.q.GetPlatformPrimaryDNSState(ctx, string(environmentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return state, err == nil, err
}

func (s *DomainStore) PrimaryCheckPath(ctx context.Context, environmentID platform.EnvironmentID, name string) (string, bool, error) {
	path, err := s.q.GetPlatformPrimaryCheckPath(ctx, query.GetPlatformPrimaryCheckPathParams{
		EnvironmentID: string(environmentID), Name: name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return path, err == nil, err
}

func (s *DomainStore) SetExpectedAddressesByID(ctx context.Context, id platform.DomainID, addresses []string) (int64, error) {
	return s.q.SetPlatformDomainExpectedAddressesByID(ctx, query.SetPlatformDomainExpectedAddressesByIDParams{ID: string(id), Addresses: addresses})
}

func (s *DomainStore) SetExpectedAddressesByName(ctx context.Context, name string, addresses []string) (int64, error) {
	return s.q.SetPlatformDomainExpectedAddressesByName(ctx, query.SetPlatformDomainExpectedAddressesByNameParams{Name: name, Addresses: addresses})
}

func (s *DomainStore) SetCheckPath(ctx context.Context, name, checkPath string) (int64, error) {
	return s.q.SetPlatformDomainCheckPath(ctx, query.SetPlatformDomainCheckPathParams{Name: name, CheckPath: checkPath})
}

func (s *DomainStore) ClearPrimaryExcept(ctx context.Context, environmentID platform.EnvironmentID, name string) (int64, error) {
	return s.q.ClearPlatformPrimaryExcept(ctx, query.ClearPlatformPrimaryExceptParams{EnvironmentID: string(environmentID), Name: name})
}

func (s *DomainStore) PromotePrimary(ctx context.Context, name string) (int64, error) {
	return s.q.PromotePlatformDomainPrimary(ctx, name)
}

func (s *DomainStore) ClearPrimary(ctx context.Context, environmentID platform.EnvironmentID) (int64, error) {
	return s.q.ClearPlatformPrimary(ctx, string(environmentID))
}

func (s *DomainStore) Insert(ctx context.Context, insert DomainInsert) (bool, error) {
	affected, err := s.q.InsertPlatformDomain(ctx, query.InsertPlatformDomainParams{
		ID: string(insert.ID), Name: insert.Domain, EnvironmentID: string(insert.EnvironmentID),
		IsPrimary: insert.Primary, Addresses: insert.Addresses, CheckPath: insert.CheckPath,
	})
	return affected == 1, err
}

func (s *DomainStore) List(ctx context.Context, filter DomainFilter) ([]platform.DomainBinding, error) {
	if filter.Limit < 1 {
		return nil, errors.New("entities: domain list limit must be positive")
	}
	rows, err := s.q.ListPlatformDomains(ctx, query.ListPlatformDomainsParams{
		App: platformOptionalText(filter.App), Environment: platformOptionalText(filter.Environment),
		MaxRows: int64(filter.Limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]platform.DomainBinding, 0, len(rows))
	for _, row := range rows {
		item, err := domainBindingRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (s *DomainStore) DefinitionSnapshot(ctx context.Context, environmentID platform.EnvironmentID) ([]DomainDefinitionSnapshot, error) {
	rows, err := s.q.GetPlatformDomainDefinitionSnapshot(ctx, string(environmentID))
	if err != nil {
		return nil, err
	}
	result := make([]DomainDefinitionSnapshot, 0, len(rows))
	for _, row := range rows {
		result = append(result, DomainDefinitionSnapshot{
			ID: platform.DomainID(row.ID), Domain: row.Name, Primary: row.IsPrimary, CheckPath: row.CheckPath,
			DesiredRevision: row.DesiredRevision, ZoneID: row.ZoneID, Generation: row.Generation,
			Provider: row.Provider, ZoneName: row.ZoneName, OwnershipMarker: row.OwnershipMarker,
			DesiredOrigins: row.DesiredOrigins, DesiredProxied: row.DesiredProxied, State: row.State,
			AppliedGeneration: row.AppliedGeneration, LastOperationID: contract.OperationID(row.LastOperationID),
		})
	}
	return result, nil
}

func (s *DomainStore) ActionFacts(ctx context.Context, app, environment, domain string) (DomainActionFacts, bool, error) {
	row, err := s.q.GetPlatformDomainActionFacts(ctx, query.GetPlatformDomainActionFactsParams{App: app, Environment: environment, Domain: domain})
	if errors.Is(err, pgx.ErrNoRows) {
		return DomainActionFacts{}, false, nil
	}
	if err != nil {
		return DomainActionFacts{}, false, err
	}
	now, err := timestampAny(row.ClockTimestamp)
	if err != nil {
		return DomainActionFacts{}, false, err
	}
	generation := int64(0)
	if row.Generation != nil {
		generation = *row.Generation
	}
	appliedGeneration := int64(0)
	if row.AppliedGeneration != nil {
		appliedGeneration = *row.AppliedGeneration
	}
	proxied := false
	if row.DesiredProxied.Valid {
		proxied = row.DesiredProxied.Bool
	}
	return DomainActionFacts{
		Now: now, ID: platform.DomainID(row.ID), EnvironmentID: platform.EnvironmentID(row.EnvironmentID),
		Primary: row.IsPrimary, DesiredRevision: row.DesiredRevision, AppliedRevision: row.AppliedRevision,
		RouteState: platform.RouteState(row.RouteState), RouteObservedAt: platformTimePtr(row.RouteObservedAt),
		RouteAppliedAt: platformTimePtr(row.RouteAppliedAt), DNSState: platform.DNSState(row.DnsState),
		DNSObservedAt: platformTimePtr(row.DnsObservedAt), CertificateState: platform.CertificateState(row.CertificateState),
		CertificateObservedAt: platformTimePtr(row.CertificateObservedAt), PublicState: platform.PublicState(row.PublicState),
		PublicObservedAt: platformTimePtr(row.PublicObservedAt), Provider: row.Provider, ZoneName: row.ZoneName,
		OwnershipMarker: row.OwnershipMarker, Generation: generation, AppliedGeneration: appliedGeneration,
		DesiredOrigins: row.DesiredOrigins, DesiredProxied: proxied, LedgerState: row.State,
		LastOperationID: contract.OperationID(row.LastOperationID), LedgerUpdatedAt: platformTimePtr(row.UpdatedAt),
		CleanupRequestedAt: platformTimePtr(row.CleanupRequestedAt), CleanupVerifiedAt: platformTimePtr(row.CleanupVerifiedAt),
	}, true, nil
}

func (s *DomainStore) RemovalData(ctx context.Context, app, environment, domain string) (DomainRemovalData, bool, error) {
	row, err := s.q.GetPlatformDomainRemovalData(ctx, query.GetPlatformDomainRemovalDataParams{App: app, Environment: environment, Domain: domain})
	if errors.Is(err, pgx.ErrNoRows) {
		return DomainRemovalData{}, false, nil
	}
	return DomainRemovalData{
		DomainID: platform.DomainID(row.ID), EnvironmentID: platform.EnvironmentID(row.ID_2), Domain: row.Name,
		Primary: row.IsPrimary, ManagedDNS: row.Exists, DNSState: coalesceString(row.Coalesce),
	}, err == nil, err
}

func (s *DomainStore) BindingSummary(ctx context.Context, domain string) (DomainBindingSummary, bool, error) {
	row, err := s.q.GetPlatformDomainBindingSummary(ctx, domain)
	if errors.Is(err, pgx.ErrNoRows) {
		return DomainBindingSummary{}, false, nil
	}
	created, err := platformTime(row.CreatedAt)
	if err != nil {
		return DomainBindingSummary{}, false, err
	}
	return DomainBindingSummary{App: row.Name, Environment: row.Name_2, Primary: row.IsPrimary, CreatedAt: created}, true, nil
}

func (s *DomainStore) ObservationTargets(ctx context.Context, after string, limit int) ([]DomainObservationTarget, error) {
	rows, err := s.q.ListPlatformDomainObservationTargets(ctx, query.ListPlatformDomainObservationTargetsParams{
		AfterName: after, MaxRows: int64(limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]DomainObservationTarget, 0, len(rows))
	for _, row := range rows {
		result = append(result, DomainObservationTarget{Domain: row.Name, ExpectedAddresses: row.ExpectedAddresses, CheckPath: row.HealthPath})
	}
	return result, nil
}

func (s *DomainStore) RecordObservation(ctx context.Context, name string, observation platform.DomainBinding) (int64, error) {
	return s.q.RecordPlatformDomainObservation(ctx, query.RecordPlatformDomainObservationParams{
		Domain: name, DnsState: string(observation.DNSState), DnsAddresses: observation.DNSAddresses,
		DnsError: observation.DNSError, DnsObservedAt: platformOptionalTime(observation.DNSObservedAt),
		CertificateState: string(observation.CertificateState), CertificateNotAfter: platformOptionalTime(observation.CertificateNotAfter),
		CertificateError: observation.CertificateError, CertificateObservedAt: platformOptionalTime(observation.CertificateObservedAt),
		PublicState: string(observation.PublicState), PublicStatus: optionalInt64(observation.PublicStatus),
		PublicError: observation.PublicError, PublicObservedAt: platformOptionalTime(observation.PublicObservedAt),
		ObservedAt: platformOptionalTime(observation.ObservedAt),
	})
}

func (s *DomainStore) BumpDesiredRevision(ctx context.Context, environmentID platform.EnvironmentID) (int64, error) {
	return s.q.BumpPlatformDomainDesiredRevisionForEnvironment(ctx, string(environmentID))
}

func (s *DomainStore) RouteTargets(ctx context.Context) ([]RouteTarget, error) {
	rows, err := s.q.ListPlatformRouteTargets(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]RouteTarget, 0, len(rows))
	for _, row := range rows {
		result = append(result, RouteTarget{Domain: row.Name, EnvironmentID: platform.EnvironmentID(row.ID), Primary: row.IsPrimary, Instances: row.Instances})
	}
	return result, nil
}

func (s *DomainStore) MarkRoutesApplied(ctx context.Context, names []string, at time.Time) (int64, error) {
	return s.q.MarkPlatformRoutesApplied(ctx, query.MarkPlatformRoutesAppliedParams{Names: names, At: pgtype.Timestamptz{Time: at, Valid: true}})
}

func (s *DomainStore) MarkRoutesFailed(ctx context.Context, names []string, message string, at time.Time) (int64, error) {
	return s.q.MarkPlatformRoutesFailed(ctx, query.MarkPlatformRoutesFailedParams{Names: names, Message: message, At: pgtype.Timestamptz{Time: at, Valid: true}})
}

func (s *DomainStore) MarkRoutesPending(ctx context.Context, names []string, message string, at time.Time) (int64, error) {
	return s.q.MarkPlatformRoutesPending(ctx, query.MarkPlatformRoutesPendingParams{Names: names, Message: message, At: pgtype.Timestamptz{Time: at, Valid: true}})
}

func (s *DomainStore) LockRouteWrite(ctx context.Context, key int64) error {
	return s.q.LockPlatformRouteWrite(ctx, key)
}

func (s *DomainStore) DeleteForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) (int64, error) {
	return s.q.DeletePlatformDomainsForEnvironment(ctx, string(environmentID))
}

func (s *DomainStore) DeleteBinding(ctx context.Context, domain, app, environment string) (int64, error) {
	return s.q.DeletePlatformDomainBinding(ctx, query.DeletePlatformDomainBindingParams{Domain: domain, App: app, Environment: environment})
}

func (s *DomainStore) DeleteForEnvironments(ctx context.Context, environmentIDs []string) (int64, error) {
	return s.q.DeletePlatformDomainsForEnvironments(ctx, environmentIDs)
}

func (s *DomainStore) DeleteBindingAndDNSCleanup(ctx context.Context, environmentIDs []string) (int64, error) {
	return s.q.DeletePlatformDomainAndDNSCleanup(ctx, environmentIDs)
}

func domainBindingRow(row query.ListPlatformDomainsRow) (platform.DomainBinding, error) {
	item := platform.DomainBinding{
		ID: platform.DomainID(row.ID), Domain: row.Name, App: row.Name_2, Environment: row.Name_3,
		Primary: row.IsPrimary, CheckPath: row.CheckPath, DesiredRevision: row.DesiredRevision,
		AppliedRevision: row.AppliedRevision, RouteState: platform.RouteState(row.RouteState),
		RouteError: row.RouteError, RouteAppliedAt: platformTimePtr(row.RouteAppliedAt),
		RouteObservedAt: platformTimePtr(row.RouteObservedAt), ExpectedAddresses: row.ExpectedAddresses,
		DNSState: platform.DNSState(row.DnsState), DNSAddresses: row.DnsAddresses, DNSError: row.DnsError,
		DNSObservedAt: platformTimePtr(row.DnsObservedAt), CertificateState: platform.CertificateState(row.CertificateState),
		CertificateNotAfter: platformTimePtr(row.CertificateNotAfter), CertificateError: row.CertificateError,
		CertificateObservedAt: platformTimePtr(row.CertificateObservedAt), PublicState: platform.PublicState(row.PublicState),
		PublicStatus: int(row.PublicStatus), PublicError: row.PublicError, PublicObservedAt: platformTimePtr(row.PublicObservedAt),
		ObservedAt: platformTimePtr(row.ObservedAt),
	}
	if row.ExpectedAddresses == nil {
		item.ExpectedAddresses = []string{}
	}
	if row.DnsAddresses == nil {
		item.DNSAddresses = []string{}
	}
	if len(row.Instances) > 0 {
		var instances []platform.DeploymentInstance
		if err := json.Unmarshal(row.Instances, &instances); err == nil {
			item.Upstreams = platform.WebUpstreams(instances)
		}
	}
	if item.Upstreams == nil {
		item.Upstreams = []string{}
	}
	if row.Provider != "" {
		item.ManagedDNS = &platform.ManagedDNSBinding{
			Provider: row.Provider, ZoneID: row.ZoneID, ZoneName: row.ZoneName, Origins: row.DesiredOrigins,
			Proxied: row.DesiredProxied, State: row.State, Generation: row.Generation, AppliedGeneration: row.AppliedGeneration,
		}
	}
	return item, nil
}

type DomainDNSStore struct{ q *query.Queries }

func (s *DomainDNSStore) GetByAppEnvironmentDomain(ctx context.Context, app, environment, domain string) (DomainDNSLedger, bool, error) {
	row, err := s.q.GetPlatformDomainDNSByAppEnvironmentDomain(ctx, query.GetPlatformDomainDNSByAppEnvironmentDomainParams{App: app, Environment: environment, Domain: domain})
	if errors.Is(err, pgx.ErrNoRows) {
		return DomainDNSLedger{}, false, nil
	}
	if err != nil {
		return DomainDNSLedger{}, false, err
	}
	return domainDNSRow(row), true, nil
}

func (s *DomainDNSStore) GetByDomainID(ctx context.Context, domainID platform.DomainID) (DomainDNSLedger, bool, error) {
	row, err := s.q.GetPlatformDomainDNSByDomainID(ctx, string(domainID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DomainDNSLedger{}, false, nil
	}
	if err != nil {
		return DomainDNSLedger{}, false, err
	}
	return domainDNSRow(query.GetPlatformDomainDNSByAppEnvironmentDomainRow(row)), true, nil
}

func (s *DomainDNSStore) CleanupViolations(ctx context.Context, app, environment string) ([]DomainCleanupViolation, error) {
	rows, err := s.q.ListPlatformDomainDNSCleanupViolations(ctx, query.ListPlatformDomainDNSCleanupViolationsParams{
		App: app, Environment: platformOptionalText(environment),
	})
	if err != nil {
		return nil, err
	}
	result := make([]DomainCleanupViolation, 0, len(rows))
	for _, row := range rows {
		result = append(result, DomainCleanupViolation{Domain: row.Name, ZoneID: row.ZoneID, ZoneName: row.ZoneName, State: row.State, AppliedGeneration: row.AppliedGeneration})
	}
	return result, nil
}

// ListForEnvironments reads the exact ledgers of several environments in one
// statement, ordered by app, environment and domain.
func (s *DomainDNSStore) ListForEnvironments(ctx context.Context, environmentIDs []platform.EnvironmentID) ([]DomainDNSLedger, error) {
	ids := make([]string, len(environmentIDs))
	for index, id := range environmentIDs {
		ids[index] = string(id)
	}
	rows, err := s.q.ListPlatformDomainDNSForEnvironments(ctx, ids)
	if err != nil {
		return nil, err
	}
	result := make([]DomainDNSLedger, 0, len(rows))
	for _, row := range rows {
		result = append(result, DomainDNSLedger{
			DomainID: platform.DomainID(row.DomainID), Domain: row.Name, App: row.Name_2, Environment: row.Name_3,
			Provider: row.Provider, ZoneID: row.ZoneID, ZoneName: row.ZoneName, OwnershipMarker: row.OwnershipMarker,
			Generation: row.Generation, DesiredOrigins: row.DesiredOrigins, DesiredProxied: row.DesiredProxied,
			State: row.State, LastOperationID: contract.OperationID(row.LastOperationID), LastError: row.LastError,
			AppliedGeneration: row.AppliedGeneration, CleanupError: row.CleanupError,
		})
	}
	return result, nil
}

func (s *DomainDNSStore) LockLedger(ctx context.Context, domainID platform.DomainID) (string, bool, error) {
	zone, err := s.q.LockPlatformDomainDNSLedger(ctx, string(domainID))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return zone, err == nil, err
}

func (s *DomainDNSStore) UpsertIntent(ctx context.Context, domainID platform.DomainID, zoneID, zoneName, marker string, origins []string, proxied bool, operationID contract.OperationID) (int64, error) {
	if _, found, err := s.LockLedger(ctx, domainID); err != nil {
		return 0, err
	} else if found {
		return s.q.UpdatePlatformDomainDNSIntent(ctx, query.UpdatePlatformDomainDNSIntentParams{
			DomainID: string(domainID), Origins: origins, Proxied: proxied, OperationID: string(operationID),
		})
	}
	return s.q.InsertPlatformDomainDNSIntent(ctx, query.InsertPlatformDomainDNSIntentParams{
		DomainID: string(domainID), ZoneID: zoneID, ZoneName: zoneName, OwnershipMarker: marker,
		Origins: origins, Proxied: proxied, OperationID: string(operationID),
	})
}

func (s *DomainDNSStore) InsertCleanupIntent(ctx context.Context, domainID platform.DomainID, zoneID, zoneName, marker string, operationID contract.OperationID) (int64, error) {
	return s.q.InsertPlatformDomainDNSCleanupIntent(ctx, query.InsertPlatformDomainDNSCleanupIntentParams{
		DomainID: string(domainID), ZoneID: zoneID, ZoneName: zoneName, OwnershipMarker: marker, OperationID: string(operationID),
	})
}

func (s *DomainDNSStore) FenceOperation(ctx context.Context, domainID platform.DomainID, operationID contract.OperationID) (int64, error) {
	return s.q.FencePlatformDomainDNSOperation(ctx, query.FencePlatformDomainDNSOperationParams{DomainID: string(domainID), OperationID: string(operationID)})
}

func (s *DomainDNSStore) MarkCleanupPending(ctx context.Context, domainID platform.DomainID, operationID contract.OperationID) (int64, error) {
	return s.q.MarkPlatformDomainDNSCleanupPending(ctx, query.MarkPlatformDomainDNSCleanupPendingParams{DomainID: string(domainID), OperationID: string(operationID)})
}

func (s *DomainDNSStore) MarkCleanupPendingForEnvironments(ctx context.Context, app, environment string, operationID contract.OperationID) (int64, error) {
	return s.q.MarkPlatformDomainDNSCleanupPendingForEnvironments(ctx, query.MarkPlatformDomainDNSCleanupPendingForEnvironmentsParams{
		App: app, Environment: platformOptionalText(environment), OperationID: string(operationID),
	})
}

func (s *DomainDNSStore) MarkCleanupVerified(ctx context.Context, domainID platform.DomainID, operationID contract.OperationID) (int64, error) {
	return s.q.MarkPlatformDomainDNSCleanupVerified(ctx, query.MarkPlatformDomainDNSCleanupVerifiedParams{DomainID: string(domainID), OperationID: string(operationID)})
}

func (s *DomainDNSStore) MarkCleanupVerifiedForGeneration(ctx context.Context, domainID platform.DomainID, operationID contract.OperationID, generation int64) (int64, error) {
	return s.q.MarkPlatformDomainDNSCleanupVerifiedForGeneration(ctx, query.MarkPlatformDomainDNSCleanupVerifiedForGenerationParams{
		DomainID: string(domainID), OperationID: string(operationID), Generation: generation,
	})
}

func (s *DomainDNSStore) MarkCleanupFailed(ctx context.Context, domainID platform.DomainID, operationID contract.OperationID, message string) (int64, error) {
	return s.q.MarkPlatformDomainDNSCleanupFailed(ctx, query.MarkPlatformDomainDNSCleanupFailedParams{DomainID: string(domainID), OperationID: string(operationID), Message: message})
}

func (s *DomainDNSStore) MarkApplied(ctx context.Context, domainID platform.DomainID, operationID contract.OperationID) (int64, error) {
	return s.q.MarkPlatformDomainDNSApplied(ctx, query.MarkPlatformDomainDNSAppliedParams{DomainID: string(domainID), OperationID: string(operationID)})
}

func (s *DomainDNSStore) MarkFailed(ctx context.Context, domainID platform.DomainID, operationID contract.OperationID, generation int64, message string) (int64, error) {
	return s.q.MarkPlatformDomainDNSFailed(ctx, query.MarkPlatformDomainDNSFailedParams{DomainID: string(domainID), OperationID: string(operationID), Generation: generation, Message: message})
}

func (s *DomainDNSStore) SetCleanupError(ctx context.Context, domainID platform.DomainID, operationID contract.OperationID, generation int64, message string) (int64, error) {
	return s.q.SetPlatformDomainDNSCleanupError(ctx, query.SetPlatformDomainDNSCleanupErrorParams{DomainID: string(domainID), OperationID: string(operationID), Generation: generation, Message: message})
}

func (s *DomainDNSStore) CredentialZoneIDs(ctx context.Context, environmentID platform.EnvironmentID) ([]string, error) {
	return s.q.ListPlatformDomainDNSCredentialZoneIDs(ctx, string(environmentID))
}

func (s *DomainDNSStore) ZoneIDsForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) ([]string, error) {
	return s.q.ListPlatformDomainDNSZoneIDsForEnvironment(ctx, string(environmentID))
}

func (s *DomainDNSStore) StateForDefinitionStep(ctx context.Context, domainID platform.DomainID) (DomainDNSState, bool, error) {
	row, err := s.q.GetPlatformDomainDNSStateForDefinitionStep(ctx, string(domainID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DomainDNSState{}, false, nil
	}
	return DomainDNSState{State: row.State, LastError: row.LastError, CleanupError: row.CleanupError, LastOperationID: contract.OperationID(row.LastOperationID)}, err == nil, err
}

func (s *DomainDNSStore) ExistsForOperation(ctx context.Context, operationID contract.OperationID) (bool, error) {
	return s.q.PlatformDomainDNSExistsForOperation(ctx, string(operationID))
}

func (s *DomainDNSStore) ExistsForDomain(ctx context.Context, domainID platform.DomainID) (bool, error) {
	return s.q.PlatformDomainDNSExistsForDomain(ctx, string(domainID))
}

func (s *DomainDNSStore) DeleteForBinding(ctx context.Context, domain string, environmentID platform.EnvironmentID) (int64, error) {
	return s.q.DeletePlatformDomainDNSForBinding(ctx, query.DeletePlatformDomainDNSForBindingParams{Domain: domain, EnvironmentID: string(environmentID)})
}

func (s *DomainDNSStore) DeleteCleanupForEnvironments(ctx context.Context, environmentIDs []string) (int64, error) {
	return s.q.DeletePlatformDomainDNSCleanupForEnvironments(ctx, environmentIDs)
}

func domainDNSRow(row query.GetPlatformDomainDNSByAppEnvironmentDomainRow) DomainDNSLedger {
	return DomainDNSLedger{
		DomainID: platform.DomainID(row.DomainID), Domain: row.Name, App: row.Name_2, Environment: row.Name_3,
		Provider: row.Provider, ZoneID: row.ZoneID, ZoneName: row.ZoneName, OwnershipMarker: row.OwnershipMarker,
		Generation: row.Generation, DesiredOrigins: row.DesiredOrigins, DesiredProxied: row.DesiredProxied,
		State: row.State, LastOperationID: contract.OperationID(row.LastOperationID), LastError: row.LastError,
		AppliedGeneration: row.AppliedGeneration, CleanupError: row.CleanupError,
	}
}

type DeploymentOwnerStore struct{ q *query.Queries }

func (s *DeploymentOwnerStore) Get(ctx context.Context, environmentID platform.EnvironmentID) (Owner, bool, error) {
	row, err := s.q.GetPlatformDeploymentOwner(ctx, string(environmentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Owner{}, false, nil
	}
	if err != nil {
		return Owner{}, false, err
	}
	owner, err := ownerRow(environmentID, row)
	return owner, err == nil, err
}

func (s *DeploymentOwnerStore) ExistsForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) (bool, error) {
	return s.q.PlatformDeploymentOwnerExists(ctx, string(environmentID))
}

func (s *DeploymentOwnerStore) CountForAppEnvironment(ctx context.Context, app, environment string) (int64, error) {
	return s.q.CountPlatformDeploymentOwners(ctx, query.CountPlatformDeploymentOwnersParams{App: app, Environment: platformOptionalText(environment)})
}

func (s *DeploymentOwnerStore) Insert(ctx context.Context, insert OwnerInsert) (bool, error) {
	affected, err := s.q.InsertPlatformDeploymentOwner(ctx, query.InsertPlatformDeploymentOwnerParams{
		EnvironmentID: string(insert.EnvironmentID), OperationID: string(insert.OperationID),
		DeploymentID: string(insert.DeploymentID), OwnerAttempt: int64(insert.Attempt), Phase: insert.Phase,
		RouteTarget: platformOptionalText(string(insert.RouteTarget)), RestoreRelease: platformOptionalText(string(insert.RestoreRelease)),
		RestoreDeployment: platformOptionalText(string(insert.RestoreDeployment)),
	})
	return affected == 1, err
}

func (s *DeploymentOwnerStore) Renew(ctx context.Context, environmentID platform.EnvironmentID, operationID contract.OperationID, attempt uint32) (bool, error) {
	affected, err := s.q.RenewPlatformDeploymentOwner(ctx, query.RenewPlatformDeploymentOwnerParams{
		EnvironmentID: string(environmentID), OperationID: string(operationID), OwnerAttempt: int64(attempt),
	})
	return affected == 1, err
}

func (s *DeploymentOwnerStore) SetRoute(ctx context.Context, environmentID platform.EnvironmentID, operationID contract.OperationID, deploymentID platform.DeploymentID, routeTarget platform.DeploymentID, phase string, attempt uint32) (Owner, bool, error) {
	row, err := s.q.SetPlatformDeploymentOwnerRoute(ctx, query.SetPlatformDeploymentOwnerRouteParams{
		EnvironmentID: string(environmentID), OperationID: string(operationID), DeploymentID: string(deploymentID),
		RouteTarget: platformOptionalText(string(routeTarget)), Phase: phase, OwnerAttempt: int64(attempt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Owner{}, false, nil
	}
	if err != nil {
		return Owner{}, false, err
	}
	owner, err := ownerRow(environmentID, query.GetPlatformDeploymentOwnerRow(row))
	return owner, err == nil, err
}

func (s *DeploymentOwnerStore) ListSettleable(ctx context.Context) ([]Owner, error) {
	rows, err := s.q.ListPlatformDeploymentOwnersToSettle(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]Owner, 0, len(rows))
	for _, row := range rows {
		owner, err := ownerRow(platform.EnvironmentID(row.EnvironmentID), query.GetPlatformDeploymentOwnerRow{
			OperationID: row.OperationID, DeploymentID: row.DeploymentID, OwnerAttempt: row.OwnerAttempt,
			Phase: row.Phase, RouteTargetDeploymentID: row.RouteTargetDeploymentID,
			RestoreReleaseID: row.RestoreReleaseID, RestoreDeploymentID: row.RestoreDeploymentID,
		})
		if err != nil {
			return nil, err
		}
		owner.App, owner.Environment = row.Name, row.Name_2
		result = append(result, owner)
	}
	return result, nil
}

func (s *DeploymentOwnerStore) CleanupProtected(ctx context.Context, environmentID platform.EnvironmentID) (CleanupProtectedFacts, bool, error) {
	row, err := s.q.GetPlatformDeploymentCleanupProtected(ctx, string(environmentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return CleanupProtectedFacts{}, false, nil
	}
	return CleanupProtectedFacts{
		ServingDeployment: platform.DeploymentID(row.ServingDeploymentID), OwnerPhase: row.Phase,
		OwnerDeployment: platform.DeploymentID(row.DeploymentID), RouteTarget: platform.DeploymentID(row.RouteTargetDeploymentID),
		RestoreDeployment: platform.DeploymentID(row.RestoreDeploymentID), HasOwner: anyBool(row.Column6),
	}, err == nil, err
}

func (s *DeploymentOwnerStore) ResolveAmbiguous(ctx context.Context, environmentID platform.EnvironmentID, routeTarget platform.DeploymentID) (int64, error) {
	return s.q.ResolvePlatformAmbiguousDeploymentOwner(ctx, query.ResolvePlatformAmbiguousDeploymentOwnerParams{
		EnvironmentID: string(environmentID), RouteTarget: platformOptionalText(string(routeTarget)),
	})
}

func (s *DeploymentOwnerStore) ActiveForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) ([]contract.Operation, error) {
	rows, err := s.q.ListPlatformActiveDeploymentOwnersForEnvironment(ctx, string(environmentID))
	if err != nil {
		return nil, err
	}
	result := make([]contract.Operation, 0, len(rows))
	for _, row := range rows {
		attempt, err := platformUint32(row.Attempt)
		if err != nil {
			return nil, err
		}
		result = append(result, contract.Operation{ID: contract.OperationID(row.ID), Attempt: attempt, State: contract.State(row.State)})
	}
	return result, nil
}

func (s *DeploymentOwnerStore) CountActiveForEnvironment(ctx context.Context, environmentID platform.EnvironmentID) (int64, error) {
	return s.q.CountPlatformActiveDeploymentOwnersForEnvironment(ctx, string(environmentID))
}

func (s *DeploymentOwnerStore) ClearAmbiguous(ctx context.Context, environmentID platform.EnvironmentID) (int64, error) {
	return s.q.DeletePlatformAmbiguousDeploymentOwner(ctx, string(environmentID))
}

func (s *DeploymentOwnerStore) DeleteRestoredByOperation(ctx context.Context, operationID contract.OperationID) (int64, error) {
	return s.q.DeletePlatformRestoredDeploymentOwnerByOperation(ctx, string(operationID))
}

func (s *DeploymentOwnerStore) DeleteServedByOperation(ctx context.Context, operationID contract.OperationID) (int64, error) {
	return s.q.DeletePlatformServedDeploymentOwnerByOperation(ctx, string(operationID))
}

func (s *DeploymentOwnerStore) DeleteRestored(ctx context.Context, owner Owner) (int64, error) {
	return s.q.DeletePlatformRestoredDeploymentOwner(ctx, query.DeletePlatformRestoredDeploymentOwnerParams{
		EnvironmentID: string(owner.EnvironmentID), OperationID: string(owner.OperationID), OwnerAttempt: int64(owner.Attempt),
	})
}

func (s *DeploymentOwnerStore) DeleteServed(ctx context.Context, owner Owner) (int64, error) {
	return s.q.DeletePlatformServedDeploymentOwner(ctx, query.DeletePlatformServedDeploymentOwnerParams{
		EnvironmentID: string(owner.EnvironmentID), OperationID: string(owner.OperationID), OwnerAttempt: int64(owner.Attempt),
	})
}

func ownerRow(environmentID platform.EnvironmentID, row query.GetPlatformDeploymentOwnerRow) (Owner, error) {
	attempt, err := platformUint32(row.OwnerAttempt)
	if err != nil {
		return Owner{}, err
	}
	return Owner{
		EnvironmentID: environmentID, OperationID: contract.OperationID(row.OperationID),
		DeploymentID: platform.DeploymentID(row.DeploymentID), Attempt: attempt, Phase: row.Phase,
		RouteTarget:    platform.DeploymentID(row.RouteTargetDeploymentID),
		RestoreRelease: platform.ReleaseID(row.RestoreReleaseID), RestoreDeployment: platform.DeploymentID(row.RestoreDeploymentID),
	}, nil
}

type ContainerCleanupStore struct{ q *query.Queries }

func (s *ContainerCleanupStore) RecordFailure(ctx context.Context, record CleanupRecord) (int64, error) {
	return s.q.RecordPlatformContainerCleanup(ctx, query.RecordPlatformContainerCleanupParams{
		ContainerName: record.ContainerName, EnvironmentID: string(record.EnvironmentID), Node: record.Node,
		DeploymentID: platformOptionalText(string(record.DeploymentID)), Reason: record.Reason, LastError: record.LastError,
	})
}

func (s *ContainerCleanupStore) Ensure(ctx context.Context, record CleanupRecord) (int64, error) {
	return s.q.EnsurePlatformContainerCleanup(ctx, query.EnsurePlatformContainerCleanupParams{
		ContainerName: record.ContainerName, EnvironmentID: string(record.EnvironmentID), Node: record.Node,
		DeploymentID: platformOptionalText(string(record.DeploymentID)), Reason: record.Reason, LastError: record.LastError,
	})
}

func (s *ContainerCleanupStore) Reschedule(ctx context.Context, containerName string, delay time.Duration) (int64, error) {
	return s.q.ReschedulePlatformContainerCleanup(ctx, query.ReschedulePlatformContainerCleanupParams{
		ContainerName: containerName, DelaySeconds: int64(delay.Seconds()),
	})
}

func (s *ContainerCleanupStore) Due(ctx context.Context, limit int) ([]CleanupRow, error) {
	rows, err := s.q.ListDuePlatformContainerCleanup(ctx, int64(limit))
	if err != nil {
		return nil, err
	}
	result := make([]CleanupRow, 0, len(rows))
	for _, row := range rows {
		created, err := platformTime(row.CreatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, CleanupRow{
			ContainerName: row.ContainerName, EnvironmentID: platform.EnvironmentID(row.EnvironmentID),
			App: row.Name, Environment: row.Name_2, Node: row.Node,
			DeploymentID: platform.DeploymentID(row.DeploymentID), Reason: row.Reason,
			Attempts: row.Attempts, CreatedAt: created,
		})
	}
	return result, nil
}

func (s *ContainerCleanupStore) ForDeployment(ctx context.Context, environmentID platform.EnvironmentID, deploymentID platform.DeploymentID, reason string) ([]CleanupRow, error) {
	rows, err := s.q.ListPlatformContainerCleanupForDeployment(ctx, query.ListPlatformContainerCleanupForDeploymentParams{
		EnvironmentID: string(environmentID), DeploymentID: string(deploymentID), Reason: platformOptionalText(reason),
	})
	if err != nil {
		return nil, err
	}
	result := make([]CleanupRow, 0, len(rows))
	for _, row := range rows {
		created, err := platformTime(row.CreatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, CleanupRow{
			ContainerName: row.ContainerName, EnvironmentID: platform.EnvironmentID(row.EnvironmentID),
			App: row.Name, Environment: row.Name_2, Node: row.Node,
			DeploymentID: platform.DeploymentID(row.DeploymentID), Reason: row.Reason,
			Attempts: row.Attempts, CreatedAt: created,
		})
	}
	return result, nil
}

func (s *ContainerCleanupStore) Delete(ctx context.Context, containerName string) (int64, error) {
	return s.q.DeletePlatformContainerCleanup(ctx, containerName)
}

type ReleaseCommandStore struct{ q *query.Queries }

func (s *ReleaseCommandStore) Get(ctx context.Context, releaseID platform.ReleaseID, digest string) (ReleaseCommandRun, bool, error) {
	row, err := s.q.GetPlatformReleaseCommandRun(ctx, query.GetPlatformReleaseCommandRunParams{ReleaseID: string(releaseID), CommandDigest: digest})
	if errors.Is(err, pgx.ErrNoRows) {
		return ReleaseCommandRun{}, false, nil
	}
	return ReleaseCommandRun{State: row.State, OperationID: contract.OperationID(row.OperationID), Active: row.Exists}, err == nil, err
}

func (s *ReleaseCommandStore) Start(ctx context.Context, releaseID platform.ReleaseID, digest string, operationID contract.OperationID) (int64, error) {
	return s.q.StartPlatformReleaseCommandRun(ctx, query.StartPlatformReleaseCommandRunParams{
		ReleaseID: string(releaseID), CommandDigest: digest, OperationID: string(operationID),
	})
}

func (s *ReleaseCommandStore) Finish(ctx context.Context, releaseID platform.ReleaseID, digest string, operationID contract.OperationID, state string, code contract.Code, message string) (int64, error) {
	return s.q.FinishPlatformReleaseCommandRun(ctx, query.FinishPlatformReleaseCommandRunParams{
		ReleaseID: string(releaseID), CommandDigest: digest, OperationID: string(operationID),
		State: state, ErrorCode: string(code), Message: message,
	})
}

func (s *ReleaseCommandStore) FinishRecovered(ctx context.Context, releaseID platform.ReleaseID, digest string, operationID contract.OperationID, state string, code contract.Code, message string) (int64, error) {
	return s.q.FinishRecoveredPlatformReleaseCommandRun(ctx, query.FinishRecoveredPlatformReleaseCommandRunParams{
		ReleaseID: string(releaseID), CommandDigest: digest, OperationID: string(operationID),
		State: state, ErrorCode: string(code), Message: message,
	})
}

func (s *ReleaseCommandStore) CheckForDeployment(ctx context.Context, releaseID platform.ReleaseID, digest string) (ReleaseCommandRun, bool, error) {
	row, err := s.q.GetPlatformReleaseCommandForDeployment(ctx, query.GetPlatformReleaseCommandForDeploymentParams{
		ReleaseID: string(releaseID), CommandDigest: digest,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ReleaseCommandRun{}, false, nil
	}
	return ReleaseCommandRun{State: row.State, OperationID: contract.OperationID(row.OperationID)}, err == nil, err
}

func (s *ReleaseCommandStore) DeleteForRelease(ctx context.Context, releaseID platform.ReleaseID) (int64, error) {
	return s.q.DeletePlatformReleaseCommandRuns(ctx, string(releaseID))
}

func platformOptionalTime(value *time.Time) pgtype.Timestamptz {
	if value == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *value, Valid: true}
}

func platformOptionalInt(value int) *int64 {
	if value == 0 {
		return nil
	}
	converted := int64(value)
	return &converted
}

func optionalInt64(value int) *int64 { return platformOptionalInt(value) }
