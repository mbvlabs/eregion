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

	// Enrollment facts. A node that is only "registered" has declared facts and
	// nothing verified; only an "enrolled" node receives remote work.
	EnrollmentState     EnrollmentState `json:"enrollment_state"`
	EnrollmentError     string          `json:"enrollment_error,omitempty"`
	Transport           string          `json:"transport"`
	SSHUser             string          `json:"ssh_user,omitempty"`
	HostKeyFingerprint  string          `json:"host_key_fingerprint,omitempty"`
	IdentityFingerprint string          `json:"identity_fingerprint,omitempty"`
	OS                  string          `json:"os,omitempty"`
	Arch                string          `json:"arch,omitempty"`
	BuildConcurrency    int             `json:"build_concurrency,omitempty"`
	WireGuard           *NodeWireGuard  `json:"wireguard,omitempty"`
	EnrolledAt          *time.Time      `json:"enrolled_at,omitempty"`
	// LastCheckAt is the freshness of the last authenticated observation.
	LastCheckAt     *time.Time `json:"last_check_at,omitempty"`
	LastCheckState  string     `json:"last_check_state,omitempty"`
	LastCheckDetail string     `json:"last_check_detail,omitempty"`
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

// SourceBinding is the durable source declaration in an environment revision.
// A git binding is an HTTPS repository with an optional read-credential secret
// NAME; the value lives in the vault and is never part of the revision.
type SourceBinding struct {
	Kind                 string            `json:"kind"`
	URL                  string            `json:"url,omitempty"`
	Ref                  string            `json:"ref,omitempty"`
	CredentialSecretName string            `json:"credential_secret_name,omitempty"`
	ContextPath          string            `json:"context_path,omitempty"`
	Builder              string            `json:"builder,omitempty"`
	BuildEnv             map[string]string `json:"build_env,omitempty"`
	Image                string            `json:"image,omitempty"`
}

// RuntimeSpec is the environment runtime declaration. Node is mirrored on the
// environment row so placement remains queryable without reading the revision.
type RuntimeSpec struct {
	Node                 string       `json:"node,omitempty"`
	HealthType           string       `json:"health_type,omitempty"`
	HealthPath           string       `json:"health_path,omitempty"`
	HealthTimeoutSeconds int          `json:"health_timeout_seconds,omitempty"`
	ReleaseCommand       *CommandSpec `json:"release_command,omitempty"`
}

// ProcessSpec is one process formation entry. Web and service processes own a
// container port; a worker has no published port.
type ProcessSpec struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Command       string   `json:"command,omitempty"`
	Args          []string `json:"args,omitempty"`
	Replicas      int      `json:"replicas,omitempty"`
	ContainerPort int      `json:"container_port,omitempty"`
}

// RevisionState is the complete desired state carried by one environment
// revision. SecretNames are vault names only; values are resolved per deploy.
type RevisionState struct {
	Source            SourceBinding     `json:"source"`
	AutoDeploy        bool              `json:"auto_deploy"`
	WebhookSecretName string            `json:"webhook_secret_name,omitempty"`
	Runtime           RuntimeSpec       `json:"runtime"`
	Processes         []ProcessSpec     `json:"processes"`
	Config            map[string]string `json:"config"`
	SecretNames       []string          `json:"secret_names"`
	// DeployIntent records that the operator asked for a first deploy. The
	// control plane queues that deploy with the revision.
	DeployIntent bool `json:"deploy_intent,omitempty"`
}

// EnvironmentRevision is one immutable, numbered environment desired state.
type EnvironmentRevision struct {
	ID          RevisionID           `json:"id"`
	App         string               `json:"app,omitempty"`
	Environment string               `json:"environment,omitempty"`
	Number      int                  `json:"number"`
	OperationID contract.OperationID `json:"operation_id,omitempty"`
	Actor       contract.Actor       `json:"actor"`
	Operation   string               `json:"operation"`
	Summary     string               `json:"summary"`
	RevisionState
	CreatedAt time.Time `json:"created_at"`
}

// Deployability is the report MASTER section 4 requires before a deploy can
// run. DNS stays unknown until the domain work list row observes it.
type Deployability struct {
	Deployable bool     `json:"deployable"`
	Missing    []string `json:"missing"`
	DNS        string   `json:"dns"`
}

// DeployabilityState supplies the non-revision observations.
type DeployabilityState struct {
	ArtifactReady bool
	PrimaryDomain string
	DNS           string
}

type EnvironmentDeployabilityReport struct {
	App               string        `json:"app"`
	Environment       string        `json:"environment"`
	CurrentRevisionID RevisionID    `json:"current_revision_id,omitempty"`
	Deployability     Deployability `json:"deployability"`
}

// EvaluateDeployability is pure: it reads only the revision and observations.
func EvaluateDeployability(revision *EnvironmentRevision, state DeployabilityState) Deployability {
	dns := state.DNS
	if dns == "" {
		dns = "unknown"
	}
	missing := make([]string, 0, 6)
	if revision == nil || !sourceReady(revision.Source) {
		missing = append(missing, "source")
	}
	if !state.ArtifactReady {
		missing = append(missing, "artifact")
	}
	if revision == nil || !runtimeReady(revision.Runtime) {
		missing = append(missing, "runtime")
	}
	if revision == nil || !webProcessReady(revision.Processes) {
		missing = append(missing, "web_process")
	}
	if state.PrimaryDomain == "" {
		missing = append(missing, "domain")
	}
	if dns == "missing" {
		missing = append(missing, "dns")
	}
	return Deployability{Deployable: len(missing) == 0, Missing: missing, DNS: dns}
}

func sourceReady(source SourceBinding) bool {
	switch source.Kind {
	case "git":
		return source.URL != ""
	case "image":
		return source.Image != ""
	default:
		return false
	}
}

func runtimeReady(runtime RuntimeSpec) bool {
	return runtime.Node != "" && runtime.HealthPath != ""
}

func webProcessReady(processes []ProcessSpec) bool {
	for _, process := range processes {
		if process.Kind == "web" && process.Replicas > 0 {
			return true
		}
	}
	return false
}

type HealthState string

const (
	HealthUnknown   HealthState = "unknown"
	HealthHealthy   HealthState = "healthy"
	HealthUnhealthy HealthState = "unhealthy"
)

type Environment struct {
	ID   EnvironmentID `json:"id"`
	App  string        `json:"app"`
	Name string        `json:"name"`
	Node string        `json:"node"`
	// BuildNode optionally places Buildpacks execution on a distinct builder
	// node. When empty, builds run on Node. It must have builder + buildpacks,
	// and it is always authenticated through the node transport, never a name.
	BuildNode      string            `json:"build_node,omitempty"`
	HostPort       int               `json:"host_port"`
	CandidatePort  int               `json:"candidate_port"`
	ContainerPort  int               `json:"container_port"`
	Bind           string            `json:"bind"`
	Health         HealthConfig      `json:"health"`
	Env            map[string]string `json:"env"`
	SecretRefs     map[string]string `json:"secret_refs,omitempty"`
	SecretRevision int64             `json:"secret_revision,omitempty"`
	DesiredRelease ReleaseID         `json:"desired_release_id,omitempty"`
	AppliedRelease ReleaseID         `json:"applied_release_id,omitempty"`
	ServingRelease ReleaseID         `json:"serving_release_id,omitempty"`
	// ServingDeploymentID identifies the deployment whose recorded instance
	// ports are the Caddy upstreams.
	ServingDeploymentID DeploymentID `json:"serving_deployment_id,omitempty"`
	HealthState         HealthState  `json:"health_state"`
	HealthDetail        string       `json:"health_detail,omitempty"`
	HealthCheckedAt     *time.Time   `json:"health_checked_at,omitempty"`
	// Domains are the public hostnames routed through Caddy to the serving web
	// process replicas.
	Domains       []string             `json:"domains"`
	PrimaryDomain string               `json:"primary_domain,omitempty"`
	ArtifactReady bool                 `json:"artifact_ready"`
	RevisionID    RevisionID           `json:"current_revision_id,omitempty"`
	Revision      *EnvironmentRevision `json:"revision,omitempty"`
	Processes     []ProcessSpec        `json:"processes"`
	Deployability Deployability        `json:"deployability"`
	CreatedAt     time.Time            `json:"created_at"`
	UpdatedAt     time.Time            `json:"updated_at"`
}

// BuildNodeName is where a build source for this environment runs. An empty
// build_node means the environment's application node, which works for the
// co-located main node in development. A distinct build node is accepted and
// validated, but an artifact cannot cross nodes without a shared registry.
func (e Environment) BuildNodeName() string {
	if e.BuildNode != "" {
		return e.BuildNode
	}
	return e.Node
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
	// CredentialSecretName is the vault NAME used by a private HTTPS clone. The
	// value is resolved at execution and never stored here.
	CredentialSecretName string `json:"credential_secret_name,omitempty"`
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
	ID          ReleaseID            `json:"id"`
	OperationID contract.OperationID `json:"operation_id,omitempty"`
	App         string               `json:"app"`
	Environment string               `json:"environment"`
	Image       string               `json:"image"`
	ImageID     string               `json:"image_id,omitempty"`
	Source      Source               `json:"source"`
	Build       *Build               `json:"build,omitempty"`
	Command     *CommandSpec         `json:"command,omitempty"`
	BuildID     BuildID              `json:"build_id,omitempty"`
	CommitSHA   string               `json:"commit_sha,omitempty"`
	Status      ReleaseStatus        `json:"status"`
	ErrorCode   contract.Code        `json:"error_code,omitempty"`
	Error       string               `json:"error,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	CompletedAt *time.Time           `json:"completed_at,omitempty"`
}

type BuildState string

const (
	BuildPending   BuildState = "pending"
	BuildRunning   BuildState = "running"
	BuildSucceeded BuildState = "succeeded"
	BuildFailed    BuildState = "failed"
	BuildCancelled BuildState = "cancelled"
)

// BuildRecord is one attempt to turn an exact source commit into an image. Its
// log stream is the owning operation's events; the record keeps the durable
// lifecycle and commit identity.
type BuildRecord struct {
	ID          BuildID              `json:"id"`
	OperationID contract.OperationID `json:"operation_id"`
	App         string               `json:"app"`
	Environment string               `json:"environment"`
	RevisionID  RevisionID           `json:"revision_id,omitempty"`
	ReleaseID   ReleaseID            `json:"release_id,omitempty"`
	Source      Source               `json:"source"`
	CommitSHA   string               `json:"commit_sha,omitempty"`
	State       BuildState           `json:"state"`
	ErrorCode   contract.Code        `json:"error_code,omitempty"`
	Error       string               `json:"error,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	StartedAt   *time.Time           `json:"started_at,omitempty"`
	CompletedAt *time.Time           `json:"completed_at,omitempty"`
}

type DeploymentAction string

const (
	ActionDeploy   DeploymentAction = "deploy"
	ActionRollback DeploymentAction = "rollback"
	ActionRestart  DeploymentAction = "restart"
)

// DeploymentInstance records one process container created for a deployment.
// Web and service instances carry the dynamic loopback host port Caddy and
// private access use; a worker has no port.
type DeploymentInstance struct {
	Name        string    `json:"name"`
	Process     string    `json:"process"`
	Kind        string    `json:"kind"`
	Replica     string    `json:"replica"`
	ReleaseID   ReleaseID `json:"release_id"`
	HostPort    int       `json:"host_port,omitempty"`
	ContainerID string    `json:"container_id,omitempty"`
	State       string    `json:"state"`
}

type DeploymentState string

const (
	DeploymentPending   DeploymentState = "pending"
	DeploymentRunning   DeploymentState = "running"
	DeploymentSucceeded DeploymentState = "succeeded"
	DeploymentFailed    DeploymentState = "failed"
	DeploymentCancelled DeploymentState = "cancelled"
)

// SecretRevisionRef is the non-secret identity of one resolved vault revision.
type SecretRevisionRef struct {
	Revision  uint64    `json:"revision"`
	Hash      string    `json:"hash"`
	KeyID     string    `json:"key_id"`
	CreatedAt time.Time `json:"created_at"`
}

type Deployment struct {
	ID                   DeploymentID                 `json:"id"`
	OperationID          contract.OperationID         `json:"operation_id"`
	App                  string                       `json:"app"`
	Environment          string                       `json:"environment"`
	Action               DeploymentAction             `json:"action"`
	ReleaseID            ReleaseID                    `json:"release_id"`
	PreviousReleaseID    ReleaseID                    `json:"previous_release_id,omitempty"`
	PreviousDeploymentID DeploymentID                 `json:"previous_deployment_id,omitempty"`
	RevisionID           RevisionID                   `json:"revision_id,omitempty"`
	SecretRevisions      map[string]SecretRevisionRef `json:"secret_revisions,omitempty"`
	Instances            []DeploymentInstance         `json:"instances"`
	State                DeploymentState              `json:"state"`
	ErrorCode            contract.Code                `json:"error_code,omitempty"`
	Message              string                       `json:"message,omitempty"`
	CreatedAt            time.Time                    `json:"created_at"`
	UpdatedAt            time.Time                    `json:"updated_at"`
	CompletedAt          *time.Time                   `json:"completed_at,omitempty"`
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
