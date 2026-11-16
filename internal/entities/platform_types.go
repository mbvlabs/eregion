package entities

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

var (
	// ErrPlatformPointerIntegrity marks a row whose pointer ownership does not
	// match its parent. The caller keeps the existing contract fault mapping.
	ErrPlatformPointerIntegrity = errors.New("entities: platform reference is missing or belongs to another environment")
	// ErrPlatformCorrupt marks a row whose stored identity cannot be mapped.
	ErrPlatformCorrupt = errors.New("entities: platform row is malformed")
)

type AppFilter struct {
	Name  string
	Limit int
}

type AppInsert struct {
	ID              platform.AppID
	Name            string
	ImageRepository string
}

type AppRepository struct {
	ID              platform.AppID
	ImageRepository string
}

type AppUploadIdentity struct {
	ID              platform.AppID
	Name            string
	ImageRepository string
}

type AppActionTimestamps struct {
	ID        platform.AppID
	Now       time.Time
	UpdatedAt time.Time
}

type EnvironmentFilter struct {
	App         string
	Environment string
	Node        string
	State       string
	Limit       int
}

type EnvironmentObservation struct {
	Environment platform.Environment
	PrimaryDNS  platform.DNSState
}

type EnvironmentInsert struct {
	ID                   platform.EnvironmentID
	AppID                platform.AppID
	Name                 string
	Node                 string
	BuildNode            string
	HostPort             int
	CandidatePort        int
	ContainerPort        int
	Bind                 string
	HealthType           string
	HealthPath           string
	HealthTimeoutSeconds int
	Env                  map[string]string
	SecretRefs           map[string]string
	Protected            bool
}

type EnvironmentMirror struct {
	ID                   platform.EnvironmentID
	Node                 string
	BuildNode            string
	ContainerPort        int
	HealthType           string
	HealthPath           string
	HealthTimeoutSeconds int
	Env                  map[string]string
	SecretRefs           map[string]string
	RevisionID           platform.RevisionID
}

type EnvironmentServing struct {
	ID           platform.EnvironmentID
	ReleaseID    platform.ReleaseID
	DeploymentID platform.DeploymentID
	HealthState  platform.HealthState
	HealthDetail string
	CheckedAt    time.Time
}

type EnvironmentIdentity struct {
	ID    platform.EnvironmentID
	AppID platform.AppID
	App   string
	Name  string
}

type EnvironmentRevisionPointers struct {
	Current platform.RevisionID
	Serving platform.RevisionID
}

type EnvironmentRetentionRow struct {
	ID             platform.EnvironmentID
	App            string
	Name           string
	Node           string
	ServingRelease platform.ReleaseID
	ServingDeploy  platform.DeploymentID
	Retention      json.RawMessage
}

type RevisionFilter struct {
	App         string
	Environment string
	State       string
	Limit       int
}

type RevisionInsert struct {
	ID            platform.RevisionID
	EnvironmentID platform.EnvironmentID
	Number        int
	OperationID   contract.OperationID
	Actor         contract.Actor
	Operation     string
	Summary       string
	State         platform.RevisionState
}

type FirstDeployActionRow struct {
	RevisionID     platform.RevisionID
	EnvironmentID  platform.EnvironmentID
	Environment    string
	AppID          platform.AppID
	App            string
	Number         int
	DeployIntent   bool
	DeployOpID     contract.OperationID
	Processes      []platform.ProcessSpec
	Config         map[string]string
	PreviousConfig map[string]string
	HasPrevious    bool
	SecretNames    []string
	Resources      []string
}

type ChangeFilter struct {
	App         string
	Environment string
	Kind        string
	Limit       int
}

type ChangeInsert struct {
	ID            platform.ChangeID
	AppID         platform.AppID
	EnvironmentID platform.EnvironmentID
	Sequence      uint64
	Kind          string
	OperationID   contract.OperationID
	Actor         contract.Actor
	Summary       string
	Desired       json.RawMessage
	Applied       json.RawMessage
}
