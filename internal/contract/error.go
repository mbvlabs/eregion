// Package contract defines transport- and presentation-independent agent contracts.
package contract

import (
	"context"
	"errors"
	"strings"
)

// Schema versions the result and event wire contracts, not the binary release.
const Schema = "eregion/v1"

type Code string

const (
	Usage                Code = "usage_error"
	InvalidOutputMode    Code = "invalid_output_mode"
	InputRequired        Code = "input_required"
	ValidationFailed     Code = "validation_failed"
	MissingTool          Code = "missing_tool"
	NotFound             Code = "not_found"
	Unauthenticated      Code = "unauthenticated"
	Forbidden            Code = "forbidden"
	PreconditionFailed   Code = "precondition_failed"
	ConfirmationRequired Code = "confirmation_required"
	Conflict             Code = "conflict"
	NodeVersionMismatch  Code = "node_version_mismatch"
	RemoteUnreachable    Code = "remote_unreachable"
	Unavailable          Code = "unavailable"
	Cancelled            Code = "cancelled"
	Internal             Code = "internal_error"
)

var exits = map[Code]int{
	Usage: 2, InvalidOutputMode: 2, InputRequired: 2, ValidationFailed: 2, MissingTool: 2,
	NotFound: 3, Unauthenticated: 4, Forbidden: 4,
	PreconditionFailed: 5, ConfirmationRequired: 5, Conflict: 5, NodeVersionMismatch: 5,
	RemoteUnreachable: 6, Unavailable: 6, Internal: 70, Cancelled: 130,
}

func KnownCode(code Code) bool { _, ok := exits[code]; return ok }

// Fault is safe to expose after redaction. Causes are never serialized.
type Fault struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Hint    string         `json:"hint,omitempty"`
	Details map[string]any `json:"details,omitempty"`
	Cause   error          `json:"-"`
}

func (e *Fault) Error() string { return e.Message }
func (e *Fault) Unwrap() error { return e.Cause }

func NewFault(code Code, message, hint string) *Fault {
	return &Fault{Code: code, Message: message, Hint: hint}
}

// Classify never leaks arbitrary implementation errors or their causes.
func Classify(err error) *Fault {
	var fault *Fault
	if errors.As(err, &fault) && fault != nil && KnownCode(fault.Code) && fault.Message != "" {
		copy := *fault
		return &copy
	}
	if errors.Is(err, context.Canceled) {
		return NewFault(Cancelled, "request interrupted", "An interrupted viewer does not cancel server-side work.")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return NewFault(Unavailable, "request deadline exceeded", "Inspect the existing operation before submitting again.")
	}
	return &Fault{Code: Internal, Message: "an internal error occurred", Hint: "Report the command and binary version.", Cause: err}
}

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	return exits[Classify(err).Code]
}

type Envelope struct {
	Schema  string   `json:"schema"`
	OK      bool     `json:"ok"`
	Command string   `json:"command"`
	Data    any      `json:"data,omitempty"`
	Error   *Fault   `json:"error,omitempty"`
	Next    []string `json:"next,omitempty"`
}

func (e Envelope) Validate() error {
	if e.Schema != Schema || strings.TrimSpace(e.Command) == "" {
		return NewFault(ValidationFailed, "invalid result identity", "Provide the supported schema and command path.")
	}
	if e.OK && e.Error != nil {
		return NewFault(ValidationFailed, "success cannot carry an error", "Use a success or failure envelope, not both.")
	}
	if !e.OK && (e.Data != nil || e.Error == nil || !KnownCode(e.Error.Code) || strings.TrimSpace(e.Error.Message) == "") {
		return NewFault(ValidationFailed, "invalid failure result", "Provide a known error code and message without success data.")
	}
	for _, next := range e.Next {
		if strings.TrimSpace(next) == "" {
			return NewFault(ValidationFailed, "empty next step", "Provide actionable next steps or omit them.")
		}
	}
	return nil
}

func Success(command string, data any, next []string) Envelope {
	return Envelope{Schema: Schema, OK: true, Command: command, Data: data, Next: next}
}

func Failure(command string, err error) Envelope {
	return Envelope{Schema: Schema, OK: false, Command: command, Error: Classify(err)}
}
