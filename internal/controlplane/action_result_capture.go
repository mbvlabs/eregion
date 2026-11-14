package controlplane

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
)

var capturedContainerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// retainNodeResponse records evidence carried by the original node execution.
// It must not change the step outcome, rerun work, or prevent safety cleanup.
func (s *Service) retainNodeResponse(parent context.Context, current engine.Execution, response node.Response) error {
	if len(response.Result) == 0 || (response.Status != engine.StatusFailed && response.Status != engine.StatusCompleted) {
		return nil
	}
	var evidence contract.NodeExecutionEvidence
	if response.Status == engine.StatusFailed {
		if json.Unmarshal(response.Result, &evidence) != nil || evidence.OutputAvailability == "" {
			return nil // Older result or no execution evidence.
		}
	} else {
		var success struct {
			Previous         *contract.NodeContainerEvidence `json:"previous_container_evidence"`
			RedactionContext string                          `json:"redaction_context"`
		}
		if json.Unmarshal(response.Result, &success) != nil || success.Previous == nil {
			return nil
		}
		evidence.Stage = current.Step
		evidence.OutputAvailability = "not_collected"
		evidence.RedactionContext = success.RedactionContext
		evidence.Previous = success.Previous
	}
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
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	_, _, storeErr := s.mutateOwned(ctx, current.OperationID, current.Attempt, func(tx pgx.Tx, op *contract.Operation) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_operation_events
			WHERE operation_id=$1 AND event->'node_evidence'->>'execution_id'=$2
			AND event->'node_evidence'->>'outcome'=$3)`,
			op.ID, evidence.ExecutionID, evidence.Outcome).Scan(&exists); err != nil {
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
		_, _, _ = s.mutateOwned(ctx, current.OperationID, current.Attempt, func(tx pgx.Tx, op *contract.Operation) error {
			return appendEvent(ctx, tx, op, contract.Event{Kind: "warning", Stage: "evidence",
				Message:      "Node execution evidence could not be retained.",
				EvidenceLoss: &contract.NodeEvidenceLoss{ExecutionID: evidence.ExecutionID, Attempt: evidence.Attempt}})
		})
	}
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

func nodeResultEvidence(ctx context.Context, tx pgx.Tx, result *contract.ActionResult, through uint64) error {
	rows, err := tx.Query(ctx, `SELECT sequence,event->'node_evidence' FROM eregion_operation_events
		WHERE operation_id=$1 AND event->'node_evidence'->>'attempt'=$2
		AND event->'node_evidence'->>'outcome'='failed' AND sequence<=$3
		AND ($4='action' OR event->>'step'=$4) ORDER BY sequence LIMIT 1`,
		result.OperationID, strconv.FormatUint(uint64(result.Attempt), 10), through, result.Stage)
	if err != nil {
		return err
	}
	for rows.Next() {
		var sequence uint64
		var raw []byte
		if err := rows.Scan(&sequence, &raw); err != nil {
			rows.Close()
			return err
		}
		var evidence contract.NodeExecutionEvidence
		if err := json.Unmarshal(raw, &evidence); err != nil {
			rows.Close()
			return err
		}
		if evidence.Stage != "" {
			result.Stage = evidence.Stage
		}
		if evidence.TextOmitted != "" && result.Excerpt == "" {
			result.ExcerptOmitted = evidence.TextOmitted
		}
		status := "partial" // Node command output and logs are bounded.
		if evidence.StdoutTruncated || evidence.StderrTruncated ||
			(evidence.Container != nil && evidence.Container.Truncated) ||
			(evidence.Previous != nil && evidence.Previous.Truncated) {
			status = "truncated"
		}
		result.EvidenceSources = append(result.EvidenceSources, contract.EvidenceSource{Kind: "node_execution", Status: status})
		switch {
		case result.Evidence == "interrupted":
			// A later capture cannot complete an interrupted candidate claim.
		case result.Evidence == "truncated" || status == "truncated":
			result.Evidence = "truncated"
		default:
			result.Evidence = "partial"
		}
		excerpt := evidence.Stderr
		if excerpt == "" {
			excerpt = evidence.Stdout
		}
		if excerpt == "" && evidence.Container != nil {
			excerpt = evidence.Container.Logs
		}
		if excerpt != "" && result.Excerpt == "" {
			if len(excerpt) > 4096 {
				excerpt = excerpt[:4096]
				for !utf8.ValidString(excerpt) {
					excerpt = excerpt[:len(excerpt)-1]
				}
			}
			result.Excerpt = excerpt
			result.ExcerptOmitted = ""
			result.ExcerptCursor = &contract.Cursor{OperationID: result.OperationID, After: sequence - 1}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var lost bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_operation_events lost
		WHERE lost.operation_id=$1 AND lost.event->'evidence_loss'->>'attempt'=$2 AND lost.sequence<=$3 AND lost.event->>'stage'='evidence'
		AND lost.event->>'message'='Node execution evidence could not be retained.'
		AND NOT EXISTS(SELECT 1 FROM eregion_operation_events recovered
			WHERE recovered.operation_id=lost.operation_id AND recovered.sequence<=$3
			AND recovered.event->'node_evidence'->>'execution_id'=lost.event->'evidence_loss'->>'execution_id'))`,
		result.OperationID, strconv.FormatUint(uint64(result.Attempt), 10), through).Scan(&lost); err != nil {
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
