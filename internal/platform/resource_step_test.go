package platform

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func testResourcePlan(t *testing.T) ResourcePlan {
	t.Helper()
	id := ResourceID("res_00000000000000000000000000000001")
	envID := EnvironmentID("env_00000000000000000000000000000002")
	plan, err := PlanResource(envID, "main", id, "api", "production", "db", testResourceSpec())
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func testResourceExecution(step string) engine.Execution {
	return engine.Execution{
		OperationID: contract.OperationID("op_00000000000000000000000000000001"),
		Attempt:     1,
		Step:        step,
		RequestID:   "req_test",
	}
}

func TestResourceRunArgsKeepSecretValuesOutOfArgvAndDisablePublishing(t *testing.T) {
	plan := testResourcePlan(t)
	stepID := ResourceRunStepID(plan)
	payload := ResourceRunPayload{
		Step: stepID, Name: plan.Container, ImageID: "sha256:" + strings.Repeat("a", 64),
		Network: plan.Network, Alias: plan.Alias, Env: plan.Spec.Env,
		SecretKeys: []string{"POSTGRES_PASSWORD"}, Mounts: plan.Volumes,
		ConfigHash: "config", App: "api", Environment: "production",
		Resource: "db", ResourceID: string(plan.ID), Restart: "unless-stopped",
	}
	raw, _ := json.Marshal(payload)
	step, err := buildResourceRunStep(testResourceExecution(stepID), raw)
	if err != nil {
		t.Fatal(err)
	}
	run := step.(*ResourceRunStep)
	args, env, err := run.runArgs(engine.Runtime{SecretValues: map[string]string{"POSTGRES_PASSWORD": "hunter2"}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")
	if strings.Contains(joined, "hunter2") {
		t.Fatalf("secret value entered argv: %v", args)
	}
	if !strings.Contains(joined, "--network\x00"+plan.Network) || !strings.Contains(joined, "--network-alias\x00db") {
		t.Fatalf("private network arguments are missing: %v", args)
	}
	publishes := false
	for _, arg := range args {
		if arg == "-p" || arg == "--publish" || strings.Contains(arg, "0.0.0.0") {
			publishes = true
		}
	}
	if publishes {
		t.Fatalf("a resource container tried to publish a host port: %v", args)
	}
	if !strings.Contains(joined, "--mount\x00type=volume,source="+plan.Volumes[0].Name+",destination="+plan.Volumes[0].Destination) {
		t.Fatalf("named-volume mount is missing: %v", args)
	}
	if !strings.Contains(joined, "-e\x00POSTGRES_PASSWORD") || !strings.Contains(joined, "-e\x00POSTGRES_DB") {
		t.Fatalf("environment keys are missing: %v", args)
	}
	if !strings.Contains(strings.Join(env, "\x00"), "POSTGRES_PASSWORD=hunter2") {
		t.Fatal("secret value was not passed in the child environment")
	}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(inputs), "hunter2") {
		t.Fatalf("step inputs leaked a secret value: %s", inputs)
	}
}

func TestResourceRunBuilderRejectsMutableImageAndForeignNetwork(t *testing.T) {
	plan := testResourcePlan(t)
	stepID := ResourceRunStepID(plan)
	base := ResourceRunPayload{
		Step: stepID, Name: plan.Container, ImageID: "sha256:" + strings.Repeat("a", 64),
		Network: plan.Network, Alias: plan.Alias, ConfigHash: "config", App: "api",
		Environment: "production", Resource: "db", ResourceID: string(plan.ID), Restart: "unless-stopped",
	}
	for name, mutate := range map[string]func(*ResourceRunPayload){
		"mutable image": func(payload *ResourceRunPayload) { payload.ImageID = "postgres:17" },
		"host network":  func(payload *ResourceRunPayload) { payload.Network = "host" },
		"host port":     func(payload *ResourceRunPayload) { payload.Restart = "always" },
	} {
		payload := base
		mutate(&payload)
		raw, _ := json.Marshal(payload)
		if _, err := buildResourceRunStep(testResourceExecution(stepID), raw); err == nil {
			t.Errorf("%s: was accepted", name)
		}
	}
}

func TestValidateArgvAcceptsOnlyNamedVolumeAndResolverMounts(t *testing.T) {
	resolverSource := withTestResolver(t)
	resolverMount := "type=bind,source=" + resolverSource + ",destination=/run/eregion/eregion,readonly=true"
	volume := "er-res-" + strings.Repeat("0", 32) + "-data"
	for _, argv := range [][]string{
		{"docker", "run", "-d", "--name", "res", "--mount", "type=volume,source=" + volume + ",destination=/data", "sha256:" + strings.Repeat("a", 64)},
		{"docker", "run", "-d", "--name", "res", "--mount", resolverMount, "sha256:" + strings.Repeat("a", 64)},
		{"docker", "volume", "create", "--label", "io.eregion.app=api", volume},
		{"docker", "exec", "res", "pg_isready", "-U", "app"},
	} {
		if err := validateArgv(argv); err != nil {
			t.Fatalf("safe argv was refused: %v: %v", argv, err)
		}
	}
	for _, argv := range [][]string{
		{"docker", "run", "--mount", "type=bind,source=/etc,destination=/data", "image"},
		{"docker", "run", "--mount", "type=bind,source=/etc,destination=/run/eregion/eregion,readonly=true", "image"},
		{"docker", "run", "--mount", "type=volume,source=" + volume + ",destination=/etc", "image"},
		{"docker", "run", "-v", volume + ":/data", "image"},
		{"docker", "exec", "res", "--env", "PASSWORD=hunter2", "true"},
		{"docker", "run", "--env-file", "/etc/passwd", "image"},
	} {
		if err := validateArgv(argv); err == nil {
			t.Fatalf("unsafe argv was accepted: %v", argv)
		}
	}
}

func TestResourceVolumeAndDestroyBuildersValidateIdentity(t *testing.T) {
	plan := testResourcePlan(t)
	volumeStep := ResourceVolumeStepID(plan, 0)
	raw, _ := json.Marshal(ResourceVolumePayload{
		Step: volumeStep, Volume: plan.Volumes[0].Name, App: "api", Environment: "production",
		Resource: "db", ResourceID: string(plan.ID),
	})
	if _, err := buildResourceVolumeStep(testResourceExecution(volumeStep), raw); err != nil {
		t.Fatal(err)
	}
	badVolume, _ := json.Marshal(ResourceVolumePayload{
		Step: volumeStep, Volume: "foreign", App: "api", Environment: "production",
		Resource: "db", ResourceID: string(plan.ID),
	})
	if _, err := buildResourceVolumeStep(testResourceExecution(volumeStep), badVolume); err == nil {
		t.Fatal("a foreign volume name was accepted")
	}
	destroy := ResourceDestroyStepID(plan.ID)
	raw, _ = json.Marshal(ResourceDestroyPayload{
		Step: destroy, Container: plan.Container, App: "api", Environment: "production",
		Resource: "db", ResourceID: string(plan.ID),
	})
	if _, err := buildResourceDestroyStep(testResourceExecution(destroy), raw); err != nil {
		t.Fatal(err)
	}
	remove := ResourceVolumeRemoveStepID(plan, 0)
	raw, _ = json.Marshal(ResourceVolumeRemovePayload{
		Step: remove, Volume: plan.Volumes[0].Name, App: "api", Environment: "production",
		Resource: "db", ResourceID: string(plan.ID),
	})
	if _, err := buildResourceVolumeRemoveStep(testResourceExecution(remove), raw); err != nil {
		t.Fatal(err)
	}
	badRemove, _ := json.Marshal(ResourceVolumeRemovePayload{
		Step: remove, Volume: "foreign", App: "api", Environment: "production",
		Resource: "db", ResourceID: string(plan.ID),
	})
	if _, err := buildResourceVolumeRemoveStep(testResourceExecution(remove), badRemove); err == nil {
		t.Fatal("a foreign volume name was accepted for data deletion")
	}
}

func TestResourceHealthBuilderRejectsEmptyAndOptionShapedCommands(t *testing.T) {
	for name, command := range map[string][]string{
		"empty":        nil,
		"option shape": {"--help"},
		"control":      {"pg_isready\nrm -rf"},
	} {
		raw, _ := json.Marshal(ResourceHealthPayload{
			Step: "platform-resource-health-000000000000-v1", Container: "er-res-" + strings.Repeat("0", 32),
			Command: command,
		})
		if _, err := buildResourceHealthStep(testResourceExecution("platform-resource-health-000000000000-v1"), raw); err == nil {
			t.Errorf("%s: was accepted", name)
		}
	}
}

func TestResourceOwnershipRequiresAllLabels(t *testing.T) {
	plan := testResourcePlan(t)
	labels := ResourceOwnershipLabels(plan.App, plan.Environment, plan.Name, plan.ID)
	if !ResourceContainerOwned(plan, labels) || !ResourceVolumeOwned(plan, labels) {
		t.Fatal("exact ownership labels were refused")
	}
	delete(labels, "io.eregion.resource-id")
	if ResourceContainerOwned(plan, labels) || ResourceVolumeOwned(plan, labels) {
		t.Fatal("an incomplete label set established ownership")
	}
}

func TestResourceContainerCurrentAvoidsNeedlessRestart(t *testing.T) {
	plan := testResourcePlan(t)
	payload := ResourceRunPayload{
		ImageID: "sha256:" + strings.Repeat("a", 64), ConfigHash: "config",
		Network: plan.Network, Alias: plan.Alias,
	}
	labels := ResourceOwnershipLabels(plan.App, plan.Environment, plan.Name, plan.ID)
	labels["io.eregion.config"] = "config"
	current := resourceContainerObservation{
		Running: true, Image: payload.ImageID, Labels: labels,
	}
	if !resourceContainerCurrent(current, payload, ResourceOwnershipLabels(plan.App, plan.Environment, plan.Name, plan.ID)) {
		t.Fatal("an unchanged running container was not recognized as converged")
	}
	changed := payload
	changed.ConfigHash = "other"
	if resourceContainerCurrent(current, changed, ResourceOwnershipLabels(plan.App, plan.Environment, plan.Name, plan.ID)) {
		t.Fatal("a changed configuration was treated as converged")
	}
	stopped := current
	stopped.Running = false
	if resourceContainerCurrent(stopped, payload, ResourceOwnershipLabels(plan.App, plan.Environment, plan.Name, plan.ID)) {
		t.Fatal("a stopped container was treated as converged")
	}
}

func TestResourceDetailIsRedactedAndSanitized(t *testing.T) {
	rt := engine.Runtime{Secrets: &secrets.Registry{}}
	rt.Secrets.Add("hunter2")
	detail := redactedResourceDetail(rt, "password=hunter2 \x1b[31mred\x1b[0m")
	if strings.Contains(detail, "hunter2") || strings.ContainsRune(detail, '\x1b') {
		t.Fatalf("detail was not sanitized: %q", detail)
	}
}
