package output

import (
	"encoding/json"
	"reflect"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// FollowResult is the explicitly structured follow response shared by CLI and
// output. Observation is a viewer state, not a free-form message.
type FollowResult struct {
	Operation    contract.Operation `json:"operation"`
	State        contract.State     `json:"state"`
	Attempt      uint32             `json:"attempt"`
	ErrorCode    contract.Code      `json:"error_code,omitempty"`
	Events       uint64             `json:"events"`
	Cursor       uint64             `json:"cursor,string"`
	LastSequence uint64             `json:"last_sequence,string"`
	Observation  string             `json:"observation"`
	Detached     bool               `json:"detached"`
}

func cleanResult(r *secrets.Registry, in contract.ActionResult) contract.ActionResult {
	in.Message = r.Redact(in.Message)
	in.Excerpt = r.Redact(in.Excerpt)
	in.ExcerptOmitted = r.Redact(in.ExcerptOmitted)
	// Data is the source-sanitized action output contract, not arbitrary JSON.
	return in
}

func cleanEvidence(r *secrets.Registry, in contract.ActionEvidenceView) contract.ActionEvidenceView {
	in.Excerpt = r.Redact(in.Excerpt)
	in.ExcerptOmitted = r.Redact(in.ExcerptOmitted)
	return in
}

func cleanOperation(r *secrets.Registry, in contract.Operation) contract.Operation {
	in.ResultReadError = r.Redact(in.ResultReadError)
	in.EvidenceReadError = r.Redact(in.EvidenceReadError)
	if in.LatestResult != nil {
		result := cleanResult(r, *in.LatestResult)
		in.LatestResult = &result
	}
	if in.Evidence != nil {
		evidence := cleanEvidence(r, *in.Evidence)
		in.Evidence = &evidence
	}
	return in
}

func cleanEvent(r *secrets.Registry, in contract.Event) contract.Event {
	in.Message = r.Redact(in.Message)
	if in.Progress != nil {
		progress := *in.Progress
		progress.Unit = r.Redact(progress.Unit)
		in.Progress = &progress
	}
	if in.Health != nil {
		health := *in.Health
		health.Detail = r.Redact(health.Detail)
		health.Target = r.Redact(health.Target)
		in.Health = &health
	}
	if in.NodeEvidence != nil {
		e := *in.NodeEvidence
		e.TextOmitted = r.Redact(e.TextOmitted)
		e.Stdout = r.Redact(e.Stdout)
		e.Stderr = r.Redact(e.Stderr)
		if e.Previous != nil {
			p := *e.Previous
			p.Logs = r.Redact(p.Logs)
			e.Previous = &p
		}
		if e.Container != nil {
			c := *e.Container
			c.Logs = r.Redact(c.Logs)
			e.Container = &c
		}
		in.NodeEvidence = &e
	}
	if in.Result != nil {
		result := cleanResult(r, *in.Result)
		in.Result = &result
	}
	return in
}

func redactData(r *secrets.Registry, value any) ([]byte, error) {
	switch v := value.(type) {
	case contract.Operation:
		return json.Marshal(cleanOperation(r, v))
	case *contract.Operation:
		if v == nil {
			return json.Marshal(nil)
		}
		return json.Marshal(cleanOperation(r, *v))
	case contract.ActionResult:
		return json.Marshal(cleanResult(r, v))
	case *contract.ActionResult:
		if v == nil {
			return json.Marshal(nil)
		}
		return json.Marshal(cleanResult(r, *v))
	case contract.ActionEvidenceView:
		return json.Marshal(cleanEvidence(r, v))
	case *contract.ActionEvidenceView:
		if v == nil {
			return json.Marshal(nil)
		}
		return json.Marshal(cleanEvidence(r, *v))
	case contract.Event:
		return json.Marshal(cleanEvent(r, v))
	case *contract.Event:
		if v == nil {
			return json.Marshal(nil)
		}
		return json.Marshal(cleanEvent(r, *v))
	case []contract.Operation:
		if v == nil {
			return json.Marshal(nil)
		}
		return json.Marshal(cleanOperationList(r, v))
	case []contract.Event:
		if v == nil {
			return json.Marshal(nil)
		}
		return json.Marshal(cleanEventList(r, v))
	case operations.ListPage:
		v.Items = cleanOperationList(r, v.Items)
		return json.Marshal(v)
	case operations.EventPage:
		v.Events = cleanEventList(r, v.Events)
		return json.Marshal(v)
	case operations.SubmissionResult:
		v.Operation = cleanOperation(r, v.Operation)
		return json.Marshal(v)
	case FollowResult:
		v.Operation = cleanOperation(r, v.Operation)
		return json.Marshal(v)
	default:
		return r.JSON(value)
	}
}

func cleanOperationList(r *secrets.Registry, items []contract.Operation) []contract.Operation {
	if items == nil {
		return nil
	}
	cleaned := make([]contract.Operation, len(items))
	for i := range items {
		cleaned[i] = cleanOperation(r, items[i])
	}
	return cleaned
}

func cleanEventList(r *secrets.Registry, events []contract.Event) []contract.Event {
	if events == nil {
		return nil
	}
	cleaned := make([]contract.Event, len(events))
	for i := range events {
		cleaned[i] = cleanEvent(r, events[i])
	}
	return cleaned
}

func redactFaultDetails(r *secrets.Registry, details map[string]any, companion *contract.FollowFaultDetails) ([]byte, error) {
	if companion == nil {
		return r.JSON(details)
	}
	unknown := make(map[string]any)
	known := make(map[string]any)
	for key, value := range details {
		original, ok := companion.Values[key]
		if !ok || !reflect.DeepEqual(value, original) {
			unknown[key] = value
			continue
		}
		switch key {
		case "operation_id", "events", "cursor", "state", "error_code", "resume":
			known[key] = value
		case "result_read_error", "evidence_read_error":
			if text, ok := value.(string); ok {
				known[key] = r.Redact(text)
			} else {
				unknown[key] = value
			}
		case "result", "evidence":
			raw, err := redactData(r, value)
			if err != nil {
				return nil, err
			}
			known[key] = json.RawMessage(raw)
		default:
			unknown[key] = value
		}
	}
	raw, err := r.JSON(unknown)
	if err != nil {
		return nil, err
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(raw, &extra); err != nil {
		return nil, err
	}
	for key, value := range known {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		extra[key] = raw
	}
	return json.Marshal(extra)
}
