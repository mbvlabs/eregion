package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/rpc"
	"github.com/mbvlabs/eregion/internal/secrets"
)

const (
	methodWorkloads      = "platform.workloads"
	methodLogs           = "platform.logs"
	methodOperationsList = "operations.list"
)

type rpcWorkloadParams struct {
	App         string `json:"app"`
	Environment string `json:"environment,omitempty"`
}

type rpcLogParams struct {
	App          string `json:"app"`
	Environment  string `json:"environment"`
	DeploymentID string `json:"deployment_id,omitempty"`
	Process      string `json:"process,omitempty"`
	Follow       bool   `json:"follow,omitempty"`
	Since        string `json:"since,omitempty"`
	Tail         int    `json:"tail,omitempty"`
}

type rpcOperationsListParams struct {
	App         string `json:"app,omitempty"`
	Environment string `json:"environment,omitempty"`
	State       string `json:"state,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}

func (n *nodeClient) Workloads(ctx context.Context, app, environment string) (platform.Workloads, error) {
	raw, err := n.call(ctx, methodWorkloads, rpcWorkloadParams{App: app, Environment: environment}, rpcCallOptions{})
	if err != nil {
		return platform.Workloads{}, err
	}
	var workloads platform.Workloads
	if err := json.Unmarshal(raw, &workloads); err != nil || workloads.Containers == nil {
		return platform.Workloads{}, invalidNodeData("workload observation")
	}
	return workloads, nil
}

func (n *nodeClient) Logs(ctx context.Context, query platform.LogQuery, onLine func(platform.LogLine) error) (platform.LogResult, error) {
	if err := query.Validate(); err != nil {
		return platform.LogResult{}, err
	}
	params := rpcLogParams{
		App: query.App, Environment: query.Environment, DeploymentID: string(query.DeploymentID),
		Process: query.Process, Follow: query.Follow, Since: query.Since.String(), Tail: query.Tail,
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var callbackErr error
	opts := rpcCallOptions{OnEvent: func(event rpc.EventBody) {
		if event.Kind != "log" || event.Log == nil || callbackErr != nil {
			return
		}
		line := platform.LogLine{
			App: query.App, Environment: query.Environment,
			Process: event.Log.Process, Replica: event.Log.Replica,
			Container: event.Log.Container, Stream: event.Log.Stream, Message: event.Log.Message,
		}
		if err := onLine(line); err != nil {
			callbackErr = err
			cancel()
		}
	}}
	raw, err := n.call(ctx, methodLogs, params, opts)
	if callbackErr != nil {
		return platform.LogResult{}, callbackErr
	}
	if err != nil {
		return platform.LogResult{}, err
	}
	var result platform.LogResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return platform.LogResult{}, invalidNodeData("log result")
	}
	return result, nil
}

func (n *nodeClient) ListOperations(ctx context.Context, query operations.ListQuery) (operations.ListPage, error) {
	if err := query.Validate(); err != nil {
		return operations.ListPage{}, err
	}
	raw, err := n.call(ctx, methodOperationsList, rpcOperationsListParams{
		App: query.App, Environment: query.Environment, State: query.State, Limit: query.Limit,
	}, rpcCallOptions{})
	if err != nil {
		return operations.ListPage{}, err
	}
	var page operations.ListPage
	if json.Unmarshal(raw, &page) != nil || page.Items == nil || len(page.Items) > page.Limit {
		return operations.ListPage{}, invalidNodeData("operation list")
	}
	for _, op := range page.Items {
		if op.Validate() != nil {
			return operations.ListPage{}, invalidNodeData("operation list")
		}
	}
	return page, nil
}

// observeNodeWorkloads reads the node-local Docker ownership labels. It is a
// read-only method; no journal, operation, or database row is written.
func observeNodeWorkloads(ctx context.Context, params rpcWorkloadParams) (platform.Workloads, error) {
	return platform.ObserveLocalWorkloads(ctx, params.App, params.Environment)
}

// runDockerCombinedRead merges the container's stdout and stderr streams for a
// bounded, non-following log read. Docker keeps those streams separate.
func runDockerCombinedRead(ctx context.Context, argv []string) ([]byte, error) {
	command := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: fixed argv, no shell.
	var combined bytes.Buffer
	command.Stdout, command.Stderr = &combined, &combined
	err := command.Run()
	if err == nil {
		return combined.Bytes(), nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	detail := boundedDockerOutput(combined.String())
	if errors.Is(err, exec.ErrNotFound) || strings.Contains(strings.ToLower(detail), "cannot connect to the docker daemon") {
		return nil, contract.NewFault(contract.PreconditionFailed, "Docker is not available on this node", "Install Docker and start it before observing or deleting workloads.")
	}
	if detail == "" {
		detail = err.Error()
	}
	return nil, contract.NewFault(contract.Unavailable, "the node could not read container logs", detail)
}

func boundedDockerOutput(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 2048 {
		value = value[:2048]
	}
	return value
}

// streamNodeLogs reads the serving formation's containers and streams each line
// as an RPC event. Following stops when the operator disconnects or Docker
// exits; nothing is persisted.
func streamNodeLogs(ctx context.Context, params rpcLogParams, out io.Writer, registry *secrets.Registry) (platform.LogResult, error) {
	query := platform.LogQuery{
		App: params.App, Environment: params.Environment,
		DeploymentID: platform.DeploymentID(params.DeploymentID), Process: params.Process,
		Follow: params.Follow, Tail: params.Tail,
	}
	query.Since = 10 * time.Minute
	if params.Since != "" {
		since, err := time.ParseDuration(params.Since)
		if err != nil {
			return platform.LogResult{}, contract.NewFault(contract.ValidationFailed, "invalid log lookback", "Use a positive duration such as 15m or 2h.")
		}
		query.Since = since
	}
	if query.Tail == 0 {
		query.Tail = 200
	}
	if err := query.Validate(); err != nil {
		return platform.LogResult{}, err
	}
	if registry == nil {
		registry = &secrets.Registry{}
	}
	workloads, err := observeNodeWorkloads(ctx, rpcWorkloadParams{App: params.App, Environment: params.Environment})
	if err != nil {
		return platform.LogResult{}, err
	}
	containers := make([]platform.ContainerObservation, 0, len(workloads.Containers))
	for _, container := range workloads.Containers {
		if params.DeploymentID != "" && container.Deployment != params.DeploymentID {
			continue
		}
		if params.Process != "" && container.Process != params.Process {
			continue
		}
		containers = append(containers, container)
	}
	result := platform.LogResult{Follow: params.Follow}
	if len(containers) > 64 {
		containers = containers[:64]
		result.Truncated = true
	}
	if !params.Follow {
		for _, container := range containers {
			raw, err := runDockerCombinedRead(ctx, dockerLogsArgs(query, container.ContainerID))
			if err != nil {
				return result, err
			}
			for _, message := range splitLogLines(raw) {
				if len(result.Lines) >= 10000 {
					result.Truncated = true
					break
				}
				result.Lines = append(result.Lines, containerLogLine(container, registry, message))
			}
		}
		result.Count = len(result.Lines)
		return result, nil
	}
	count, err := followNodeLogs(ctx, containers, query, out, registry)
	result.Count = count
	return result, err
}

func dockerLogsArgs(query platform.LogQuery, containerID string) []string {
	args := []string{"docker", "logs", "--tail", strconv.Itoa(query.Tail)}
	if query.Since > 0 {
		args = append(args, "--since", query.Since.String())
	}
	if query.Follow {
		args = append(args, "--follow")
	}
	return append(args, containerID)
}

func splitLogLines(raw []byte) []string {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func containerLogLine(container platform.ContainerObservation, registry *secrets.Registry, message string) platform.LogLine {
	if len(message) > 32*1024 {
		message = message[:32*1024] + "…"
	}
	return platform.LogLine{
		App: container.App, Environment: container.Environment,
		Process: container.Process, Replica: container.Replica,
		Container: container.Name, Stream: "combined", Message: registry.Redact(message),
	}
}

func followNodeLogs(ctx context.Context, containers []platform.ContainerObservation, query platform.LogQuery, out io.Writer, registry *secrets.Registry) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var writeErr error
	count := 0
	emit := func(line platform.LogLine) error {
		body := rpc.EventBody{Kind: "log", Log: &rpc.LogLine{
			Process: line.Process, Replica: line.Replica, Container: line.Container,
			Stream: line.Stream, Message: line.Message,
		}}
		raw, err := rpc.MarshalEvent(body)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		if writeErr != nil {
			return writeErr
		}
		if _, err := out.Write(raw); err != nil {
			writeErr = err
			return err
		}
		count++
		return nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(containers))
	for _, container := range containers {
		container := container
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := followContainerLogs(ctx, container, query, registry, emit); err != nil {
				errs <- err
				cancel()
			}
		}()
	}
	wg.Wait()
	close(errs)
	var firstErr error
	for err := range errs {
		if err == nil || errors.Is(err, context.Canceled) {
			continue
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return count, firstErr
	}
	if ctx.Err() != nil {
		return count, ctx.Err()
	}
	return count, nil
}

func followContainerLogs(ctx context.Context, container platform.ContainerObservation, query platform.LogQuery, registry *secrets.Registry, emit func(platform.LogLine) error) error {
	command := exec.CommandContext(ctx, "docker", dockerLogsArgs(query, container.ContainerID)[1:]...) //nolint:gosec // G204: fixed argv, no shell.
	reader, writer := io.Pipe()
	command.Stdout, command.Stderr = writer, writer
	if err := command.Start(); err != nil {
		_ = writer.Close()
		return contract.NewFault(contract.Unavailable, "the node could not start docker logs", boundedDockerOutput(err.Error()))
	}
	go func() {
		err := command.Wait()
		_ = writer.CloseWithError(err)
	}()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		if err := emit(containerLogLine(container, registry, scanner.Text())); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return contract.NewFault(contract.Unavailable, "container log stream ended unexpectedly", boundedDockerOutput(err.Error()))
	}
	return nil
}
