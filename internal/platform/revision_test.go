package platform

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/retention"
)

func TestEnvironmentUpdateWebhookIntentIsCanonicalAndNonsecret(t *testing.T) {
	normalize := func(input map[string]any) Normalized {
		t.Helper()
		raw, _ := json.Marshal(input)
		normalized, err := Default().Normalize(KindEnvironmentUpdate, "env/api/production", raw)
		if err != nil {
			t.Fatal(err)
		}
		return normalized
	}
	enabled := normalize(map[string]any{"auto_deploy": true})
	var update EnvironmentUpdateInput
	if err := json.Unmarshal(enabled.Canonical, &update); err != nil {
		t.Fatal(err)
	}
	if update.WebhookSecretName == nil || *update.WebhookSecretName != DefaultWebhookSecretName {
		t.Fatalf("default webhook name = %+v", update.WebhookSecretName)
	}
	if update.RotateWebhookSecret {
		t.Fatal("enable unexpectedly requested rotation")
	}
	rotated := normalize(map[string]any{"rotate_webhook_secret": true})
	if !strings.Contains(string(rotated.Canonical), `"rotate_webhook_secret":true`) {
		t.Fatalf("rotation intent missing from canonical input: %s", rotated.Canonical)
	}
	raw, _ := json.Marshal(map[string]any{"auto_deploy": false, "rotate_webhook_secret": true})
	if _, err := Default().Normalize(KindEnvironmentUpdate, "env/api/production", raw); err == nil {
		t.Fatal("rotation with auto_deploy=false was accepted")
	}
}

func TestValidatePushToDeployRequiresGitHubBranchAndSecretName(t *testing.T) {
	valid := RevisionState{
		Source:            SourceBinding{URL: "https://github.com/acme/api", Ref: "main"},
		AutoDeploy:        true,
		WebhookSecretName: DefaultWebhookSecretName,
	}
	if err := ValidatePushToDeploy(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RevisionState){
		"non-git source": func(state *RevisionState) { state.Source = SourceBinding{} },
		"non-github source": func(state *RevisionState) {
			state.Source.URL = "https://gitlab.com/acme/api"
		},
		"missing branch": func(state *RevisionState) { state.Source.Ref = "" },
		"missing secret": func(state *RevisionState) { state.WebhookSecretName = "" },
	} {
		t.Run(name, func(t *testing.T) {
			state := valid
			mutate(&state)
			err := ValidatePushToDeploy(state)
			if err == nil || contract.Classify(err).Code != contract.ValidationFailed {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestEnvironmentRevisionNormalizers(t *testing.T) {
	registry := NewRegistry()
	normalize := func(kind, target string, input map[string]any) (Normalized, error) {
		raw, _ := json.Marshal(input)
		return registry.Normalize(kind, target, raw)
	}
	app, err := normalize(KindAppCreate, "platform", map[string]any{
		"name": "api", "deploy": true,
		"environments": []map[string]any{{
			"name": "production", "node": "app-1",
			"source":  map[string]any{"url": "https://github.com/acme/api", "ref": "main"},
			"runtime": map[string]any{"health_path": "/healthz"},
			"processes": []map[string]any{{
				"name": "web", "kind": "web", "replicas": 2, "container_port": 3000,
			}},
			"config":       map[string]string{"APP_ENV": "production"},
			"secret_names": []string{"DATABASE_URL"},
		}, {
			"name": "staging", "node": "app-1",
			"source": map[string]any{"url": "https://github.com/acme/api", "ref": "main"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	created := app.Input.(*AppCreateInput)
	if len(created.Environments) != 2 || created.Environments[0].Processes[0].ContainerPort != 3000 {
		t.Fatalf("first environment was not normalized: %+v", created.Environments)
	}
	if created.Environments[0].Source.Builder != "" || created.Environments[0].Runtime.HealthTimeoutSeconds != DefaultHealthTimeout {
		t.Fatalf("revision defaults were not applied: %+v", created.Environments[0])
	}
	if created.Environments[0].Protected == nil || !*created.Environments[0].Protected {
		t.Fatal("production was not protected by default")
	}
	if created.Environments[1].Protected == nil || !*created.Environments[1].Protected {
		t.Fatal("staging was not protected by default")
	}
	if _, err := normalize(KindEnvironmentCreate, "app/api", map[string]any{
		"name": "pr-42", "node": "app-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := normalize(KindAppCreate, "platform", map[string]any{"name": "api", "deploy": true}); err == nil {
		t.Fatal("deploy without an environment must be refused")
	}
	if _, err := normalize(KindAppCreate, "platform", map[string]any{
		"name": "api", "environments": []map[string]any{{"name": "production", "node": "app-1", "unknown": true}},
	}); err == nil {
		t.Fatal("unknown nested environment field must be refused")
	}

	if _, err := normalize(KindEnvironmentUpdate, "env/api/production", map[string]any{
		"node":   "app-2",
		"source": map[string]any{"url": "https://github.com/acme/api"},
		"processes": []map[string]any{{
			"name": "web", "kind": "web", "container_port": 8080, "replicas": 1,
		}},
		"config":       map[string]string{"APP_ENV": "staging"},
		"secret_names": []string{"DATABASE_URL"},
	}); err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string]map[string]any{
		"unknown field":               {"health": map[string]any{"path": "/"}},
		"bad source":                  {"source": map[string]any{"url": "http://github.com/acme/api"}},
		"bad process":                 {"processes": []map[string]any{{"name": "web", "kind": "web"}}},
		"bad secret name":             {"secret_names": []string{"database_url"}},
		"bad retention":               {"retention": map[string]any{"keep_releases": 0}},
		"server-only retention field": {"retention": map[string]any{"buildpack_cache_max_bytes": 1 << 30}},
	} {
		if _, err := normalize(KindEnvironmentUpdate, "env/api/production", input); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	if _, err := normalize(KindEnvironmentUpdate, "env/api/production", map[string]any{
		"retention": map[string]any{"keep_releases": 3, "artifact_grace_days": 30},
	}); err != nil {
		t.Fatalf("a valid retention override was refused: %v", err)
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

func TestNormalizeRevisionStateRejectsSourceCredentialAliasing(t *testing.T) {
	base := RevisionState{
		Source: SourceBinding{
			URL: "https://github.com/acme/api", Ref: "main",
			CredentialSecretName: "GIT_READ_TOKEN",
		},
		Runtime: RuntimeSpec{Node: "app-1", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30},
		Processes: []ProcessSpec{
			{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080},
		},
		Config: map[string]string{},
	}
	if err := NormalizeRevisionState(&base); err != nil {
		t.Fatalf("the valid base revision was refused: %v", err)
	}

	runtimeAlias := base
	runtimeAlias.SecretNames = []string{"GIT_READ_TOKEN"}
	if fault := contract.Classify(NormalizeRevisionState(&runtimeAlias)); fault.Code != contract.ValidationFailed || !strings.Contains(fault.Hint, "runtime secret") {
		t.Fatalf("runtime-secret alias error = %+v", fault)
	}

	webhookAlias := base
	webhookAlias.WebhookSecretName = "GIT_READ_TOKEN"
	if fault := contract.Classify(NormalizeRevisionState(&webhookAlias)); fault.Code != contract.ValidationFailed || !strings.Contains(fault.Hint, "webhook secret") {
		t.Fatalf("webhook-secret alias error = %+v", fault)
	}

	valid := base
	valid.SecretNames = []string{"DATABASE_URL"}
	valid.WebhookSecretName = "EREGION_WEBHOOK_SECRET"
	if err := NormalizeRevisionState(&valid); err != nil {
		t.Fatalf("distinct credential roles were refused: %v", err)
	}

	names := []string{"GIT_READ_TOKEN"}
	merged := ApplyEnvironmentPatch(base, EnvironmentUpdateInput{SecretNames: &names})
	if fault := contract.Classify(NormalizeRevisionState(&merged)); fault.Code != contract.ValidationFailed || !strings.Contains(fault.Hint, "runtime secret") {
		t.Fatalf("merged runtime-secret alias error = %+v", fault)
	}
}

func TestEnvironmentRevisionPatchMerge(t *testing.T) {
	base := RevisionState{
		Source:      SourceBinding{URL: "https://github.com/acme/api", Ref: "main"},
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

func TestEnvironmentUpdateExplicitEmptyCollectionsApply(t *testing.T) {
	base := RevisionState{
		Source:      SourceBinding{},
		Runtime:     RuntimeSpec{Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30},
		Processes:   []ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}, {Name: "worker", Kind: "worker", Command: "bin/worker", Replicas: 1}},
		Config:      map[string]string{"APP_ENV": "production"},
		SecretNames: []string{"DATABASE_URL"},
		Resources:   []string{"db"},
	}
	raw := json.RawMessage(`{"processes":[],"config":{},"secret_names":[],"resources":[]}`)
	normalized, err := NewRegistry().Normalize(KindEnvironmentUpdate, "env/api/production", raw)
	if err != nil {
		t.Fatal(err)
	}
	patch, ok := normalized.Input.(*EnvironmentUpdateInput)
	if !ok {
		t.Fatalf("normalized input = %T", normalized.Input)
	}
	if patch.Processes == nil || len(*patch.Processes) != 0 ||
		patch.Config == nil || len(*patch.Config) != 0 ||
		patch.SecretNames == nil || len(*patch.SecretNames) != 0 ||
		patch.Resources == nil || len(*patch.Resources) != 0 {
		t.Fatalf("explicit empty collections were not preserved: %+v", patch)
	}
	var canonical map[string]json.RawMessage
	if err := json.Unmarshal(normalized.Canonical, &canonical); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"processes", "secret_names", "resources"} {
		if got := string(canonical[name]); got != "[]" {
			t.Fatalf("%s canonical value = %s, want an explicit empty array; canonical = %s", name, got, normalized.Canonical)
		}
	}
	if got := string(canonical["config"]); got != "{}" {
		t.Fatalf("config canonical value = %s, want an explicit empty object; canonical = %s", got, normalized.Canonical)
	}
	next := ApplyEnvironmentPatch(base, *patch)
	if err := NormalizeRevisionState(&next); err != nil {
		t.Fatal(err)
	}
	if len(next.Processes) != 0 || len(next.Config) != 0 || len(next.SecretNames) != 0 || len(next.Resources) != 0 {
		t.Fatalf("explicit empty collections were not applied: %+v", next)
	}
	if next.SecretNames == nil || next.Resources == nil || next.Processes == nil || next.Config == nil {
		t.Fatalf("an applied empty collection became nil again: %+v", next)
	}
	if len(base.Processes) != 2 || len(base.Config) != 1 || len(base.SecretNames) != 1 || len(base.Resources) != 1 {
		t.Fatalf("the base revision was mutated: %+v", base)
	}

	nullNormalized, err := NewRegistry().Normalize(KindEnvironmentUpdate, "env/api/production", json.RawMessage(`{"secret_names":null,"resources":null}`))
	if err != nil {
		t.Fatal(err)
	}
	nullPatch := nullNormalized.Input.(*EnvironmentUpdateInput)
	if nullPatch.SecretNames != nil || nullPatch.Resources != nil {
		t.Fatalf("JSON null should remain an omitted collection: %+v", nullPatch)
	}
	preserved := ApplyEnvironmentPatch(base, *nullPatch)
	if len(preserved.SecretNames) != 1 || len(preserved.Resources) != 1 {
		t.Fatalf("JSON null did not preserve the current collections: %+v", preserved)
	}
}

func TestRetentionOverrideIsCarriedByOtherRevisionChanges(t *testing.T) {
	keep := 5
	base := RevisionState{
		Runtime:   RuntimeSpec{Node: "app-1", HealthType: "http", HealthPath: "/healthz", HealthTimeoutSeconds: 30},
		Processes: []ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}},
		Config:    map[string]string{},
		Retention: retention.Override{KeepReleases: &keep},
	}
	config := map[string]string{"LOG_LEVEL": "debug"}
	merged := ApplyEnvironmentPatch(base, EnvironmentUpdateInput{Config: &config})
	if merged.Retention.KeepReleases == nil || *merged.Retention.KeepReleases != 5 {
		t.Fatalf("a config change dropped the retention override: %+v", merged.Retention)
	}
	*merged.Retention.KeepReleases = 9
	if *base.Retention.KeepReleases != 5 {
		t.Fatal("a patched revision aliased the previous retention override")
	}
	grace := 30
	replaced := ApplyEnvironmentPatch(base, EnvironmentUpdateInput{Retention: &retention.Override{ArtifactGraceDays: &grace}})
	if replaced.Retention.KeepReleases != nil || replaced.Retention.ArtifactGraceDays == nil || *replaced.Retention.ArtifactGraceDays != 30 {
		t.Fatalf("the retention patch did not replace the override block: %+v", replaced.Retention)
	}
	zero := 0
	invalidRevision := base
	invalidRevision.Retention = retention.Override{KeepReleases: &zero}
	if err := NormalizeRevisionState(&invalidRevision); err == nil {
		t.Fatal("a revision with keep_releases < 1 was normalized")
	}
	if err := NormalizeRevisionState(&base); err != nil {
		t.Fatalf("a valid retention override was refused: %v", err)
	}
}

func TestEvaluateDeployability(t *testing.T) {
	revision := &EnvironmentRevision{RevisionState: RevisionState{
		Source:    SourceBinding{URL: "https://github.com/acme/api"},
		Runtime:   RuntimeSpec{Node: "app-1", HealthPath: "/"},
		Processes: []ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}},
	}}
	ready := EvaluateDeployability(revision, DeployabilityState{ArtifactReady: true, PrimaryDomain: "api.example.com"})
	if !ready.Deployable || len(ready.Missing) != 0 || ready.DNS != "unknown" {
		t.Fatalf("ready report=%+v", ready)
	}
	empty := &EnvironmentRevision{}
	blocked := EvaluateDeployability(empty, DeployabilityState{DNS: string(DNSWrong)})
	want := []string{"artifact", "runtime", "web_process", "domain", "dns"}
	if !reflect.DeepEqual(blocked.Missing, want) || blocked.Deployable {
		t.Fatalf("blocked report=%+v", blocked)
	}
	// A merely unobserved DNS record does not block the first deploy; an
	// observed proxied answer does.
	unobserved := EvaluateDeployability(revision, DeployabilityState{
		ArtifactReady: true, PrimaryDomain: "api.example.com", DNS: string(DNSPending),
	})
	if !unobserved.Deployable {
		t.Fatalf("unobserved DNS blocked deployment: %+v", unobserved)
	}
	proxied := EvaluateDeployability(revision, DeployabilityState{
		ArtifactReady: true, PrimaryDomain: "api.example.com", DNS: string(DNSProxied),
	})
	if !proxied.Deployable {
		t.Fatalf("a Cloudflare-proxied record blocked deployment: %+v", proxied)
	}
	wrong := EvaluateDeployability(revision, DeployabilityState{
		ArtifactReady: true, PrimaryDomain: "api.example.com", DNS: string(DNSWrong),
	})
	if wrong.Deployable || len(wrong.Missing) != 1 || wrong.Missing[0] != "dns" {
		t.Fatalf("wrong DNS report=%+v", wrong)
	}
	if EvaluateDeployability(nil, DeployabilityState{ArtifactReady: true, PrimaryDomain: "api.example.com"}).Missing[0] != "runtime" {
		t.Fatal("a nil revision must report missing runtime state")
	}
	// An explicit release supplies its artifact, so it deploys without the
	// revision's source binding.
	noSource := &EnvironmentRevision{RevisionState: RevisionState{
		Runtime:   RuntimeSpec{Node: "app-1", HealthPath: "/"},
		Processes: []ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}},
	}}
	byRelease := EvaluateDeployability(noSource, DeployabilityState{
		ArtifactReady: true, ExplicitRelease: true, PrimaryDomain: "api.example.com",
	})
	if !byRelease.Deployable {
		t.Fatalf("an explicit release still required a source binding: %+v", byRelease)
	}
	if fromSource := EvaluateDeployability(noSource, DeployabilityState{
		ArtifactReady: true, PrimaryDomain: "api.example.com",
	}); !fromSource.Deployable {
		t.Fatalf("a Git-less revision with a ready artifact was not deployable: %+v", fromSource)
	}
}
