package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/platform"
)

// appendActionResultTx runs under the operation row lock and shares the
// transaction with settlement. A retry cannot append the same outcome twice.
func (s *Service) appendActionResultTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, status contract.State, code contract.Code, message string, final bool) error {
	exists, err := tx.Operations().ResultExists(ctx, op.ID, op.Attempt, status, final)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	started := op.CreatedAt
	if attemptStarted, found, err := tx.Operations().AttemptStartedAt(ctx, op.ID, op.Attempt); err != nil {
		return err
	} else if found {
		started = attemptStarted
	}
	now := time.Now().UTC()
	if started.After(now) {
		started = now
	}
	result := contract.ActionResult{
		OperationID: op.ID, Action: op.Kind, Target: op.Target, Attempt: op.Attempt,
		Status: status, Final: final, StartedAt: started, CompletedAt: now,
		TimingBasis: "operation_created",
		Stage:       "action", Code: code, Message: s.resultText(message),
		Evidence: "not_collected", History: contract.Cursor{OperationID: op.ID},
	}
	tryEnrichActionResultTx(ctx, tx, &result)
	if op.PendingResult != nil {
		result.Data = op.PendingResult.Data
		result.DataStatus = op.PendingResult.DataStatus
		if op.PendingResult.Message != "" {
			result.Message = op.PendingResult.Message
		}
	}
	tryActionOutputTx(ctx, tx, &result)
	tryActionPayloadTx(ctx, tx, op, &result, s)
	if err := appendEvent(ctx, tx, op, contract.Event{Kind: "step", Stage: result.Stage, Result: &result}); err != nil {
		return err
	}
	op.LatestResult = &result
	return nil
}

func enrichActionResultTx(ctx context.Context, tx *entities.StoreTx, result *contract.ActionResult) error {
	id := result.OperationID
	if attemptStarted, found, err := tx.Operations().AttemptStartedAt(ctx, id, result.Attempt); err != nil {
		return err
	} else if found && !attemptStarted.After(result.CompletedAt) {
		result.StartedAt = attemptStarted
		result.TimingBasis = "attempt_observation"
	}
	if result.Status == contract.Failed {
		stage, err := tx.Results().FailedStage(ctx, id, result.Attempt)
		if err != nil {
			return err
		}
		result.Stage = stage
	}
	identity, err := tx.Platform().Deployments().ResultIdentity(ctx, id)
	if err != nil {
		return err
	}
	result.Identities.BuildID = string(identity.BuildID)
	result.Identities.DeploymentID = string(identity.DeploymentID)
	result.Identities.RevisionID = string(identity.RevisionID)
	result.Identities.ReleaseID = string(identity.ReleaseID)
	result.Identities.Image = identity.Image
	result.Identities.ImageID = identity.ImageID
	result.Identities.CommitSHA = identity.CommitSHA
	lastSequence, found, err := tx.Operations().LastSequence(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		lastSequence = 0
	}
	if err := candidateResultEvidence(ctx, tx, result, lastSequence); err != nil {
		return err
	}
	if result.Status == contract.Failed {
		if err := nodeResultEvidence(ctx, tx, result, lastSequence); err != nil {
			return err
		}
	}
	return nil
}

// A read failure must not veto domain settlement or cleanup. A savepoint also
// clears PostgreSQL's aborted-transaction state after a failed enrichment query.
func tryEnrichActionResultTx(ctx context.Context, tx *entities.StoreTx, result *contract.ActionResult) {
	if tx == nil {
		result.Evidence = "unavailable"
		return
	}
	copy := *result
	if err := tx.WithinSavepoint(ctx, func(nested *entities.StoreTx) error {
		return enrichActionResultTx(ctx, nested, &copy)
	}); err != nil {
		result.Evidence = "unavailable"
		return
	}
	*result = copy
}

// candidateResultEvidence only uses already-redacted retained capture events
// for this attempt. A claim without its summary is interrupted, not complete.
func candidateResultEvidence(ctx context.Context, tx *entities.StoreTx, result *contract.ActionResult, through uint64) error {
	rows, err := tx.Results().CandidateDiagnostics(ctx, result.OperationID, result.Attempt, through)
	if err != nil {
		return err
	}
	captures := make(map[string]*candidateCaptureState)
	for _, row := range rows {
		// Step identifies the capture claim. Older events without a step are
		// grouped by deployment and remain generically readable.
		key := row.DeploymentID + "\x00" + row.Step
		capture := captures[key]
		if capture == nil {
			capture = &candidateCaptureState{}
			captures[key] = capture
		}
		if row.Status == "captured" || row.Status == "observed" {
			capture.captured = true
		}
		if row.Status == "captured" && result.Excerpt == "" && row.Message != "" {
			applyCandidateExcerpt(result, row)
		}
		if strings.HasPrefix(row.Message, "Candidate diagnostic capture: ") {
			capture.summary = row.Status
		}
	}
	result.Evidence = candidateEvidence(captures)
	if len(captures) != 0 {
		result.EvidenceSources = append(result.EvidenceSources, contract.EvidenceSource{Kind: "candidate", Status: result.Evidence})
	}
	return nil
}

type candidateCaptureState struct {
	captured bool
	summary  string
}

func applyCandidateExcerpt(result *contract.ActionResult, row entities.CandidateDiagnostic) {
	if row.RedactionContext != "original" && row.RedactionContext != "not_required" {
		result.ExcerptOmitted = "original redaction context unavailable"
		return
	}
	// Retained candidate log lines have already passed the original action's
	// pinned redaction boundary and existing event limits.
	message := row.Message
	const max = 4096 // Existing worker event text bound.
	if len(message) > max {
		message = message[:max]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	result.Excerpt = message
	result.ExcerptOmitted = ""
	cursor := contract.Cursor{OperationID: result.OperationID, After: row.Sequence - 1}
	result.ExcerptCursor = &cursor
}

func candidateEvidence(captures map[string]*candidateCaptureState) string {
	interrupted, truncated, partial, unavailable, complete := false, false, false, false, false
	for _, capture := range captures {
		switch {
		case capture.summary == "":
			interrupted = true
		case capture.summary == "truncated" || strings.HasSuffix(capture.summary, "_truncated"):
			truncated = true
		case capture.summary == "complete":
			complete = true
		case capture.summary == "unavailable" && !capture.captured:
			unavailable = true
		default:
			partial = true
		}
	}
	switch {
	case len(captures) == 0:
		return "not_collected"
	case interrupted:
		return "interrupted"
	case truncated:
		return "truncated"
	case partial || (unavailable && complete):
		return "partial"
	case unavailable:
		return "unavailable"
	default:
		return "full"
	}
}

func (s *Service) resultText(text string) string {
	text = s.redact(text)
	text = output.SafeText(text)
	text = s.redact(text)
	const max = 4096
	if len(text) > max {
		text = text[:max]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return s.redact(text)
}

// sanitizeResultData redacts decoded strings, including escaped control
// characters, before serializing the secret-free extension result.
func (s *Service) sanitizeResultData(value any, preserve map[string]bool) (any, int) {
	switch v := value.(type) {
	case string:
		return s.resultText(v), 0
	case []any:
		omitted := 0
		for i := range v {
			var count int
			v[i], count = s.sanitizeResultData(v[i], nil)
			omitted += count
		}
		return v, omitted
	case map[string]any:
		clean := make(map[string]any, len(v))
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		// Fixed fields win over dynamic fields that redact to the same key.
		sort.Slice(keys, func(i, j int) bool {
			if fixedResultField(keys[i]) != fixedResultField(keys[j]) {
				return fixedResultField(keys[i])
			}
			return keys[i] < keys[j]
		})
		omitted := 0
		for _, k := range keys {
			key := k
			// Protocol field names are public structure, not captured output.
			// Unknown extension keys remain untrusted text.
			if !fixedResultField(key) {
				key = s.resultText(key)
			}
			if _, exists := clean[key]; exists {
				omitted++
				continue
			}
			if preserve[k] {
				clean[key] = v[k]
			} else {
				var count int
				clean[key], count = s.sanitizeResultData(v[k], nil)
				omitted += count
			}
		}
		return clean, omitted
	}
	return value, 0
}

func fixedResultField(key string) bool {
	switch key {
	case "stdout", "stderr", "status", "stage", "value", "counter",
		"operation_id", "execution_id", "attempt", "exit_code",
		"image", "image_id", "release_id", "deployment_id", "revision_id",
		"app", "environment", "domain", "name", "node", "resource",
		"message", "data", "available", "truncated", "zone", "changes",
		"applied", "unchanged", "id", "subject", "device_id", "routes",
		"expires_at", "revoked_at":
		return true
	default:
		return false
	}
}

// trustedIntegrationFields preserves only validated identities emitted by the
// actual registered integration executor, never an arbitrary extension map.
func trustedIntegrationFields(op contract.Operation, normalized platform.Normalized, executor platform.Executor, value any) map[string]bool {
	out := map[string]bool{}
	registered := normalized.Executor()
	typ := reflect.TypeOf(executor)
	if normalized.Kind != op.Kind || typ == nil || typ.Kind() != reflect.Pointer ||
		typ.Elem().PkgPath() != "github.com/mbvlabs/eregion/internal/integrations" ||
		typ.Elem().Name() != "actionExecutor" || reflect.TypeOf(registered) != typ ||
		reflect.ValueOf(executor).Pointer() != reflect.ValueOf(registered).Pointer() {
		return out
	}
	data, ok := value.(map[string]any)
	if !ok {
		return out
	}
	var input struct {
		ZoneName string   `json:"zone_name"`
		GrantID  string   `json:"grant_id"`
		Subject  string   `json:"subject"`
		DeviceID string   `json:"device_id"`
		Routes   []string `json:"routes"`
	}
	if json.Unmarshal(normalized.Canonical, &input) != nil {
		return out
	}
	match := func(key, expected string) {
		if expected != "" && data[key] == expected {
			out[key] = true
		}
	}
	switch normalized.Kind {
	case integrations.KindDNSReconcile:
		match("zone", input.ZoneName)
		match("operation_id", string(op.ID))
	case integrations.KindAccessGrant:
		sum := sha256.Sum256([]byte("eregion/access/grant/" + string(op.ID)))
		match("id", "grant_"+hex.EncodeToString(sum[:16]))
		match("subject", input.Subject)
		match("device_id", input.DeviceID)
		if routes, ok := data["routes"].([]any); ok && len(routes) == len(input.Routes) && len(routes) > 0 {
			same := true
			for i := range routes {
				if routes[i] != input.Routes[i] {
					same = false
					break
				}
			}
			out["routes"] = same
		}
		validResultTime(data, out, "expires_at")
	case integrations.KindAccessRevoke:
		match("id", input.GrantID)
		validResultTime(data, out, "revoked_at")
	}
	return out
}

func validResultTime(data map[string]any, out map[string]bool, key string) {
	if value, ok := data[key].(string); ok {
		if _, err := time.Parse(time.RFC3339Nano, value); err == nil {
			out[key] = true
		}
	}
}

func (s *Service) latestActionResult(ctx context.Context, op *contract.Operation) error {
	result, found, err := s.entities.Operations().LatestResult(ctx, op.ID, op.LastSequence)
	if err != nil {
		return unavailable(err)
	}
	if found {
		op.LatestResult = result
	}
	return nil
}

func (s *Service) populateActionResult(ctx context.Context, op *contract.Operation) error {
	if err := s.latestActionResult(ctx, op); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		op.ResultReadError = "operation result history is unavailable"
		return nil
	}
	if op.LatestResult != nil {
		if err := s.populateEvidenceView(ctx, op); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			op.EvidenceReadError = "operation evidence history is unavailable"
		}
	}
	return nil
}

func (s *Service) populateEvidenceView(ctx context.Context, op *contract.Operation) error {
	var result contract.ActionResult
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		result = contract.ActionResult{
			OperationID: op.ID, Attempt: op.LatestResult.Attempt, Status: op.LatestResult.Status,
			Stage: "action", Evidence: "not_collected", History: contract.Cursor{OperationID: op.ID},
		}
		if err := candidateResultEvidence(ctx, tx, &result, op.LastSequence); err != nil {
			return err
		}
		return nodeResultEvidence(ctx, tx, &result, op.LastSequence)
	})
	if err != nil {
		return err
	}
	op.Evidence = &contract.ActionEvidenceView{
		Attempt: result.Attempt, Availability: result.Evidence, Sources: result.EvidenceSources,
		Excerpt: result.Excerpt, ExcerptOmitted: result.ExcerptOmitted, ExcerptCursor: result.ExcerptCursor, History: result.History, Through: op.LastSequence,
	}
	return nil
}

func (s *Service) recordRetryResult(ctx context.Context, id contract.OperationID, attempt uint32, code contract.Code, message string) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		return s.appendActionResultTx(ctx, tx, op, contract.Failed, code, message, false)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}
