package platform

import (
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// Roles and capabilities are placement facts. They are declared on a node and
// checked before work is accepted; they are never inferred from a hostname.
const (
	RoleApplication  = "application"
	RoleBuilder      = "builder"
	RoleEdge         = "edge"
	RoleRegistry     = "registry"
	RoleTelemetry    = "telemetry"
	RoleControlPlane = "control-plane"

	CapabilityDocker      = "docker"
	CapabilityBuildpacks  = "buildpacks"
	CapabilityRegistry    = "registry"
	CapabilityHTTPIngress = "http-ingress"
)

var knownRoles = []string{RoleApplication, RoleBuilder, RoleEdge, RoleRegistry, RoleTelemetry, RoleControlPlane}

type NodeStatus string

const (
	NodeRegistered  NodeStatus = "registered"
	NodeReady       NodeStatus = "ready"
	NodeUnreachable NodeStatus = "unreachable"
	NodeDisabled    NodeStatus = "disabled"
)

type Node struct {
	Name         string     `json:"name"`
	Roles        []string   `json:"roles"`
	Capabilities []string   `json:"capabilities"`
	Address      string     `json:"address,omitempty"`
	Status       NodeStatus `json:"status"`
	LastSeenAt   *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type App struct {
	ID        AppID     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type HealthConfig struct {
	Type           string `json:"type"`
	Path           string `json:"path"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type HealthState string

const (
	HealthUnknown   HealthState = "unknown"
	HealthHealthy   HealthState = "healthy"
	HealthUnhealthy HealthState = "unhealthy"
)

type Environment struct {
	ID              EnvironmentID     `json:"id"`
	App             string            `json:"app"`
	Name            string            `json:"name"`
	Node            string            `json:"node"`
	HostPort        int               `json:"host_port"`
	CandidatePort   int               `json:"candidate_port"`
	ContainerPort   int               `json:"container_port"`
	Bind            string            `json:"bind"`
	Health          HealthConfig      `json:"health"`
	Env             map[string]string `json:"env"`
	SecretRefs      map[string]string `json:"secret_refs,omitempty"`
	SecretRevision  int64             `json:"secret_revision,omitempty"`
	DesiredRelease  ReleaseID         `json:"desired_release_id,omitempty"`
	AppliedRelease  ReleaseID         `json:"applied_release_id,omitempty"`
	ServingRelease  ReleaseID         `json:"serving_release_id,omitempty"`
	HealthState     HealthState       `json:"health_state"`
	HealthDetail    string            `json:"health_detail,omitempty"`
	HealthCheckedAt *time.Time        `json:"health_checked_at,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

type ReleaseStatus string

const (
	ReleasePending ReleaseStatus = "pending"
	ReleaseReady   ReleaseStatus = "ready"
	ReleaseFailed  ReleaseStatus = "failed"
)

type Source struct {
	Type  string `json:"type"`
	Image string `json:"image,omitempty"`
	Path  string `json:"path,omitempty"`
	URL   string `json:"url,omitempty"`
	Ref   string `json:"ref,omitempty"`
}

type Build struct {
	// Image is the optional output image reference. When empty the control
	// plane derives one from the app, environment, and operation.
	Image   string            `json:"image,omitempty"`
	Builder string            `json:"builder,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// CommandSpec is an optional one-off release command run in the release image
// before traffic switches. It is not a shell: Argv is executed directly.
type CommandSpec struct {
	Argv           []string `json:"argv"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

type Release struct {
	ID          ReleaseID     `json:"id"`
	App         string        `json:"app"`
	Environment string        `json:"environment"`
	Image       string        `json:"image"`
	ImageID     string        `json:"image_id,omitempty"`
	Source      Source        `json:"source"`
	Build       *Build        `json:"build,omitempty"`
	Command     *CommandSpec  `json:"command,omitempty"`
	Status      ReleaseStatus `json:"status"`
	ErrorCode   contract.Code `json:"error_code,omitempty"`
	Error       string        `json:"error,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	CompletedAt *time.Time    `json:"completed_at,omitempty"`
}

type DeploymentAction string

const (
	ActionDeploy   DeploymentAction = "deploy"
	ActionRollback DeploymentAction = "rollback"
)

type DeploymentState string

const (
	DeploymentPending   DeploymentState = "pending"
	DeploymentRunning   DeploymentState = "running"
	DeploymentSucceeded DeploymentState = "succeeded"
	DeploymentFailed    DeploymentState = "failed"
	DeploymentCancelled DeploymentState = "cancelled"
)

type Deployment struct {
	ID                DeploymentID         `json:"id"`
	OperationID       contract.OperationID `json:"operation_id"`
	App               string               `json:"app"`
	Environment       string               `json:"environment"`
	Action            DeploymentAction     `json:"action"`
	ReleaseID         ReleaseID            `json:"release_id"`
	PreviousReleaseID ReleaseID            `json:"previous_release_id,omitempty"`
	State             DeploymentState      `json:"state"`
	ErrorCode         contract.Code        `json:"error_code,omitempty"`
	Message           string               `json:"message,omitempty"`
	CreatedAt         time.Time            `json:"created_at"`
	UpdatedAt         time.Time            `json:"updated_at"`
	CompletedAt       *time.Time           `json:"completed_at,omitempty"`
}

type Change struct {
	ID          ChangeID             `json:"id"`
	App         string               `json:"app"`
	Environment string               `json:"environment,omitempty"`
	Sequence    uint64               `json:"sequence,string"`
	Kind        string               `json:"kind"`
	OperationID contract.OperationID `json:"operation_id"`
	Actor       contract.Actor       `json:"actor"`
	Summary     string               `json:"summary"`
	Desired     any                  `json:"desired,omitempty"`
	Applied     any                  `json:"applied,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
}

// InventoryQuery is the bounded read contract for /v1/platform.
type InventoryQuery struct {
	Collection  string
	App         string
	Environment string
	Node        string
	State       string
	Limit       int
}

type Inventory struct {
	Collection string `json:"collection"`
	Items      any    `json:"items"`
	Limit      int    `json:"limit"`
}

// Preview is a dry-run result. It never includes secret values.
type Preview struct {
	Kind     string   `json:"kind"`
	Target   string   `json:"target"`
	DryRun   bool     `json:"dry_run"`
	Input    any      `json:"input"`
	Summary  string   `json:"summary"`
	Steps    []string `json:"steps,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}
