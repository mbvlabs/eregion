package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// Hooks are deterministic test seams. Production callers leave them nil.
type Hooks struct {
	// BeforeRun runs after the request is validated and before Check/Apply.
	BeforeRun func(context.Context, engine.Execution) error
}

// Dispatcher executes node requests against one local state directory.
type Dispatcher struct {
	engine *engine.Engine
	hooks  Hooks
}

func NewDispatcher(stateDir, nodeName string, registry *secrets.Registry) (*Dispatcher, error) {
	nodeEngine, err := engine.New(stateDir, nodeName, registry)
	if err != nil {
		return nil, err
	}
	return &Dispatcher{engine: nodeEngine}, nil
}

// SetHooks installs test seams. It is not part of the wire protocol.
func (d *Dispatcher) SetHooks(hooks Hooks) { d.hooks = hooks }

func (d *Dispatcher) Call(ctx context.Context, request Request, onEvent func(Event)) (Response, error) {
	if err := request.Execution.Validate(); err != nil {
		return Response{}, fmt.Errorf("invalid execution identity: %w", err)
	}
	// Register request secrets before any event, log, error, or journal write.
	registry := &secrets.Registry{}
	for _, value := range request.Secrets {
		registry.Add(value)
	}
	requestEngine := *d.engine
	requestEngine.Secrets = registry
	report := func(event engine.Event) {
		if onEvent != nil {
			onEvent(Event{Kind: event.Kind, Step: event.Step, ExecutionID: request.Execution.ExecutionID(), Message: registry.Redact(event.Message)})
		}
	}
	switch request.Method {
	case MethodStatus:
		status, err := requestEngine.Status(ctx, request.Execution)
		if err != nil {
			return Response{}, err
		}
		return responseFromStatus(status), nil
	case MethodRun:
		if d.hooks.BeforeRun != nil {
			if err := d.hooks.BeforeRun(ctx, request.Execution); err != nil {
				return Response{Status: engine.StatusFailed, ExecutionID: request.Execution.ExecutionID(), Retryable: true, Detail: registry.Redact(err.Error())}, nil
			}
		}
		step, err := diagnosticStep(request)
		if err != nil {
			return Response{Status: engine.StatusFailed, ExecutionID: request.Execution.ExecutionID(), Detail: registry.Redact(err.Error())}, nil
		}
		status, err := requestEngine.Run(ctx, step, request.Execution, report)
		if err != nil {
			if errors.Is(err, engine.ErrLockHeld) {
				return Response{
					Status:      engine.StatusRunning,
					ExecutionID: request.Execution.ExecutionID(),
					InputsHash:  status.InputsHash,
					Retryable:   true,
					Detail:      registry.Redact(status.Detail),
				}, nil
			}
			return Response{
				Status:         engine.StatusFailed,
				ExecutionID:    request.Execution.ExecutionID(),
				InputsHash:     status.InputsHash,
				Retryable:      !permanent(err),
				InputsMismatch: errors.Is(err, engine.ErrInputsMismatch),
				Detail:         registry.Redact(err.Error()),
			}, nil
		}
		return responseFromStatus(status), nil
	default:
		return Response{}, fmt.Errorf("unknown node method %q", request.Method)
	}
}

func permanent(err error) bool {
	return errors.Is(err, engine.ErrJournalCorrupt) ||
		errors.Is(err, engine.ErrInputsMismatch) ||
		errors.Is(err, engine.ErrPermanent)
}

func responseFromStatus(status engine.TaskStatus) Response {
	return Response{
		Status:         status.Status,
		ExecutionID:    status.ExecutionID,
		Attempt:        status.Attempt,
		AppliedAttempt: status.AppliedAttempt,
		InputsHash:     status.InputsHash,
		Executed:       status.Executed,
		Result:         status.Result,
		Detail:         status.Detail,
	}
}

func diagnosticStep(request Request) (engine.Step, error) {
	var payload DiagnosticPayload
	if len(request.Payload) == 0 {
		return nil, errors.New("diagnostic step payload is required")
	}
	if err := json.Unmarshal(request.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid diagnostic step payload: %w", err)
	}
	return NewDiagnosticStep(request.Execution, payload)
}
