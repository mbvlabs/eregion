package platform

import (
	"encoding/json"
	"testing"
)

func TestEnvironmentBuildNodeIsValidatedAndDefaultsToRuntimeNode(t *testing.T) {
	registry := NewRegistry()
	normalize := func(input map[string]any) (Normalized, error) {
		raw, _ := json.Marshal(input)
		return registry.Normalize(KindEnvironmentCreate, "app/api", raw)
	}
	good, err := normalize(map[string]any{"name": "production", "node": "app-1", "build_node": "build-1"})
	if err != nil {
		t.Fatal(err)
	}
	input := good.Input.(*EnvironmentCreateInput)
	if input.BuildNode != "build-1" || input.Node != "app-1" {
		t.Fatalf("build placement was not preserved: %+v", input)
	}
	env := Environment{Node: "app-1", BuildNode: "build-1"}
	if env.BuildNodeName() != "build-1" {
		t.Fatalf("explicit build node was ignored: %s", env.BuildNodeName())
	}
	if (Environment{Node: "app-1"}).BuildNodeName() != "app-1" {
		t.Fatal("an omitted build node must default to the runtime node")
	}
	for name, input := range map[string]map[string]any{
		"invalid build node": {"name": "production", "node": "app-1", "build_node": "Build_1"},
		"empty node":         {"name": "production", "build_node": "build-1"},
	} {
		if _, err := normalize(input); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

func TestEnvironmentPreviewMentionsDistinctBuildNode(t *testing.T) {
	input := &EnvironmentCreateInput{App: "api", Name: "production", Node: "app-1", BuildNode: "build-1"}
	if input.Target().String() != "app/api" {
		t.Fatalf("target=%s", input.Target())
	}
	// Preview is the input itself; the worker adds the registry-transfer warning.
	if got := input.Preview().(*EnvironmentCreateInput); got.BuildNode != "build-1" {
		t.Fatalf("preview lost build node: %+v", got)
	}
}

func TestEnvironmentCreateDeployNeedsASource(t *testing.T) {
	registry := NewRegistry()
	normalize := func(input map[string]any) (Normalized, error) {
		raw, _ := json.Marshal(input)
		return registry.Normalize(KindEnvironmentCreate, "app/api", raw)
	}
	if _, err := normalize(map[string]any{"name": "production", "node": "app-1", "deploy": true}); err == nil {
		t.Fatal("deploy without a source must be refused")
	}
	good, err := normalize(map[string]any{
		"name": "production", "node": "app-1", "deploy": true,
		"source": map[string]any{"kind": "image", "image": "ghcr.io/acme/api:1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := good.Input.(*EnvironmentCreateInput)
	if !input.Deploy {
		t.Fatal("deploy intent was lost")
	}
}

func TestDeployNormalizesSourceAndReleaseForms(t *testing.T) {
	registry := NewRegistry()
	normalize := func(kind, target string, input map[string]any) (Normalized, error) {
		raw, _ := json.Marshal(input)
		return registry.Normalize(kind, target, raw)
	}
	source, err := normalize(KindDeploy, "env/api/production", map[string]any{"ref": "release/1"})
	if err != nil {
		t.Fatal(err)
	}
	deploy := source.Input.(*DeployInput)
	if deploy.ReleaseID != "" || deploy.Ref != "release/1" {
		t.Fatalf("source deploy=%+v", deploy)
	}
	release, err := normalize(KindDeploy, "env/api/production", map[string]any{"release_id": "rel_00000000000000000000000000000001"})
	if err != nil || release.Input.(*DeployInput).ReleaseID == "" {
		t.Fatalf("release deploy err=%v", err)
	}
	if _, err := normalize(KindDeploy, "env/api/production", map[string]any{"release_id": "rel_00000000000000000000000000000001", "ref": "main"}); err == nil {
		t.Fatal("release and ref together must be refused")
	}
	buildTarget := "build/bld_00000000000000000000000000000001"
	stop, err := normalize(KindBuildStop, buildTarget, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if stop.Input.(*BuildStopInput).BuildID != "bld_00000000000000000000000000000001" {
		t.Fatalf("build stop input=%+v", stop.Input)
	}
	if _, err := normalize(KindBuildRetry, "env/api/production", map[string]any{}); err == nil {
		t.Fatal("build retry requires a build target")
	}
}

func TestEnvironmentSpecCarriesItsFirstDomain(t *testing.T) {
	registry := NewRegistry()
	raw, _ := json.Marshal(map[string]any{
		"name": "production", "node": "app-1",
		"domain": "API.Example.com.", "domain_primary": true,
		"domain_addresses": []string{"203.0.113.10", "203.0.113.10"},
	})
	normalized, err := registry.Normalize(KindEnvironmentCreate, "app/api", raw)
	if err != nil {
		t.Fatal(err)
	}
	spec := normalized.Input.(*EnvironmentCreateInput).EnvironmentSpec
	if spec.Domain != "api.example.com" || !spec.DomainPrimary || len(spec.DomainAddresses) != 1 || spec.DomainAddresses[0] != "203.0.113.10" {
		t.Fatalf("domain binding was not normalized: %+v", spec)
	}
	if _, err := registry.Normalize(KindEnvironmentCreate, "app/api", mustJSONInput(map[string]any{
		"name": "staging", "node": "app-1", "domain_primary": true,
	})); err == nil {
		t.Fatal("a primary flag without a domain must be refused")
	}
}

func mustJSONInput(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func TestContextPathInputGateRejectsPathsOutsideTheRepository(t *testing.T) {
	registry := NewRegistry()
	normalize := func(contextPath string) error {
		raw, _ := json.Marshal(map[string]any{
			"name": "production", "node": "app-1",
			"source": map[string]any{
				"kind": "git", "url": "https://github.com/acme/app.git", "context_path": contextPath,
			},
		})
		_, err := registry.Normalize(KindEnvironmentCreate, "app/api", raw)
		return err
	}
	if err := normalize("apps/web"); err != nil {
		t.Fatalf("a relative context path was refused: %v", err)
	}
	for _, bad := range []string{"../x", "/srv/x", "apps/../../x"} {
		if err := normalize(bad); err == nil {
			t.Fatalf("context path %q was accepted", bad)
		}
	}
}
