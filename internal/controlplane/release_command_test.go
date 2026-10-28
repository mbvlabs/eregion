package controlplane

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestReleaseCommandAction(t *testing.T) {
	current := contract.OperationID("op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	other := contract.OperationID("op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	tests := []struct {
		name    string
		row     releaseCommandRun
		action  platform.DeploymentAction
		current contract.OperationID
		want    releaseCommandDecision
	}{
		{
			name:   "deploy with no row runs the command",
			action: platform.ActionDeploy,
			want:   releaseCommandRunNow,
		},
		{
			name:   "deploy skips a succeeded command",
			row:    releaseCommandRun{State: "succeeded", OperationID: other},
			action: platform.ActionDeploy,
			want:   releaseCommandSkip,
		},
		{
			name:   "deploy reruns a failed command",
			row:    releaseCommandRun{State: "failed", OperationID: other},
			action: platform.ActionDeploy,
			want:   releaseCommandRunNow,
		},
		{
			name:    "deploy resumes its own running command",
			row:     releaseCommandRun{State: "running", OperationID: current, Live: true},
			action:  platform.ActionDeploy,
			current: current,
			want:    releaseCommandRunNow,
		},
		{
			name:   "deploy recovers an unresolved running row owned by a terminal operation",
			row:    releaseCommandRun{State: "running", OperationID: other, Live: false},
			action: platform.ActionDeploy,
			want:   releaseCommandRecover,
		},
		{
			name:    "deploy refuses another live command",
			row:     releaseCommandRun{State: "running", OperationID: other, Live: true},
			action:  platform.ActionDeploy,
			current: current,
			want:    releaseCommandRefuse,
		},
		{
			name:   "restart skips a succeeded command",
			row:    releaseCommandRun{State: "succeeded"},
			action: platform.ActionRestart,
			want:   releaseCommandSkip,
		},
		{
			name:   "restart refuses without a succeeded command",
			row:    releaseCommandRun{},
			action: platform.ActionRestart,
			want:   releaseCommandRefuse,
		},
		{
			name:   "restart refuses a failed command",
			row:    releaseCommandRun{State: "failed"},
			action: platform.ActionRestart,
			want:   releaseCommandRefuse,
		},
		{
			name:   "rollback refuses a running command",
			row:    releaseCommandRun{State: "running", Live: true},
			action: platform.ActionRollback,
			want:   releaseCommandRefuse,
		},
		{
			name:   "rollback skips a succeeded command",
			row:    releaseCommandRun{State: "succeeded"},
			action: platform.ActionRollback,
			want:   releaseCommandSkip,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := releaseCommandAction(test.row, test.action, test.current); got != test.want {
				t.Fatalf("releaseCommandAction() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestReleaseCommandErrorIsTerminal(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "no error", err: nil, want: false},
		{name: "lost transport outcome", err: fmt.Errorf("node run %s: %w", "platform-command-01-v1", errors.New("connection reset")), want: false},
		{name: "unavailable status", err: contract.NewFault(contract.Unavailable, "node status is unavailable", "Retry."), want: false},
		{name: "acknowledgement precondition", err: contract.NewFault(contract.PreconditionFailed, "node step was not started in the database", "Begin the step first."), want: false},
		{name: "node reported a permanent step failure", err: errors.Join(errNodeStepPermanentFailure,
			contract.NewFault(contract.PreconditionFailed, "node step failed: exit status 1", "Inspect the command.")), want: true},
		{name: "invalid permanent step", err: fmt.Errorf("%w: invalid platform step", engine.ErrPermanent), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := releaseCommandErrorIsTerminal(test.err); got != test.want {
				t.Fatalf("releaseCommandErrorIsTerminal() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestReleaseCommandRecoveryOutcome(t *testing.T) {
	identity := StepRecord{Step: deploymentCommandStep, State: "running", Attempt: 1, ExecutionID: "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:1:" + deploymentCommandStep, InputsHash: "hash-a"}
	tests := []struct {
		name       string
		record     StepRecord
		hasRecord  bool
		status     engine.Status
		inputsHash string
		want       releaseCommandRecovery
	}{
		{name: "no step record is insufficient", hasRecord: false, want: releaseCommandRecoveryDeferred},
		{name: "missing execution identity is insufficient", record: StepRecord{State: "running", InputsHash: "hash-a"}, hasRecord: true, want: releaseCommandRecoveryDeferred},
		{name: "missing inputs hash is insufficient", record: StepRecord{State: "running", Attempt: 1, ExecutionID: "id"}, hasRecord: true, want: releaseCommandRecoveryDeferred},
		{name: "completed step record proves success", record: func() StepRecord { r := identity; r.State = "completed"; return r }(), hasRecord: true, want: releaseCommandRecoverySucceeded},
		{name: "failed step record alone is ambiguous", record: func() StepRecord { r := identity; r.State = "failed"; return r }(), hasRecord: true, want: releaseCommandRecoveryDeferred},
		{name: "failed step record with failed node status proves failure", record: func() StepRecord { r := identity; r.State = "failed"; return r }(), hasRecord: true, status: engine.StatusFailed, inputsHash: "hash-a", want: releaseCommandRecoveryFailed},
		{name: "failed step record with completed node status proves success", record: func() StepRecord { r := identity; r.State = "failed"; return r }(), hasRecord: true, status: engine.StatusCompleted, inputsHash: "hash-a", want: releaseCommandRecoverySucceeded},
		{name: "cancelled step record with absent node status is ambiguous", record: func() StepRecord { r := identity; r.State = "failed"; return r }(), hasRecord: true, status: engine.StatusAbsent, want: releaseCommandRecoveryDeferred},
		{name: "completed node status proves success", record: identity, hasRecord: true, status: engine.StatusCompleted, inputsHash: "hash-a", want: releaseCommandRecoverySucceeded},
		{name: "failed node status proves failure", record: identity, hasRecord: true, status: engine.StatusFailed, inputsHash: "hash-a", want: releaseCommandRecoveryFailed},
		{name: "failed status query without effect evidence defers", record: identity, hasRecord: true, status: engine.StatusFailed, want: releaseCommandRecoveryDeferred},
		{name: "completed status without effect evidence defers", record: identity, hasRecord: true, status: engine.StatusCompleted, want: releaseCommandRecoveryDeferred},
		{name: "running node status is ambiguous", record: identity, hasRecord: true, status: engine.StatusRunning, want: releaseCommandRecoveryDeferred},
		{name: "absent node status is ambiguous", record: identity, hasRecord: true, status: engine.StatusAbsent, want: releaseCommandRecoveryDeferred},
		{name: "stale node status is ambiguous", record: identity, hasRecord: true, status: engine.StatusStale, want: releaseCommandRecoveryDeferred},
		{name: "completed input mismatch defers", record: identity, hasRecord: true, status: engine.StatusCompleted, inputsHash: "hash-b", want: releaseCommandRecoveryDeferred},
		{name: "running input mismatch defers", record: identity, hasRecord: true, status: engine.StatusRunning, inputsHash: "hash-b", want: releaseCommandRecoveryDeferred},
		{name: "failed input mismatch defers", record: identity, hasRecord: true, status: engine.StatusFailed, inputsHash: "hash-b", want: releaseCommandRecoveryDeferred},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := releaseCommandRecoveryOutcome(test.record, test.hasRecord, test.status, test.inputsHash); got != test.want {
				t.Fatalf("releaseCommandRecoveryOutcome() = %v, want %v", got, test.want)
			}
		})
	}
}
