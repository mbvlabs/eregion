package telemetry

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// HTTPConfig points at a telemetry node's private read endpoints. The endpoints
// are private-network services without public credentials; tokens used for OTLP
// ingest are not reused for queries.
type HTTPConfig struct {
	MetricsURL string
	LogsURL    string
	// MetricsTokenFile and LogsTokenFile are 0600 files containing the bearer
	// token enforced by the read gateway. The token is read per request and is
	// never stored in the query or result.
	MetricsTokenFile string
	LogsTokenFile    string
	// MetricsToken and LogsToken are optional in-process credentials from the
	// environment. They are used only when no token file is configured and are
	// never written to output.
	MetricsToken string
	LogsToken    string
	HTTPClient   *http.Client
	// StaleAfter is measured against the query window end (or now when the
	// window still includes the present).
	StaleAfter time.Duration
	MaxBody    int64
	Now        func() time.Time
}

type HTTPDataSource struct {
	metricsURL       string
	logsURL          string
	metricsTokenFile string
	logsTokenFile    string
	metricsToken     string
	logsToken        string
	http             *http.Client
	staleAfter       time.Duration
	maxBody          int64
	now              func() time.Time
}

func NewHTTPDataSource(cfg HTTPConfig) (*HTTPDataSource, error) {
	metricsURL, err := validateReadURL(cfg.MetricsURL)
	if err != nil {
		return nil, err
	}
	logsURL, err := validateReadURL(cfg.LogsURL)
	if err != nil {
		return nil, err
	}
	staleAfter := cfg.StaleAfter
	if staleAfter == 0 {
		staleAfter = 5 * time.Minute
	}
	if staleAfter < 30*time.Second || staleAfter > 24*time.Hour {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid telemetry staleness threshold", "Use 30 seconds–24 hours.")
	}
	maxBody := cfg.MaxBody
	if maxBody == 0 {
		maxBody = 8 << 20
	}
	if maxBody < 4096 || maxBody > 32<<20 {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid telemetry response bound", "Use 4 KiB–32 MiB.")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	// Never let an injected client follow a redirect to a different origin.
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client = &clientCopy
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &HTTPDataSource{
		metricsURL: metricsURL, logsURL: logsURL,
		metricsTokenFile: cfg.MetricsTokenFile, logsTokenFile: cfg.LogsTokenFile,
		metricsToken: cfg.MetricsToken, logsToken: cfg.LogsToken,
		http: client, staleAfter: staleAfter, maxBody: maxBody, now: now,
	}, nil
}

func validateReadURL(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return "", contract.NewFault(contract.ValidationFailed, "invalid telemetry data-source URL", "Use an origin URL without credentials, query, or path.")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !isPrivateOrLoopbackHost(parsed.Hostname()) {
			return "", contract.NewFault(contract.ValidationFailed, "plain HTTP telemetry endpoints must stay on a private network", "Use HTTPS for a public endpoint, or a private RFC 1918/ULA address for the telemetry node.")
		}
	default:
		return "", contract.NewFault(contract.ValidationFailed, "invalid telemetry data-source URL scheme", "Use HTTPS, or HTTP to a private telemetry node address.")
	}
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

func isPrivateOrLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func (d *HTTPDataSource) Logs(ctx context.Context, query LogQuery) (LogResult, error) {
	result := LogResult{State: StateError, Source: query.Source, Unit: "entries", Window: query.Window, Entries: []LogEntry{}, FetchedAt: d.now().UTC()}
	if err := query.Window.Validate(); err != nil {
		return result, err
	}
	if strings.TrimSpace(query.Source) == "" || len(query.Source) > 4096 || len(query.Filter) > 4096 {
		return result, contract.NewFault(contract.ValidationFailed, "a bounded log source is required", "Provide a LogsQL stream selector or filter of at most 4096 bytes.")
	}
	limit := query.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return result, contract.NewFault(contract.ValidationFailed, "invalid log limit", "Use 1–1000 entries.")
	}
	logSQL := query.Source
	if query.Filter != "" {
		logSQL += " " + query.Filter
	}
	params := url.Values{}
	params.Set("query", logSQL)
	params.Set("start", query.Window.Start.UTC().Format(time.RFC3339Nano))
	params.Set("end", query.Window.End.UTC().Format(time.RFC3339Nano))
	params.Set("limit", strconv.Itoa(limit))
	endpoint := d.logsURL + "/select/logsql/query?" + params.Encode()
	response, err := d.get(ctx, endpoint, d.logsTokenFile, d.logsToken)
	if err != nil {
		result.Message = "The telemetry log backend is unavailable."
		return result, err
	}
	defer response.Body.Close()
	rawBody, err := io.ReadAll(io.LimitReader(response.Body, d.maxBody+1))
	if err != nil || int64(len(rawBody)) > d.maxBody {
		result.Message = "The telemetry log backend returned an unreadable or oversized response."
		return result, contract.NewFault(contract.Unavailable, "telemetry log response was invalid", "Check backend compatibility; do not render this as empty data.")
	}
	entries := make([]LogEntry, 0)
	scanner := bufio.NewScanner(bytes.NewReader(rawBody))
	scanner.Buffer(make([]byte, 0, 64*1024), int(d.maxBody))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			result.Message = "The telemetry log backend returned an invalid response."
			return result, contract.NewFault(contract.Unavailable, "telemetry log response was invalid", "Check backend compatibility; do not render this as empty data.")
		}
		entry, err := logEntry(raw, query.Source)
		if err != nil {
			result.Message = "The telemetry log backend returned an invalid timestamp."
			return result, contract.NewFault(contract.Unavailable, "telemetry log response was invalid", "Check backend compatibility; do not render this as empty data.")
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		result.Message = "The telemetry log backend returned an invalid response."
		return result, contract.NewFault(contract.Unavailable, "telemetry log response was invalid", "Check backend compatibility; do not render this as empty data.")
	}
	result.Entries = entries
	if len(entries) == 0 {
		result.State = StateNoData
		result.Message = "No log entries matched the source and time window."
		return result, nil
	}
	result.ObservedAt = entries[0].Timestamp
	for _, entry := range entries {
		if entry.Timestamp.After(result.ObservedAt) {
			result.ObservedAt = entry.Timestamp
		}
	}
	result.State = d.freshness(query.Window, result.ObservedAt)
	if result.State == StateStale {
		result.Message = "Logs exist but the newest matching entry is stale for this window."
	}
	return result, nil
}

func logEntry(raw map[string]any, fallbackSource string) (LogEntry, error) {
	entry := LogEntry{Source: fallbackSource, Fields: map[string]string{}}
	timeText, _ := raw["_time"].(string)
	if timeText == "" {
		return entry, fmt.Errorf("missing _time")
	}
	timestamp, err := time.Parse(time.RFC3339Nano, timeText)
	if err != nil {
		return entry, err
	}
	entry.Timestamp = timestamp.UTC()
	if message, ok := raw["_msg"].(string); ok {
		entry.Message = message
	}
	if stream, ok := raw["_stream"].(string); ok && stream != "" {
		entry.Source = stream
	}
	for key, value := range raw {
		switch key {
		case "_time", "_msg", "_stream":
			continue
		}
		switch typed := value.(type) {
		case string:
			entry.Fields[key] = typed
		case nil:
		default:
			encoded, _ := json.Marshal(typed)
			entry.Fields[key] = string(encoded)
		}
	}
	if len(entry.Fields) == 0 {
		entry.Fields = nil
	}
	return entry, nil
}

func (d *HTTPDataSource) Metrics(ctx context.Context, query MetricQuery) (MetricResult, error) {
	result := MetricResult{State: StateError, Source: query.Source, Unit: query.Unit, Window: query.Window, Series: []MetricSeries{}, FetchedAt: d.now().UTC()}
	if err := query.Window.Validate(); err != nil {
		return result, err
	}
	if strings.TrimSpace(query.Source) == "" || len(query.Source) > 256 {
		return result, contract.NewFault(contract.ValidationFailed, "a metric source is required", "Name the logical producer; the backend does not infer it.")
	}
	if strings.TrimSpace(query.Unit) == "" || len(query.Unit) > 64 {
		return result, contract.NewFault(contract.ValidationFailed, "a metric unit is required", "Provide an explicit unit such as percent, bytes, or requests/second.")
	}
	if strings.TrimSpace(query.Expression) == "" || len(query.Expression) > 4096 {
		return result, contract.NewFault(contract.ValidationFailed, "a bounded metric expression is required", "Provide PromQL/MetricsQL of at most 4096 bytes.")
	}
	step := query.Step
	if step == 0 {
		step = defaultStep(query.Window)
	}
	if step < time.Second || step > 24*time.Hour {
		return result, contract.NewFault(contract.ValidationFailed, "invalid metric step", "Use 1 second–24 hours.")
	}
	params := url.Values{}
	params.Set("query", query.Expression)
	params.Set("start", strconv.FormatInt(query.Window.Start.Unix(), 10))
	params.Set("end", strconv.FormatInt(query.Window.End.Unix(), 10))
	params.Set("step", strconv.FormatInt(int64(step/time.Second), 10))
	endpoint := d.metricsURL + "/api/v1/query_range?" + params.Encode()
	response, err := d.get(ctx, endpoint, d.metricsTokenFile, d.metricsToken)
	if err != nil {
		result.Message = "The telemetry metric backend is unavailable."
		return result, err
	}
	defer response.Body.Close()
	var payload struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	rawBody, err := io.ReadAll(io.LimitReader(response.Body, d.maxBody+1))
	if err != nil || int64(len(rawBody)) > d.maxBody {
		result.Message = "The telemetry metric backend returned an unreadable or oversized response."
		return result, contract.NewFault(contract.Unavailable, "telemetry metric response was invalid", "Check backend compatibility; do not render this as zero values.")
	}
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		result.Message = "The telemetry metric backend returned an invalid response."
		return result, contract.NewFault(contract.Unavailable, "telemetry metric response was invalid", "Check backend compatibility; do not render this as zero values.")
	}
	if payload.Status != "success" || payload.Data.ResultType != "matrix" {
		result.Message = "The telemetry metric backend rejected the query."
		return result, contract.NewFault(contract.Unavailable, "telemetry metric query failed", "Inspect the expression, source, and backend health.")
	}
	if len(payload.Data.Result) == 0 {
		result.State = StateNoData
		result.Message = "No metric samples matched the expression and time window."
		return result, nil
	}
	var observed time.Time
	for _, rawSeries := range payload.Data.Result {
		series := MetricSeries{Labels: rawSeries.Metric, Samples: make([]MetricSample, 0, len(rawSeries.Values))}
		for _, pair := range rawSeries.Values {
			if len(pair) != 2 {
				return result, contract.NewFault(contract.Unavailable, "telemetry metric response had an invalid sample", "Retry later; do not render partial data as complete.")
			}
			seconds, ok := number(pair[0])
			if !ok {
				return result, contract.NewFault(contract.Unavailable, "telemetry metric response had an invalid timestamp", "Retry later; do not render partial data as complete.")
			}
			valueText, ok := pair[1].(string)
			if !ok {
				return result, contract.NewFault(contract.Unavailable, "telemetry metric response had an invalid value", "Retry later; do not render partial data as complete.")
			}
			value, err := strconv.ParseFloat(valueText, 64)
			if err != nil {
				return result, contract.NewFault(contract.Unavailable, "telemetry metric response had an invalid value", "Retry later; do not render partial data as complete.")
			}
			timestamp := time.Unix(int64(seconds), int64(math.Round((seconds-math.Floor(seconds))*1e9))).UTC()
			series.Samples = append(series.Samples, MetricSample{Timestamp: timestamp, Value: value})
			if timestamp.After(observed) {
				observed = timestamp
			}
		}
		if len(series.Samples) > 0 {
			result.Series = append(result.Series, series)
		}
	}
	if len(result.Series) == 0 {
		result.State = StateNoData
		result.Message = "No metric samples matched the expression and time window."
		return result, nil
	}
	result.ObservedAt = observed
	result.State = d.freshness(query.Window, observed)
	if result.State == StateStale {
		result.Message = "Metric samples exist but the newest sample is stale for this window."
	}
	return result, nil
}

func defaultStep(window Window) time.Duration {
	span := window.End.Sub(window.Start)
	step := span / 250
	if step < time.Second {
		step = time.Second
	}
	if step > 5*time.Minute {
		step = 5 * time.Minute
	}
	return step
}

func number(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case string:
		parsed, err := strconv.ParseFloat(typed, 64)
		return parsed, err == nil
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func (d *HTTPDataSource) freshness(window Window, observed time.Time) State {
	if observed.IsZero() {
		return StateNoData
	}
	reference := window.End
	if now := d.now(); now.Before(reference) {
		reference = now
	}
	if observed.Before(reference.Add(-d.staleAfter)) {
		return StateStale
	}
	return StateOK
}

func (d *HTTPDataSource) get(ctx context.Context, endpoint, tokenFile, directToken string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, contract.NewFault(contract.Internal, "could not construct telemetry query", "Check the configured read endpoint.")
	}
	request.Header.Set("Accept", "application/json")
	token := strings.TrimSpace(directToken)
	if tokenFile != "" {
		raw, err := readSecretFile(tokenFile, 512)
		if err != nil {
			return nil, err
		}
		token = strings.TrimRight(string(raw), "\r\n")
		if token == "" {
			return nil, contract.NewFault(contract.PreconditionFailed, "telemetry read-token file is empty", "Write the gateway bearer token to the referenced 0600 file.")
		}
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := d.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, contract.NewFault(contract.Unavailable, "telemetry backend is unreachable", "Check the telemetry node and private network; operation history remains available.")
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		response.Body.Close()
		return nil, contract.NewFault(contract.Forbidden, "telemetry read was rejected", "Check the read endpoint and its private-network policy.")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		response.Body.Close()
		return nil, contract.NewFault(contract.Unavailable, "telemetry backend returned an error", "Check backend health; do not render this as no data.")
	}
	return response, nil
}

var _ DataSource = (*HTTPDataSource)(nil)
