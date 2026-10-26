package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/telemetry"
	"github.com/spf13/cobra"
)

var telemetryEnvironment = []string{
	"EREGION_TELEMETRY_URL",
	"EREGION_TELEMETRY_LOGS_URL",
	"EREGION_TELEMETRY_METRICS_URL",
	"EREGION_TELEMETRY_LOGS_TOKEN_FILE",
	"EREGION_TELEMETRY_METRICS_TOKEN_FILE",
	"EREGION_TELEMETRY_LOGS_TOKEN",
	"EREGION_TELEMETRY_METRICS_TOKEN",
}

func addTelemetrySourceFlags(cmd *cobra.Command) {
	cmd.Flags().String("telemetry-url", "", "Telemetry node origin, such as http://10.77.0.10; ports 8427/9427 are derived")
	cmd.Flags().String("logs-url", "", "Logs read-gateway origin (defaults to EREGION_TELEMETRY_LOGS_URL)")
	cmd.Flags().String("metrics-url", "", "Metrics read-gateway origin (defaults to EREGION_TELEMETRY_METRICS_URL)")
	cmd.Flags().String("logs-token-file", "", "0600 file containing the logs read token (defaults to EREGION_TELEMETRY_LOGS_TOKEN_FILE)")
	cmd.Flags().String("metrics-token-file", "", "0600 file containing the metrics read token (defaults to EREGION_TELEMETRY_METRICS_TOKEN_FILE)")
}

// logsSpecs replaces the telemetry-backed query with the operator channel read
// required by D25. The workspace graph still uses the telemetry data source for
// metrics; logs never depend on it.
func (a *app) logsSpecs() []spec {
	meta := operationMeta("Read an environment's serving container logs over the operator channel",
		"eregion logs api production --since 15m --tail 200")
	meta.WhenToUse = []string{
		"Read Docker container logs from the environment's serving deployment through SSH inside WireGuard.",
		"Use --follow to stream new lines; Ctrl-C detaches without cancelling or changing the deployment.",
	}
	meta.Examples = []string{
		"eregion logs api production --since 15m --tail 200",
		"eregion logs api production --process worker --since 1h",
		"eregion logs api production --follow --output ndjson",
	}
	meta.Next = []string{"eregion env status <app> <environment> --json", "eregion operations list --app <app> --json"}
	return []spec{{
		use:  "logs <app> <environment>",
		meta: meta,
		args: platformTwoNames("app", "environment"),
		flags: func(cmd *cobra.Command) {
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
	env, _, err := a.readEnvironment(cmd, appName, environment)
	if err != nil {
		return result{}, err
	}
	if env.ServingDeploymentID == "" {
		return result{}, contract.NewFault(contract.PreconditionFailed, "environment has no serving deployment",
			"Deploy a release before reading container logs; the previous logs belong to a formation that is no longer serving.")
	}
	process, _ := cmd.Flags().GetString("process")
	follow, _ := cmd.Flags().GetBool("follow")
	since, _ := cmd.Flags().GetDuration("since")
	tail, _ := cmd.Flags().GetInt("tail")
	query := platform.LogQuery{
		App: appName, Environment: environment, DeploymentID: env.ServingDeploymentID,
		Process: process, Follow: follow, Since: since, Tail: tail,
	}
	if err := query.Validate(); err != nil {
		return result{}, err
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	config := followConfig(cmd)
	out := cmd.OutOrStdout()
	if config != nil {
		out = config.out
	}
	streamJSON := config != nil && (config.mode == "ndjson" || (follow && config.mode == string(output.JSON)))
	clean := a.followCleaner()
	emit := func(line platform.LogLine) error {
		if streamJSON {
			return writeLogRecord(out, line)
		}
		_, err := fmt.Fprintln(out, clean(platform.FormatLogLine(line)))
		return err
	}
	lines, err := client.Logs(cmd.Context(), query, emit)
	if err != nil {
		if follow && (errors.Is(err, context.Canceled) || cmd.Context().Err() != nil) {
			if streamJSON {
				_ = writeLogResult(out, contract.Failure(cmd.CommandPath(), detachedLogsFault(lines.Count)))
			}
			return result{data: lines, streamed: true}, detachedLogsFault(lines.Count)
		}
		return result{}, err
	}
	if streamJSON {
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
	if follow {
		return result{data: lines, streamed: true}, nil
	}
	var plain strings.Builder
	if len(lines.Lines) == 0 {
		plain.WriteString("No container log lines matched.\n")
	}
	for _, line := range lines.Lines {
		plain.WriteString(clean(platform.FormatLogLine(line)) + "\n")
	}
	if lines.Truncated {
		plain.WriteString("warning: output was truncated\n")
	}
	return result{data: lines, plain: plain.String()}, nil
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

// telemetryDataSource remains for the workspace metrics graph. `logs` no longer
// calls it.
func (a *app) telemetryDataSource(cmd *cobra.Command) (telemetry.DataSource, error) {
	base := flagOrEnvironment(cmd, "telemetry-url", "EREGION_TELEMETRY_URL")
	logsURL := flagOrEnvironment(cmd, "logs-url", "EREGION_TELEMETRY_LOGS_URL")
	metricsURL := flagOrEnvironment(cmd, "metrics-url", "EREGION_TELEMETRY_METRICS_URL")
	logsTokenFile := flagOrEnvironment(cmd, "logs-token-file", "EREGION_TELEMETRY_LOGS_TOKEN_FILE")
	metricsTokenFile := flagOrEnvironment(cmd, "metrics-token-file", "EREGION_TELEMETRY_METRICS_TOKEN_FILE")
	logsToken := os.Getenv("EREGION_TELEMETRY_LOGS_TOKEN")
	metricsToken := os.Getenv("EREGION_TELEMETRY_METRICS_TOKEN")
	a.secrets.Add(logsToken, metricsToken)
	if logsURL == "" && metricsURL == "" && base == "" {
		return telemetry.Unconfigured{}, nil
	}
	if base != "" {
		derivedLogs, derivedMetrics, err := telemetryNodeEndpoints(base)
		if err != nil {
			return nil, err
		}
		if logsURL == "" {
			logsURL = derivedLogs
		}
		if metricsURL == "" {
			metricsURL = derivedMetrics
		}
	}
	if logsURL == "" || metricsURL == "" {
		return nil, contract.NewFault(contract.InputRequired, "both telemetry read endpoints are required", "Set --logs-url and --metrics-url, or one --telemetry-url whose standard ports are derived.")
	}
	return telemetry.NewHTTPDataSource(telemetry.HTTPConfig{
		MetricsURL: metricsURL, LogsURL: logsURL,
		MetricsTokenFile: metricsTokenFile, LogsTokenFile: logsTokenFile,
		MetricsToken: metricsToken, LogsToken: logsToken,
	})
}

func flagOrEnvironment(cmd *cobra.Command, flag, environment string) string {
	value, _ := cmd.Flags().GetString(flag)
	if value == "" {
		value = os.Getenv(environment)
	}
	return strings.TrimSpace(value)
}

func telemetryNodeEndpoints(base string) (string, string, error) {
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", "", contract.NewFault(contract.ValidationFailed, "invalid telemetry node URL", "Use an origin such as http://10.77.0.10 without a path, query, or credentials.")
	}
	if parsed.Port() != "" {
		return "", "", contract.NewFault(contract.ValidationFailed, "telemetry node URL must not include a port", "Use the origin only; read ports 8427 and 9427 are derived, or set --logs-url/--metrics-url explicitly.")
	}
	host := parsed.Hostname()
	return parsed.Scheme + "://" + net.JoinHostPort(host, "9427"),
		parsed.Scheme + "://" + net.JoinHostPort(host, "8427"), nil
}
