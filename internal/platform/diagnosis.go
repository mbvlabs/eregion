package platform

import "github.com/mbvlabs/eregion/internal/contract"

// Diagnosis is a read-only snapshot. Missing evidence is represented by a
// reason, not by a fabricated conclusion or an empty successful observation.
type Diagnosis struct {
	Schema        string                     `json:"schema"`
	Operation     contract.Operation         `json:"operation"`
	Build         *DiagnosisBuild            `json:"build,omitempty"`
	SourceBuild   *DiagnosisBuild            `json:"release_source_build,omitempty"`
	Release       *DiagnosisRelease          `json:"release,omitempty"`
	Deployment    *DiagnosisDeployment       `json:"deployment,omitempty"`
	Historical    *DiagnosisRevision         `json:"historical_revision,omitempty"`
	BuildRevision *DiagnosisRevision         `json:"build_revision,omitempty"`
	RecordedBuild *DiagnosisBuildSettings    `json:"recorded_release_build_settings,omitempty"`
	Current       *DiagnosisRevision         `json:"current_desired_revision,omitempty"`
	Image         *DiagnosisImage            `json:"image_observation,omitempty"`
	LiveCandidate *CandidateDiagnosticResult `json:"live_candidate,omitempty"`
	Failure       *DiagnosisFailure          `json:"failure_observation,omitempty"`
	Events        []contract.Event           `json:"events"`
	Cursor        contract.Cursor            `json:"cursor"`
	Partial       bool                       `json:"partial"`
	Unavailable   []string                   `json:"unavailable"`
}

type DiagnosisFailure struct {
	Message     string `json:"message,omitempty"`
	Stage       string `json:"stage,omitempty"`
	StageSource string `json:"stage_source,omitempty"`
	StepMessage string `json:"step_message,omitempty"`
}

type DiagnosisRevision struct {
	ID        RevisionID             `json:"id"`
	Number    int                    `json:"number"`
	Node      string                 `json:"node,omitempty"`
	Processes []DiagnosisProcess     `json:"processes"`
	Build     DiagnosisBuildSettings `json:"build"`
}

type DiagnosisProcess struct {
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	Replicas       int    `json:"replicas"`
	ContainerPort  int    `json:"container_port,omitempty"`
	CommandOmitted bool   `json:"command_omitted"`
}

type DiagnosisBuildSettings struct {
	Builder             string            `json:"builder,omitempty"`
	ContextPath         string            `json:"context_path,omitempty"`
	Order               []string          `json:"order,omitempty"`
	Effective           map[string]string `json:"effective_typed_values"`
	RawOverridesOmitted []string          `json:"raw_overrides_omitted,omitempty"`
	RawBuildEnvPresent  bool              `json:"raw_build_env_present"`
	GoBuildFlagsPresent bool              `json:"go_build_flags_present"`
	OmissionReason      string            `json:"omission_reason"`
}

type DiagnosisImage struct {
	ImageID               string               `json:"image_id"`
	Buildpacks            []DiagnosisBuildpack `json:"buildpacks"`
	ProcessTypes          []string             `json:"process_types"`
	MetadataLabelsPresent int                  `json:"metadata_labels_present"`
	DeclaredLayerCount    int                  `json:"declared_layer_count"`
	Layers                []DiagnosisLayer     `json:"allowlisted_layers"`
	Note                  string               `json:"note"`
}

type DiagnosisBuildpack struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
}

type DiagnosisLayer struct {
	Buildpack string `json:"buildpack"`
	Name      string `json:"name"`
}

type DiagnosisBuild struct {
	ID          BuildID              `json:"id"`
	OperationID contract.OperationID `json:"operation_id"`
	App         string               `json:"app"`
	Environment string               `json:"environment"`
	RevisionID  RevisionID           `json:"revision_id,omitempty"`
	ReleaseID   ReleaseID            `json:"release_id,omitempty"`
	CommitSHA   string               `json:"commit_sha,omitempty"`
	State       BuildState           `json:"state"`
}

type DiagnosisRelease struct {
	ID          ReleaseID     `json:"id"`
	BuildID     BuildID       `json:"build_id,omitempty"`
	CommitSHA   string        `json:"commit_sha,omitempty"`
	ImageDigest string        `json:"image_digest,omitempty"`
	ImageID     string        `json:"image_id,omitempty"`
	Status      ReleaseStatus `json:"status"`
	Pruned      bool          `json:"pruned"`
}

type DiagnosisDeployment struct {
	ID         DeploymentID    `json:"id"`
	ReleaseID  ReleaseID       `json:"release_id"`
	RevisionID RevisionID      `json:"revision_id,omitempty"`
	State      DeploymentState `json:"state"`
}
