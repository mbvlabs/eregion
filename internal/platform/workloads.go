package platform

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// ObserveLocalWorkloads reads Docker ownership labels on the node that runs
// this process. It is read-only: no journal, operation or database row.
func ObserveLocalWorkloads(ctx context.Context, app, environment string) (Workloads, error) {
	if err := ValidateName(app); err != nil {
		return Workloads{}, err
	}
	if environment != "" {
		if err := ValidateName(environment); err != nil {
			return Workloads{}, err
		}
	}
	args := []string{"docker", "ps", "--all", "--format", "{{json .}}", "--filter", "label=io.eregion.app=" + app}
	if environment != "" {
		args = append(args, "--filter", "label=io.eregion.environment="+environment)
	}
	raw, err := runDockerRead(ctx, args)
	if err != nil {
		return Workloads{}, err
	}
	containers, err := ParseDockerPS(raw)
	if err != nil {
		return Workloads{}, contract.NewFault(contract.Unavailable, "the node returned an unreadable Docker observation", err.Error())
	}
	return Workloads{
		App: app, Environment: environment,
		ObservedAt: time.Now().UTC(), Containers: containers,
	}, nil
}

func runDockerRead(ctx context.Context, argv []string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(runCtx, argv[0], argv[1:]...) //nolint:gosec // G204: fixed argv, no shell.
	var stderr bytes.Buffer
	command.Stderr = &stderr
	raw, err := command.Output()
	if err == nil {
		return raw, nil
	}
	if runCtx.Err() != nil {
		return nil, runCtx.Err()
	}
	detail := boundedCommandOutput(stderr.String())
	if errors.Is(err, exec.ErrNotFound) || strings.Contains(strings.ToLower(detail), "cannot connect to the docker daemon") {
		return nil, contract.NewFault(contract.PreconditionFailed, "Docker is not available on this node", "Install Docker and start it before observing or deleting workloads.")
	}
	if detail == "" {
		detail = err.Error()
	}
	return nil, contract.NewFault(contract.Unavailable, "the node could not read Docker", detail)
}

func boundedCommandOutput(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 4096 {
		value = value[:4096]
	}
	return value
}
