package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
)

// The payloads below deliberately contain only individually selected fields.
// In particular, revision configuration and operation input are never emitted.
type actionRevision struct {
	ID                     string          `json:"id"`
	EnvironmentID          string          `json:"environment_id"`
	Environment            string          `json:"environment"`
	AppID                  string          `json:"app_id"`
	App                    string          `json:"app"`
	Number                 int             `json:"number"`
	DeployIntent           bool            `json:"deploy_intent"`
	FirstDeployOperationID string          `json:"first_deploy_operation_id,omitempty"`
	ChangedConfigKeys      []string        `json:"changed_config_keys,omitempty"`
	ChangedConfigAvailable bool            `json:"changed_config_available"`
	Processes              []actionProcess `json:"processes,omitempty"`
	SecretNames            []string        `json:"secret_names,omitempty"`
	Resources              []string        `json:"resources,omitempty"`
}

type actionProcess struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Replicas int    `json:"replicas"`
}

type revisionPayload struct {
	Availability string           `json:"availability"`
	Reason       string           `json:"reason,omitempty"`
	AppName      string           `json:"app_name,omitempty"`
	CurrentApp   *appObservation  `json:"current_app,omitempty"`
	Revisions    []actionRevision `json:"revisions,omitempty"`
}

type appObservation struct {
	ID         string    `json:"id"`
	ObservedAt time.Time `json:"observed_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type executionPayload struct {
	Availability         string                 `json:"availability"`
	Reason               string                 `json:"reason,omitempty"`
	BuildID              string                 `json:"build_id,omitempty"`
	BuildState           string                 `json:"build_state,omitempty"`
	CommitSHA            string                 `json:"commit_sha,omitempty"`
	DeploymentID         string                 `json:"deployment_id,omitempty"`
	DeployState          string                 `json:"deployment_state,omitempty"`
	RevisionID           string                 `json:"revision_id,omitempty"`
	ReleaseID            string                 `json:"release_id,omitempty"`
	Image                string                 `json:"image,omitempty"`
	ImageID              string                 `json:"image_id,omitempty"`
	PreviousReleaseID    string                 `json:"previous_release_id,omitempty"`
	PreviousDeploymentID string                 `json:"previous_deployment_id,omitempty"`
	Steps                []diagnosticStep       `json:"steps,omitempty"`
	CurrentObservation   *deploymentObservation `json:"current_observation,omitempty"`
}

type deploymentObservation struct {
	ObservedAt              time.Time  `json:"observed_at"`
	EnvironmentUpdatedAt    time.Time  `json:"environment_updated_at"`
	ServingDeploymentID     string     `json:"serving_deployment_id,omitempty"`
	ActionDeploymentServing bool       `json:"action_deployment_serving"`
	ServingReleaseID        string     `json:"serving_release_id,omitempty"`
	DesiredReleaseID        string     `json:"desired_release_id,omitempty"`
	AppliedReleaseID        string     `json:"applied_release_id,omitempty"`
	HealthState             string     `json:"health_state"`
	HealthCheckedAt         *time.Time `json:"health_checked_at,omitempty"`
	OwnerOperationID        string     `json:"owner_operation_id,omitempty"`
	OwnerDeploymentID       string     `json:"owner_deployment_id,omitempty"`
	OwnerPhase              string     `json:"owner_phase,omitempty"`
	RouteTargetDeploymentID string     `json:"route_target_deployment_id,omitempty"`
	CandidateCleanupPending int        `json:"candidate_cleanup_pending"`
	PreviousCleanupPending  int        `json:"previous_cleanup_pending"`
}

type definitionChild struct {
	Ordinal     int            `json:"ordinal"`
	Kind        string         `json:"kind"`
	Target      string         `json:"target"`
	OperationID string         `json:"operation_id,omitempty"`
	Status      contract.State `json:"status,omitempty"`
}

type definitionPayload struct {
	Availability  string            `json:"availability"`
	Reason        string            `json:"reason,omitempty"`
	EnvironmentID string            `json:"environment_id,omitempty"`
	FinishedAt    *time.Time        `json:"finished_at,omitempty"`
	Children      []definitionChild `json:"children,omitempty"`
}

type retentionPayload struct {
	Availability     string                  `json:"availability"`
	Reason           string                  `json:"reason,omitempty"`
	RunID            string                  `json:"run_id,omitempty"`
	Status           string                  `json:"status,omitempty"`
	PlannedBytes     int64                   `json:"planned_bytes"`
	ReclaimedBytes   int64                   `json:"reclaimed_bytes"`
	StartedAt        *time.Time              `json:"started_at,omitempty"`
	FinishedAt       *time.Time              `json:"finished_at,omitempty"`
	Planned          []retentionPayloadItem  `json:"planned,omitempty"`
	Removed          []retentionPayloadItem  `json:"removed,omitempty"`
	PlannedCount     int                     `json:"planned_count"`
	RemovedCount     int                     `json:"removed_count"`
	SkippedCount     int                     `json:"skipped_count"`
	PlannedSkipCount int                     `json:"planned_skip_count"`
	ErrorCount       int                     `json:"error_count"`
	Errors           []retentionPayloadError `json:"errors,omitempty"`
	CacheBytes       *int64                  `json:"cache_bytes,omitempty"`
	CacheMeasuredAt  *time.Time              `json:"cache_measured_at,omitempty"`
	CacheError       string                  `json:"cache_measurement_error,omitempty"`
	GCGeneration     int                     `json:"gc_generation"`
}

type retentionPayloadItem struct {
	Kind        retention.ItemKind `json:"kind"`
	App         string             `json:"app,omitempty"`
	Environment string             `json:"environment,omitempty"`
	Release     string             `json:"release,omitempty"`
	Reference   string             `json:"reference,omitempty"`
	Tags        []string           `json:"tags,omitempty"`
	Bytes       int64              `json:"bytes"`
	Reason      string             `json:"reason,omitempty"`
}

type retentionPayloadError struct {
	Stage     string `json:"stage,omitempty"`
	Reference string `json:"reference,omitempty"`
	Message   string `json:"message"`
}

type unavailablePayload struct {
	Availability string `json:"availability"`
	Reason       string `json:"reason"`
}

type protectionObservation struct {
	ObservedAt    time.Time `json:"observed_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	EnvironmentID string    `json:"environment_id"`
	Protected     bool      `json:"protected"`
}
type protectionPayload struct {
	Availability       string                 `json:"availability"`
	Reason             string                 `json:"reason,omitempty"`
	App                string                 `json:"app"`
	Environment        string                 `json:"environment"`
	DesiredProtected   bool                   `json:"desired_protected"`
	CurrentObservation *protectionObservation `json:"current_observation,omitempty"`
}

// Current records can be overwritten by later actions. Their observation time
// and last linked operation make that distinction explicit.
type nodeObservation struct {
	ObservedAt          time.Time  `json:"observed_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	Roles               []string   `json:"roles"`
	Capabilities        []string   `json:"capabilities"`
	Status              string     `json:"status"`
	EnrollmentState     string     `json:"enrollment_state"`
	EnrollmentOperation string     `json:"enrollment_operation_id,omitempty"`
	EnrolledAt          *time.Time `json:"enrolled_at,omitempty"`
	IdentityFingerprint string     `json:"identity_fingerprint,omitempty"`
	LastCheckState      string     `json:"last_check_state,omitempty"`
	LastCheckAt         *time.Time `json:"last_check_at,omitempty"`
	WireGuardState      string     `json:"wireguard_state,omitempty"`
}

type nodePayload struct {
	Availability         string           `json:"availability"`
	Reason               string           `json:"reason,omitempty"`
	Name                 string           `json:"name,omitempty"`
	DeclaredRoles        []string         `json:"declared_roles,omitempty"`
	DeclaredCapabilities []string         `json:"declared_capabilities,omitempty"`
	CurrentObservation   *nodeObservation `json:"current_observation,omitempty"`
}

type actionDomainObservation struct {
	ObservedAt            time.Time  `json:"observed_at"`
	ID                    string     `json:"id"`
	EnvironmentID         string     `json:"environment_id"`
	Primary               bool       `json:"primary"`
	DesiredRevision       int64      `json:"desired_revision"`
	AppliedRevision       int64      `json:"applied_revision"`
	RouteState            string     `json:"route_state"`
	RouteObservedAt       *time.Time `json:"route_observed_at,omitempty"`
	RouteAppliedAt        *time.Time `json:"route_applied_at,omitempty"`
	DNSState              string     `json:"dns_state"`
	DNSObservedAt         *time.Time `json:"dns_observed_at,omitempty"`
	CertificateState      string     `json:"certificate_state"`
	CertificateObservedAt *time.Time `json:"certificate_observed_at,omitempty"`
	PublicState           string     `json:"public_state"`
	PublicObservedAt      *time.Time `json:"public_observed_at,omitempty"`
	Provider              string     `json:"provider,omitempty"`
	ZoneName              string     `json:"zone_name,omitempty"`
	OwnershipMarker       string     `json:"ownership_marker,omitempty"`
	Generation            *int64     `json:"generation,omitempty"`
	AppliedGeneration     *int64     `json:"applied_generation,omitempty"`
	DesiredOrigins        []string   `json:"desired_origins,omitempty"`
	DesiredProxied        *bool      `json:"desired_proxied,omitempty"`
	ProviderState         string     `json:"provider_state,omitempty"`
	LastOperationID       string     `json:"last_operation_id,omitempty"`
	LedgerUpdatedAt       *time.Time `json:"ledger_updated_at,omitempty"`
	CleanupRequestedAt    *time.Time `json:"cleanup_requested_at,omitempty"`
	CleanupVerifiedAt     *time.Time `json:"cleanup_verified_at,omitempty"`
}

type domainPayload struct {
	Availability       string                   `json:"availability"`
	Reason             string                   `json:"reason,omitempty"`
	Name               string                   `json:"name,omitempty"`
	App                string                   `json:"app,omitempty"`
	Environment        string                   `json:"environment,omitempty"`
	CurrentObservation *actionDomainObservation `json:"current_observation,omitempty"`
}

type resourceObservation struct {
	ObservedAt      time.Time  `json:"observed_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	ID              string     `json:"id"`
	EnvironmentID   string     `json:"environment_id"`
	Node            string     `json:"node"`
	State           string     `json:"state"`
	Image           string     `json:"image,omitempty"`
	ImageID         string     `json:"image_id,omitempty"`
	ContainerName   string     `json:"container_name,omitempty"`
	NetworkName     string     `json:"network_name,omitempty"`
	VolumeNames     []string   `json:"volume_names,omitempty"`
	HealthState     string     `json:"health_state"`
	HealthCheckedAt *time.Time `json:"health_checked_at,omitempty"`
	LastOperationID string     `json:"last_operation_id,omitempty"`
	DestroyedAt     *time.Time `json:"destroyed_at,omitempty"`
}

type resourcePayload struct {
	Availability        string               `json:"availability"`
	Reason              string               `json:"reason,omitempty"`
	App                 string               `json:"app,omitempty"`
	Environment         string               `json:"environment,omitempty"`
	Name                string               `json:"name,omitempty"`
	DeleteDataRequested *bool                `json:"delete_data_requested,omitempty"`
	DataOutcome         string               `json:"data_outcome,omitempty"`
	CurrentObservation  *resourceObservation `json:"current_observation,omitempty"`
}

type buildObservation struct {
	ObservedAt  time.Time  `json:"observed_at"`
	ID          string     `json:"id"`
	OperationID string     `json:"operation_id"`
	State       string     `json:"state"`
	CommitSHA   string     `json:"commit_sha,omitempty"`
	RevisionID  string     `json:"revision_id,omitempty"`
	ReleaseID   string     `json:"release_id,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type buildStopPayload struct {
	Availability       string            `json:"availability"`
	Reason             string            `json:"reason,omitempty"`
	TargetBuildID      string            `json:"target_build_id,omitempty"`
	CurrentObservation *buildObservation `json:"current_observation,omitempty"`
}

type diagnosticStep struct {
	Step        string     `json:"step"`
	State       string     `json:"state"`
	Attempt     uint32     `json:"attempt"`
	ExecutionID string     `json:"execution_id,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type diagnosticPayloadResult struct {
	Availability      string           `json:"availability"`
	RequestedSteps    int              `json:"requested_steps"`
	AcknowledgedSteps int              `json:"acknowledged_steps"`
	Steps             []diagnosticStep `json:"steps,omitempty"`
}

// actionPayloadTx projects durable, operation-linked facts while settlement
// holds the operation lock. Absence is represented explicitly; it is not
// inferred from the action's requested input or from unrelated current rows.
func (s *Service) actionPayloadTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, status contract.State) (json.RawMessage, error) {
	var payload any
	switch op.Kind {
	case platform.KindAppDelete, platform.KindEnvironmentDelete:
		payload = unavailablePayload{Availability: "unavailable", Reason: "Deletion outcome requires the pre-delete snapshot supplied by the deletion worker."}
	case platform.KindEnvironmentProtect:
		result, err := protectionActionPayload(ctx, tx, op)
		if err != nil {
			return nil, err
		}
		payload = result
	case platform.KindNodeRegister, platform.KindNodeEnroll, platform.KindNodeReconcile, platform.KindNodeCheck:
		result, err := nodeActionPayload(ctx, tx, op)
		if err != nil {
			return nil, err
		}
		payload = result
	case platform.KindDomainAdd, platform.KindDomainRemove:
		result, err := domainActionPayload(ctx, tx, op)
		if err != nil {
			return nil, err
		}
		payload = result
	case platform.KindResourceApply, platform.KindResourceDestroy:
		result, err := resourceActionPayload(ctx, tx, op)
		if err != nil {
			return nil, err
		}
		payload = result
	case platform.KindBuildStop:
		result, err := buildStopActionPayload(ctx, tx, op)
		if err != nil {
			return nil, err
		}
		payload = result
	case operations.TestKind:
		result, err := diagnosticActionPayload(ctx, tx, op)
		if err != nil {
			return nil, err
		}
		payload = result
	case platform.KindAppCreate, platform.KindEnvironmentCreate, platform.KindEnvironmentUpdate,
		platform.KindProcessScale, platform.KindConfigSet, platform.KindConfigUnset,
		platform.KindEnvironmentSecretAttach, platform.KindEnvironmentResourceAttach, platform.KindEnvironmentResourceDetach:
		result := revisionPayload{Availability: "unavailable", Reason: "No revision was recorded for this operation, or its revision no longer exists."}
		if op.Kind == platform.KindAppCreate {
			var in platform.AppCreateInput
			if err := actionInputTx(ctx, tx, op.ID, &in); err != nil {
				return nil, err
			}
			result.AppName = in.Name
			app, found, err := tx.Platform().Apps().ActionTimestamps(ctx, in.Name)
			if err != nil {
				return nil, err
			}
			if found {
				result.CurrentApp = &appObservation{ID: string(app.ID), ObservedAt: app.Now, UpdatedAt: app.UpdatedAt}
			}
			result.Availability = "partial"
		}
		rows, err := tx.Platform().Revisions().FirstDeployActionRows(ctx, op.ID)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			revision := actionRevision{
				ID: string(row.RevisionID), EnvironmentID: string(row.EnvironmentID), Environment: row.Environment,
				AppID: string(row.AppID), App: row.App, Number: row.Number, DeployIntent: row.DeployIntent,
				FirstDeployOperationID: string(row.DeployOpID), ChangedConfigAvailable: row.HasPrevious,
				SecretNames: row.SecretNames, Resources: row.Resources,
			}
			for _, process := range row.Processes {
				revision.Processes = append(revision.Processes, actionProcess{Name: process.Name, Kind: process.Kind, Replicas: process.Replicas})
			}
			currentConfig, err := rawConfig(row.Config)
			if err != nil {
				return nil, err
			}
			previousConfig, err := rawConfig(row.PreviousConfig)
			if err != nil {
				return nil, err
			}
			if revision.ChangedConfigAvailable {
				for key, value := range currentConfig {
					if old, ok := previousConfig[key]; !ok || !bytes.Equal(value, old) {
						revision.ChangedConfigKeys = append(revision.ChangedConfigKeys, key)
					}
				}
				for key := range previousConfig {
					if _, ok := currentConfig[key]; !ok {
						revision.ChangedConfigKeys = append(revision.ChangedConfigKeys, key)
					}
				}
			}
			sort.Strings(revision.ChangedConfigKeys)
			result.Revisions = append(result.Revisions, revision)
		}
		if len(result.Revisions) > 0 {
			result.Availability = "available"
			result.Reason = ""
		}
		payload = result
	case platform.KindReleaseCreate, platform.KindBuildRetry, platform.KindDeploy, platform.KindRollback, platform.KindRestart, platform.KindPromote:
		result := executionPayload{Availability: "unavailable", Reason: "No build, release or deployment linked to this operation remains available."}
		facts, found, err := tx.Platform().Deployments().BuildDeploymentFacts(ctx, op.ID)
		if err != nil {
			return nil, err
		}
		if found {
			result.BuildID = string(facts.BuildID)
			result.BuildState = string(facts.BuildState)
			result.CommitSHA = facts.CommitSHA
			result.DeploymentID = string(facts.DeploymentID)
			result.DeployState = string(facts.DeployState)
			result.RevisionID = string(facts.RevisionID)
			result.ReleaseID = string(facts.ReleaseID)
			result.Image = facts.Image
			result.ImageID = facts.ImageID
		}
		if result.BuildID != "" || result.DeploymentID != "" || result.ReleaseID != "" {
			result.Availability = "partial"
			result.Reason = "Step and serving facts are limited to durable records and current observations."
		}
		if result.DeploymentID != "" {
			if err := executionDeploymentFacts(ctx, tx, op, &result); err != nil {
				return nil, err
			}
		}
		payload = result
	case platform.KindDefinitionApply:
		result := definitionPayload{Availability: "unavailable", Reason: "No definition run linked to this operation remains available."}
		run, found, err := tx.DefinitionRuns().Get(ctx, op.ID)
		if err != nil {
			return nil, err
		}
		if found {
			result.Availability = "available"
			result.Reason = ""
			result.EnvironmentID = run.EnvironmentID
			result.FinishedAt = run.FinishedAt
			rows, err := tx.DefinitionSteps().ListChildJournal(ctx, op.ID)
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				result.Children = append(result.Children, definitionChild{
					Ordinal: row.Ordinal, Kind: row.Kind, Target: row.Target,
					OperationID: string(row.ChildID), Status: row.ChildState,
				})
			}
		}
		payload = result
	case platform.KindRetentionPrune:
		result, err := s.retentionActionPayload(ctx, tx, op)
		if err != nil {
			return nil, err
		}
		payload = result
	default:
		reason := "This operation kind is not registered and has no built-in result projector."
		if def, ok := platform.Default().Lookup(op.Kind); ok && def.Executor != nil {
			reason = "The extension did not provide result data; the platform has no extension-specific projector."
		}
		payload = unavailablePayload{Availability: "unavailable", Reason: reason}
	}
	return json.Marshal(payload)
}

func rawConfig(values map[string]string) (map[string]json.RawMessage, error) {
	result := make(map[string]json.RawMessage, len(values))
	for key, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		result[key] = raw
	}
	return result, nil
}

func actionInputTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, dst any) error {
	raw, found, err := tx.Operations().Input(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return contract.NewFault(contract.NotFound, "operation input not found", "Read the operation before projecting its input.")
	}
	return json.Unmarshal(raw, dst)
}

func protectionActionPayload(ctx context.Context, tx *entities.StoreTx, op *contract.Operation) (protectionPayload, error) {
	result := protectionPayload{Availability: "partial"}
	target, err := platform.ParseTarget(op.Target)
	if err != nil || target.Kind != platform.TargetEnvironment {
		return result, errors.New("invalid protection action target")
	}
	var in platform.EnvironmentProtectInput
	if err := actionInputTx(ctx, tx, op.ID, &in); err != nil {
		return result, err
	}
	result.App, result.Environment, result.DesiredProtected = target.App, target.Environment, in.Protected
	facts, found, err := tx.Platform().Outcomes().ProtectionFacts(ctx, target.App, target.Environment)
	if err != nil {
		return result, err
	}
	if found {
		result.CurrentObservation = &protectionObservation{
			ObservedAt: facts.Now, UpdatedAt: facts.UpdatedAt, EnvironmentID: string(facts.ID), Protected: facts.Protected,
		}
	} else {
		result.Reason = "The environment no longer exists; its current protection cannot be observed."
	}
	return result, nil
}

func executionDeploymentFacts(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, result *executionPayload) error {
	facts, found, err := tx.Platform().Deployments().ExecutionFacts(ctx, platform.DeploymentID(result.DeploymentID), op.ID)
	if err != nil {
		return err
	}
	if found {
		result.PreviousReleaseID = string(facts.PreviousReleaseID)
		result.PreviousDeploymentID = string(facts.PreviousDeploymentID)
		observation := deploymentObservation{
			ObservedAt: facts.Now, EnvironmentUpdatedAt: facts.EnvironmentUpdatedAt,
			ServingDeploymentID: string(facts.ServingDeploymentID), ServingReleaseID: string(facts.ServingReleaseID),
			DesiredReleaseID: string(facts.DesiredReleaseID), AppliedReleaseID: string(facts.AppliedReleaseID),
			HealthState: string(facts.HealthState), HealthCheckedAt: facts.HealthCheckedAt,
			OwnerOperationID: string(facts.OwnerOperationID), OwnerDeploymentID: string(facts.OwnerDeploymentID),
			OwnerPhase: facts.OwnerPhase, RouteTargetDeploymentID: string(facts.RouteTarget),
			CandidateCleanupPending: int(facts.CleanupCount), PreviousCleanupPending: int(facts.PreviousCleanupCount),
			ActionDeploymentServing: string(facts.ServingDeploymentID) == result.DeploymentID,
		}
		result.CurrentObservation = &observation
	}
	records, err := tx.Steps().ListByPattern(ctx, op.ID,
		[]string{"platform-health-%", "platform-cleanup-%"},
		[]string{deploymentRouteStep, deploymentPublicStep, deploymentRestoreRouteStep},
	)
	if err != nil {
		return err
	}
	for _, record := range records {
		result.Steps = append(result.Steps, diagnosticStep{
			Step: record.Step, State: record.State, Attempt: record.Attempt,
			ExecutionID: record.ExecutionID, StartedAt: record.StartedAt, CompletedAt: record.CompletedAt,
		})
	}
	return nil
}

func (s *Service) retentionActionPayload(ctx context.Context, tx *entities.StoreTx, op *contract.Operation) (retentionPayload, error) {
	result := retentionPayload{Availability: "unavailable", Reason: "No retention run linked to this operation remains available."}
	run, found, err := tx.Retention().LatestRunForOperation(ctx, op.ID)
	if err != nil {
		return result, err
	}
	if !found {
		return result, nil
	}
	result.RunID, result.Status = run.ID, run.Status
	result.PlannedBytes, result.ReclaimedBytes = run.PlannedBytes, run.ReclaimedBytes
	result.StartedAt, result.FinishedAt = &run.StartedAt, run.FinishedAt
	result.CacheBytes, result.CacheMeasuredAt = run.CacheBytes, run.CacheMeasuredAt
	result.CacheError, result.GCGeneration = run.CacheMeasurementError, run.GCGeneration
	var plan, removals []retention.Item
	var errs []retention.RunError
	if err := json.Unmarshal(run.PlannedItems, &plan); err != nil {
		return result, err
	}
	if err := json.Unmarshal(run.RemovedItems, &removals); err != nil {
		return result, err
	}
	if err := json.Unmarshal(run.Errors, &errs); err != nil {
		return result, err
	}
	project := func(item retention.Item) retentionPayloadItem {
		kind := item.Kind
		switch kind {
		case retention.ItemRelease, retention.ItemRegistryArtifact, retention.ItemRegistrySkipped, retention.ItemRegistryGC,
			retention.ItemImage, retention.ItemImageSkipped, retention.ItemCacheSkipped, retention.ItemBuildpackCache,
			retention.ItemContainer, retention.ItemDanglingImage:
		default:
			kind = "unknown"
		}
		value := retentionPayloadItem{Kind: kind, App: s.resultText(item.App), Environment: s.resultText(item.Environment),
			Release: s.resultText(item.Release), Reference: s.resultText(item.Reference), Bytes: item.Bytes, Reason: s.resultText(item.Reason)}
		for _, tag := range item.Tags {
			value.Tags = append(value.Tags, s.resultText(tag))
		}
		return value
	}
	for _, item := range plan {
		result.Planned = append(result.Planned, project(item))
		if item.Kind == retention.ItemRegistrySkipped || item.Kind == retention.ItemImageSkipped || item.Kind == retention.ItemCacheSkipped {
			result.PlannedSkipCount++
		}
	}
	for _, item := range removals {
		result.Removed = append(result.Removed, project(item))
	}
	for _, item := range errs {
		result.Errors = append(result.Errors, retentionPayloadError{Stage: s.resultText(item.Stage), Reference: s.resultText(item.Reference), Message: s.resultText(item.Message)})
		if item.Stage == "skipped" || item.Stage == "stale_plan" || strings.HasSuffix(item.Stage, "_skipped") {
			result.SkippedCount++
		}
	}
	result.PlannedCount, result.RemovedCount = len(plan), len(removals)
	result.ErrorCount = len(errs)
	result.CacheError = s.resultText(result.CacheError)
	result.Availability, result.Reason = "available", ""
	return result, nil
}

func nodeActionPayload(ctx context.Context, tx *entities.StoreTx, op *contract.Operation) (nodePayload, error) {
	result := nodePayload{Availability: "partial", Reason: "The selected node has no current record; registration is not authentication."}
	if op.Kind == platform.KindNodeRegister {
		var in platform.NodeRegisterInput
		if err := actionInputTx(ctx, tx, op.ID, &in); err != nil {
			return result, err
		}
		if err := platform.ValidateName(in.Name); err != nil {
			return result, err
		}
		result.Name, result.DeclaredRoles, result.DeclaredCapabilities = in.Name, in.Roles, in.Capabilities
	} else {
		target, err := platform.ParseTarget(op.Target)
		if err != nil || target.Kind != platform.TargetNode {
			return result, errors.New("invalid node action target")
		}
		result.Name = target.Node
		if op.Kind == platform.KindNodeEnroll {
			var in platform.NodeEnrollInput
			if err := actionInputTx(ctx, tx, op.ID, &in); err != nil {
				return result, err
			}
			result.DeclaredRoles = in.Roles
		}
	}
	facts, found, err := tx.Nodes().ActionFacts(ctx, result.Name)
	if err != nil {
		return result, err
	}
	if found {
		result.CurrentObservation = &nodeObservation{
			ObservedAt: facts.Now, UpdatedAt: facts.UpdatedAt, Roles: facts.Roles,
			Capabilities: facts.Capabilities, Status: string(facts.Status),
			EnrollmentState: string(facts.Enrollment), EnrollmentOperation: string(facts.EnrollmentOperation),
			EnrolledAt: facts.EnrolledAt, IdentityFingerprint: facts.IdentityFingerprint,
			LastCheckState: facts.LastCheckState, LastCheckAt: facts.LastCheckAt,
			WireGuardState: string(facts.WGState),
		}
		result.Reason = "Current node facts may have changed since this operation; registration alone is not verification."
	}
	return result, nil
}

func domainActionPayload(ctx context.Context, tx *entities.StoreTx, op *contract.Operation) (domainPayload, error) {
	result := domainPayload{Availability: "partial", Reason: "The selected binding is absent; a removed binding's identity and cleanup outcome require a pre-delete snapshot."}
	target, err := platform.ParseTarget(op.Target)
	if err != nil || target.Kind != platform.TargetEnvironment {
		return result, errors.New("invalid domain action target")
	}
	var in platform.DomainInput
	if err := actionInputTx(ctx, tx, op.ID, &in); err != nil {
		return result, err
	}
	result.App, result.Environment, result.Name = target.App, target.Environment, in.Domain
	facts, found, err := tx.Platform().Domains().ActionFacts(ctx, result.App, result.Environment, result.Name)
	if err != nil {
		return result, err
	}
	if found {
		observation := actionDomainObservation{
			ObservedAt: facts.Now, ID: string(facts.ID), EnvironmentID: string(facts.EnvironmentID),
			Primary: facts.Primary, DesiredRevision: facts.DesiredRevision, AppliedRevision: facts.AppliedRevision,
			RouteState: string(facts.RouteState), RouteObservedAt: facts.RouteObservedAt,
			RouteAppliedAt: facts.RouteAppliedAt, DNSState: string(facts.DNSState),
			DNSObservedAt: facts.DNSObservedAt, CertificateState: string(facts.CertificateState),
			CertificateObservedAt: facts.CertificateObservedAt, PublicState: string(facts.PublicState),
			PublicObservedAt: facts.PublicObservedAt, Provider: facts.Provider, ZoneName: facts.ZoneName,
			OwnershipMarker: facts.OwnershipMarker, DesiredOrigins: facts.DesiredOrigins,
			ProviderState: facts.LedgerState, LastOperationID: string(facts.LastOperationID),
			LedgerUpdatedAt: facts.LedgerUpdatedAt, CleanupRequestedAt: facts.CleanupRequestedAt,
			CleanupVerifiedAt: facts.CleanupVerifiedAt,
		}
		if facts.LedgerState != "" {
			generation, applied := facts.Generation, facts.AppliedGeneration
			proxied := facts.DesiredProxied
			observation.Generation, observation.AppliedGeneration, observation.DesiredProxied = &generation, &applied, &proxied
		}
		result.CurrentObservation = &observation
		result.Reason = "The binding and DNS ledger are current observations and may have changed since this operation."
	}
	return result, nil
}

func resourceActionPayload(ctx context.Context, tx *entities.StoreTx, op *contract.Operation) (resourcePayload, error) {
	result := resourcePayload{Availability: "partial", Reason: "The selected resource has no current record; its historical state is unavailable."}
	target, err := platform.ParseTarget(op.Target)
	if err != nil || target.Kind != platform.TargetResource {
		return result, errors.New("invalid resource action target")
	}
	result.App, result.Environment, result.Name = target.App, target.Environment, target.Resource
	if op.Kind == platform.KindResourceDestroy {
		var in platform.ResourceDestroyInput
		if err := actionInputTx(ctx, tx, op.ID, &in); err != nil {
			return result, err
		}
		result.DeleteDataRequested = &in.DeleteData
		result.DataOutcome = "unavailable"
	}
	facts, found, err := tx.Resources().ActionFacts(ctx, result.App, result.Environment, result.Name)
	if err != nil {
		return result, err
	}
	if found {
		observation := resourceObservation{
			ObservedAt: facts.Now, UpdatedAt: facts.UpdatedAt, ID: string(facts.ID), EnvironmentID: string(facts.EnvironmentID),
			Node: facts.Node, State: string(facts.State), Image: facts.Image, ImageID: facts.ImageID,
			ContainerName: facts.ContainerName, NetworkName: facts.NetworkName,
			HealthState: string(facts.HealthState), HealthCheckedAt: facts.HealthCheckedAt,
			LastOperationID: string(facts.OperationID), DestroyedAt: facts.DestroyedAt,
		}
		var owned []platform.ResourceVolumeStatus
		if err := json.Unmarshal(facts.Volumes, &owned); err != nil {
			return result, err
		}
		var spec platform.ResourceSpec
		if err := json.Unmarshal(facts.Spec, &spec); err != nil {
			return result, err
		}
		if err := validateStoredResource(platform.Resource{
			ID: facts.ID, App: result.App, Environment: result.Environment, Name: result.Name,
			Node: facts.Node, State: facts.State, Spec: spec, Image: facts.Image, ImageID: facts.ImageID,
			ContainerName: facts.ContainerName, NetworkName: facts.NetworkName, Volumes: owned,
			HealthState: facts.HealthState, HealthChecked: facts.HealthCheckedAt,
			OperationID: facts.OperationID, DestroyedAt: facts.DestroyedAt,
		}); err != nil {
			return result, err
		}
		for _, volume := range owned {
			if platform.ResourceVolumeNameValid(volume.Name) && strings.HasPrefix(volume.Name, "er-res-"+strings.TrimPrefix(observation.ID, "res_")+"-") {
				observation.VolumeNames = append(observation.VolumeNames, volume.Name)
			}
		}
		result.CurrentObservation = &observation
		result.Reason = "Resource state is a current observation and may have changed since this operation."
		if result.DeleteDataRequested != nil && observation.LastOperationID == string(op.ID) && observation.State == string(platform.ResourceDestroyed) {
			if *result.DeleteDataRequested {
				result.DataOutcome = "delete_completed"
			} else {
				result.DataOutcome = "retained"
			}
		}
	}
	return result, nil
}

func buildStopActionPayload(ctx context.Context, tx *entities.StoreTx, op *contract.Operation) (buildStopPayload, error) {
	result := buildStopPayload{Availability: "partial", Reason: "The target build record is no longer available."}
	target, err := platform.ParseTarget(op.Target)
	if err != nil || target.Kind != platform.TargetBuild {
		return result, errors.New("invalid build stop target")
	}
	result.TargetBuildID = string(target.Build)
	facts, found, err := tx.Platform().Builds().ActionFacts(ctx, platform.BuildID(result.TargetBuildID))
	if err != nil {
		return result, err
	}
	if found {
		result.CurrentObservation = &buildObservation{
			ObservedAt: facts.Now, ID: string(facts.ID), OperationID: string(facts.OperationID),
			State: string(facts.State), CommitSHA: facts.CommitSHA, RevisionID: string(facts.RevisionID),
			ReleaseID: string(facts.ReleaseID), CompletedAt: facts.CompletedAt,
		}
		result.Reason = "Build state is a current observation, not proof of the stop action's historical outcome."
	}
	return result, nil
}

func diagnosticActionPayload(ctx context.Context, tx *entities.StoreTx, op *contract.Operation) (diagnosticPayloadResult, error) {
	result := diagnosticPayloadResult{Availability: "available"}
	var in operations.TestInput
	if err := actionInputTx(ctx, tx, op.ID, &in); err != nil {
		return result, err
	}
	result.RequestedSteps = in.Steps
	rows, err := tx.Steps().List(ctx, op.ID)
	if err != nil {
		return result, err
	}
	for _, record := range rows {
		step := diagnosticStep{
			Step: record.Step, State: record.State, Attempt: record.Attempt,
			ExecutionID: record.ExecutionID, StartedAt: record.StartedAt, CompletedAt: record.CompletedAt,
		}
		if step.State == "completed" {
			result.AcknowledgedSteps++
		}
		result.Steps = append(result.Steps, step)
	}
	return result, nil
}
