// Package rpc holds the node RPC wire types shared by the operator and the node.
package rpc

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// Schema is the request and result schema. Later versions may only add
// optional fields.
const Schema = "eregion-rpc/v1"

// MaxRequestBytes caps the request read from stdin.
const MaxRequestBytes = 1 << 20

// Request is one node RPC call. Secrets carries transient values that are never
// logged, persisted or copied into an operation input.
type Request struct {
	Schema         string            `json:"schema"`
	RequestID      string            `json:"request_id"`
	Method         string            `json:"method"`
	Params         json.RawMessage   `json:"params,omitempty"`
	Secrets        map[string]string `json:"secrets,omitempty"`
	DryRun         bool              `json:"dry_run"`
	Confirm        string            `json:"confirm,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	Operator       string            `json:"operator"`
	ClientVersion  string            `json:"client_version"`
	ClientCommit   string            `json:"client_commit"`
	ClientChannel  string            `json:"client_channel,omitempty"`
	// Stream reports that stdin continues after the request line with the
	// method's payload.
	Stream bool `json:"stream,omitempty"`
}

// Loggable returns the request with secret values dropped. Only the secret
// names remain.
func (r Request) Loggable() map[string]any {
	names := make([]string, 0, len(r.Secrets))
	for name := range r.Secrets {
		names = append(names, name)
	}
	out := map[string]any{
		"schema":          r.Schema,
		"request_id":      r.RequestID,
		"method":          r.Method,
		"dry_run":         r.DryRun,
		"confirm":         r.Confirm,
		"idempotency_key": r.IdempotencyKey,
		"operator":        r.Operator,
		"client_version":  r.ClientVersion,
		"client_commit":   r.ClientCommit,
		"client_channel":  r.ClientChannel,
		"stream":          r.Stream,
		"secret_names":    names,
	}
	if len(r.Params) > 0 {
		out["params"] = r.Params
	}
	return out
}

// Event is one NDJSON progress line before the result.
type Event struct {
	Schema string    `json:"schema"`
	Type   string    `json:"type"`
	Event  EventBody `json:"event"`
}

// EventBody is a transport-neutral progress message. Log carries one
// container-log line when Kind is "log"; Message remains the redacted plain
// text for ordinary progress events.
type EventBody struct {
	Kind    string   `json:"kind,omitempty"`
	Step    string   `json:"step,omitempty"`
	Message string   `json:"message,omitempty"`
	Log     *LogLine `json:"log,omitempty"`
}

// LogLine is one container-log line streamed by the node before the final RPC
// result. It never carries a configured secret value.
type LogLine struct {
	Process   string `json:"process,omitempty"`
	Replica   string `json:"replica,omitempty"`
	Container string `json:"container,omitempty"`
	Stream    string `json:"stream,omitempty"`
	Message   string `json:"message"`
}

// Result is the final NDJSON line. It is written exactly once.
type Result struct {
	Schema string          `json:"schema"`
	Type   string          `json:"type"`
	OK     bool            `json:"ok"`
	Data   json.RawMessage `json:"data,omitempty"`
	Error  *contract.Fault `json:"error,omitempty"`
}

// Build identifies one binary build. The node version check compares it with
// the operator's build.
type Build struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// SameBuild applies the version lockstep rule: versions must match, and a dev
// build must also match on commit because every dev build reports "dev".
func SameBuild(left, right Build) bool {
	if left.Version == "" || left.Version != right.Version {
		return false
	}
	if left.Version == "dev" {
		return left.Commit != "" && left.Commit == right.Commit
	}
	return true
}

var requestIDPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// methodPattern is <group>.<verb>; words may contain inner hyphens.
var methodPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*(\.[a-z][a-z0-9]*(-[a-z0-9]+)*)*$`)

// DecodeRequest reads exactly one JSON request line from r, within
// MaxRequestBytes, and returns a reader for whatever follows it. A request
// without a newline ends at EOF. A streamed request keeps the remaining stdin
// available as its payload; any other request must be followed only by
// whitespace. A request without request_id gets a new one.
func DecodeRequest(r io.Reader) (Request, io.Reader, error) {
	buffered := bufio.NewReaderSize(r, 64*1024)
	line, err := readRequestLine(buffered)
	if err != nil {
		return Request{}, nil, err
	}
	rest := io.MultiReader(buffered, r)
	dec := json.NewDecoder(bytes.NewReader(line))
	var req Request
	if err := dec.Decode(&req); err != nil {
		return Request{}, nil, usage("request is not valid JSON", err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Request{}, nil, usage("request must be exactly one JSON object", "Send one JSON object, then close stdin.")
	}
	if req.Schema != Schema {
		return Request{}, nil, usage("request schema must be "+Schema, "Set \"schema\": \""+Schema+"\".")
	}
	if !methodPattern.MatchString(req.Method) {
		return Request{}, nil, usage("request method is missing or malformed", "Set \"method\", for example \"version\".")
	}
	if req.RequestID == "" {
		req.RequestID = NewRequestID()
	}
	req.RequestID = strings.ToUpper(strings.TrimSpace(req.RequestID))
	if !requestIDPattern.MatchString(req.RequestID) {
		return Request{}, nil, usage("request_id must be a ULID", "Omit request_id and the node assigns one.")
	}
	if len(req.Params) > 0 && !bytes.HasPrefix(bytes.TrimSpace(req.Params), []byte("{")) && string(bytes.TrimSpace(req.Params)) != "null" {
		return Request{}, nil, usage("params must be a JSON object", "")
	}
	if req.Stream {
		return req, rest, nil
	}
	tail, err := io.ReadAll(io.LimitReader(rest, MaxRequestBytes+1))
	if err != nil {
		return Request{}, nil, usage("could not read the request", err.Error())
	}
	if len(bytes.TrimSpace(tail)) > 0 {
		return Request{}, nil, usage("request must be exactly one JSON object", "Send one JSON object, then close stdin; set \"stream\": true only for a method with a payload.")
	}
	return req, nil, nil
}

// readRequestLine consumes one newline-terminated line without reading the
// payload that follows it into a second buffer.
func readRequestLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > MaxRequestBytes {
			return nil, usage("request is larger than 1 MiB", "Send one JSON object of at most 1 MiB.")
		}
		switch {
		case err == nil:
			return bytes.TrimRight(line, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return bytes.TrimRight(line, "\r\n"), nil
		default:
			return nil, usage("could not read the request", err.Error())
		}
	}
}

// ValidMethod reports whether name is a well-formed method name.
func ValidMethod(name string) bool { return methodPattern.MatchString(name) }

// MarshalResult encodes the final result line.
func MarshalResult(result Result) ([]byte, error) {
	result.Schema = Schema
	result.Type = "result"
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// MarshalEvent encodes one event line.
func MarshalEvent(event EventBody) ([]byte, error) {
	raw, err := json.Marshal(Event{Schema: Schema, Type: "event", Event: event})
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func usage(message, hint string) *contract.Fault {
	return contract.NewFault(contract.Usage, message, hint)
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewRequestID returns a ULID: a 48-bit millisecond timestamp and 80 random
// bits in Crockford base32.
func NewRequestID() string {
	var id [16]byte
	ms := uint64(time.Now().UnixMilli()) //nolint:gosec // G115: wall-clock milliseconds are positive.
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], ms)
	copy(id[:6], ts[2:])
	if _, err := rand.Read(id[6:]); err != nil {
		panic(err)
	}
	return encodeULID(id)
}

func encodeULID(id [16]byte) string {
	hi := binary.BigEndian.Uint64(id[:8])
	lo := binary.BigEndian.Uint64(id[8:])
	out := make([]byte, 26)
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&0x1f]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out)
}
