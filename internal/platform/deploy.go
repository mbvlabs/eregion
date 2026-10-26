package platform

import (
	"fmt"
	"strconv"
)

// PlannedContainer is one process container in a deployment formation.
type PlannedContainer struct {
	Process       ProcessSpec
	Replica       string
	Name          string
	Labels        map[string]string
	Publish       bool
	ContainerPort int
	Command       []string
	// HostPort is zero for a dynamically allocated loopback port.
	HostPort int
}

// PlanDeployment expands an environment revision into its process containers.
// Web and service processes publish a dynamic loopback port; workers publish
// none. The plan is pure so the worker and tests agree on names and labels.
func PlanDeployment(app, environment string, revision RevisionState, release ReleaseID, deployment DeploymentID) ([]PlannedContainer, error) {
	plan := make([]PlannedContainer, 0, len(revision.Processes))
	for _, process := range revision.Processes {
		if process.Replicas < 1 {
			return nil, fmt.Errorf("process %s has no replicas", process.Name)
		}
		for replica := 1; replica <= process.Replicas; replica++ {
			key := strconv.Itoa(replica)
			publish := process.Kind == "web" || process.Kind == "service"
			containerPort := 0
			if publish {
				containerPort = process.ContainerPort
			}
			command := make([]string, 0, len(process.Args)+1)
			if process.Command != "" {
				command = append(command, process.Command)
			}
			command = append(command, process.Args...)
			plan = append(plan, PlannedContainer{
				Process:       process,
				Replica:       key,
				Name:          WorkloadContainerName(app, environment, process.Name, key, deployment),
				Labels:        WorkloadLabels(app, environment, process.Name, process.Kind, key, release, deployment),
				Publish:       publish,
				ContainerPort: containerPort,
				Command:       command,
			})
		}
	}
	return plan, nil
}
