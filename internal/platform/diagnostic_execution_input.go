package platform

import (
	"encoding/json"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/mbvlabs/eregion/internal/contract"
)

// KindDiagnosticExecution is intentionally not registered as a generic action.
// Only the dedicated, trusted server-side admission method may create it.
const KindDiagnosticExecution = "platform.diagnostic.execute"

// DiagnosticExecutionInput is durable internal input, constructed from an
// original operation's historical relationships during admission. Argv is
// ordinary, non-secret operation history; callers must never put secrets in it.
type DiagnosticExecutionInput struct {
	OriginalOperationID contract.OperationID `json:"original_operation_id"`
	Argv                []string             `json:"argv"`
	ImageID             string               `json:"image_id"`
	Node                string               `json:"node"`
	ReleaseID           ReleaseID            `json:"release_id"`
	DeploymentID        DeploymentID         `json:"deployment_id"`
	RevisionID          RevisionID           `json:"revision_id"`
}

// DiagnosticExecutionPreview contains only a historical pin and non-secret
// command size, not command output or proof that the image is present.
type DiagnosticExecutionPreview struct {
	DryRun              bool                 `json:"dry_run"`
	OriginalOperationID contract.OperationID `json:"original_operation_id"`
	Node                string               `json:"node"`
	ImageID             string               `json:"image_id"`
	ReleaseID           ReleaseID            `json:"release_id"`
	DeploymentID        DeploymentID         `json:"deployment_id"`
	RevisionID          RevisionID           `json:"revision_id"`
	ArgCount            int                  `json:"argument_count"`
	Replay              *contract.Operation  `json:"replay,omitempty"`
	Sandbox             []string             `json:"sandbox"`
	Note                string               `json:"note"`
}

// ValidateDiagnosticExecutionArgv validates exact argv without shell parsing,
// expansion, whitespace normalization or secret detection. Since the argument
// values are persisted, the caller is responsible for supplying no secrets.
func ValidateDiagnosticExecutionArgv(argv []string) error {
	invalid := func() error {
		return contract.NewFault(contract.ValidationFailed, "invalid diagnostic command", "Use an absolute executable and at most 128 non-secret arguments totaling at most 32768 UTF-8 bytes; NUL is not allowed.")
	}
	if len(argv) == 0 || len(argv) > 128 || !path.IsAbs(argv[0]) {
		return invalid()
	}
	total := 0
	for _, arg := range argv {
		total += len(arg)
		if !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) || total > 32768 {
			return invalid()
		}
	}
	return nil
}

// Validate checks the complete admission-pinned input, not a caller request.
func (in DiagnosticExecutionInput) Validate() error {
	if err := in.OriginalOperationID.Validate(); err != nil {
		return err
	}
	if err := ValidateDiagnosticExecutionArgv(in.Argv); err != nil {
		return err
	}
	if !diagnosisImageIDPattern.MatchString(in.ImageID) {
		return contract.NewFault(contract.ValidationFailed, "invalid diagnostic image ID", "Use the exact immutable local image ID pinned at admission.")
	}
	if err := ValidateName(in.Node); err != nil {
		return err
	}
	if err := in.ReleaseID.Validate(); err != nil {
		return err
	}
	if err := in.DeploymentID.Validate(); err != nil {
		return err
	}
	return in.RevisionID.Validate()
}

// NormalizeDiagnosticExecutionInput strictly decodes a stored pinned input.
// It must not be used as authority to accept caller-supplied placement.
func NormalizeDiagnosticExecutionInput(raw json.RawMessage) (DiagnosticExecutionInput, json.RawMessage, error) {
	var in DiagnosticExecutionInput
	if err := strictDecode(raw, &in); err != nil {
		return in, nil, err
	}
	if err := in.Validate(); err != nil {
		return in, nil, err
	}
	canonical, err := json.Marshal(in)
	return in, canonical, err
}
