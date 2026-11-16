package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/viewer"
	"github.com/spf13/cobra"
)

// logsSpecs reads container logs over the operator channel as required by D25.
// It never depends on node telemetry.
func (a *app) logsSpecs() []spec {
	meta := followingMeta(operationMeta("Read serving, selected deployment, or captured candidate logs over the operator channel",
		"eregion logs api production --since 15m --tail 200"))
	meta.WhenToUse = []string{
		"Read Docker container logs from the environment's serving deployment through SSH inside WireGuard.",
		"Use --deployment for available live containers, or --operation for bounded captured candidate diagnostics after cleanup. Selectors must belong to this app and environment.",
		"Use --follow to stream new lines; Ctrl-C detaches without cancelling or changing the deployment.",
		"Captured operation logs are snapshots; use operations follow ID --output ndjson for in-progress operation events.",
	}
	meta.Examples = []string{
		"eregion logs api production --since 15m --tail 200",
		"eregion logs api production --process worker --since 1h",
		"eregion logs api production --follow --output ndjson",
		"eregion logs api production --operation op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --json",
	}
	meta.Next = []string{"eregion env status <app> <environment> --json", "eregion operations list --app <app> --json"}
	return []spec{{
		use:  "logs <app> <environment>",
		meta: meta,
		args: platformTwoNames(),
		flags: func(cmd *cobra.Command) {
			cmd.Flags().String("operation", "", "Read captured diagnostics for this deployment operation")
			cmd.Flags().String("deployment", "", "Read available live containers of this deployment")
			cmd.Flags().String("process", "", "Only read this process's containers")
			cmd.Flags().Bool("follow", false, "Continue streaming new lines until interrupted")
			cmd.Flags().Duration("since", 10*time.Minute, "Read lines newer than this lookback (maximum 24h)")
			cmd.Flags().Int("tail", 200, "Read at most this many lines per container (1–10000)")
		},
		run: a.logs,
	}}
}

func (a *app) logs(cmd *cobra.Command, args []string) (result, error) {
	appName, environment := args[0], args[1]
	request, err := parseLogsRequest(cmd, appName, environment)
	if err != nil {
		return result{}, err
	}
	config := followConfig(cmd)
	streamJSON, err := logsOutputMode(config, request.follow)
	if err != nil {
		return result{}, err
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	query, err := a.selectLogDeployment(cmd, client, request, appName, environment)
	if err != nil {
		return result{}, err
	}
	out := cmd.OutOrStdout()
	if config != nil {
		out = config.out
	}
	clean := a.followCleaner()
	emit := func(line platform.LogLine) error {
		if streamJSON {
			return writeLogRecord(out, line)
		}
		_, err := fmt.Fprintln(out, clean(platform.FormatLogLine(line)))
		return err
	}
	var lines platform.LogResult
	if request.operation != "" {
		lines, err = candidateOperationLogs(cmd.Context(), client, contract.OperationID(request.operation), query)
	} else {
		lines, err = client.Logs(cmd.Context(), query, emit)
	}
	return finishLogs(cmd, out, lines, request.follow, streamJSON, err, emit, clean)
}

type logsRequest struct {
	follow     bool
	operation  string
	deployment string
	process    string
	query      platform.LogQuery
}

func parseLogsRequest(cmd *cobra.Command, appName, environment string) (logsRequest, error) {
	follow, _ := cmd.Flags().GetBool("follow")
	operation, _ := cmd.Flags().GetString("operation")
	deployment, _ := cmd.Flags().GetString("deployment")
	if operation != "" && deployment != "" {
		return logsRequest{}, contract.NewFault(contract.Usage, "--operation and --deployment are mutually exclusive", "Choose one selector.")
	}
	if operation != "" && follow {
		return logsRequest{}, contract.NewFault(contract.Usage, "--operation cannot follow container logs", "Use eregion operations follow "+operation+" --output ndjson to follow operation events.")
	}
	process, _ := cmd.Flags().GetString("process")
	since, _ := cmd.Flags().GetDuration("since")
	tail, _ := cmd.Flags().GetInt("tail")
	query := platform.LogQuery{
		App: appName, Environment: environment, DeploymentID: platform.DeploymentID(deployment),
		Process: process, Follow: follow, Since: since, Tail: tail,
	}
	if err := query.Validate(); err != nil {
		return logsRequest{}, err
	}
	return logsRequest{follow: follow, operation: operation, deployment: deployment, process: process, query: query}, nil
}

func (a *app) selectLogDeployment(cmd *cobra.Command, client Backend, request logsRequest, appName, environment string) (platform.LogQuery, error) {
	query := request.query
	if request.operation == "" && request.deployment == "" {
		env, err := a.readEnvironment(cmd, appName, environment)
		if err != nil {
			return query, err
		}
		if env.ServingDeploymentID == "" {
			return query, contract.NewFault(contract.PreconditionFailed, "environment has no serving deployment", "Use --operation to inspect captured candidate diagnostics, or deploy a release first.")
		}
		query.DeploymentID = env.ServingDeploymentID
	}
	if request.deployment == "" {
		return query, nil
	}
	raw, err := client.PlatformInventory(cmd.Context(), "deployments", url.Values{"app": {appName}, "environment": {environment}, "deployment": {request.deployment}, "limit": {"1"}})
	if err != nil {
		return query, err
	}
	var page struct {
		Items []platform.Deployment `json:"items"`
	}
	if json.Unmarshal(raw, &page) != nil {
		return query, invalidNodeData("deployment inventory")
	}
	if len(page.Items) != 1 || string(page.Items[0].ID) != request.deployment || page.Items[0].App != appName || page.Items[0].Environment != environment {
		return query, contract.NewFault(contract.NotFound, "deployment not found for this environment", "Choose a deployment belonging to the requested app and environment.")
	}
	for _, instance := range page.Items[0].Instances {
		if request.process == "" || request.process == instance.Process {
			query.ExpectedContainers = append(query.ExpectedContainers, instance.Name)
		}
	}
	return query, nil
}

func finishLogs(
	cmd *cobra.Command,
	out io.Writer,
	lines platform.LogResult,
	follow, streamJSON bool,
	err error,
	emit func(platform.LogLine) error,
	clean viewer.Cleaner,
) (result, error) {
	if err != nil {
		return finishLogsError(cmd, out, lines, follow, streamJSON, err)
	}
	if streamJSON {
		return finishStreamingLogs(cmd, out, lines, follow, emit)
	}
	if follow {
		if lines.Unavailable {
			_, _ = fmt.Fprintln(out, "warning: some selected containers are unavailable or removed")
		}
		return result{data: lines, streamed: true}, nil
	}
	return result{data: lines, plain: plainLogsText(lines, clean)}, nil
}

func finishLogsError(
	cmd *cobra.Command,
	out io.Writer,
	lines platform.LogResult,
	follow, streamJSON bool,
	err error,
) (result, error) {
	if follow && (errors.Is(err, context.Canceled) || cmd.Context().Err() != nil) {
		if streamJSON {
			_ = writeLogResult(out, contract.Failure(cmd.CommandPath(), detachedLogsFault(lines.Count)))
		}
		return result{data: lines, streamed: true}, detachedLogsFault(lines.Count)
	}
	return result{}, err
}

func finishStreamingLogs(
	cmd *cobra.Command,
	out io.Writer,
	lines platform.LogResult,
	follow bool,
	emit func(platform.LogLine) error,
) (result, error) {
	if !follow {
		for _, line := range lines.Lines {
			if err := emit(line); err != nil {
				return result{}, err
			}
		}
	}
	if err := writeLogResult(out, contract.Success(cmd.CommandPath(), lines, nil)); err != nil {
		return result{}, contract.NewFault(contract.Internal, "could not write command output", "Check the output stream and retry.")
	}
	return result{data: lines, streamed: true}, nil
}

func plainLogsText(lines platform.LogResult, clean viewer.Cleaner) string {
	var plain strings.Builder
	if len(lines.Lines) == 0 {
		if lines.Unavailable {
			plain.WriteString("Container logs unavailable (containers may have been removed or capture was incomplete).\n")
		} else {
			plain.WriteString("No container log lines matched.\n")
		}
	}
	for _, line := range lines.Lines {
		plain.WriteString(clean(platform.FormatLogLine(line)) + "\n")
	}
	if lines.Truncated {
		plain.WriteString("warning: output was truncated\n")
	}
	if lines.Unavailable && len(lines.Lines) > 0 {
		plain.WriteString("warning: some selected container logs are unavailable\n")
	}
	if lines.DiagnosticStatus != "" {
		plain.WriteString("Candidate capture: " + lines.DiagnosticStatus + "\n")
	}
	return plain.String()
}

func candidateOperationLogs(ctx context.Context, client Backend, id contract.OperationID, query platform.LogQuery) (platform.LogResult, error) {
	op, result, err := loadCandidateOperation(ctx, client, id, query)
	if err != nil {
		return result, err
	}
	lines, result, err := collectCandidateLines(ctx, client, op, query, result)
	if err != nil {
		return result, err
	}
	result.Lines = tailLogLines(lines, query.Tail)
	result.Count = len(result.Lines)
	return result, nil
}

func loadCandidateOperation(ctx context.Context, client Backend, id contract.OperationID, query platform.LogQuery) (contract.Operation, platform.LogResult, error) {
	result := platform.LogResult{Unavailable: true, DiagnosticStatus: "unavailable"}
	if err := id.Validate(); err != nil {
		return contract.Operation{}, result, err
	}
	raw, err := client.Get(ctx, id)
	if err != nil {
		return contract.Operation{}, result, err
	}
	var op contract.Operation
	if json.Unmarshal(raw, &op) != nil || op.Validate() != nil {
		return contract.Operation{}, result, invalidNodeData("operation")
	}
	if op.ID != id || op.Target != "env/"+query.App+"/"+query.Environment || (op.Kind != platform.KindDeploy && op.Kind != platform.KindRollback && op.Kind != platform.KindRestart && op.Kind != platform.KindPromote) {
		return contract.Operation{}, result, contract.NewFault(contract.NotFound, "deployment operation not found for this environment", "Choose a deployment operation belonging to the requested app and environment.")
	}
	return op, result, nil
}

func collectCandidateLines(
	ctx context.Context,
	client Backend,
	op contract.Operation,
	query platform.LogQuery,
	result platform.LogResult,
) ([]platform.LogLine, platform.LogResult, error) {
	var lines []platform.LogLine
	cutoff := time.Now().Add(-query.Since)
	for cursor := uint64(0); cursor < op.LastSequence; {
		raw, err := client.Events(ctx, contract.Cursor{OperationID: op.ID, After: cursor}, 200)
		if err != nil {
			return nil, result, err
		}
		var page operations.EventPage
		if json.Unmarshal(raw, &page) != nil || page.Cursor.OperationID != op.ID || page.Cursor.After <= cursor || len(page.Events) == 0 {
			return nil, result, invalidNodeData("operation event page")
		}
		for _, event := range page.Events {
			if event.Sequence > op.LastSequence {
				break
			}
			appendCandidateEvent(event, query, cutoff, &result, &lines)
		}
		cursor = page.Cursor.After
	}
	return lines, result, nil
}

func appendCandidateEvent(event contract.Event, query platform.LogQuery, cutoff time.Time, result *platform.LogResult, lines *[]platform.LogLine) {
	d := event.CandidateDiagnostic
	if event.Stage != platform.CandidateDiagnosticStage || d == nil {
		return
	}
	if d.Status != "captured" {
		result.DiagnosticStatus = d.Status
		result.Truncated = strings.Contains(d.Status, "truncated")
		result.Unavailable = strings.Contains(d.Status, "unavailable")
		return
	}
	if (query.Process != "" && query.Process != d.Process) || d.Timestamp.Before(cutoff) {
		return
	}
	at := d.Timestamp
	*lines = append(*lines, platform.LogLine{App: query.App, Environment: query.Environment, Process: d.Process, Replica: d.Replica, Container: d.Container, Stream: "combined", Timestamp: &at, Message: event.Message})
}

func tailLogLines(lines []platform.LogLine, tail int) []platform.LogLine {
	counts := map[string]int{}
	tailed := make([]platform.LogLine, 0, len(lines))
	for i := len(lines) - 1; i >= 0; i-- {
		key := lines[i].Container
		counts[key]++
		if counts[key] <= tail {
			tailed = append(tailed, lines[i])
		}
	}
	for i, j := 0, len(tailed)-1; i < j; i, j = i+1, j-1 {
		tailed[i], tailed[j] = tailed[j], tailed[i]
	}
	return tailed
}

// logsOutputMode selects the logs wire format. Only --output ndjson streams
// records; --json --follow is refused because one JSON envelope cannot also
// carry an open stream, and --json without --follow is one final envelope.
func logsOutputMode(config *followIO, follow bool) (bool, error) {
	mode := string(output.Plain)
	if config != nil {
		mode = config.mode
	}
	if follow && mode == string(output.JSON) {
		return false, contract.NewFault(contract.Usage, "--json cannot follow a log stream", "Use --output ndjson with --follow, or omit --follow for one JSON result.")
	}
	return mode == "ndjson", nil
}

func detachedLogsFault(count int) error {
	return &contract.Fault{
		Code:    contract.Cancelled,
		Message: "log stream detached",
		Hint:    "The deployment was not changed; run the command again to read logs.",
		Details: map[string]any{"lines": count},
	}
}

type logNDJSONRecord struct {
	Schema string             `json:"schema"`
	Type   string             `json:"type"`
	Log    *platform.LogLine  `json:"log,omitempty"`
	Result *contract.Envelope `json:"result,omitempty"`
}

func writeLogRecord(out io.Writer, line platform.LogLine) error {
	return writeLogNDJSON(out, logNDJSONRecord{Schema: contract.Schema, Type: "log", Log: &line})
}

func writeLogResult(out io.Writer, envelope contract.Envelope) error {
	return writeLogNDJSON(out, logNDJSONRecord{Schema: contract.Schema, Type: "result", Result: &envelope})
}

func writeLogNDJSON(out io.Writer, record logNDJSONRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return contract.NewFault(contract.Internal, "could not encode log output", "Report the command and binary version.")
	}
	raw = append(raw, '\n')
	n, err := out.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	return err
}
