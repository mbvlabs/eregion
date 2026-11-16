package controlplane

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
)

var capturedContainerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// retainNodeResponse records evidence carried by the original node execution.
// It must not change the step outcome, rerun work, or prevent safety cleanup.
func (s *Service) retainNodeResponse(parent context.Context, current engine.Execution, response node.Response) error {
	evidence, ok, err := nodeResponseEvidence(current, response)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	_, _, storeErr := s.mutateOwned(ctx, current.OperationID, current.Attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		exists, err := tx.Operations().NodeEvidenceExists(ctx, op.ID, evidence.ExecutionID, evidence.Outcome)
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
		return appendEvent(ctx, tx, op, contract.Event{
			Kind: "step", Stage: current.Step, Step: current.Step,
			ExecutionID: evidence.ExecutionID, NodeEvidence: &evidence,
			Message: "Original node execution evidence retained.",
		})
	})
	if storeErr != nil && ctx.Err() == nil {
		// A lost capture must not veto cleanup. If the database recovered,
		// retain the loss separately; never imply that output was complete.
		_, _, _ = s.mutateOwned(ctx, current.OperationID, current.Attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
			return appendEvent(ctx, tx, op, contract.Event{Kind: "warning", Stage: "evidence",
				Message:      "Node execution evidence could not be retained.",
				EvidenceLoss: &contract.NodeEvidenceLoss{ExecutionID: evidence.ExecutionID, Attempt: evidence.Attempt}})
		})
	}
	return nil
}

// nodeResponseEvidence decodes and bounds the evidence carried by a terminal
// node response. It returns ok=false for old or evidence-free payloads.
func nodeResponseEvidence(current engine.Execution, response node.Response) (contract.NodeExecutionEvidence, bool, error) {
	evidence, ok := decodeNodeResponseEvidence(current, response)
	if !ok {
		return evidence, false, nil
	}
	if err := finalizeNodeResponseEvidence(current, response, &evidence); err != nil {
		return evidence, false, err
	}
	return evidence, true, nil
}

func decodeNodeResponseEvidence(current engine.Execution, response node.Response) (contract.NodeExecutionEvidence, bool) {
	var evidence contract.NodeExecutionEvidence
	if len(response.Result) == 0 || (response.Status != engine.StatusFailed && response.Status != engine.StatusCompleted) {
		return evidence, false
	}
	if response.Status == engine.StatusFailed {
		if json.Unmarshal(response.Result, &evidence) != nil || evidence.OutputAvailability == "" {
			return evidence, false // Older result or no execution evidence.
		}
		return evidence, true
	}
	var success struct {
		Previous         *contract.NodeContainerEvidence `json:"previous_container_evidence"`
		RedactionContext string                          `json:"redaction_context"`
	}
	if json.Unmarshal(response.Result, &success) != nil || success.Previous == nil {
		return evidence, false
	}
	evidence.Stage = current.Step
	evidence.OutputAvailability = "not_collected"
	evidence.RedactionContext = success.RedactionContext
	evidence.Previous = success.Previous
	return evidence, true
}

func finalizeNodeResponseEvidence(current engine.Execution, response node.Response, evidence *contract.NodeExecutionEvidence) error {
	if response.Attempt == 0 || response.Attempt > current.Attempt || response.ExecutionID == "" ||
		response.ExecutionID != (engine.Execution{OperationID: current.OperationID, Attempt: response.Attempt, Step: current.Step}).ExecutionID() {
		return nodeEffectIdentityFault("the evidence does not match the original node execution")
	}
	evidence.ExecutionID, evidence.Attempt, evidence.Outcome = response.ExecutionID, response.Attempt, string(response.Status)
	if evidence.RedactionContext != "original" && evidence.RedactionContext != "not_required" {
		evidence.Stdout, evidence.Stderr = "", ""
		if evidence.Container != nil {
			evidence.Container.Logs = ""
		}
		if evidence.Previous != nil {
			evidence.Previous.Logs = ""
		}
		evidence.TextOmitted = "original redaction context unavailable"
	}
	if evidence.Stage == "" {
		evidence.Stage = current.Step
	}
	// The node capture has already used the original pinned secret registry,
	// including after sanitization. Keep the existing capture limits; reject
	// malformed or oversized frames rather than trying current vault values.
	if len(evidence.Stage) > 128 || strings.ContainsAny(evidence.Stage, "\x00\r\n") {
		evidence.Stage = current.Step
	}
	var truncated, omitted bool
	evidence.Stdout, truncated, omitted = boundedCapturedText(evidence.Stdout, 8192)
	evidence.StdoutTruncated = evidence.StdoutTruncated || truncated
	if omitted {
		evidence.TextOmitted = "invalid captured output"
	}
	evidence.Stderr, truncated, omitted = boundedCapturedText(evidence.Stderr, 4096)
	evidence.StderrTruncated = evidence.StderrTruncated || truncated
	if omitted {
		evidence.TextOmitted = "invalid captured output"
	}
	evidence.Container = normalizeNodeContainerEvidence(evidence.Container)
	evidence.Previous = normalizeNodeContainerEvidence(evidence.Previous)
	return nil
}

func boundedCapturedText(text string, max int) (string, bool, bool) {
	if !utf8.ValidString(text) {
		return "", false, true
	}
	if len(text) <= max {
		return text, false, false
	}
	text = text[:max]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text, true, false
}

func normalizeNodeContainerEvidence(value *contract.NodeContainerEvidence) *contract.NodeContainerEvidence {
	if value == nil {
		return nil
	}
	if value.Availability != "partial" && value.Availability != "unavailable" ||
		(value.ContainerID != "" && !capturedContainerID.MatchString(value.ContainerID)) {
		return &contract.NodeContainerEvidence{Availability: "unavailable"}
	}
	logs, truncated, omitted := boundedCapturedText(value.Logs, 32*1024)
	value.Logs = logs
	value.Truncated = value.Truncated || truncated || omitted
	return value
}

func nodeResultEvidence(ctx context.Context, tx *entities.StoreTx, result *contract.ActionResult, through uint64) error {
	sequence, raw, found, err := tx.Operations().FailedNodeEvidence(ctx, result.OperationID, through, result.Attempt, result.Stage)
	if err != nil {
		return err
	}
	if found {
		if err := applyNodeExecutionEvidence(result, raw, sequence); err != nil {
			return err
		}
	}
	lost, err := tx.Operations().EvidenceLost(ctx, result.OperationID, through, result.Attempt)
	if err != nil {
		return err
	}
	if lost {
		result.EvidenceSources = append(result.EvidenceSources, contract.EvidenceSource{Kind: "node_capture", Status: "unavailable"})
		switch result.Evidence {
		case "interrupted", "truncated":
		case "not_collected", "unavailable":
			result.Evidence = "unavailable"
		default:
			result.Evidence = "partial"
		}
	}
	return nil
}

func applyNodeExecutionEvidence(result *contract.ActionResult, raw json.RawMessage, sequence uint64) error {
	var evidence contract.NodeExecutionEvidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return err
	}
	if evidence.Stage != "" {
		result.Stage = evidence.Stage
	}
	if evidence.TextOmitted != "" && result.Excerpt == "" {
		result.ExcerptOmitted = evidence.TextOmitted
	}
	status := nodeEvidenceStatus(evidence)
	result.EvidenceSources = append(result.EvidenceSources, contract.EvidenceSource{Kind: "node_execution", Status: status})
	switch {
	case result.Evidence == "interrupted":
		// A later capture cannot complete an interrupted candidate claim.
	case result.Evidence == "truncated" || status == "truncated":
		result.Evidence = "truncated"
	default:
		result.Evidence = "partial"
	}
	excerpt := nodeEvidenceExcerpt(evidence)
	if excerpt == "" || result.Excerpt != "" {
		return nil
	}
	if len(excerpt) > 4096 {
		excerpt = excerpt[:4096]
		for !utf8.ValidString(excerpt) {
			excerpt = excerpt[:len(excerpt)-1]
		}
	}
	result.Excerpt = excerpt
	result.ExcerptOmitted = ""
	result.ExcerptCursor = &contract.Cursor{OperationID: result.OperationID, After: sequence - 1}
	return nil
}

func nodeEvidenceStatus(evidence contract.NodeExecutionEvidence) string {
	status := "partial" // Node command output and logs are bounded.
	if evidence.StdoutTruncated || evidence.StderrTruncated ||
		(evidence.Container != nil && evidence.Container.Truncated) ||
		(evidence.Previous != nil && evidence.Previous.Truncated) {
		return "truncated"
	}
	return status
}

func nodeEvidenceExcerpt(evidence contract.NodeExecutionEvidence) string {
	excerpt := evidence.Stderr
	if excerpt == "" {
		excerpt = evidence.Stdout
	}
	if excerpt == "" && evidence.Container != nil {
		excerpt = evidence.Container.Logs
	}
	return excerpt
}
