// Package secrets redacts explicitly registered values at serialization boundaries.
// Registries belong to an invocation/request; there is no process-global registry.
package secrets

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const Placeholder = "[REDACTED]"

// Registry's zero value is usable. Never copy a Registry after first use.
type Registry struct {
	mu     sync.RWMutex
	values map[string]struct{}
}

// Add registers even short secrets. Callers must register secrets before emitting
// anything derived from them. Empty values have no content to redact.
func (r *Registry) Add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = make(map[string]struct{})
	}
	for _, value := range values {
		if value != "" {
			r.values[value] = struct{}{}
			// Cobra/pflag can quote rejected inputs before we see their errors.
			// Register that common representation too, including its bare interior.
			quoted := strconv.Quote(value)
			r.values[quoted] = struct{}{}
			r.values[quoted[1:len(quoted)-1]] = struct{}{}
		}
	}
}

func (r *Registry) Redact(text string) string {
	if r == nil {
		return text
	}
	r.mu.RLock()
	values := make([]string, 0, len(r.values))
	for value := range r.values {
		values = append(values, value)
	}
	r.mu.RUnlock()
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})
	pairs := make([]string, 0, len(values)*2)
	for _, value := range values {
		pairs = append(pairs, value, Placeholder)
	}
	// One pass avoids re-redacting the placeholder and prefers longest matches.
	return strings.NewReplacer(pairs...).Replace(text)
}

// JSON redacts decoded strings and object keys before re-encoding. UseNumber
// preserves integers such as event cursors; no float64 round trip is permitted.
// A serialization failure returns an error, never partial/raw sensitive output.
func (r *Registry) JSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}
	return json.Marshal(r.walk(generic))
}

func (r *Registry) walk(value any) any {
	switch value := value.(type) {
	case string:
		return r.Redact(value)
	case []any:
		for i, item := range value {
			value[i] = r.walk(item)
		}
		return value
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[r.Redact(key)] = r.walk(item)
		}
		return out
	default:
		return value
	}
}
