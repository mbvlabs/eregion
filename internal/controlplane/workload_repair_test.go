package controlplane

import (
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestMissingServingInstances(t *testing.T) {
	deployment := platform.DeploymentID("dep_00000000000000000000000000000001")
	workload := servingWorkload{
		App: "demo", Environment: "production", Deployment: deployment,
		Instances: []platform.DeploymentInstance{
			{Name: "web-1"}, {Name: "web-2"}, {Name: "worker-1"},
		},
	}
	observed := []platform.ContainerObservation{
		{Name: "web-1", App: "demo", Environment: "production", Deployment: string(deployment), State: "running"},
		{Name: "web-2", App: "demo", Environment: "production", Deployment: string(deployment), State: "exited"},
		{Name: "web-3", App: "demo", Environment: "production", Deployment: string(deployment), State: "running"},
		{Name: "worker-1", App: "demo", Environment: "production", Deployment: "dep_00000000000000000000000000000002", State: "running"},
	}
	missing := missingServingInstances(workload, observed)
	if len(missing) != 2 || missing[0] != "web-2" || missing[1] != "worker-1" {
		t.Fatalf("missing = %v, want [web-2 worker-1]", missing)
	}
}

func TestServingContainerRunning(t *testing.T) {
	for _, state := range []string{"running", "restarting", "Running"} {
		if !servingContainerRunning(state) {
			t.Errorf("state %q must count as running", state)
		}
	}
	for _, state := range []string{"exited", "created", "dead", "paused", "removing", ""} {
		if servingContainerRunning(state) {
			t.Errorf("state %q must count as missing", state)
		}
	}
}

func TestRepairIdempotencyKeyStableAndOrderIndependent(t *testing.T) {
	deployment := platform.DeploymentID("dep_00000000000000000000000000000001")
	first := repairIdempotencyKey(deployment, []string{"web-2", "worker-1"})
	second := repairIdempotencyKey(deployment, []string{"worker-1", "web-2"})
	if first != second {
		t.Fatalf("key depends on order: %s != %s", first, second)
	}
	if err := contract.ValidateIdempotencyKey(first); err != nil {
		t.Fatalf("repair key is invalid: %v", err)
	}
	if third := repairIdempotencyKey(deployment, []string{"web-2"}); third == first {
		t.Fatal("a different missing set reused the repair key")
	}
	other := platform.DeploymentID("dep_00000000000000000000000000000002")
	if fourth := repairIdempotencyKey(other, []string{"web-2", "worker-1"}); fourth == first {
		t.Fatal("a different deployment reused the repair key")
	}
}

func TestEnvironmentOperationBlocksRepair(t *testing.T) {
	tests := []struct {
		kind  string
		state contract.State
		want  bool
	}{
		{kind: platform.KindDeploy, state: contract.Running, want: true},
		{kind: platform.KindDeploy, state: contract.Queued, want: true},
		{kind: platform.KindRestart, state: contract.Queued, want: true},
		{kind: platform.KindRollback, state: contract.Cancelling, want: true},
		{kind: platform.KindEnvironmentDelete, state: contract.Running, want: true},
		{kind: platform.KindAppDelete, state: contract.Queued, want: true},
		{kind: platform.KindConfigSet, state: contract.Running, want: false},
		{kind: platform.KindConfigSet, state: contract.Cancelling, want: false},
		{kind: platform.KindDeploy, state: contract.Succeeded, want: false},
		{kind: platform.KindDeploy, state: contract.Failed, want: false},
	}
	for _, test := range tests {
		if got := environmentOperationBlocksRepair(test.kind, test.state); got != test.want {
			t.Errorf("environmentOperationBlocksRepair(%s,%s) = %v, want %v", test.kind, test.state, got, test.want)
		}
	}
}

func TestRepairSubmissionAllowed(t *testing.T) {
	observed := platform.DeploymentID("dep_00000000000000000000000000000001")
	other := platform.DeploymentID("dep_00000000000000000000000000000002")
	tests := []struct {
		name     string
		expected platform.DeploymentID
		serving  platform.DeploymentID
		active   []activeOperation
		want     bool
	}{
		{name: "current serving and no operation", expected: observed, serving: observed, want: true},
		{name: "serving deployment changed", expected: observed, serving: other, want: false},
		{name: "deploy accepted", expected: observed, serving: observed, active: []activeOperation{{Kind: platform.KindDeploy, State: contract.Queued}}, want: false},
		{name: "restart running", expected: observed, serving: observed, active: []activeOperation{{Kind: platform.KindRestart, State: contract.Running}}, want: false},
		{name: "environment delete cancelling", expected: observed, serving: observed, active: []activeOperation{{Kind: platform.KindEnvironmentDelete, State: contract.Cancelling}}, want: false},
		{name: "non-blocking config change", expected: observed, serving: observed, active: []activeOperation{{Kind: platform.KindConfigSet, State: contract.Running}}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := repairSubmissionAllowed(test.expected, test.serving, test.active); got != test.want {
				t.Fatalf("repairSubmissionAllowed() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRepairRestartStillApplies(t *testing.T) {
	current := platform.DeploymentID("dep_00000000000000000000000000000002")
	env := platform.Environment{ServingDeploymentID: current}
	if !repairRestartStillApplies(platform.RestartInput{ExpectedServingDeploymentID: current}, env) {
		t.Fatal("a repair queued for the current serving deployment was refused")
	}
	if repairRestartStillApplies(platform.RestartInput{ExpectedServingDeploymentID: "dep_00000000000000000000000000000001"}, env) {
		t.Fatal("a stale repair was accepted")
	}
	if !repairRestartStillApplies(platform.RestartInput{}, env) {
		t.Fatal("an operator restart with no expected deployment was refused")
	}
}
