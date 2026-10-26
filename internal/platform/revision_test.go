package platform

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestEnvironmentRevisionNormalizers(t *testing.T) {
	registry := NewRegistry()
	normalize := func(kind, target string, input map[string]any) (Normalized, error) {
		raw, _ := json.Marshal(input)
		return registry.Normalize(kind, target, raw)
	}
	app, err := normalize(KindAppCreate, "platform", map[string]any{
		"name": "api", "deploy": true,
		"environment": map[string]any{
			"name": "production", "node": "app-1",
			"source":  map[string]any{"kind": "git", "url": "https://github.com/acme/api", "ref": "main"},
			"runtime": map[string]any{"health_path": "/healthz"},
			"processes": []map[string]any{{
				"name": "web", "kind": "web", "replicas": 2, "container_port": 3000,
			}},
			"config":       map[string]string{"APP_ENV": "production"},
			"secret_names": []string{"DATABASE_URL"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	created := app.Input.(*AppCreateInput)
	if created.Environment == nil || created.Environment.Processes[0].ContainerPort != 3000 {
		t.Fatalf("first environment was not normalized: %+v", created.Environment)
	}
	if created.Environment.Source.Builder != DefaultBuilder || created.Environment.Runtime.HealthTimeoutSeconds != DefaultHealthTimeout {
		t.Fatalf("revision defaults were not applied: %+v", created.Environment)
	}
	if _, err := normalize(KindAppCreate, "platform", map[string]any{"name": "api", "deploy": true}); err == nil {
		t.Fatal("deploy without an environment must be refused")
	}
	if _, err := normalize(KindAppCreate, "platform", map[string]any{
		"name": "api", "environment": map[string]any{"name": "production", "node": "app-1", "unknown": true},
	}); err == nil {
		t.Fatal("unknown nested environment field must be refused")
	}

	if _, err := normalize(KindEnvironmentUpdate, "env/api/production", map[string]any{
		"node":   "app-2",
		"source": map[string]any{"kind": "image", "image": "ghcr.io/acme/api:1"},
		"processes": []map[string]any{{
			"name": "web", "kind": "web", "container_port": 8080, "replicas": 1,
		}},
		"config":       map[string]string{"APP_ENV": "staging"},
		"secret_names": []string{"DATABASE_URL"},
	}); err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string]map[string]any{
		"unknown field":   {"health": map[string]any{"path": "/"}},
		"bad source":      {"source": map[string]any{"kind": "git", "url": "http://github.com/acme/api"}},
		"bad process":     {"processes": []map[string]any{{"name": "web", "kind": "web"}}},
		"bad secret name": {"secret_names": []string{"database_url"}},
	} {
		if _, err := normalize(KindEnvironmentUpdate, "env/api/production", input); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}

	if _, err := normalize(KindProcessScale, "env/api/production", map[string]any{"process": "web", "replicas": 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := normalize(KindProcessScale, "env/api/production", map[string]any{"process": "web", "replicas": 0}); err == nil {
		t.Fatal("zero replicas must be refused")
	}
	if _, err := normalize(KindConfigSet, "env/api/production", map[string]any{"config": map[string]string{"LOG_LEVEL": "debug"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := normalize(KindConfigUnset, "env/api/production", map[string]any{"keys": []string{"LOG_LEVEL"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := normalize(KindConfigUnset, "env/api/production", map[string]any{"keys": []string{"bad key"}}); err == nil {
		t.Fatal("invalid configuration key must be refused")
	}
	if _, err := normalize(KindEnvironmentSecretAttach, "env/api/production", map[string]any{"name": "DATABASE_URL"}); err != nil {
		t.Fatal(err)
	}
	if _, err := normalize(KindEnvironmentSecretAttach, "env/api/production", map[string]any{"name": "database_url"}); err == nil {
		t.Fatal("lowercase secret name must be refused")
	}
}

func TestEnvironmentRevisionPatchMerge(t *testing.T) {
	base := RevisionState{
		Source:      SourceBinding{Kind: "git", URL: "https://github.com/acme/api", Ref: "main"},
		Runtime:     RuntimeSpec{Node: "app-1", HealthType: "http", HealthPath: "/healthz", HealthTimeoutSeconds: 30},
		Processes:   []ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}, {Name: "worker", Kind: "worker", Command: "bin/worker", Replicas: 1}},
		Config:      map[string]string{"APP_ENV": "production"},
		SecretNames: []string{"DATABASE_URL"},
	}
	node := "app-2"
	config := map[string]string{"APP_ENV": "staging", "LOG_LEVEL": "debug"}
	merged := ApplyEnvironmentPatch(base, EnvironmentUpdateInput{Node: &node, Config: &config})
	if merged.Runtime.Node != "app-2" || merged.Config["LOG_LEVEL"] != "debug" {
		t.Fatalf("patch was not applied: %+v", merged)
	}
	if base.Runtime.Node != "app-1" || len(base.Config) != 1 {
		t.Fatal("the previous revision was mutated")
	}
	scaled, err := ScaleProcess(merged, "web", 4)
	if err != nil {
		t.Fatal(err)
	}
	if scaled.Processes[0].Replicas != 4 || merged.Processes[0].Replicas != 1 {
		t.Fatalf("scale aliased the old revision: %+v", scaled.Processes)
	}
	withConfig := SetConfig(scaled, map[string]string{"NEW_KEY": "value"})
	without := UnsetConfig(withConfig, []string{"LOG_LEVEL"})
	if without.Config["NEW_KEY"] != "value" {
		t.Fatal("config set was lost")
	}
	if _, exists := without.Config["LOG_LEVEL"]; exists {
		t.Fatal("config unset did not remove the key")
	}
	attached := AttachSecret(without, "REDIS_URL")
	if !reflect.DeepEqual(attached.SecretNames, []string{"DATABASE_URL", "REDIS_URL"}) {
		t.Fatalf("secret attach order=%v", attached.SecretNames)
	}
	if len(without.SecretNames) != 1 {
		t.Fatal("secret attach mutated the previous revision")
	}
}

func TestEvaluateDeployability(t *testing.T) {
	revision := &EnvironmentRevision{RevisionState: RevisionState{
		Source:    SourceBinding{Kind: "git", URL: "https://github.com/acme/api"},
		Runtime:   RuntimeSpec{Node: "app-1", HealthPath: "/"},
		Processes: []ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}},
	}}
	ready := EvaluateDeployability(revision, DeployabilityState{ArtifactReady: true, PrimaryDomain: "api.example.com"})
	if !ready.Deployable || len(ready.Missing) != 0 || ready.DNS != "unknown" {
		t.Fatalf("ready report=%+v", ready)
	}
	empty := &EnvironmentRevision{}
	blocked := EvaluateDeployability(empty, DeployabilityState{DNS: "missing"})
	want := []string{"source", "artifact", "runtime", "web_process", "domain", "dns"}
	if !reflect.DeepEqual(blocked.Missing, want) || blocked.Deployable {
		t.Fatalf("blocked report=%+v", blocked)
	}
	if EvaluateDeployability(nil, DeployabilityState{ArtifactReady: true, PrimaryDomain: "api.example.com"}).Missing[0] != "source" {
		t.Fatal("a nil revision must report a missing source")
	}
}
