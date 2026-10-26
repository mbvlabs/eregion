package cli

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/platform"
)

func TestBuildGuidedUpdateMapsPrefilledAnswers(t *testing.T) {
	current := platform.Environment{
		App: "api", Name: "production", Node: "main",
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source: platform.SourceBinding{
				Kind: "git", URL: "https://github.com/acme/api", Ref: "main",
				CredentialSecretName: "GIT_READ_TOKEN", Builder: platform.DefaultBuilder,
			},
			Runtime: platform.RuntimeSpec{
				Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30,
				ReleaseCommand: &platform.CommandSpec{Argv: []string{"./migrate"}, TimeoutSeconds: 120},
			},
			Processes: []platform.ProcessSpec{
				{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080},
				{Name: "worker", Kind: "worker", Replicas: 1, Command: "./worker", Args: []string{"--queue", "default"}},
			},
		}},
	}
	answers := guidedUpdateAnswers{
		Node: "main2", BuildNode: "builder",
		Source: platform.SourceBinding{
			Kind: "git", URL: "https://github.com/acme/api", Ref: "main",
			CredentialSecretName: "GIT_READ_TOKEN", Builder: platform.DefaultBuilder,
		},
		Runtime: platform.RuntimeSpec{
			Node: "main2", HealthType: "http", HealthPath: "/readyz", HealthTimeoutSeconds: 30,
			ReleaseCommand: &platform.CommandSpec{Argv: []string{"./migrate"}, TimeoutSeconds: 120},
		},
		Processes:      []platform.ProcessSpec{{Name: "web", Kind: "web", Replicas: 2, ContainerPort: 8080}},
		Credential:     []byte("new-token"),
		CredentialName: "GIT_READ_TOKEN",
	}
	edit := buildGuidedUpdate(current, answers)
	if edit.Patch.Node == nil || *edit.Patch.Node != "main2" {
		t.Fatalf("node patch = %+v", edit.Patch.Node)
	}
	if edit.Patch.BuildNode == nil || *edit.Patch.BuildNode != "builder" {
		t.Fatalf("build node patch = %+v", edit.Patch.BuildNode)
	}
	if edit.Patch.Source == nil || edit.Patch.Source.URL != "https://github.com/acme/api" {
		t.Fatalf("source patch = %+v", edit.Patch.Source)
	}
	if edit.Patch.Runtime == nil || edit.Patch.Runtime.Node != "main2" ||
		edit.Patch.Runtime.HealthPath != "/readyz" || edit.Patch.Runtime.ReleaseCommand == nil {
		t.Fatalf("runtime patch = %+v", edit.Patch.Runtime)
	}
	if edit.Patch.Processes == nil || len(*edit.Patch.Processes) != 1 ||
		(*edit.Patch.Processes)[0].Replicas != 2 {
		t.Fatalf("processes patch = %+v", edit.Patch.Processes)
	}
	if edit.Patch.Config != nil || edit.Patch.SecretNames != nil || edit.Patch.AutoDeploy != nil {
		t.Fatalf("untouched fields were sent: %+v", edit.Patch)
	}
	if got := strings.Join(edit.Changed, ","); got != "node,build_node,source,runtime,processes" {
		t.Fatalf("changed = %s", got)
	}
	diff := strings.Join(edit.Diff, "\n")
	for _, want := range []string{
		"node: main → main2",
		"build node: (none) → builder",
		"source.read_credential: replaced",
		"runtime.health_path: / → /readyz",
		"process web.replicas: 1 → 2",
		"process worker: removed",
	} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, diff)
		}
	}
}

func TestBuildGuidedUpdateKeepsUnchangedRevision(t *testing.T) {
	current := platform.Environment{
		App: "api", Name: "production", Node: "main",
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source:    platform.SourceBinding{Kind: "image", Image: "ghcr.io/acme/api:1"},
			Runtime:   platform.RuntimeSpec{Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30},
			Processes: []platform.ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}},
		}},
	}
	edit := buildGuidedUpdate(current, guidedUpdateAnswers{
		Node: "main", Source: current.Revision.Source, Runtime: current.Revision.Runtime,
		Processes: current.Revision.Processes,
	})
	if len(edit.Changed) != 0 || len(edit.Diff) != 0 {
		t.Fatalf("unchanged revision produced changes: %+v %+v", edit.Changed, edit.Diff)
	}
}
