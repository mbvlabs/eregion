package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
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
func (s *Service) actionPayloadTx(ctx context.Context, tx pgx.Tx, op *contract.Operation, status contract.State) (json.RawMessage, error) {
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
			var app appObservation
			err := tx.QueryRow(ctx, `SELECT id,clock_timestamp(),updated_at FROM eregion_platform_apps WHERE name=$1`, in.Name).
				Scan(&app.ID, &app.ObservedAt, &app.UpdatedAt)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			if err == nil {
				result.CurrentApp = &app
			}
			result.Availability = "partial"
		}
		rows, err := tx.Query(ctx, `SELECT r.id,r.environment_id,e.name,a.id,a.name,r.number,r.deploy_intent,
			coalesce((SELECT o.id FROM eregion_operations o
				JOIN eregion_platform_deployments first_dep ON first_dep.operation_id=o.id
				WHERE r.deploy_intent AND o.kind='platform.deploy'
				AND o.idempotency_key='first-deploy:' || r.id
				AND o.actor_kind=source_op.actor_kind AND o.actor_subject=source_op.actor_subject
				AND o.target='env/' || a.name || '/' || e.name
				AND first_dep.revision_id=r.id AND first_dep.environment_id=r.environment_id),''),
			r.processes,r.config,coalesce(p.config,'{}'::jsonb),(r.number=1 OR p.id IS NOT NULL),r.secret_names,r.resources
			FROM eregion_platform_environment_revisions r
			JOIN eregion_operations source_op ON source_op.id=r.operation_id
			JOIN eregion_platform_environments e ON e.id=r.environment_id
			JOIN eregion_platform_apps a ON a.id=e.app_id
			LEFT JOIN LATERAL (SELECT id,config FROM eregion_platform_environment_revisions prev
				WHERE prev.environment_id=r.environment_id AND prev.number=r.number-1) p ON true
			WHERE r.operation_id=$1 ORDER BY a.name,e.name,r.number`, op.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var revision actionRevision
			var processes, config, previous []byte
			if err := rows.Scan(&revision.ID, &revision.EnvironmentID, &revision.Environment, &revision.AppID, &revision.App, &revision.Number, &revision.DeployIntent, &revision.FirstDeployOperationID, &processes, &config, &previous, &revision.ChangedConfigAvailable, &revision.SecretNames, &revision.Resources); err != nil {
				rows.Close()
				return nil, err
			}
			if err := json.Unmarshal(processes, &revision.Processes); err != nil {
				rows.Close()
				return nil, err
			}
			var currentConfig, previousConfig map[string]json.RawMessage
			if err := json.Unmarshal(config, &currentConfig); err != nil {
				rows.Close()
				return nil, err
			}
			if err := json.Unmarshal(previous, &previousConfig); err != nil {
				rows.Close()
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
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(result.Revisions) > 0 {
			result.Availability = "available"
			result.Reason = ""
		}
		payload = result
	case platform.KindReleaseCreate, platform.KindBuildRetry, platform.KindDeploy, platform.KindRollback, platform.KindRestart, platform.KindPromote:
		result := executionPayload{Availability: "unavailable", Reason: "No build, release or deployment linked to this operation remains available."}
		err := tx.QueryRow(ctx, `SELECT coalesce(b.id,''),coalesce(b.state,''),coalesce(b.commit_sha,''),
			coalesce(d.id,''),coalesce(d.state,''),coalesce(d.revision_id,b.revision_id,''),
			coalesce(d.release_id,b.release_id,r.id,''),coalesce(r.image,''),coalesce(r.image_id,'')
			FROM (SELECT $1::text AS operation_id) o
			LEFT JOIN eregion_platform_builds b ON b.operation_id=o.operation_id
			LEFT JOIN eregion_platform_deployments d ON d.operation_id=o.operation_id
			LEFT JOIN eregion_platform_releases r ON r.id=coalesce(d.release_id,b.release_id)
				OR (d.id IS NULL AND b.id IS NULL AND r.operation_id=o.operation_id)
			LIMIT 1`, op.ID).Scan(&result.BuildID, &result.BuildState, &result.CommitSHA,
			&result.DeploymentID, &result.DeployState, &result.RevisionID, &result.ReleaseID, &result.Image, &result.ImageID)
		if err != nil {
			return nil, err
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
		err := tx.QueryRow(ctx, `SELECT environment_id,finished_at FROM eregion_definition_runs WHERE operation_id=$1`, op.ID).
			Scan(&result.EnvironmentID, &result.FinishedAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			result.Availability = "available"
			result.Reason = ""
			rows, err := tx.Query(ctx, `SELECT s.ordinal,s.kind,s.target,coalesce(s.child_operation_id,''),coalesce(o.state,'')
				FROM eregion_definition_steps s LEFT JOIN eregion_operations o ON o.id=s.child_operation_id
				WHERE s.parent_operation_id=$1 ORDER BY s.ordinal`, op.ID)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var child definitionChild
				if err := rows.Scan(&child.Ordinal, &child.Kind, &child.Target, &child.OperationID, &child.Status); err != nil {
					rows.Close()
					return nil, err
				}
				result.Children = append(result.Children, child)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
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

func actionInputTx(ctx context.Context, tx pgx.Tx, id contract.OperationID, dst any) error {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT input FROM eregion_operations WHERE id=$1`, id).Scan(&raw); err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

func protectionActionPayload(ctx context.Context, tx pgx.Tx, op *contract.Operation) (protectionPayload, error) {
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
	var observation protectionObservation
	err = tx.QueryRow(ctx, `SELECT clock_timestamp(),e.updated_at,e.id,e.protected
		FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id
		WHERE a.name=$1 AND e.name=$2`, target.App, target.Environment).
		Scan(&observation.ObservedAt, &observation.UpdatedAt, &observation.EnvironmentID, &observation.Protected)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	if err == nil {
		result.CurrentObservation = &observation
	} else {
		result.Reason = "The environment no longer exists; its current protection cannot be observed."
	}
	return result, nil
}

func executionDeploymentFacts(ctx context.Context, tx pgx.Tx, op *contract.Operation, result *executionPayload) error {
	var observation deploymentObservation
	err := tx.QueryRow(ctx, `SELECT coalesce(d.previous_release_id,''),coalesce(d.previous_deployment_id,''),
		clock_timestamp(),e.updated_at,coalesce(e.serving_deployment_id,''),coalesce(e.serving_release_id,''),
		coalesce(e.desired_release_id,''),coalesce(e.applied_release_id,''),e.health_state,e.health_checked_at,
		coalesce(o.operation_id,''),coalesce(o.deployment_id,''),coalesce(o.phase,''),coalesce(o.route_target_deployment_id,''),
		(SELECT count(*) FROM eregion_platform_container_cleanup c WHERE c.deployment_id=d.id),
		(SELECT count(*) FROM eregion_platform_container_cleanup c WHERE c.deployment_id=d.previous_deployment_id)
		FROM eregion_platform_deployments d JOIN eregion_platform_environments e ON e.id=d.environment_id
		LEFT JOIN eregion_platform_deployment_owners o ON o.environment_id=e.id
		WHERE d.id=$1 AND d.operation_id=$2`, result.DeploymentID, op.ID).
		Scan(&result.PreviousReleaseID, &result.PreviousDeploymentID, &observation.ObservedAt, &observation.EnvironmentUpdatedAt,
			&observation.ServingDeploymentID, &observation.ServingReleaseID, &observation.DesiredReleaseID,
			&observation.AppliedReleaseID, &observation.HealthState, &observation.HealthCheckedAt,
			&observation.OwnerOperationID, &observation.OwnerDeploymentID, &observation.OwnerPhase,
			&observation.RouteTargetDeploymentID, &observation.CandidateCleanupPending, &observation.PreviousCleanupPending)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil {
		observation.ActionDeploymentServing = observation.ServingDeploymentID == result.DeploymentID
		result.CurrentObservation = &observation
	}
	rows, err := tx.Query(ctx, `SELECT step,state,attempt,execution_id,started_at,completed_at
		FROM eregion_operation_steps WHERE operation_id=$1 AND
		(step LIKE 'platform-health-%' OR step LIKE 'platform-cleanup-%' OR step IN ($2,$3,$4))
		ORDER BY step`, op.ID, deploymentRouteStep, deploymentPublicStep, deploymentRestoreRouteStep)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var step diagnosticStep
		if err := rows.Scan(&step.Step, &step.State, &step.Attempt, &step.ExecutionID, &step.StartedAt, &step.CompletedAt); err != nil {
			return err
		}
		result.Steps = append(result.Steps, step)
	}
	return rows.Err()
}

func (s *Service) retentionActionPayload(ctx context.Context, tx pgx.Tx, op *contract.Operation) (retentionPayload, error) {
	result := retentionPayload{Availability: "unavailable", Reason: "No retention run linked to this operation remains available."}
	var planned, removed, runErrors []byte
	err := tx.QueryRow(ctx, `SELECT id,status,planned_bytes,reclaimed_bytes,started_at,finished_at,
		planned_items,removed_items,errors,cache_bytes,cache_measured_at,cache_measurement_error,gc_generation
		FROM eregion_retention_runs WHERE operation_id=$1 ORDER BY started_at DESC LIMIT 1`, op.ID).
		Scan(&result.RunID, &result.Status, &result.PlannedBytes, &result.ReclaimedBytes, &result.StartedAt,
			&result.FinishedAt, &planned, &removed, &runErrors, &result.CacheBytes, &result.CacheMeasuredAt,
			&result.CacheError, &result.GCGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	var plan, removals []retention.Item
	var errs []retention.RunError
	if err := json.Unmarshal(planned, &plan); err != nil {
		return result, err
	}
	if err := json.Unmarshal(removed, &removals); err != nil {
		return result, err
	}
	if err := json.Unmarshal(runErrors, &errs); err != nil {
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

func nodeActionPayload(ctx context.Context, tx pgx.Tx, op *contract.Operation) (nodePayload, error) {
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
	var observation nodeObservation
	err := tx.QueryRow(ctx, `SELECT clock_timestamp(),updated_at,roles,capabilities,status,enrollment_state,
		coalesce(enroll_operation_id,''),enrolled_at,identity_fingerprint,last_check_state,last_check_at,wg_state
		FROM eregion_platform_nodes WHERE name=$1`, result.Name).
		Scan(&observation.ObservedAt, &observation.UpdatedAt, &observation.Roles, &observation.Capabilities,
			&observation.Status, &observation.EnrollmentState, &observation.EnrollmentOperation, &observation.EnrolledAt,
			&observation.IdentityFingerprint, &observation.LastCheckState, &observation.LastCheckAt, &observation.WireGuardState)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	if err == nil {
		result.CurrentObservation = &observation
		result.Reason = "Current node facts may have changed since this operation; registration alone is not verification."
	}
	return result, nil
}

func domainActionPayload(ctx context.Context, tx pgx.Tx, op *contract.Operation) (domainPayload, error) {
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
	var observation actionDomainObservation
	err = tx.QueryRow(ctx, `SELECT clock_timestamp(),d.id,d.environment_id,d.is_primary,d.desired_revision,d.applied_revision,
		d.route_state,d.route_observed_at,d.route_applied_at,d.dns_state,d.dns_observed_at,
		d.certificate_state,d.certificate_observed_at,d.public_state,d.public_observed_at,
		coalesce(x.provider,''),coalesce(x.zone_name,''),coalesce(x.ownership_marker,''),x.generation,x.applied_generation,
		x.desired_origins,x.desired_proxied,coalesce(x.state,''),coalesce(x.last_operation_id,''),x.updated_at,
		x.cleanup_requested_at,x.cleanup_verified_at
		FROM eregion_platform_domains d
		JOIN eregion_platform_environments e ON e.id=d.environment_id
		JOIN eregion_platform_apps a ON a.id=e.app_id
		LEFT JOIN eregion_platform_domain_dns x ON x.domain_id=d.id
		WHERE a.name=$1 AND e.name=$2 AND d.name=$3`, result.App, result.Environment, result.Name).
		Scan(&observation.ObservedAt, &observation.ID, &observation.EnvironmentID, &observation.Primary,
			&observation.DesiredRevision, &observation.AppliedRevision, &observation.RouteState, &observation.RouteObservedAt,
			&observation.RouteAppliedAt, &observation.DNSState, &observation.DNSObservedAt, &observation.CertificateState,
			&observation.CertificateObservedAt, &observation.PublicState, &observation.PublicObservedAt,
			&observation.Provider, &observation.ZoneName, &observation.OwnershipMarker, &observation.Generation,
			&observation.AppliedGeneration, &observation.DesiredOrigins, &observation.DesiredProxied,
			&observation.ProviderState, &observation.LastOperationID, &observation.LedgerUpdatedAt,
			&observation.CleanupRequestedAt, &observation.CleanupVerifiedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	if err == nil {
		result.CurrentObservation = &observation
		result.Reason = "The binding and DNS ledger are current observations and may have changed since this operation."
	}
	return result, nil
}

func resourceActionPayload(ctx context.Context, tx pgx.Tx, op *contract.Operation) (resourcePayload, error) {
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
	var observation resourceObservation
	var volumes []byte
	err = tx.QueryRow(ctx, `SELECT clock_timestamp(),r.updated_at,r.id,r.environment_id,r.node_name,r.state,
		r.image,r.image_id,r.container_name,r.network_name,r.volumes,r.health_state,r.health_checked_at,
		coalesce(r.operation_id,''),r.destroyed_at
		FROM eregion_platform_resources r JOIN eregion_platform_environments e ON e.id=r.environment_id
		JOIN eregion_platform_apps a ON a.id=r.app_id
		WHERE a.name=$1 AND e.name=$2 AND r.name=$3`, result.App, result.Environment, result.Name).
		Scan(&observation.ObservedAt, &observation.UpdatedAt, &observation.ID, &observation.EnvironmentID,
			&observation.Node, &observation.State, &observation.Image, &observation.ImageID,
			&observation.ContainerName, &observation.NetworkName, &volumes, &observation.HealthState,
			&observation.HealthCheckedAt, &observation.LastOperationID, &observation.DestroyedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	if err == nil {
		var owned []platform.ResourceVolumeStatus
		if err := json.Unmarshal(volumes, &owned); err != nil {
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

func buildStopActionPayload(ctx context.Context, tx pgx.Tx, op *contract.Operation) (buildStopPayload, error) {
	result := buildStopPayload{Availability: "partial", Reason: "The target build record is no longer available."}
	target, err := platform.ParseTarget(op.Target)
	if err != nil || target.Kind != platform.TargetBuild {
		return result, errors.New("invalid build stop target")
	}
	result.TargetBuildID = string(target.Build)
	var observation buildObservation
	err = tx.QueryRow(ctx, `SELECT clock_timestamp(),id,operation_id,state,commit_sha,coalesce(revision_id,''),
		coalesce(release_id,''),completed_at FROM eregion_platform_builds WHERE id=$1`, result.TargetBuildID).
		Scan(&observation.ObservedAt, &observation.ID, &observation.OperationID, &observation.State,
			&observation.CommitSHA, &observation.RevisionID, &observation.ReleaseID, &observation.CompletedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	if err == nil {
		result.CurrentObservation = &observation
		result.Reason = "Build state is a current observation, not proof of the stop action's historical outcome."
	}
	return result, nil
}

func diagnosticActionPayload(ctx context.Context, tx pgx.Tx, op *contract.Operation) (diagnosticPayloadResult, error) {
	result := diagnosticPayloadResult{Availability: "available"}
	var in operations.TestInput
	if err := actionInputTx(ctx, tx, op.ID, &in); err != nil {
		return result, err
	}
	result.RequestedSteps = in.Steps
	rows, err := tx.Query(ctx, `SELECT step,state,attempt,execution_id,started_at,completed_at
		FROM eregion_operation_steps WHERE operation_id=$1 ORDER BY step`, op.ID)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var step diagnosticStep
		if err := rows.Scan(&step.Step, &step.State, &step.Attempt, &step.ExecutionID, &step.StartedAt, &step.CompletedAt); err != nil {
			return result, err
		}
		if step.State == "completed" {
			result.AcknowledgedSteps++
		}
		result.Steps = append(result.Steps, step)
	}
	return result, rows.Err()
}
