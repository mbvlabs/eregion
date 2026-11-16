// Package output renders contracts without depending on command handlers or transports.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/colorprofile"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
)

type Mode string

const (
	Plain Mode = "plain"
	JSON  Mode = "json"
)

// OptionsFromArgs recovers output intent even when Cobra rejects a command/flag
// before parsing persistent flags. It respects -- and never consumes stdin.
// On conflicting/invalid modes, an explicit JSON request still gets a JSON error.
func OptionsFromArgs(args []string, valueFlags ...string) (Mode, error) {
	mode := Plain
	var selected string
	var outputSet, jsonEnabled bool
	var parseErr error
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		switch {
		case arg == "--json", arg == "--json=true":
			jsonEnabled = true
		case arg == "--json=false":
			jsonEnabled = false
		case strings.HasPrefix(arg, "--json="):
			parseErr = contract.NewFault(contract.Usage, "invalid --json value", "Use --json, --json=true, or --json=false.")
		case arg == "--output":
			outputSet = true
			value, next, ok := outputValue(args, i)
			if !ok {
				parseErr = contract.NewFault(contract.InputRequired, "--output requires a value", "Use --output plain or --output json.")
				continue
			}
			i = next
			selected = value
		case strings.HasPrefix(arg, "--output="):
			outputSet = true
			selected = strings.TrimPrefix(arg, "--output=")
		case isValueFlag(arg, valueFlags) && i+1 < len(args):
			i++ // A value that resembles --json is still a value.
		}
	}
	if selected == string(JSON) || jsonEnabled {
		mode = JSON
	}
	if parseErr != nil {
		return mode, parseErr
	}
	if outputSet && selected != string(Plain) && selected != string(JSON) {
		return mode, contract.NewFault(contract.InvalidOutputMode, "invalid output mode", "Use plain or json; NDJSON is reserved for event-following commands.")
	}
	if jsonEnabled && outputSet && selected != string(JSON) {
		return mode, contract.NewFault(contract.InvalidOutputMode, "conflicting output modes", "Use --json or --output json; do not combine --json with --output plain.")
	}
	return mode, nil
}

func outputValue(args []string, index int) (string, int, bool) {
	if index+1 >= len(args) || strings.HasPrefix(args[index+1], "-") {
		return "", index, false
	}
	return args[index+1], index + 1, true
}

func isValueFlag(arg string, names []string) bool {
	for _, name := range names {
		if arg == "--"+name {
			return true
		}
	}
	return false
}

type Writer struct {
	Out     io.Writer
	Err     io.Writer
	Mode    Mode
	Secrets *secrets.Registry
}

func (w Writer) Write(envelope contract.Envelope, plain string) error {
	if err := envelope.Validate(); err != nil {
		return err
	}
	if w.Mode == JSON {
		clean, err := redactEnvelope(w.Secrets, envelope)
		if err != nil {
			return err
		}
		return writeJSON(w.Out, clean)
	}
	if !envelope.OK {
		fault := envelope.Error
		text := fmt.Sprintf("error [%s]: %s\n", fault.Code, fault.Message)
		if lines := outputTail(fault.Details); len(lines) > 0 {
			text += "last output:\n  " + strings.Join(lines, "\n  ") + "\n"
		}
		if fault.Hint != "" {
			text += "hint: " + fault.Hint + "\n"
		}
		return writeAll(w.Err, []byte(SafeText(w.Secrets.Redact(text))))
	}
	text := w.Secrets.Redact(strings.TrimRight(plain, "\n") + "\n")
	return writeAll(w.Out, []byte(safePlain(w.Out, text)))
}

// safePlain keeps human styling only where the destination supports it: the
// colour profile is resolved against the real writer, so a pipe, redirected
// file, dumb terminal or NO_COLOR gets plain text. SafeStyled then guards the
// remaining control characters while preserving the palette's SGR sequences.
func safePlain(out io.Writer, text string) string {
	var styled bytes.Buffer
	writer := colorprofile.Writer{Forward: &styled, Profile: colorprofile.Detect(out, os.Environ())}
	_, _ = writer.Write([]byte(text))
	return SafeStyled(styled.String())
}

// SafeStyled is SafeText for plain text that already carries the ui palette's
// SGR colour sequences: a numeric SGR sequence is kept, every other control
// character (including OSC, cursor movement and C1 controls) is made visible.
func SafeStyled(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		if text[i] == '\x1b' && i+1 < len(text) && text[i+1] == '[' {
			end := i + 2
			for end < len(text) && (text[end] >= '0' && text[end] <= '9' || text[end] == ';' || text[end] == ':') {
				end++
			}
			if end < len(text) && text[end] == 'm' {
				out.WriteString(text[i : end+1])
				i = end + 1
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.In(r, unicode.Cf)) {
			fmt.Fprintf(&out, "\\u%04x", r)
		} else {
			out.WriteRune(r)
		}
		i += size
	}
	return out.String()
}

// outputTail is a failed script's last output lines, as a host step fault
// carries them locally ([]string) or decoded from a node envelope ([]any).
func outputTail(details map[string]any) []string {
	switch lines := details["output_tail"].(type) {
	case []string:
		return lines
	case []any:
		out := make([]string, 0, len(lines))
		for _, line := range lines {
			if text, ok := line.(string); ok {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

// SafeText makes untrusted terminal controls visible; it never interprets ANSI/OSC.
// Newlines and tabs remain useful in append-only text. Machine JSON is escaped by
// the JSON encoder instead, preserving its actual string data for clients.
func SafeText(text string) string {
	var out strings.Builder
	for _, r := range text {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.In(r, unicode.Cf)) {
			fmt.Fprintf(&out, "\\u%04x", r)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func writeJSON(out io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeAll(out, append(data, '\n'))
}

// Redact dynamic content before wrapping it in protocol structure. A secret such
// as "schema" or "v1" must not rename wire keys or corrupt fixed schema/error-code
// constants. Static protocol literals are public, not secret-derived data.
func redactEnvelope(registry *secrets.Registry, envelope contract.Envelope) (contract.Envelope, error) {
	clean := envelope
	clean.Command = registry.Redact(envelope.Command)
	clean.Next = make([]string, len(envelope.Next))
	for i, next := range envelope.Next {
		clean.Next[i] = registry.Redact(next)
	}
	if envelope.Data != nil {
		data, err := redactData(registry, envelope.Data)
		if err != nil {
			return contract.Envelope{}, err
		}
		clean.Data = json.RawMessage(data)
	}
	if envelope.Error != nil {
		fault, err := redactFault(registry, envelope.Error)
		if err != nil {
			return contract.Envelope{}, err
		}
		clean.Error = fault
	}
	return clean, nil
}

func redactFault(registry *secrets.Registry, fault *contract.FaultError) (*contract.FaultError, error) {
	clean := *fault
	clean.Message = registry.Redact(fault.Message)
	clean.Hint = registry.Redact(fault.Hint)
	if clean.Details == nil {
		return &clean, nil
	}
	data, err := redactFaultDetails(registry, clean.Details, clean.FollowDetails)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	clean.Details = nil // Decode into a fresh map; never retain/mutate raw keys.
	if err := decoder.Decode(&clean.Details); err != nil {
		return nil, err
	}
	return &clean, nil
}

func writeAll(out io.Writer, data []byte) error {
	n, err := out.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}
