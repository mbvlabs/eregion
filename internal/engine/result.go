package engine

import (
	"bytes"
	"encoding/json"

	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// safeTaskResult preserves the schema and validated identities used for
// reconciliation while treating other result strings as presentation text.
func safeTaskResult(registry *secrets.Registry, value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var item any
	if err := decoder.Decode(&item); err != nil {
		return nil, err
	}
	var clean func(string, any) any
	clean = func(key string, value any) any {
		switch value := value.(type) {
		case string:
			if key == "redaction_context" && (value == "original" || value == "not_required" || value == "unavailable") {
				return value
			}
			switch key {
			case "id", "container", "container_id", "image", "image_id", "reference", "digest",
				"operation_id", "release_id", "deployment_id", "revision_id", "build_id",
				"execution_id", "step", "stage", "state", "status", "availability",
				"output_availability", "health", "process", "replica", "kind",
				"app", "environment", "commit", "commit_sha", "timestamp", "observed_at",
				"started_at", "finished_at", "log_timestamp_source", "volume", "unit":
				return value
			default:
				return registry.Redact(output.SafeText(registry.Redact(value)))
			}
		case []any:
			for i := range value {
				value[i] = clean(key, value[i])
			}
			return value
		case map[string]any:
			for k, child := range value {
				value[k] = clean(k, child)
			}
			return value
		default:
			return value
		}
	}
	return json.Marshal(clean("", item))
}
