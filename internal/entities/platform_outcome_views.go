package entities

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
	"github.com/mbvlabs/eregion/internal/platform"
)

type ProtectionFacts struct {
	Now       time.Time
	UpdatedAt time.Time
	ID        platform.EnvironmentID
	Protected bool
}

type DeletionCounts struct {
	Environments    int64
	Releases        int64
	Deployments     int64
	Builds          int64
	Domains         int64
	Secrets         int64
	AppEnvironments int64
}

type WebhookBinding struct {
	App               string
	Environment       string
	CurrentRevisionID platform.RevisionID
	AutoDeploy        bool
	WebhookSecretName string
}

type PushWebhookBinding struct {
	WebhookBinding
	EnvironmentID     platform.EnvironmentID
	Source            json.RawMessage
	SecretState       string
	LatestRevision    int64
	Revision          int64
	KeyID             string
	Ciphertext        []byte
	CiphertextSHA256  string
	RevisionHash      string
	RevisionCreatedAt *time.Time
}

type SecretBindingSnapshot struct {
	CurrentRevisionID platform.RevisionID
	AutoDeploy        bool
	WebhookSecretName string
	SecretState       string
	LatestRevision    int64
	RevisionHash      string
}

type DefinitionChildIdentity struct {
	App         string
	Environment string
	ID          platform.EnvironmentID
}

type DefinitionCleanupState struct {
	DNSProgress     json.RawMessage
	DNSPlan         json.RawMessage
	DNSCleanup      json.RawMessage
	Input           json.RawMessage
	Approved        json.RawMessage
	DomainID        platform.DomainID
	State           string
	LastError       string
	CleanupError    string
	LastOperationID contract.OperationID
}

type OutcomeStore struct{ q *query.Queries }

func (s *OutcomeStore) ProtectionFacts(ctx context.Context, app, environment string) (ProtectionFacts, bool, error) {
	row, err := s.q.GetPlatformProtectionActionFacts(ctx, query.GetPlatformProtectionActionFactsParams{App: app, Environment: environment})
	if errors.Is(err, pgx.ErrNoRows) {
		return ProtectionFacts{}, false, nil
	}
	if err != nil {
		return ProtectionFacts{}, false, err
	}
	now, err := timestampAny(row.ClockTimestamp)
	if err != nil {
		return ProtectionFacts{}, false, err
	}
	updated, err := platformTime(row.UpdatedAt)
	if err != nil {
		return ProtectionFacts{}, false, err
	}
	return ProtectionFacts{Now: now, UpdatedAt: updated, ID: platform.EnvironmentID(row.ID), Protected: row.Protected}, true, nil
}

func (s *OutcomeStore) DeletionCounts(ctx context.Context, app, environment string) (DeletionCounts, error) {
	row, err := s.q.GetPlatformDeletionCounts(ctx, query.GetPlatformDeletionCountsParams{
		App: app, Environment: platformOptionalText(environment),
	})
	if err != nil {
		return DeletionCounts{}, err
	}
	return DeletionCounts{
		Environments: row.Count, Releases: row.Count_2, Deployments: row.Count_3,
		Builds: row.Count_4, Domains: row.Count_5, Secrets: row.Count_6, AppEnvironments: row.Count_7,
	}, nil
}

func (s *OutcomeStore) WebhookBinding(ctx context.Context, environmentID platform.EnvironmentID) (WebhookBinding, bool, error) {
	row, err := s.q.GetPlatformWebhookBinding(ctx, string(environmentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return WebhookBinding{}, false, nil
	}
	return WebhookBinding{
		App: row.Name, Environment: row.Name_2, CurrentRevisionID: platform.RevisionID(row.CurrentRevisionID),
		AutoDeploy: row.AutoDeploy, WebhookSecretName: row.WebhookSecretName,
	}, err == nil, err
}

func (s *OutcomeStore) PushWebhookBinding(ctx context.Context, app, environment string) (PushWebhookBinding, bool, error) {
	row, err := s.q.GetPlatformPushWebhookBinding(ctx, query.GetPlatformPushWebhookBindingParams{App: app, Environment: environment})
	if errors.Is(err, pgx.ErrNoRows) {
		return PushWebhookBinding{}, false, nil
	}
	if err != nil {
		return PushWebhookBinding{}, false, err
	}
	return PushWebhookBinding{
		WebhookBinding: WebhookBinding{
			App: row.Name, Environment: row.Name_2, CurrentRevisionID: platform.RevisionID(row.CurrentRevisionID),
			AutoDeploy: row.AutoDeploy, WebhookSecretName: row.WebhookSecretName,
		},
		EnvironmentID: platform.EnvironmentID(row.ID),
		Source:        row.Source, SecretState: row.State, LatestRevision: row.LatestRevision,
		Revision: row.Revision, KeyID: row.KeyID, Ciphertext: row.Ciphertext,
		CiphertextSHA256: row.CiphertextSha256, RevisionHash: row.RevisionHash,
		RevisionCreatedAt: platformTimePtr(row.CreatedAt),
	}, true, nil
}

func (s *OutcomeStore) SecretBinding(ctx context.Context, environmentID platform.EnvironmentID) (SecretBindingSnapshot, bool, error) {
	row, err := s.q.GetPlatformSecretBindingSnapshot(ctx, string(environmentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return SecretBindingSnapshot{}, false, nil
	}
	return SecretBindingSnapshot{
		CurrentRevisionID: platform.RevisionID(row.CurrentRevisionID), AutoDeploy: row.AutoDeploy,
		WebhookSecretName: row.WebhookSecretName, SecretState: row.State,
		LatestRevision: row.LatestRevision, RevisionHash: coalesceString(row.Coalesce),
	}, err == nil, err
}

func (s *OutcomeStore) DefinitionChildIdentity(ctx context.Context, operationID contract.OperationID) (DefinitionChildIdentity, bool, error) {
	row, err := s.q.GetPlatformDefinitionChildEnvironmentIdentity(ctx, string(operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DefinitionChildIdentity{}, false, nil
	}
	return DefinitionChildIdentity{App: row.Name, Environment: row.Name_2, ID: platform.EnvironmentID(row.ID)}, err == nil, err
}

func (s *OutcomeStore) DomainDNSDefinitionCleanupState(ctx context.Context, operationID contract.OperationID) (DefinitionCleanupState, bool, error) {
	row, err := s.q.ListPlatformDomainDNSDefinitionCleanupState(ctx, string(operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DefinitionCleanupState{}, false, nil
	}
	if err != nil {
		return DefinitionCleanupState{}, false, err
	}
	return DefinitionCleanupState{
		DNSProgress: row.DnsProgress, DNSPlan: derefRaw(row.DnsPlan), DNSCleanup: derefRaw(row.DnsCleanup), Input: row.Input,
		Approved: row.Approved, DomainID: platform.DomainID(row.DomainID), State: row.State,
		LastError: row.LastError, CleanupError: row.CleanupError, LastOperationID: contract.OperationID(row.LastOperationID),
	}, true, nil
}

func (s *OutcomeStore) DomainDNSDefinitionCleanupPending(ctx context.Context, parentOperationID contract.OperationID) (bool, error) {
	return s.q.PlatformDomainDNSDefinitionCleanupPending(ctx, string(parentOperationID))
}

func (s *OutcomeStore) DefinitionReservationExists(ctx context.Context, app, environment string, operationID contract.OperationID, kind, target string, input json.RawMessage, parentKind string, actor contract.Actor) (bool, error) {
	return s.q.PlatformDefinitionReservationExists(ctx, query.PlatformDefinitionReservationExistsParams{
		App: app, Environment: platformOptionalText(environment), OperationID: string(operationID),
		Kind: kind, Target: target, Input: input, ParentKind: parentKind,
		ActorKind: actor.Kind, ActorSubject: actor.Subject,
	})
}

type DefinitionCleanupCounts struct {
	Successes  int64
	Authorized int64
	Error      string
}

func (s *OutcomeStore) DefinitionCleanupCounts(ctx context.Context, parentOperationID contract.OperationID, ordinal int, successKind, authzKind, failedKind string, operationID contract.OperationID) (DefinitionCleanupCounts, error) {
	row, err := s.q.GetPlatformDefinitionCleanupCounts(ctx, query.GetPlatformDefinitionCleanupCountsParams{
		ParentOperationID: string(parentOperationID), Ordinal: int64(ordinal), SuccessKind: successKind,
		AuthzKind: authzKind, FailedKind: failedKind, OperationID: string(operationID),
	})
	if err != nil {
		return DefinitionCleanupCounts{}, err
	}
	return DefinitionCleanupCounts{Successes: row.Count, Authorized: row.Count_2, Error: coalesceString(row.Coalesce)}, nil
}
