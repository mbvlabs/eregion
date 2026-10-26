package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/output"
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

func telemetryReadMeta(summary, example, next string) CommandMeta {
	meta := readMeta(summary,
		[]string{summary + ". Reads the private telemetry read gateway with an explicit source, unit, time window, and honest absent/stale/no-data states."},
		[]string{example}, next)
	meta.Remote = true
	meta.Environment = telemetryEnvironment
	meta.ErrorCodes = []contract.Code{
		contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.ValidationFailed,
		contract.NotFound, contract.Forbidden, contract.PreconditionFailed, contract.Unavailable,
		contract.Internal, contract.Cancelled,
	}
	return meta
}

func addTelemetrySourceFlags(cmd *cobra.Command) {
	cmd.Flags().String("telemetry-url", "", "Telemetry node origin, such as http://10.77.0.10; ports 8427/9427 are derived")
	cmd.Flags().String("logs-url", "", "Logs read-gateway origin (defaults to EREGION_TELEMETRY_LOGS_URL)")
	cmd.Flags().String("metrics-url", "", "Metrics read-gateway origin (defaults to EREGION_TELEMETRY_METRICS_URL)")
	cmd.Flags().String("logs-token-file", "", "0600 file containing the logs read token (defaults to EREGION_TELEMETRY_LOGS_TOKEN_FILE)")
	cmd.Flags().String("metrics-token-file", "", "0600 file containing the metrics read token (defaults to EREGION_TELEMETRY_METRICS_TOKEN_FILE)")
}

func addTelemetryWindowFlags(cmd *cobra.Command, since time.Duration) {
	cmd.Flags().Duration("since", since, "Look back this far from --end or now")
	cmd.Flags().String("start", "", "Explicit start timestamp (RFC 3339); conflicts with --since")
	cmd.Flags().String("end", "", "Explicit exclusive end timestamp (RFC 3339); defaults to now")
}

func (a *app) logsSpecs() []spec {
	return []spec{
		{
			use:  "logs",
			meta: telemetryReadMeta("Search and stream application logs from the telemetry node", "eregion logs search --source '*' --json", "eregion logs tail --source '*' --json"),
			run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
			children: []spec{
				{
					use:  "search",
					meta: telemetryReadMeta("Read a bounded log query with source, unit, window, and freshness", "eregion logs search --source '{host=\"app-1\"}' --since 1h --limit 100 --json", "eregion logs tail --source '*' --json"),
					args: platformReadArgs,
					flags: func(cmd *cobra.Command) {
						addTelemetrySourceFlags(cmd)
						addTelemetryWindowFlags(cmd, time.Hour)
						cmd.Flags().String("source", "*", "LogsQL stream selector or source filter")
						cmd.Flags().String("filter", "", "Optional additional LogsQL filter")
						cmd.Flags().Int("limit", 100, "Maximum entries (1–1000)")
					},
					run: a.logsSearch,
				},
				{
					use:  "tail",
					meta: telemetryReadMeta("Stream newly observed log entries until interrupted or --once", "eregion logs tail --source '{host=\"app-1\"}' --json", "eregion logs search --source '*' --json"),
					args: platformReadArgs,
					flags: func(cmd *cobra.Command) {
						addTelemetrySourceFlags(cmd)
						addTelemetryWindowFlags(cmd, 5*time.Minute)
						cmd.Flags().String("source", "*", "LogsQL stream selector or source filter")
						cmd.Flags().String("filter", "", "Optional additional LogsQL filter")
						cmd.Flags().Int("limit", 200, "Maximum entries per poll (1–1000)")
						cmd.Flags().Duration("interval", time.Second, "Poll interval (100ms–1m)")
						cmd.Flags().Bool("once", false, "Read one window and exit instead of following")
						cmd.Flags().Bool("ndjson", false, "Emit NDJSON records even without --output json")
					},
					run: a.logsTail,
				},
			},
		},
	}
}

func (a *app) logsSearch(cmd *cobra.Command, _ []string) (result, error) {
	window, err := telemetryWindow(cmd, time.Hour)
	if err != nil {
		return result{}, err
	}
	source, _ := cmd.Flags().GetString("source")
	filter, _ := cmd.Flags().GetString("filter")
	limit, _ := cmd.Flags().GetInt("limit")
	dataSource, err := a.telemetryDataSource(cmd)
	if err != nil {
		return result{}, err
	}
	query := telemetry.LogQuery{Window: window, Source: source, Filter: filter, Limit: limit}
	logs, err := dataSource.Logs(cmd.Context(), query)
	if err != nil {
		return result{}, err
	}
	return result{data: logs, plain: a.formatLogs(logs)}, nil
}

func (a *app) logsTail(cmd *cobra.Command, _ []string) (result, error) {
	window, err := telemetryWindow(cmd, 5*time.Minute)
	if err != nil {
		return result{}, err
	}
	source, _ := cmd.Flags().GetString("source")
	filter, _ := cmd.Flags().GetString("filter")
	limit, _ := cmd.Flags().GetInt("limit")
	interval, _ := cmd.Flags().GetDuration("interval")
	once, _ := cmd.Flags().GetBool("once")
	forceNDJSON, _ := cmd.Flags().GetBool("ndjson")
	if interval < 100*time.Millisecond || interval > time.Minute {
		return result{}, contract.NewFault(contract.ValidationFailed, "invalid tail interval", "Use 100ms–1m.")
	}
	dataSource, err := a.telemetryDataSource(cmd)
	if err != nil {
		return result{}, err
	}
	config := followConfig(cmd)
	streamJSON := forceNDJSON || (config != nil && (config.mode == string(output.JSON) || config.mode == "ndjson"))
	out := io.Writer(cmd.OutOrStdout())
	if config != nil {
		out = config.out
	}
	clean := a.followCleaner()
	emit := func(entry telemetry.LogEntry) error {
		if streamJSON {
			return writeJSONLog(out, entry, a.secrets.Redact)
		}
		_, err := fmt.Fprintln(out, a.formatLogLine(entry, clean))
		return err
	}

	cursor := window.Start
	seen := make(map[string]bool)
	state := telemetry.State("")
	var entries uint64
	var last telemetry.LogResult
	for {
		end := time.Now().UTC()
		if !end.After(cursor) {
			end = cursor.Add(time.Second)
		}
		query := telemetry.LogQuery{
			Window: telemetry.Window{Start: cursor, End: end},
			Source: source, Filter: filter, Limit: limit,
		}
		current, queryErr := dataSource.Logs(cmd.Context(), query)
		last = current
		if queryErr != nil {
			return a.finishTail(cmd, out, streamJSON, last, entries, queryErr)
		}
		if current.State != state {
			state = current.State
			if !streamJSON {
				if message := stateMessage(current.State, current.Message); message != "" {
					if _, err := fmt.Fprintln(out, clean(message)); err != nil {
						return a.finishTail(cmd, out, streamJSON, last, entries, err)
					}
				}
			}
		}
		for _, raw := range current.Entries {
			entry := cleanLogEntry(raw, a.secrets.Redact)
			key := fmt.Sprintf("%d\x00%s\x00%s", entry.Timestamp.UnixNano(), entry.Source, entry.Message)
			if seen[key] {
				continue
			}
			seen[key] = true
			if len(seen) > 10000 {
				seen = make(map[string]bool)
			}
			if err := emit(entry); err != nil {
				return a.finishTail(cmd, out, streamJSON, last, entries, err)
			}
			entries++
			if entry.Timestamp.After(last.ObservedAt) {
				last.ObservedAt = entry.Timestamp
			}
			if entry.Timestamp.After(cursor) {
				cursor = entry.Timestamp.Add(time.Nanosecond)
			}
		}
		if once {
			return a.finishTail(cmd, out, streamJSON, last, entries, nil)
		}
		select {
		case <-cmd.Context().Done():
			return a.finishTail(cmd, out, streamJSON, last, entries, cmd.Context().Err())
		case <-time.After(interval):
		}
	}
}

func (a *app) finishTail(cmd *cobra.Command, out io.Writer, streamJSON bool, last telemetry.LogResult, entries uint64, err error) (result, error) {
	summary := struct {
		State     telemetry.State  `json:"state"`
		Source    string           `json:"source"`
		Unit      string           `json:"unit"`
		Window    telemetry.Window `json:"window"`
		FetchedAt time.Time        `json:"fetched_at"`
		Observed  time.Time        `json:"observed_at,omitempty"`
		Entries   uint64           `json:"entries"`
		Message   string           `json:"message,omitempty"`
	}{State: last.State, Source: last.Source, Unit: last.Unit, Window: last.Window,
		FetchedAt: last.FetchedAt, Observed: last.ObservedAt, Entries: entries, Message: last.Message}
	if !streamJSON {
		if err != nil {
			return result{}, err
		}
		return result{data: summary, streamed: true}, nil
	}
	var envelope contract.Envelope
	if err != nil {
		envelope = contract.Failure(cmd.CommandPath(), err)
	} else {
		envelope = contract.Success(cmd.CommandPath(), summary, nil)
	}
	if writeErr := writeJSONRecord(out, ndjsonLogRecord{Schema: contract.Schema, Type: "result", Result: &envelope}); writeErr != nil && err == nil {
		err = contract.NewFault(contract.Internal, "could not write command output", "Check the output stream and retry.")
	}
	return result{data: summary, streamed: true}, err
}

func telemetryWindow(cmd *cobra.Command, fallback time.Duration) (telemetry.Window, error) {
	since, _ := cmd.Flags().GetDuration("since")
	startText, _ := cmd.Flags().GetString("start")
	endText, _ := cmd.Flags().GetString("end")
	if cmd.Flags().Changed("since") && startText != "" {
		return telemetry.Window{}, contract.NewFault(contract.Usage, "--since and --start cannot be combined", "Use --start/--end for an absolute window, or --since for a relative one.")
	}
	now := time.Now().UTC()
	var start, end time.Time
	var err error
	if startText != "" {
		start, err = time.Parse(time.RFC3339, startText)
		if err != nil {
			return telemetry.Window{}, contract.NewFault(contract.ValidationFailed, "invalid --start timestamp", "Use an RFC 3339 timestamp such as 2026-10-20T12:00:00Z.")
		}
	}
	if endText != "" {
		end, err = time.Parse(time.RFC3339, endText)
		if err != nil {
			return telemetry.Window{}, contract.NewFault(contract.ValidationFailed, "invalid --end timestamp", "Use an RFC 3339 timestamp such as 2026-10-20T13:00:00Z.")
		}
	}
	if since == 0 {
		since = fallback
	}
	if since <= 0 {
		return telemetry.Window{}, contract.NewFault(contract.ValidationFailed, "invalid --since duration", "Use a positive lookback such as 15m or 24h.")
	}
	if end.IsZero() {
		end = now
	}
	if start.IsZero() {
		start = end.Add(-since)
	}
	window := telemetry.Window{Start: start.UTC(), End: end.UTC()}
	if err := window.Validate(); err != nil {
		return telemetry.Window{}, err
	}
	return window, nil
}

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

func (a *app) formatLogs(logs telemetry.LogResult) string {
	clean := a.followCleaner()
	var b strings.Builder
	fmt.Fprintf(&b, "state: %s  source: %s  unit: %s\n", logs.State, clean(logs.Source), clean(logs.Unit))
	fmt.Fprintf(&b, "window: %s .. %s\n", logs.Window.Start.UTC().Format(time.RFC3339), logs.Window.End.UTC().Format(time.RFC3339))
	if !logs.FetchedAt.IsZero() {
		fmt.Fprintf(&b, "fetched: %s\n", logs.FetchedAt.UTC().Format(time.RFC3339))
	}
	if !logs.ObservedAt.IsZero() {
		fmt.Fprintf(&b, "observed: %s\n", logs.ObservedAt.UTC().Format(time.RFC3339))
	}
	if message := stateMessage(logs.State, logs.Message); message != "" {
		b.WriteString(clean(message) + "\n")
	}
	for _, entry := range logs.Entries {
		b.WriteString(a.formatLogLine(entry, clean) + "\n")
	}
	return b.String()
}

func (a *app) formatLogLine(entry telemetry.LogEntry, clean func(string) string) string {
	parts := []string{entry.Timestamp.UTC().Format(time.RFC3339Nano), clean(entry.Source)}
	if entry.Message != "" {
		parts = append(parts, clean(entry.Message))
	}
	keys := make([]string, 0, len(entry.Fields))
	for key := range entry.Fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts = append(parts, clean(key)+"="+clean(entry.Fields[key]))
	}
	return strings.Join(parts, " ")
}

func stateMessage(state telemetry.State, message string) string {
	switch state {
	case telemetry.StateAbsent:
		if message == "" {
			return "No telemetry node is configured. Operation history remains available."
		}
	case telemetry.StateNoData:
		if message == "" {
			return "No data matched the source and time window."
		}
	case telemetry.StateStale:
		if message == "" {
			return "Data exists but is stale for this window."
		}
	case telemetry.StateError:
		if message == "" {
			return "The telemetry backend could not be queried."
		}
	}
	return message
}

func cleanLogEntry(entry telemetry.LogEntry, redact func(string) string) telemetry.LogEntry {
	clean := entry
	clean.Source = redact(entry.Source)
	clean.Message = redact(entry.Message)
	if entry.Fields != nil {
		clean.Fields = make(map[string]string, len(entry.Fields))
		for key, value := range entry.Fields {
			clean.Fields[redact(key)] = redact(value)
		}
	}
	return clean
}

type ndjsonLogRecord struct {
	Schema string              `json:"schema"`
	Type   string              `json:"type"`
	Log    *telemetry.LogEntry `json:"log,omitempty"`
	Result *contract.Envelope  `json:"result,omitempty"`
}

func writeJSONLog(out io.Writer, entry telemetry.LogEntry, redact func(string) string) error {
	clean := cleanLogEntry(entry, redact)
	return writeJSONRecord(out, ndjsonLogRecord{Schema: contract.Schema, Type: "log", Log: &clean})
}

func writeJSONRecord(out io.Writer, record ndjsonLogRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return contract.NewFault(contract.Internal, "could not encode telemetry output", "Report the command and binary version.")
	}
	raw = append(raw, '\n')
	n, err := out.Write(raw)
	if err == nil && n != len(raw) {
		err = fmt.Errorf("short write")
	}
	return err
}
