package platform

import (
	"strings"
	"testing"
)

func TestPlanDeploymentPerProcessReplicas(t *testing.T) {
	revision := RevisionState{
		Processes: []ProcessSpec{
			{Name: "web", Kind: "web", Replicas: 2, ContainerPort: 8080},
			{Name: "worker", Kind: "worker", Replicas: 1, Command: "worker"},
			{Name: "cache", Kind: "service", Replicas: 1, ContainerPort: 6379, Command: "cache"},
		},
	}
	plan, err := PlanDeployment("api", "production", revision, "rel_00000000000000000000000000000001", "dep_00000000000000000000000000000002")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 4 {
		t.Fatalf("plan length=%d want 4", len(plan))
	}
	seen := map[string]bool{}
	for _, container := range plan {
		if seen[container.Name] {
			t.Fatalf("duplicate container name %q", container.Name)
		}
		seen[container.Name] = true
		if len(container.Name) > 63 {
			t.Fatalf("container name %q exceeds 63 bytes", container.Name)
		}
		if container.Labels["io.eregion.app"] != "api" || container.Labels["io.eregion.environment"] != "production" {
			t.Fatalf("missing ownership labels: %+v", container.Labels)
		}
		switch container.Process.Kind {
		case "web", "service":
			if !container.Publish || container.HostPort != 0 || container.ContainerPort < 1 {
				t.Fatalf("%s container should publish a dynamic port: %+v", container.Process.Kind, container)
			}
		case "worker":
			if container.Publish || container.ContainerPort != 0 {
				t.Fatalf("worker container must not publish a port: %+v", container)
			}
			if len(container.Command) != 1 || container.Command[0] != "worker" {
				t.Fatalf("worker command was not planned: %+v", container)
			}
		}
	}
}

func TestWorkloadContainerNameIsBounded(t *testing.T) {
	name := WorkloadContainerName(strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 63), "12", "dep_00000000000000000000000000000002")
	if len(name) > 63 {
		t.Fatalf("name length=%d: %q", len(name), name)
	}
	if !strings.HasSuffix(name, "-000000000000") {
		t.Fatalf("deployment suffix missing: %q", name)
	}
}

func TestParsePublishedPort(t *testing.T) {
	port, err := ParsePublishedPort("0.0.0.0:32768\n127.0.0.1:40001\n[::]:32768\n", 8080)
	if err != nil || port != 40001 {
		t.Fatalf("port=%d err=%v", port, err)
	}
	if _, err := ParsePublishedPort("", 8080); err == nil {
		t.Fatal("missing output must fail")
	}
	if got := PublishArgument("127.0.0.1", 0, 8080); got != "127.0.0.1::8080" {
		t.Fatalf("dynamic publish=%q", got)
	}
	if got := PublishArgument("127.0.0.1", 40001, 8080); got != "127.0.0.1:40001:8080" {
		t.Fatalf("fixed publish=%q", got)
	}
}
