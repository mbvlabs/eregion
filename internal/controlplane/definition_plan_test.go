package controlplane

import (
	"encoding/json"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/appdef"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestDefinitionPlanDigestBindsAutomationAndProviderSnapshot(t *testing.T) {
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: "version = 2\n[automation]\nauto_deploy = false\n"}
	snapshot := definitionSnapshot{Workspace: WorkspaceIdentity{Configured: true, ID: "ws_123456789012345678901234", Name: "main"}, ProviderRevision: 1}
	plan := api.DefinitionPlan{DNS: []integrations.DefinitionDNSPlan{{Host: "api.example.com"}}}
	want, err := definitionPlanDigest(input, snapshot, plan)
	if err != nil {
		t.Fatal(err)
	}
	again, err := definitionPlanDigest(input, snapshot, plan)
	if err != nil || again != want {
		t.Fatalf("unstable digest: %s %s %v", want, again, err)
	}
	input.Definition = "version = 2\n[automation]\n"
	changed, _ := definitionPlanDigest(input, snapshot, plan)
	if changed == want {
		t.Fatal("omitted automation field did not change digest")
	}
	input.Definition = "version = 2\n[automation]\nauto_deploy = false\n"
	snapshot.ProviderRevision++
	changed, _ = definitionPlanDigest(input, snapshot, plan)
	if changed == want {
		t.Fatal("provider revision did not change digest")
	}
	snapshot.ProviderRevision--
	plan.DNS[0].Before = []integrations.DomainDNSRecord{{ID: "external", Type: "A", Content: "1.1.1.1"}}
	changed, _ = definitionPlanDigest(input, snapshot, plan)
	if changed == want {
		t.Fatal("provider record did not change digest")
	}
	plan.DNS[0].Before = nil
	plan.ConfirmTarget = "api/production"
	changed, _ = definitionPlanDigest(input, snapshot, plan)
	if changed == want {
		t.Fatal("confirmation target did not change digest")
	}
}

func TestDefinitionFrozenRemovalConfirmation(t *testing.T) {
	const definition = `version = 2
app = "api"
node = "main"
[source]
[automation]
auto_deploy = false
`
	input := api.DefinitionPlanInput{App: "api", Environment: "production", Definition: definition}
	r, err := appdef.ResolveSelection([]byte(definition), input.App, input.Environment)
	if err != nil {
		t.Fatal(err)
	}
	snap := definitionSnapshot{AppID: "app", EnvironmentID: "env"}
	for _, tc := range []struct {
		name   string
		mutate func(*platform.RevisionState)
	}{
		{"config", func(s *platform.RevisionState) { s.Config = map[string]string{"OLD": "value"} }},
		{"process", func(s *platform.RevisionState) {
			s.Processes = []platform.ProcessSpec{{Name: "worker", Kind: "worker", Command: "worker", Replicas: 1}}
		}},
		{"secret", func(s *platform.RevisionState) { s.SecretNames = []string{"OLD"} }},
		{"resource", func(s *platform.RevisionState) { s.Resources = []string{"cache"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := platform.RevisionState{Source: platform.SourceBinding{}}
			tc.mutate(&base)
			env := platform.Environment{App: "api", Name: "production", Node: "main", Revision: &platform.EnvironmentRevision{RevisionState: base}}
			plan := api.DefinitionPlan{App: "api", Environment: "production"}
			steps, err := compileDefinitionWork(input, r, snap, env, &plan)
			if err != nil {
				t.Fatal(err)
			}
			if plan.ConfirmTarget != "api/production" {
				t.Fatalf("target %q", plan.ConfirmTarget)
			}
			for _, value := range []string{"", "api/staging"} {
				if err := definitionConfirm(plan, value); contract.Classify(err).Code != contract.InputRequired {
					t.Fatalf("confirmation %q: %v", value, err)
				}
			}
			if err := definitionConfirm(plan, "api/production"); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, step := range steps {
				if step.Kind == platform.KindEnvironmentUpdate {
					var patch platform.EnvironmentUpdateInput
					if err := json.Unmarshal(step.Input, &patch); err != nil {
						t.Fatal(err)
					}
					if patch.Confirm != plan.ConfirmTarget {
						t.Fatalf("patch confirmation %q", patch.Confirm)
					}
					found = true
				}
			}
			if !found {
				t.Fatal("missing update child")
			}
		})
	}
	addition := input
	addition.Definition += "\n[config]\nNEW = \"value\"\n"
	r, err = appdef.ResolveSelection([]byte(addition.Definition), input.App, input.Environment)
	if err != nil {
		t.Fatal(err)
	}
	env := platform.Environment{App: "api", Name: "production", Node: "main", Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{Source: platform.SourceBinding{}}}}
	plan := api.DefinitionPlan{App: "api", Environment: "production"}
	steps, err := compileDefinitionWork(addition, r, snap, env, &plan)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ConfirmTarget != "" {
		t.Fatalf("addition target %q", plan.ConfirmTarget)
	}
	for _, step := range steps {
		if step.Kind == platform.KindEnvironmentUpdate {
			var patch platform.EnvironmentUpdateInput
			if err := json.Unmarshal(step.Input, &patch); err != nil {
				t.Fatal(err)
			}
			if patch.Confirm != "" {
				t.Fatal("addition confirmed")
			}
		}
	}
	env.Revision = nil
	if _, err := compileDefinitionWork(addition, r, snap, env, &plan); err == nil {
		t.Fatal("missing revision accepted")
	}
}

func TestCompileDefinitionWorkOrderingAndIdentity(t *testing.T) {
	input := api.DefinitionPlanInput{App: "api", Environment: "production", PlanNonce: "nonce", Definition: `version = 2
app = "api"
node = "main"
[source]
[[resources]]
name = "cache"
image = "redis:7"
[resources.health]
command = ["redis-cli", "ping"]
[[domains]]
name = "new.example.com"
primary = true
[[domains]]
name = "keep.example.com"
`}
	r, err := appdef.ResolveSelection([]byte(input.Definition), input.App, input.Environment)
	if err != nil {
		t.Fatal(err)
	}
	id := definitionPlannedDomainID("ws", "api", "production", input.PlanNonce, "new.example.com")
	snap := definitionSnapshot{Workspace: WorkspaceIdentity{ID: "ws"}, AppID: "app", EnvironmentID: "env", Policy: api.DefinitionDNSPolicy{Enabled: true, ZoneID: "0123456789abcdef0123456789abcdef", Origins: []string{"1.1.1.1"}, ProxyMode: "dns-only"}, Bindings: []definitionBinding{{ID: "old", Name: "old.example.com", Primary: true, ZoneID: "0123456789abcdef0123456789abcdef"}, {ID: "keep", Name: "keep.example.com"}}}
	env := platform.Environment{App: "api", Name: "production", Node: "main", Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{Source: platform.SourceBinding{}}}}
	plan := api.DefinitionPlan{Domains: []api.DefinitionDomainChange{{Action: "remove", Name: "old.example.com", ID: "old"}, {Action: "unchanged", Name: "keep.example.com", ID: "keep"}, {Action: "add", Name: "new.example.com", ID: id, Primary: true}}, DNS: []integrations.DefinitionDNSPlan{{Host: "new.example.com", DomainID: string(id)}, {Host: "keep.example.com", DomainID: "keep"}}, DNSCleanup: []integrations.DomainDNSPlan{{Host: "old.example.com", DomainID: "old"}}}
	steps, err := compileDefinitionWork(input, r, snap, env, &plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 5 {
		t.Fatalf("steps: %+v", steps)
	}
	for i, kind := range []string{platform.KindResourceApply, platform.KindEnvironmentUpdate, platform.KindDomainAdd, platform.KindDomainAdd, platform.KindDomainRemove} {
		if steps[i].Kind != kind {
			t.Fatalf("step %d: %+v", i, steps[i])
		}
	}
	if steps[2].AllocatedDomainID != id || steps[2].DNSPlan == nil || steps[3].DomainID != "keep" || steps[3].DNSPlan == nil || steps[4].DNSCleanup == nil {
		t.Fatalf("domain steps: %+v", steps[2:])
	}
	for _, step := range steps {
		if !json.Valid(step.Input) {
			t.Fatalf("invalid canonical input: %s", step.Input)
		}
	}
	plan.DNS[0].DomainID = "other"
	if _, err := compileDefinitionWork(input, r, snap, env, &plan); err == nil {
		t.Fatal("accepted mismatched DNS identity")
	}
	input.Deploy = true
	if _, err := compileDefinitionWork(input, r, snap, env, &plan); err == nil {
		t.Fatal("accepted implicit deploy")
	}
	input.Deploy = false
	snap.EnvironmentID = ""
	if _, err := compileDefinitionWork(input, r, snap, env, &plan); err == nil {
		t.Fatal("accepted missing environment")
	}
}

func TestDefinitionPlannedDomainIDBindsTargetAndNonce(t *testing.T) {
	base := definitionPlannedDomainID("workspace", "app", "production", "nonce", "example.com")
	if base != definitionPlannedDomainID("workspace", "app", "production", "nonce", "example.com") {
		t.Fatal("identity is not deterministic")
	}
	for _, changed := range []struct{ workspace, app, env, nonce, host string }{
		{"other", "app", "production", "nonce", "example.com"},
		{"workspace", "other", "production", "nonce", "example.com"},
		{"workspace", "app", "staging", "nonce", "example.com"},
		{"workspace", "app", "production", "other", "example.com"},
		{"workspace", "app", "production", "nonce", "other.example.com"},
	} {
		if base == definitionPlannedDomainID(changed.workspace, changed.app, changed.env, changed.nonce, changed.host) {
			t.Fatalf("identity did not bind %+v", changed)
		}
	}
}
