package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// Hooks are deterministic test seams. Production callers leave them nil.
type Hooks struct {
	// BeforeRun runs after the request is validated and before Check/Apply.
	BeforeRun func(context.Context, engine.Execution) error
}

// Dispatcher executes node requests against one local state directory.
type Dispatcher struct {
	engine  *engine.Engine
	hooks   Hooks
	steps   *StepRegistry
	dataDir string
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
	return &Dispatcher{engine: nodeEngine, steps: steps, dataDir: filepath.Dir(stateDir)}, nil
}

// SetHooks installs test seams. It is not part of the wire protocol.
func (d *Dispatcher) SetHooks(hooks Hooks) { d.hooks = hooks }

func (d *Dispatcher) Call(ctx context.Context, request Request, onEvent func(Event)) (Response, error) {
	if request.Method == MethodObserve {
		return observeLocal(ctx, request)
	}
	if request.Method == MethodObserveImage {
		return ObserveImage(ctx, request), nil
	}
	if request.Method == MethodInventory {
		return inventoryLocal(ctx, d.dataDir)
	}
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
	if requestEngine.SecretValues == nil {
		requestEngine.SecretValues = map[string]string{}
	}
	report := func(event engine.Event) {
		if onEvent != nil {
			onEvent(Event{Kind: event.Kind, Step: event.Step, ExecutionID: request.Execution.ExecutionID(), Message: registry.RedactPreservingLength(event.Message)})
		}
	}
	if request.Method == MethodStatus {
		status, err := requestEngine.Status(ctx, request.Execution)
		if err != nil {
			return Response{}, err
		}
		return ResponseFromStatus(status), nil
	}
	if request.Method == MethodRetireBuild {
		return RetireBuild(ctx, &requestEngine, request.Execution)
	}
	if request.Method == MethodRetireBuildStopMarker {
		return RetireBuildStopMarker(ctx, &requestEngine, request.Execution, request.InputsHash)
	}
	if request.Method == MethodRun {
		return d.run(ctx, requestEngine, request, registry, report)
	}
	return Response{}, fmt.Errorf("unknown node method %q", request.Method)
}

// RetireBuildStopMarker fences a marker dispatch, not its Pack process.
func RetireBuildStopMarker(ctx context.Context, e *engine.Engine, exec engine.Execution, inputsHash string) (Response, error) {
	status, err := e.RetireBuildStopMarker(ctx, exec, inputsHash)
	if err != nil {
		return Response{}, err
	}
	return ResponseFromStatus(status), nil
}

// RetireBuild accepts only Pack identities, with no payload or secrets needed.
func RetireBuild(ctx context.Context, e *engine.Engine, exec engine.Execution) (Response, error) {
	if !platform.IsPackStep(exec.Step) {
		return Response{Status: engine.StatusFailed, ErrorCode: string(contract.ValidationFailed), Detail: "retirement requires a Pack step"}, nil
	}
	status, err := e.RetireBuild(ctx, exec)
	if err != nil {
		return Response{}, err
	}
	return ResponseFromStatus(status), nil
}

// RetireProvenBuild is the local-only strict retirement path for legacy claims.
// inputsHash and exec must come from the original durable dispatch history.
func RetireProvenBuild(ctx context.Context, e *engine.Engine, exec engine.Execution, inputsHash string) (Response, error) {
	if !platform.IsPackStep(exec.Step) {
		return Response{Status: engine.StatusFailed, ErrorCode: string(contract.ValidationFailed), Detail: "retirement requires a Pack step"}, nil
	}
	status, err := e.RetireProvenBuild(ctx, exec, inputsHash)
	if err != nil {
		return Response{}, err
	}
	return ResponseFromStatus(status), nil
}

// run executes one run request against the request-scoped engine.
func (d *Dispatcher) run(ctx context.Context, requestEngine engine.Engine, request Request, registry *secrets.Registry, report engine.Reporter) (Response, error) {
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
		return runFailureResponse(request, status, err, registry), nil
	}
	return ResponseFromStatus(status), nil
}

// runFailureResponse maps an engine run error to a domain response. A lock
// contention is a live or deferred observation, never a failed task.
func runFailureResponse(request Request, status engine.TaskStatus, err error, registry *secrets.Registry) Response {
	if errors.Is(err, engine.ErrLockHeld) {
		return lockHeldResponse(status, registry)
	}
	executionID, attempt := status.ExecutionID, status.Attempt
	if executionID == "" {
		executionID = request.Execution.ExecutionID()
	}
	if attempt == 0 {
		attempt = request.Execution.Attempt
	}
	return Response{
		Status:         engine.StatusFailed,
		ExecutionID:    executionID,
		Attempt:        attempt,
		AppliedAttempt: status.AppliedAttempt,
		InputsHash:     status.InputsHash,
		Phase:          status.Phase,
		PhaseStartedAt: status.PhaseStartedAt,
		WaitMS:         maxWaitMS(status.WaitMS),
		Retryable:      !permanent(err),
		InputsMismatch: errors.Is(err, engine.ErrInputsMismatch),
		ErrorCode:      string(errorCode(err)),
		Detail:         registry.Redact(err.Error()),
		Result:         status.Result,
	}
}

// lockHeldResponse reports the observed owner rather than the request attempt
// that merely asked to join it.
func lockHeldResponse(status engine.TaskStatus, registry *secrets.Registry) Response {
	statusName := status.Status
	if statusName == "" {
		statusName = engine.StatusRunning
	}
	return Response{
		Status:         statusName,
		ExecutionID:    status.ExecutionID,
		Attempt:        status.Attempt,
		AppliedAttempt: status.AppliedAttempt,
		InputsHash:     status.InputsHash,
		Phase:          status.Phase,
		PhaseStartedAt: status.PhaseStartedAt,
		WaitMS:         maxWaitMS(status.WaitMS),
		Retryable:      true,
		Detail:         registry.Redact(status.Detail),
	}
}

// observeLocal answers a read-only Docker observation on this node.
func observeLocal(ctx context.Context, request Request) (Response, error) {
	var payload ObservePayload
	if len(request.Payload) > 0 {
		if err := json.Unmarshal(request.Payload, &payload); err != nil {
			return Response{Status: engine.StatusFailed, ErrorCode: string(contract.ValidationFailed), Detail: "invalid observation payload"}, nil
		}
	}
	workloads, err := platform.ObserveLocalWorkloads(ctx, payload.App, payload.Environment)
	if err != nil {
		fault := contract.Classify(err)
		return Response{Status: engine.StatusFailed, ErrorCode: string(fault.Code), Detail: fault.Message}, nil
	}
	raw, err := json.Marshal(workloads)
	if err != nil {
		return Response{Status: engine.StatusFailed, ErrorCode: string(contract.Internal), Detail: "the observation could not be encoded"}, nil
	}
	return Response{Status: engine.StatusCompleted, Result: raw}, nil
}

// inventoryLocal answers a read-only disk-retention observation on this node.
func inventoryLocal(ctx context.Context, dataDir string) (Response, error) {
	inventory, err := platform.ObserveRetentionInventory(ctx, platform.RetentionInventoryPaths{DataDir: dataDir}, nil)
	if err != nil {
		fault := contract.Classify(err)
		return Response{Status: engine.StatusFailed, ErrorCode: string(fault.Code), Detail: fault.Message}, nil
	}
	raw, err := json.Marshal(inventory)
	if err != nil {
		return Response{Status: engine.StatusFailed, ErrorCode: string(contract.Internal), Detail: "the inventory could not be encoded"}, nil
	}
	return Response{Status: engine.StatusCompleted, Result: raw}, nil
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
		Phase:          status.Phase,
		PhaseStartedAt: status.PhaseStartedAt,
		WaitMS:         maxWaitMS(status.WaitMS),
		Executed:       status.Executed,
		Retryable:      status.Retryable,
		ErrorCode:      status.ErrorCode,
		Result:         status.Result,
		Detail:         status.Detail,
	}
}

func maxWaitMS(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
