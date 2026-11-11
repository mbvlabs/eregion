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
	"unicode/utf8"
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
	return r.redact(text, false)
}

// RedactPreservingLength applies the same registered matches without changing
// the byte length. Short matches use asterisks; longer ones use the usual
// placeholder padded with asterisks. This is for already-budgeted event text
// that must still pass through independent downstream registries.
func (r *Registry) RedactPreservingLength(text string) string {
	return r.redact(text, true)
}

func (r *Registry) redact(text string, preserveLength bool) string {
	if r == nil {
		if !preserveLength || utf8.ValidString(text) {
			return text
		}
		return validBytes(text)
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
		if preserveLength && len(value) > len(text) {
			continue
		}
		replacement := Placeholder
		if preserveLength {
			if len(value) < len(Placeholder) {
				replacement = strings.Repeat("*", len(value))
			} else {
				replacement += strings.Repeat("*", len(value)-len(Placeholder))
			}
		}
		pairs = append(pairs, value, replacement)
	}
	// One pass avoids re-redacting the placeholder and prefers longest matches.
	result := strings.NewReplacer(pairs...).Replace(text)
	if preserveLength && !utf8.ValidString(result) {
		return validBytes(result)
	}
	return result
}

// Replace each invalid byte separately so JSON encoding cannot enlarge it.
func validBytes(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	for len(text) > 0 {
		_, size := utf8.DecodeRuneInString(text)
		if size == 1 && text[0] >= utf8.RuneSelf {
			out.WriteByte('*')
		} else {
			out.WriteString(text[:size])
		}
		text = text[size:]
	}
	return out.String()
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
