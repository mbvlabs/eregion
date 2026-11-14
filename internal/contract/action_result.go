package contract

import (
	"encoding/json"
	"time"
)

// ActionResult is an immutable observation of an action attempt. History is
// addressed separately so a bounded response never implies complete capture.
type ActionResult struct {
	OperationID     OperationID      `json:"operation_id"`
	Action          string           `json:"action"`
	Target          string           `json:"target"`
	Attempt         uint32           `json:"attempt"`
	Status          State            `json:"status"`
	Final           bool             `json:"final"`
	StartedAt       time.Time        `json:"started_at"`
	CompletedAt     time.Time        `json:"completed_at"`
	TimingBasis     string           `json:"timing_basis"`
	Stage           string           `json:"stage,omitempty"`
	Code            Code             `json:"code,omitempty"`
	Message         string           `json:"message,omitempty"`
	Excerpt         string           `json:"excerpt,omitempty"`
	ExcerptOmitted  string           `json:"excerpt_omitted,omitempty"`
	ExcerptCursor   *Cursor          `json:"excerpt_cursor,omitempty"`
	Data            json.RawMessage  `json:"data,omitempty"`
	DataStatus      string           `json:"data_status,omitempty"`
	DataCursor      *Cursor          `json:"data_cursor,omitempty"`
	Identities      ActionIdentities `json:"identities,omitempty"`
	Evidence        string           `json:"evidence"`
	EvidenceSources []EvidenceSource `json:"evidence_sources,omitempty"`
	History         Cursor           `json:"history"`
}

type EvidenceSource struct {
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

// ActionEvidenceView projects retained evidence at the snapshot cursor. It
// never changes the immutable result recorded at settlement.
type ActionEvidenceView struct {
	Attempt        uint32           `json:"attempt"`
	Availability   string           `json:"availability"`
	Sources        []EvidenceSource `json:"sources,omitempty"`
	Excerpt        string           `json:"excerpt,omitempty"`
	ExcerptOmitted string           `json:"excerpt_omitted,omitempty"`
	ExcerptCursor  *Cursor          `json:"excerpt_cursor,omitempty"`
	History        Cursor           `json:"history"`
	Through        uint64           `json:"through,string"`
}

// Validate checks newly written results; historical events without results
// remain generically readable.
func (r ActionResult) Validate() error {
	if err := r.OperationID.Validate(); err != nil {
		return err
	}
	if r.Action == "" || r.Target == "" || r.Stage == "" ||
		(r.Status != Succeeded && r.Status != Failed && r.Status != Canceled) ||
		r.StartedAt.IsZero() || r.CompletedAt.IsZero() || r.CompletedAt.Before(r.StartedAt) ||
		r.History.OperationID != r.OperationID {
		return NewFault(ValidationFailed, "invalid action result", "Require action identity, outcome, stage, timing and operation-scoped history.")
	}
	if r.Code != "" && !KnownCode(r.Code) {
		return NewFault(ValidationFailed, "invalid action result code", "Use a known error code.")
	}
	if !r.Final && r.Status != Failed || r.Status == Succeeded && r.Attempt == 0 {
		return NewFault(ValidationFailed, "invalid action result attempt", "Only a failed retry may be nonterminal; success requires an attempt.")
	}
	switch r.Evidence {
	case "not_collected", "unavailable", "interrupted", "partial", "full", "truncated":
	default:
		return NewFault(ValidationFailed, "invalid action result evidence", "Use a defined evidence availability.")
	}
	if r.ExcerptCursor != nil && r.ExcerptCursor.OperationID != r.OperationID {
		return NewFault(ValidationFailed, "invalid action result excerpt cursor", "Use the result's operation identity.")
	}
	if r.DataCursor != nil && r.DataCursor.OperationID != r.OperationID {
		return NewFault(ValidationFailed, "invalid action result data cursor", "Use the result's operation identity.")
	}
	if len(r.Data) > 0 && !json.Valid(r.Data) {
		return NewFault(ValidationFailed, "invalid action result data", "Use valid JSON.")
	}
	return nil
}

// ActionIdentities contains durable domain records linked to this operation.
type ActionIdentities struct {
	BuildID      string `json:"build_id,omitempty"`
	DeploymentID string `json:"deployment_id,omitempty"`
	RevisionID   string `json:"revision_id,omitempty"`
	ReleaseID    string `json:"release_id,omitempty"`
	Image        string `json:"image,omitempty"`
	ImageID      string `json:"image_id,omitempty"`
	CommitSHA    string `json:"commit_sha,omitempty"`
}

// NodeExecutionEvidence is retained from the original node response. Attempt
// and execution ID name that response, not the later worker that observed it.
type NodeExecutionEvidence struct {
	ExecutionID        string                 `json:"execution_id"`
	Attempt            uint32                 `json:"attempt"`
	Outcome            string                 `json:"outcome"`
	Stage              string                 `json:"stage"`
	OutputAvailability string                 `json:"output_availability"`
	RedactionContext   string                 `json:"redaction_context,omitempty"`
	TextOmitted        string                 `json:"text_omitted,omitempty"`
	Stdout             string                 `json:"stdout,omitempty"`
	Stderr             string                 `json:"stderr,omitempty"`
	StdoutTruncated    bool                   `json:"stdout_truncated"`
	StderrTruncated    bool                   `json:"stderr_truncated"`
	Previous           *NodeContainerEvidence `json:"previous,omitempty"`
	Container          *NodeContainerEvidence `json:"container,omitempty"`
}

type NodeEvidenceLoss struct {
	ExecutionID string `json:"execution_id"`
	Attempt     uint32 `json:"attempt"`
}

type NodeContainerEvidence struct {
	Availability string    `json:"availability"`
	ContainerID  string    `json:"container_id,omitempty"`
	ObservedAt   time.Time `json:"observed_at,omitempty"`
	State        string    `json:"state,omitempty"`
	Running      bool      `json:"running"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	FinishedAt   time.Time `json:"finished_at,omitempty"`
	ExitCode     *int      `json:"exit_code,omitempty"`
	OOMKilled    bool      `json:"oom_killed"`
	Health       string    `json:"health,omitempty"`
	Logs         string    `json:"logs,omitempty"`
	Truncated    bool      `json:"truncated"`
}
