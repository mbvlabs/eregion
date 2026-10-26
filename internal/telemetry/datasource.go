// Package telemetry exposes a small read interface for logs and metrics. The
// Docker Compose stack installer is deleted (D5); only the read adapters remain
// for `logs` and the workspace graph. It deliberately does not own operation
// history: durable deployment history stays in PostgreSQL.
package telemetry

import (
	"context"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// State is the honesty contract for every read. A caller must render "no data",
// "stale", and "not configured" differently from a successful empty graph.
type State string

const (
	StateOK     State = "ok"
	StateNoData State = "no_data"
	StateStale  State = "stale"
	StateAbsent State = "absent"
	StateError  State = "error"
)

// Window is an explicit, bounded query interval. End is exclusive.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

func (w Window) Validate() error {
	if w.Start.IsZero() || w.End.IsZero() || !w.End.After(w.Start) {
		return contract.NewFault(contract.ValidationFailed, "invalid telemetry time window", "Provide a start and an end later than the start.")
	}
	if w.End.Sub(w.Start) > 31*24*time.Hour {
		return contract.NewFault(contract.ValidationFailed, "telemetry time window is too large", "Limit one query to 31 days.")
	}
	return nil
}

type LogQuery struct {
	Window Window
	// Source is a LogsQL stream selector or filter, for example
	// {host="app-1"} or *. It is echoed in the result.
	Source string
	// Filter is an optional additional LogsQL filter.
	Filter string
	Limit  int
}

type LogEntry struct {
	Timestamp time.Time         `json:"timestamp"`
	Source    string            `json:"source"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields,omitempty"`
}

type LogResult struct {
	State      State      `json:"state"`
	Source     string     `json:"source"`
	Unit       string     `json:"unit"`
	Window     Window     `json:"window"`
	FetchedAt  time.Time  `json:"fetched_at"`
	ObservedAt time.Time  `json:"observed_at,omitempty"`
	Entries    []LogEntry `json:"entries"`
	Message    string     `json:"message,omitempty"`
}

type MetricQuery struct {
	Window Window
	// Source names the logical producer and is echoed; it is not inferred from
	// the expression.
	Source string
	// Expression is a PromQL/MetricsQL expression.
	Expression string
	// Unit is required. The backend never guesses a unit.
	Unit string
	Step time.Duration
}

type MetricSample struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

type MetricSeries struct {
	Labels  map[string]string `json:"labels"`
	Samples []MetricSample    `json:"samples"`
}

type MetricResult struct {
	State      State          `json:"state"`
	Source     string         `json:"source"`
	Unit       string         `json:"unit"`
	Window     Window         `json:"window"`
	FetchedAt  time.Time      `json:"fetched_at"`
	ObservedAt time.Time      `json:"observed_at,omitempty"`
	Series     []MetricSeries `json:"series"`
	Message    string         `json:"message,omitempty"`
}

// DataSource is the read boundary consumed by a terminal viewer or API adapter.
// Implementations return StateError together with an error for transport,
// authentication, and protocol failures. StateAbsent is not an error: it means
// no telemetry node or external backend is configured.
type DataSource interface {
	Logs(context.Context, LogQuery) (LogResult, error)
	Metrics(context.Context, MetricQuery) (MetricResult, error)
}

// Unconfigured is the explicit data source used when no telemetry node exists.
// It never fabricates zero values.
type Unconfigured struct{}

func (Unconfigured) Logs(_ context.Context, query LogQuery) (LogResult, error) {
	return LogResult{
		State: StateAbsent, Source: query.Source, Unit: "entries", Window: query.Window,
		Entries: []LogEntry{},
		Message: "No telemetry node is configured. Operation history remains available.",
	}, nil
}

func (Unconfigured) Metrics(_ context.Context, query MetricQuery) (MetricResult, error) {
	unit := query.Unit
	if unit == "" {
		unit = "unknown"
	}
	return MetricResult{
		State: StateAbsent, Source: query.Source, Unit: unit, Window: query.Window,
		Series:  []MetricSeries{},
		Message: "No telemetry node is configured. Operation history remains available.",
	}, nil
}

var _ DataSource = Unconfigured{}

func validState(state State) bool {
	switch state {
	case StateOK, StateNoData, StateStale, StateAbsent, StateError:
		return true
	}
	return false
}
