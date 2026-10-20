package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/mbvlabs/eregion/internal/contract"
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
	steps  *StepRegistry
}

func NewDispatcher(stateDir, nodeName string, registry *secrets.Registry) (*Dispatcher, error) {
	return NewDispatcherWithSteps(stateDir, nodeName, registry, DefaultRegistry())
}

// NewDispatcherWithSteps builds a dispatcher with an explicit step registry.
// The node binary and the control plane use the same registry so both sides
// derive the same canonical inputs for an effect.
func NewDispatcherWithSteps(stateDir, nodeName string, registry *secrets.Registry, steps *StepRegistry) (*Dispatcher, error) {
	nodeEngine, err := engine.New(stateDir, nodeName, registry)
	if err != nil {
		return nil, err
	}
	if steps == nil {
		steps = DefaultRegistry()
	}
	return &Dispatcher{engine: nodeEngine, steps: steps}, nil
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
	requestEngine.SecretValues = request.Secrets
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
		return ResponseFromStatus(status), nil
	case MethodRun:
		if d.hooks.BeforeRun != nil {
			if err := d.hooks.BeforeRun(ctx, request.Execution); err != nil {
				return Response{Status: engine.StatusFailed, ExecutionID: request.Execution.ExecutionID(), Retryable: true, Detail: registry.Redact(err.Error())}, nil
			}
		}
		step, err := d.steps.Build(request.Execution, request.Payload)
		if err != nil {
			return Response{Status: engine.StatusFailed, ExecutionID: request.Execution.ExecutionID(), ErrorCode: string(contract.PreconditionFailed), Detail: registry.Redact(err.Error())}, nil
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
				ErrorCode:      string(errorCode(err)),
				Detail:         registry.Redact(err.Error()),
			}, nil
		}
		return ResponseFromStatus(status), nil
	default:
		return Response{}, fmt.Errorf("unknown node method %q", request.Method)
	}
}

func permanent(err error) bool {
	return errors.Is(err, engine.ErrJournalCorrupt) ||
		errors.Is(err, engine.ErrInputsMismatch) ||
		errors.Is(err, engine.ErrPermanent)
}

// errorCode classifies a failed step so a caller does not have to read the
// message text. It uses the same taxonomy as the operation contract.
func errorCode(err error) contract.Code {
	switch {
	case errors.Is(err, engine.ErrInputsMismatch), errors.Is(err, engine.ErrJournalCorrupt), errors.Is(err, engine.ErrPermanent):
		return contract.PreconditionFailed
	default:
		return contract.Internal
	}
}

// ResponseFromStatus maps a node status observation to a transport response.
func ResponseFromStatus(status engine.TaskStatus) Response {
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
