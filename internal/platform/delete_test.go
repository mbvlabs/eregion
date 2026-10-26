package platform

import (
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestDeleteConfirmationValidatesCurrentTargetName(t *testing.T) {
	app, err := NewAppTarget("api")
	if err != nil {
		t.Fatal(err)
	}
	if got := DeleteConfirmationToken(app); got != "api" {
		t.Fatalf("app token = %q", got)
	}
	if err := ValidateDeleteConfirmation(app, "api"); err != nil {
		t.Fatalf("matching app token: %v", err)
	}
	if err := ValidateDeleteConfirmation(app, ""); contract.Classify(err).Code != contract.ConfirmationRequired {
		t.Fatalf("empty token code = %v", contract.Classify(err).Code)
	}
	if err := ValidateDeleteConfirmation(app, "other"); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("wrong app token code = %v", contract.Classify(err).Code)
	}

	env, err := NewEnvironmentTarget("api", "production")
	if err != nil {
		t.Fatal(err)
	}
	if got := DeleteConfirmationToken(env); got != "api/production" {
		t.Fatalf("environment token = %q", got)
	}
	if err := ValidateDeleteConfirmation(env, "api/production"); err != nil {
		t.Fatalf("matching environment token: %v", err)
	}
	if err := ValidateDeleteConfirmation(env, "api"); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("app token accepted for an environment: %v", err)
	}
}

func TestDeletionPlanCleansWorkloadsBeforePlatformState(t *testing.T) {
	steps := DeletionSteps("api", "")
	order := []string{
		"stop and remove the workload containers selected by ownership labels",
		"remove the Caddy routes and reload Caddy",
		"delete the vault secret namespaces",
		"delete deployments",
		"delete releases",
		"delete builds",
		"delete environment revisions",
		"delete every environment row",
		"delete the application row",
	}
	index := map[string]int{}
	for position, step := range steps {
		index[step] = position
	}
	last := -1
	for _, step := range order {
		position, ok := index[step]
		if !ok {
			t.Fatalf("missing step %q in %v", step, steps)
		}
		if position <= last {
			t.Fatalf("step %q is out of order in %v", step, steps)
		}
		last = position
	}
	if last != len(steps)-1 {
		t.Fatalf("application deletion is not the last step: %v", steps)
	}

	envSteps := DeletionSteps("api", "production")
	if envSteps[len(envSteps)-1] != "delete the environment row (and the application when it was the last one)" {
		t.Fatalf("environment deletion has the wrong terminal step: %v", envSteps)
	}
}

func TestFormatLogLinePrefixesProcessAndReplica(t *testing.T) {
	if got := FormatLogLine(LogLine{Process: "web", Replica: "2", Message: "listening\n"}); got != "web/2 | listening" {
		t.Fatalf("prefixed line = %q", got)
	}
	if got := FormatLogLine(LogLine{Container: "api-web-1", Message: "fallback"}); got != "api-web-1 | fallback" {
		t.Fatalf("container fallback = %q", got)
	}
}

func TestParseDockerPSUsesOwnershipLabels(t *testing.T) {
	raw := []byte(`{"ID":"abc123","Names":"api-web-1","State":"running","Status":"Up 2 minutes","Ports":"127.0.0.1:49152->8080/tcp","Labels":"io.eregion.app=api,io.eregion.environment=production,io.eregion.process=web,io.eregion.process.kind=web,io.eregion.process.replica=1,io.eregion.release=rel_11111111111111111111111111111111,io.eregion.deployment=dep_22222222222222222222222222222222"}
`)
	containers, err := ParseDockerPS(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 {
		t.Fatalf("containers = %v", containers)
	}
	container := containers[0]
	if container.App != "api" || container.Environment != "production" || container.Process != "web" || container.Replica != "1" {
		t.Fatalf("labels not parsed: %+v", container)
	}
	if container.HostPort != 49152 || container.State != "running" || container.Deployment == "" {
		t.Fatalf("state/port not parsed: %+v", container)
	}
}
