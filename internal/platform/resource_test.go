package platform

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

func testResourceSpec() ResourceSpec {
	return ResourceSpec{
		Image: "postgres:17",
		Env:   map[string]string{"POSTGRES_DB": "app"},
		SecretEnv: map[string]string{
			"POSTGRES_PASSWORD": "DATABASE_PASSWORD",
		},
		Mounts: []ResourceMount{{Name: "data", Destination: "/var/lib/postgresql/data"}},
		Health: ResourceHealthSpec{
			Type: "exec", Command: []string{"pg_isready", "-U", "app"}, TimeoutSeconds: 30, IntervalMS: 500,
		},
	}
}

func TestResourceSpecNormalizesGenericImageDeclaration(t *testing.T) {
	spec, err := NormalizeResourceSpec(testResourceSpec())
	if err != nil {
		t.Fatal(err)
	}
	if spec.SecretEnv["POSTGRES_PASSWORD"] != "DATABASE_PASSWORD" {
		t.Fatalf("secret names were not preserved: %+v", spec.SecretEnv)
	}
	if spec.Health.Type != "exec" || len(spec.Health.Command) != 3 {
		t.Fatalf("health check was not preserved: %+v", spec.Health)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "secret value") {
		t.Fatalf("resource spec carried a value: %s", raw)
	}
}

func TestResourceSpecRejectsUnsafeOrIncompleteDeclarations(t *testing.T) {
	cases := map[string]func(*ResourceSpec){
		"missing health command": func(spec *ResourceSpec) { spec.Health.Command = nil },
		"relative mount":         func(spec *ResourceSpec) { spec.Mounts[0].Destination = "var/lib/data" },
		"host mount path":        func(spec *ResourceSpec) { spec.Mounts[0].Destination = "/etc" },
		"dot segment":            func(spec *ResourceSpec) { spec.Mounts[0].Destination = "/var/lib/../etc" },
		"duplicate destination": func(spec *ResourceSpec) {
			spec.Mounts = append(spec.Mounts, ResourceMount{Name: "other", Destination: "/var/lib/postgresql/data"})
		},
		"duplicate mount name": func(spec *ResourceSpec) {
			spec.Mounts = append(spec.Mounts, ResourceMount{Name: "data", Destination: "/var/lib/other"})
		},
		"secret and plain collision": func(spec *ResourceSpec) {
			spec.Env["POSTGRES_PASSWORD"] = "not-a-secret"
		},
		"invalid secret name": func(spec *ResourceSpec) {
			spec.SecretEnv["POSTGRES_PASSWORD"] = "database-password"
		},
		"dash health command": func(spec *ResourceSpec) { spec.Health.Command = []string{"--help"} },
		"port out of range":   func(spec *ResourceSpec) { spec.Port = 70000 },
	}
	for name, mutate := range cases {
		spec := testResourceSpec()
		mutate(&spec)
		if _, err := NormalizeResourceSpec(spec); err == nil {
			t.Errorf("%s: was accepted", name)
		}
	}
}

func TestRevisionResourcesAreNormalizedAndDetachable(t *testing.T) {
	state := RevisionState{
		Runtime:   RuntimeSpec{Node: "main", HealthType: "http", HealthPath: "/"},
		Processes: []ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}},
		Config:    map[string]string{},
		Resources: []string{"db", "cache", "db"},
	}
	if err := NormalizeRevisionState(&state); err != nil {
		t.Fatal(err)
	}
	if strings.Join(state.Resources, ",") != "cache,db" {
		t.Fatalf("resources were not deduplicated and sorted: %v", state.Resources)
	}
	attached := AttachResource(state, "queue")
	if strings.Join(attached.Resources, ",") != "cache,db,queue" || len(state.Resources) != 2 {
		t.Fatalf("attach mutated or did not append: %v %v", attached.Resources, state.Resources)
	}
	detached := DetachResource(attached, "cache")
	if strings.Join(detached.Resources, ",") != "db,queue" || len(attached.Resources) != 3 {
		t.Fatalf("detach mutated or did not remove: %v %v", detached.Resources, attached.Resources)
	}
	invalid := state
	invalid.Resources = []string{"Bad Name"}
	if err := NormalizeRevisionState(&invalid); err == nil {
		t.Fatal("an invalid resource name entered a revision")
	}
}

func TestResourceImageDigestPrefersRequestedRepository(t *testing.T) {
	digests := []string{"docker.io/library/redis@sha256:" + strings.Repeat("b", 64), "ghcr.io/acme/redis@sha256:" + strings.Repeat("a", 64)}
	if got := ResourceImageDigest("ghcr.io/acme/redis:7", digests); !strings.HasPrefix(got, "ghcr.io/acme/redis@") {
		t.Fatalf("digest=%q", got)
	}
	if got := ResourceImageDigest("redis:7", digests); !strings.HasPrefix(got, "docker.io/library/redis@") {
		t.Fatalf("digest=%q", got)
	}
	if got := ResourceImageDigest("redis:7", nil); got != "" {
		t.Fatalf("empty inspection chose %q", got)
	}
}

func TestResourceActionStrictlyRejectsSecretValueFields(t *testing.T) {
	raw := mustResourceJSON(map[string]any{
		"image":      "redis:7",
		"secret_env": map[string]string{"REDIS_PASSWORD": "REDIS_PASSWORD"},
		"health":     map[string]any{"type": "exec", "command": []string{"redis-cli", "ping"}},
		"password":   "hunter2",
	})
	if _, err := NewRegistry().Normalize(KindResourceApply, "resource/api/production/cache", raw); err == nil {
		t.Fatal("a forbidden password field was accepted")
	}
	raw = mustResourceJSON(map[string]any{
		"image":   "redis:7",
		"env":     map[string]string{"REDIS_PASSWORD": "hunter2"},
		"health":  map[string]any{"type": "exec", "command": []string{"redis-cli", "ping"}},
		"secrets": map[string]string{"REDIS_PASSWORD": "hunter2"},
	})
	if _, err := NewRegistry().Normalize(KindResourceApply, "resource/api/production/cache", raw); err == nil {
		t.Fatal("a forbidden secrets field was accepted")
	}
}

func TestResourceTargetAndConfirmationGrammar(t *testing.T) {
	target, err := ParseTarget("resource/api/production/db")
	if err != nil {
		t.Fatal(err)
	}
	if target.Kind != TargetResource || target.App != "api" || target.Environment != "production" || target.Resource != "db" {
		t.Fatalf("target=%+v", target)
	}
	token, err := ConfirmationToken(target)
	if err != nil || token != "api/production/db" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if err := ValidateTargetConfirmation(target, token); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTargetConfirmation(target, "api/production/other"); err == nil {
		t.Fatal("wrong resource token was accepted")
	}
	for _, raw := range []string{"resource/api/production", "resource/api/production/db/extra", "resource/API/production/db"} {
		if _, err := ParseTarget(raw); err == nil {
			t.Fatalf("%q was accepted", raw)
		}
	}
}

func TestNormalizeResourceApplyBindsTheTarget(t *testing.T) {
	input := map[string]any{
		"image": "redis:7",
		"env":   map[string]string{"REDIS_APPENDONLY": "yes"},
		"secret_env": map[string]string{
			"REDIS_PASSWORD": "CACHE_PASSWORD",
		},
		"mounts": []map[string]any{{"name": "data", "destination": "/data"}},
		"health": map[string]any{"type": "exec", "command": []string{"redis-cli", "ping"}},
	}
	normalized, err := NewRegistry().Normalize(KindResourceApply, "resource/api/production/cache", mustResourceJSON(input))
	if err != nil {
		t.Fatal(err)
	}
	apply := normalized.Input.(*ResourceApplyInput)
	if apply.App != "api" || apply.Environment != "production" || apply.Name != "cache" {
		t.Fatalf("target was not bound: %+v", apply)
	}
	if _, ok := apply.SecretEnv["REDIS_PASSWORD"]; !ok {
		t.Fatal("secret name mapping was lost")
	}
	if _, err := NewRegistry().Normalize(KindResourceApply, "resource/api/production/cache", mustResourceJSON(map[string]any{
		"app": "other", "image": "redis:7", "health": map[string]any{"type": "exec", "command": []string{"redis-cli", "ping"}},
	})); err == nil {
		t.Fatal("a mismatched app was accepted")
	}
}

func TestResourcePlanUsesPrivateNetworkAndResourceScopedVolumes(t *testing.T) {
	id := ResourceID("res_00000000000000000000000000000001")
	envID := EnvironmentID("env_00000000000000000000000000000002")
	plan, err := PlanResource(envID, "main", id, "api", "production", "db", testResourceSpec())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Network != "er-env_00000000000000000000000000000002" {
		t.Fatalf("network=%s", plan.Network)
	}
	if plan.Alias != "db" || plan.Container != "er-res-00000000000000000000000000000001" {
		t.Fatalf("private identity=%+v", plan)
	}
	if len(plan.Volumes) != 1 || plan.Volumes[0].Name != "er-res-00000000000000000000000000000001-data" {
		t.Fatalf("volumes=%+v", plan.Volumes)
	}
	if !ResourceVolumeNameValid(plan.Volumes[0].Name) {
		t.Fatalf("volume name is not resource-scoped: %s", plan.Volumes[0].Name)
	}
	for _, step := range []string{
		ResourceNetworkStepID(plan), ResourcePullStepID(plan), ResourceImageStepID(plan),
		ResourceVolumeStepID(plan, 0), ResourceRunStepID(plan), ResourceHealthStepID(plan),
		ResourceDestroyStepID(plan.ID),
	} {
		if !engine.IDPattern.MatchString(step) {
			t.Fatalf("step ID is not versioned: %s", step)
		}
	}
	if plan.Container == plan.Name {
		t.Fatal("the container name must be collision-safe, not the resource alias")
	}
}

func TestResourceVolumeLedgerKeepsRemovedMountsAndInterruptedIdentities(t *testing.T) {
	id := ResourceID("res_00000000000000000000000000000001")
	envID := EnvironmentID("env_00000000000000000000000000000002")
	dataName := "er-res-00000000000000000000000000000001-data"
	cacheName := "er-res-00000000000000000000000000000001-cache"

	// A reprovision that drops the data mount must not forget the volume that
	// already holds this resource's data; a later --delete-data still removes it.
	existing := []ResourceVolumeStatus{{Name: dataName, Destination: "/var/lib/data"}}
	if got := MergeResourceVolumes(existing, nil); len(got) != 1 || got[0].Name != dataName {
		t.Fatalf("mount removal dropped the owned identity: %+v", got)
	}
	merged := MergeResourceVolumes(existing, []ResourceVolumeStatus{{Name: cacheName, Destination: "/cache"}})
	if len(merged) != 2 || merged[0].Name != cacheName || merged[1].Name != dataName {
		t.Fatalf("volume ledger is not cumulative and deterministic: %+v", merged)
	}
	// The same derived identity stays one volume; the latest mount facts win.
	replaced := MergeResourceVolumes(merged, []ResourceVolumeStatus{{Name: dataName, Destination: "/srv/data", ReadOnly: true}})
	if len(replaced) != 2 || replaced[1].Destination != "/srv/data" || !replaced[1].ReadOnly {
		t.Fatalf("current mount facts were not applied: %+v", replaced)
	}

	spec := testResourceSpec()
	plan, err := PlanResource(envID, "main", id, "api", "production", "db", spec)
	if err != nil {
		t.Fatal(err)
	}
	// A crash after "docker volume create" but before the first durable write is
	// represented by an empty ledger; the declaration's deterministic identity is
	// still the safe delete target because ownership labels are verified on the node.
	if got := ResourceVolumesForDestroy(nil, plan); len(got) != 1 || got[0].Name != plan.Volumes[0].Name {
		t.Fatalf("planned identity was not recoverable for an interrupted apply: %+v", got)
	}
	// After a default destroy and a changed declaration, the retained ledger
	// remains the delete target even when the current plan no longer mounts it.
	spec.Mounts = nil
	noMounts, err := PlanResource(envID, "main", id, "api", "production", "db", spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := ResourceVolumesForDestroy(existing, noMounts); len(got) != 1 || got[0].Name != dataName {
		t.Fatalf("destroy --delete-data lost a retained volume after a mount removal: %+v", got)
	}
}

func TestResourceConfigHashChangesWithSecretRevisionButNeverCarriesValues(t *testing.T) {
	id := ResourceID("res_00000000000000000000000000000001")
	envID := EnvironmentID("env_00000000000000000000000000000002")
	plan, err := PlanResource(envID, "main", id, "api", "production", "db", testResourceSpec())
	if err != nil {
		t.Fatal(err)
	}
	imageID := "sha256:" + strings.Repeat("a", 64)
	first := ResourceContainerConfigHash(plan, imageID, "revision-one", []string{"POSTGRES_PASSWORD"})
	second := ResourceContainerConfigHash(plan, imageID, "revision-two", []string{"POSTGRES_PASSWORD"})
	if first == second {
		t.Fatal("secret revision rotation did not change the container configuration hash")
	}
	if strings.Contains(first, "revision-one") || strings.Contains(first, "POSTGRES_PASSWORD") {
		t.Fatal("the configuration hash leaked a non-value identity in plain text")
	}
}

func TestParseResourceImageInspectRequiresImmutableIdentity(t *testing.T) {
	id, digests, err := ParseResourceImageInspect("sha256:" + strings.Repeat("a", 64) + "|[\"ghcr.io/acme/db@sha256:" + strings.Repeat("b", 64) + "\"]")
	if err != nil {
		t.Fatal(err)
	}
	if !ResourceImageIDValid(id) || len(digests) != 1 {
		t.Fatalf("id=%q digests=%v", id, digests)
	}
	if _, _, err := ParseResourceImageInspect("postgres:17"); err == nil {
		t.Fatal("a mutable tag was accepted as an immutable identity")
	}
}

func TestParseResourceImageInspectReadsJSONRepoDigests(t *testing.T) {
	id := "sha256:" + strings.Repeat("a", 64)
	one := "ghcr.io/acme/db@sha256:" + strings.Repeat("b", 64)
	two := "mirror.example/db@sha256:" + strings.Repeat("c", 64)
	cases := []struct {
		name    string
		stdout  string
		digests []string
	}{
		{name: "one", stdout: id + "|[\"" + one + "\"]", digests: []string{one}},
		{name: "several", stdout: id + "|[\"" + one + "\",\"" + two + "\"]", digests: []string{one, two}},
		{name: "empty", stdout: id + "|[]"},
		{name: "null", stdout: id + "|null"},
		{name: "absent", stdout: id + "|"},
		{name: "no value", stdout: id + "|<no value>"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			gotID, gotDigests, err := ParseResourceImageInspect(test.stdout)
			if err != nil {
				t.Fatal(err)
			}
			if gotID != id {
				t.Fatalf("id = %q, want %q", gotID, id)
			}
			if strings.Join(gotDigests, ",") != strings.Join(test.digests, ",") {
				t.Fatalf("digests = %v, want %v", gotDigests, test.digests)
			}
		})
	}
	if _, _, err := ParseResourceImageInspect(id + `|ghcr.io/acme/db@sha256:` + strings.Repeat("b", 64)); err == nil {
		t.Fatal("a non-JSON repository digest list was accepted")
	}
}

func TestResourceImageInspectFormatUsesJSONRepoDigests(t *testing.T) {
	if !strings.Contains(ResourceImageInspectFormat, "{{json .RepoDigests}}") {
		t.Fatalf("image inspect format does not serialize RepoDigests as JSON: %q", ResourceImageInspectFormat)
	}
	if strings.Contains(ResourceImageInspectFormat, "join .RepoDigests") {
		t.Fatalf("image inspect format still joins RepoDigests as []string: %q", ResourceImageInspectFormat)
	}
}

func TestResourceApplyPreviewDoesNotIncludeSecretValues(t *testing.T) {
	input := &ResourceApplyInput{
		App: "api", Environment: "production", Name: "db",
		ResourceSpec: testResourceSpec(),
	}
	raw, err := json.Marshal(input.Preview())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Fatalf("preview leaked a value: %s", raw)
	}
}

func TestResourceContractErrorsNeverEchoValues(t *testing.T) {
	spec := testResourceSpec()
	spec.SecretEnv["POSTGRES_PASSWORD"] = "not a valid name with hunter2"
	_, err := NormalizeResourceSpec(spec)
	if err == nil {
		t.Fatal("invalid secret name was accepted")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("validation error echoed secret-like value: %v", err)
	}
	fault := contract.Classify(err)
	if fault.Code != contract.ValidationFailed {
		t.Fatalf("code=%s", fault.Code)
	}
}

func mustResourceJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}
