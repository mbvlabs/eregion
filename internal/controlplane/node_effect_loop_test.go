package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

// scriptedNodeEffect provides deterministic status/run observations for the
// execution loop without a database or a real node.
type scriptedNodeEffect struct {
	statuses      []node.Response
	statusErrors  []error
	dispatches    []node.Response
	dispatchError error

	statusCalls   int
	dispatchCalls int
	completed     int
	failed        int
	waits         int
}

func (s *scriptedNodeEffect) Status(context.Context) (node.Response, error) {
	if s.statusCalls < len(s.statusErrors) && s.statusErrors[s.statusCalls] != nil {
		err := s.statusErrors[s.statusCalls]
		s.statusCalls++
		return node.Response{}, err
	}
	index := s.statusCalls
	s.statusCalls++
	if len(s.statuses) == 0 {
		return node.Response{Status: engine.StatusAbsent}, nil
	}
	if index >= len(s.statuses) {
		return s.statuses[len(s.statuses)-1], nil
	}
	return s.statuses[index], nil
}

func (s *scriptedNodeEffect) Dispatch(context.Context) (node.Response, error) {
	index := s.dispatchCalls
	s.dispatchCalls++
	if s.dispatchError != nil {
		return node.Response{}, s.dispatchError
	}
	if len(s.dispatches) == 0 {
		return node.Response{Status: engine.StatusRunning}, nil
	}
	if index >= len(s.dispatches) {
		return s.dispatches[len(s.dispatches)-1], nil
	}
	return s.dispatches[index], nil
}

func scriptedLoop(script *scriptedNodeEffect, cancelled func(context.Context) error) nodeEffectLoop {
	return nodeEffectLoop{
		Status:   script.Status,
		Dispatch: script.Dispatch,
		Completed: func(context.Context, node.Response, bool) error {
			script.completed++
			return nil
		},
		Failed: func(context.Context, node.Response) error {
			script.failed++
			return errors.New("scripted failed callback")
		},
		Cancelled: cancelled,
		Wait: func(context.Context, time.Duration) error {
			script.waits++
			return nil
		},
		Policy: nodeEffectPolicy{
			StatusPoll: time.Nanosecond, RetryBackoff: time.Nanosecond,
			DeferredBackoff: time.Nanosecond, ObservationEvery: time.Nanosecond,
		},
		Current: engine.Execution{OperationID: "op", Attempt: 1, Step: "step"},
	}
}

func completedResponse() node.Response {
	return completedResponseAt(1)
}

func completedResponseAt(attempt uint32) node.Response {
	return node.Response{
		Status:      engine.StatusCompleted,
		ExecutionID: engine.Execution{OperationID: "op", Attempt: attempt, Step: "step"}.ExecutionID(),
	}
}

func failedResponseAt(attempt uint32, retryable bool) node.Response {
	return node.Response{
		Status:      engine.StatusFailed,
		Retryable:   retryable,
		ErrorCode:   "internal_error",
		Detail:      "transient",
		Attempt:     attempt,
		ExecutionID: engine.Execution{OperationID: "op", Attempt: attempt, Step: "step"}.ExecutionID(),
	}
}

func deferredResponse() node.Response {
	return node.Response{
		Status: engine.StatusDeferred, ExecutionID: "op:1:step",
		Phase: engine.PhaseWaitingHostLock, WaitMS: 20000,
	}
}

func TestNodeEffectLoopPollsLiveRunningWithoutDuplicateDispatch(t *testing.T) {
	script := &scriptedNodeEffect{
		statuses: []node.Response{
			{Status: engine.StatusRunning, ExecutionID: "op:1:step", Phase: engine.PhaseApplying},
			{Status: engine.StatusRunning, ExecutionID: "op:1:step", Phase: engine.PhaseApplying},
			{Status: engine.StatusRunning, ExecutionID: "op:1:step", Phase: engine.PhaseApplying},
			completedResponse(),
		},
	}
	response, err := scriptedLoop(script, nil).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != engine.StatusCompleted {
		t.Fatalf("loop response = %+v", response)
	}
	if script.dispatchCalls != 0 {
		t.Fatalf("a live running task was dispatched again %d time(s)", script.dispatchCalls)
	}
	if script.completed != 1 || script.failed != 0 {
		t.Fatalf("callbacks completed=%d failed=%d, want 1/0", script.completed, script.failed)
	}
}

func TestNodeEffectLoopBacksOffDeferralAndDispatchesOneAttempt(t *testing.T) {
	script := &scriptedNodeEffect{
		statuses: []node.Response{
			deferredResponse(),
			{Status: engine.StatusRunning, ExecutionID: "op:1:step", Phase: engine.PhaseWaitingHostLock},
			completedResponse(),
		},
		dispatches: []node.Response{
			{Status: engine.StatusRunning, ExecutionID: "op:1:step", Phase: engine.PhaseWaitingHostLock},
		},
	}
	response, err := scriptedLoop(script, nil).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != engine.StatusCompleted {
		t.Fatalf("loop response = %+v", response)
	}
	if script.dispatchCalls != 1 {
		t.Fatalf("deferral dispatched %d times, want 1", script.dispatchCalls)
	}
	if script.waits == 0 {
		t.Fatal("deferral did not back off before dispatch")
	}
	if script.failed != 0 {
		t.Fatalf("a benign deferral called the failure callback %d time(s)", script.failed)
	}
	if script.completed != 1 {
		t.Fatalf("completed callback count = %d, want 1", script.completed)
	}
}

func TestNodeEffectLoopDeferredCancellationNeverDispatches(t *testing.T) {
	script := &scriptedNodeEffect{statuses: []node.Response{deferredResponse()}}
	response, err := scriptedLoop(script, func(context.Context) error { return errPlatformCancelled }).run(context.Background())
	if !errors.Is(err, errPlatformCancelled) {
		t.Fatalf("deferred cancellation = %v", err)
	}
	if response.Status != engine.StatusDeferred {
		t.Fatalf("deferred cancellation response = %+v", response)
	}
	if script.dispatchCalls != 0 {
		t.Fatalf("cancellation dispatched a new effect %d time(s)", script.dispatchCalls)
	}
}

func TestNodeEffectLoopCancellationDrainsLiveEffect(t *testing.T) {
	script := &scriptedNodeEffect{
		statuses: []node.Response{
			{Status: engine.StatusRunning, ExecutionID: "op:1:step", Phase: engine.PhaseApplying},
			{Status: engine.StatusRunning, ExecutionID: "op:1:step", Phase: engine.PhaseApplying},
			deferredResponse(),
		},
	}
	response, err := scriptedLoop(script, func(context.Context) error { return errPlatformCancelled }).run(context.Background())
	if !errors.Is(err, errPlatformCancelled) {
		t.Fatalf("drained cancellation = %v", err)
	}
	if response.Status != engine.StatusDeferred {
		t.Fatalf("drained cancellation response = %+v", response)
	}
	if script.dispatchCalls != 0 {
		t.Fatalf("cancellation started a second effect %d time(s)", script.dispatchCalls)
	}
	if script.statusCalls < 3 {
		t.Fatalf("cancellation did not drain the live effect: %d status calls", script.statusCalls)
	}
}

func TestNodeEffectLoopCancellationWithUnknownStatusRetainsOwnership(t *testing.T) {
	script := &scriptedNodeEffect{statusErrors: []error{node.ErrStreamLost}}
	_, err := scriptedLoop(script, func(context.Context) error { return errPlatformCancelled }).run(context.Background())
	if !node.IsStreamLost(err) {
		t.Fatalf("unknown status error = %v, want stream loss", err)
	}
	if errors.Is(err, errPlatformCancelled) {
		t.Fatal("an unknown status was converted into a cancellation boundary")
	}
	if script.dispatchCalls != 0 {
		t.Fatalf("unknown status dispatched %d time(s)", script.dispatchCalls)
	}
}

func TestNodeEffectLoopStaleAndAbsentReconcileBeforeDispatch(t *testing.T) {
	script := &scriptedNodeEffect{
		statuses:   []node.Response{{Status: engine.StatusStale}, {Status: engine.StatusAbsent}},
		dispatches: []node.Response{completedResponse()},
	}
	response, err := scriptedLoop(script, nil).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != engine.StatusCompleted || script.completed != 1 {
		t.Fatalf("stale/absent control response=%+v completed=%d", response, script.completed)
	}
	if script.dispatchCalls != 1 {
		t.Fatalf("stale/absent dispatch count = %d, want 1", script.dispatchCalls)
	}
}

func TestNodeEffectLoopInitialAbsentDispatchesWithoutWait(t *testing.T) {
	script := &scriptedNodeEffect{
		statuses:   []node.Response{{Status: engine.StatusAbsent}},
		dispatches: []node.Response{completedResponse()},
	}
	var order []string
	loop := scriptedLoop(script, nil)
	loop.Status = func(ctx context.Context) (node.Response, error) {
		order = append(order, "status")
		return script.Status(ctx)
	}
	loop.Begin = func(context.Context) error {
		order = append(order, "begin")
		return nil
	}
	loop.Dispatch = func(ctx context.Context) (node.Response, error) {
		order = append(order, "dispatch")
		return script.Dispatch(ctx)
	}
	response, err := loop.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != engine.StatusCompleted || script.completed != 1 {
		t.Fatalf("response=%+v completed=%d", response, script.completed)
	}
	if script.waits != 0 {
		t.Fatalf("initial absent observation waited %d time(s), want zero", script.waits)
	}
	if script.dispatchCalls != 1 {
		t.Fatalf("initial absent dispatch count = %d, want 1", script.dispatchCalls)
	}
	if len(order) != 3 || order[0] != "status" || order[1] != "begin" || order[2] != "dispatch" {
		t.Fatalf("status/begin/dispatch order = %v", order)
	}
}

func TestNodeEffectLoopInitialAbsentCancellationBeforeDispatchDoesNotRun(t *testing.T) {
	script := &scriptedNodeEffect{
		statuses:   []node.Response{{Status: engine.StatusAbsent}},
		dispatches: []node.Response{completedResponse()},
	}
	checks := 0
	began := false
	loop := scriptedLoop(script, func(context.Context) error {
		checks++
		if checks == 1 {
			return nil
		}
		return errPlatformCancelled
	})
	loop.Begin = func(context.Context) error {
		began = true
		return nil
	}
	_, err := loop.run(context.Background())
	if !errors.Is(err, errPlatformCancelled) {
		t.Fatalf("initial absent cancellation = %v", err)
	}
	if began || script.dispatchCalls != 0 {
		t.Fatalf("cancellation after status began=%t dispatch=%d, want false/0", began, script.dispatchCalls)
	}
	if script.statusCalls != 1 {
		t.Fatalf("status calls = %d, want 1", script.statusCalls)
	}
}

func TestNodeEffectLoopRunningThenAbsentStillBacksOff(t *testing.T) {
	script := &scriptedNodeEffect{
		statuses: []node.Response{
			{Status: engine.StatusRunning, ExecutionID: "op:1:step", Phase: engine.PhaseApplying},
			{Status: engine.StatusAbsent},
		},
		dispatches: []node.Response{completedResponse()},
	}
	response, err := scriptedLoop(script, nil).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != engine.StatusCompleted || script.completed != 1 {
		t.Fatalf("response=%+v completed=%d", response, script.completed)
	}
	if script.dispatchCalls != 1 {
		t.Fatalf("dispatch count = %d, want 1", script.dispatchCalls)
	}
	if script.waits < 2 {
		t.Fatalf("running then absent waits = %d, want running poll and absent reconciliation", script.waits)
	}
}

func TestNodeEffectLoopRepeatedAbsentStillBacksOff(t *testing.T) {
	script := &scriptedNodeEffect{
		statuses: []node.Response{
			{Status: engine.StatusAbsent},
			{Status: engine.StatusAbsent},
		},
		dispatches: []node.Response{
			{Status: engine.StatusAbsent},
			completedResponse(),
		},
	}
	response, err := scriptedLoop(script, nil).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != engine.StatusCompleted || script.completed != 1 {
		t.Fatalf("response=%+v completed=%d", response, script.completed)
	}
	if script.dispatchCalls != 2 {
		t.Fatalf("repeated absent dispatch count = %d, want 2", script.dispatchCalls)
	}
	if script.waits < 2 {
		t.Fatalf("repeated absent waits = %d, want reconciliation backoff before the second dispatch", script.waits)
	}
}

func TestNodeEffectLoopInterruptedWaitIsRetryableNotCancellationCleanup(t *testing.T) {
	script := &scriptedNodeEffect{
		statuses: []node.Response{{Status: engine.StatusRunning, ExecutionID: "op:1:step", Phase: engine.PhaseApplying}},
	}
	loop := scriptedLoop(script, nil)
	loop.Wait = func(context.Context, time.Duration) error { return context.Canceled }
	_, err := loop.run(context.Background())
	if err == nil {
		t.Fatal("an interrupted wait was accepted")
	}
	if contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("interrupted wait code = %s, want unavailable: %v", contract.Classify(err).Code, err)
	}
	if errors.Is(err, errPlatformCancelled) || errors.Is(err, context.Canceled) {
		t.Fatalf("an interrupted live wait was treated as a cancellation boundary: %v", err)
	}
	if script.dispatchCalls != 0 {
		t.Fatalf("interrupted wait dispatched %d time(s)", script.dispatchCalls)
	}
}

func TestDeploymentNodeEffectStepScope(t *testing.T) {
	for _, step := range []string{
		deploymentPreflightStep, deploymentNetworkStep, deploymentCommandStep,
		deploymentImageStep, deploymentReadyImageStep, deploymentPullStep,
		containerStepID("web", "1"), healthStepID("web", "1"),
		portStepID("web-1"), cleanupStepID("web-1"),
	} {
		if !deploymentNodeEffectStep(step) {
			t.Fatalf("%s must be observed as a node effect", step)
		}
	}
	for _, step := range []string{deploymentRouteStep, deploymentPublicStep, deploymentRestoreRouteStep} {
		if deploymentNodeEffectStep(step) {
			t.Fatalf("%s is a control-plane step, not a node effect", step)
		}
	}
	if !nodeEffectCancellationExempt(platform.DiagnosticCleanupStep) ||
		!nodeEffectCancellationExempt(cleanupStepID("web-1")) {
		t.Fatal("post-cancellation cleanup steps must remain dispatchable")
	}
	if nodeEffectCancellationExempt(deploymentNetworkStep) {
		t.Fatal("forward work must stop on cancellation")
	}
}

func TestNodeEffectLoopRetriesPriorAttemptFailure(t *testing.T) {
	script := &scriptedNodeEffect{
		statuses: []node.Response{
			failedResponseAt(1, true),
			completedResponseAt(2),
		},
		dispatches: []node.Response{completedResponseAt(2)},
	}
	loop := scriptedLoop(script, nil)
	loop.Current.Attempt = 2
	response, err := loop.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != engine.StatusCompleted || script.completed != 1 {
		t.Fatalf("prior failure retry response=%+v completed=%d", response, script.completed)
	}
	if script.failed != 0 {
		t.Fatalf("prior failure took the failure callback %d time(s)", script.failed)
	}
	if script.dispatchCalls != 1 {
		t.Fatalf("prior failure dispatch count = %d, want 1", script.dispatchCalls)
	}
}

func TestNodeEffectLoopCurrentAttemptFailureDoesNotRedispatch(t *testing.T) {
	script := &scriptedNodeEffect{statuses: []node.Response{failedResponseAt(2, true)}}
	loop := scriptedLoop(script, nil)
	loop.Current.Attempt = 2
	_, err := loop.run(context.Background())
	if err == nil {
		t.Fatal("the current attempt's retryable failure was accepted")
	}
	if script.dispatchCalls != 0 {
		t.Fatalf("the current attempt's failure dispatched another effect %d time(s)", script.dispatchCalls)
	}
	if script.failed != 1 {
		t.Fatalf("the current attempt's failure callback count = %d, want 1", script.failed)
	}
}

func TestNodeEffectLoopPriorFailureIsRetriedOnceThenBudgetReturns(t *testing.T) {
	script := &scriptedNodeEffect{
		statuses: []node.Response{
			failedResponseAt(1, true),
			failedResponseAt(2, true),
		},
		dispatches: []node.Response{{
			Status: engine.StatusRunning, Attempt: 2,
			ExecutionID: engine.Execution{OperationID: "op", Attempt: 2, Step: "step"}.ExecutionID(),
		}},
	}
	loop := scriptedLoop(script, nil)
	loop.Current.Attempt = 2
	_, err := loop.run(context.Background())
	if err == nil {
		t.Fatal("the current attempt's second failure was accepted")
	}
	if script.dispatchCalls != 1 {
		t.Fatalf("prior failure dispatch count = %d, want exactly one retry", script.dispatchCalls)
	}
	if script.failed != 1 {
		t.Fatalf("current failure callback count = %d, want 1", script.failed)
	}
}

func TestNodeEffectLoopPriorFailureCannotBeRetriedTwice(t *testing.T) {
	script := &scriptedNodeEffect{
		statuses: []node.Response{
			failedResponseAt(1, true),
			failedResponseAt(1, true),
		},
		dispatches: []node.Response{{
			Status: engine.StatusRunning, Attempt: 2,
			ExecutionID: engine.Execution{OperationID: "op", Attempt: 2, Step: "step"}.ExecutionID(),
		}},
	}
	loop := scriptedLoop(script, nil)
	loop.Current.Attempt = 2
	_, err := loop.run(context.Background())
	if err == nil {
		t.Fatal("a prior failure was redispatch a second time in the same attempt")
	}
	if script.dispatchCalls != 1 {
		t.Fatalf("prior failure dispatch count = %d, want 1", script.dispatchCalls)
	}
	if script.failed != 0 {
		t.Fatalf("repeated prior failure called the failure callback %d time(s)", script.failed)
	}
}

func TestNodeEffectLoopRetryableFailureIdentityMustBeComplete(t *testing.T) {
	for name, response := range map[string]node.Response{
		"missing attempt":     {Status: engine.StatusFailed, Retryable: true},
		"mismatched identity": {Status: engine.StatusFailed, Retryable: true, Attempt: 1, ExecutionID: "op:2:step"},
		"future attempt":      failedResponseAt(2, true),
	} {
		t.Run(name, func(t *testing.T) {
			script := &scriptedNodeEffect{statuses: []node.Response{response}}
			loop := scriptedLoop(script, nil)
			loop.Current.Attempt = 1
			_, err := loop.run(context.Background())
			if err == nil {
				t.Fatal("an ambiguous retryable failure was accepted")
			}
			if script.dispatchCalls != 0 {
				t.Fatalf("ambiguous failure dispatched %d time(s)", script.dispatchCalls)
			}
			if script.failed != 0 {
				t.Fatalf("ambiguous failure called the failure callback %d time(s)", script.failed)
			}
		})
	}
}

func TestNodeEffectLoopRetryableFailedDispatchMustNameCurrentAttempt(t *testing.T) {
	script := &scriptedNodeEffect{
		statuses:   []node.Response{{Status: engine.StatusAbsent}},
		dispatches: []node.Response{failedResponseAt(1, true)},
	}
	loop := scriptedLoop(script, nil)
	loop.Current.Attempt = 2
	_, err := loop.run(context.Background())
	if err == nil {
		t.Fatal("a retryable failed dispatch from an older attempt was accepted")
	}
	if script.dispatchCalls != 1 {
		t.Fatalf("dispatch count = %d, want 1", script.dispatchCalls)
	}
	if script.failed != 0 {
		t.Fatalf("ambiguous dispatch called the failure callback %d time(s)", script.failed)
	}
}

func TestNodeEffectLoopPermanentFailedStatusIsTerminal(t *testing.T) {
	script := &scriptedNodeEffect{
		statuses: []node.Response{{
			Status: engine.StatusFailed, Retryable: false, ErrorCode: "precondition_failed", Detail: "permanent",
		}},
	}
	_, err := scriptedLoop(script, nil).run(context.Background())
	if err == nil {
		t.Fatal("permanent failed status was accepted")
	}
	if script.failed != 1 || script.dispatchCalls != 0 {
		t.Fatalf("permanent failure callback=%d dispatch=%d, want 1/0", script.failed, script.dispatchCalls)
	}
}

func TestNodeEffectLoopFailedDispatchIsTerminalForThisDispatch(t *testing.T) {
	script := &scriptedNodeEffect{
		statuses:   []node.Response{{Status: engine.StatusAbsent}},
		dispatches: []node.Response{{Status: engine.StatusFailed, Retryable: false, Detail: "refused"}},
	}
	_, err := scriptedLoop(script, nil).run(context.Background())
	if err == nil {
		t.Fatal("a failed dispatch was accepted")
	}
	if script.failed != 1 || script.dispatchCalls != 1 {
		t.Fatalf("failed callback=%d dispatch=%d, want 1/1", script.failed, script.dispatchCalls)
	}
}
