package viewer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// Plain writes append-only, colour-free lines. It has no cursor motion,
// spinner, or hidden state, and remains useful when redirected to a file.
type Plain struct {
	Out   io.Writer
	Err   io.Writer
	Clean Cleaner

	lastObservation ObservationState
}

func NewPlain(out, errOut io.Writer, clean Cleaner) *Plain {
	if clean == nil {
		clean = DefaultClean
	}
	return &Plain{Out: out, Err: errOut, Clean: clean}
}

func (p *Plain) Observe(observation Observation) error {
	switch observation.Kind {
	case ObservationEvents:
		for _, event := range observation.Events {
			if err := p.line(formatEvent(event, p.clean())); err != nil {
				return err
			}
		}
	case ObservationSnapshot, ObservationFailed, ObservationTick:
		if observation.Observation != p.lastObservation && observation.Observation != Connecting {
			if line := p.observationLine(observation); line != "" {
				if err := p.line(line); err != nil {
					return err
				}
			}
		}
		p.lastObservation = observation.Observation
	}
	return nil
}

func (p *Plain) clean() Cleaner {
	if p.Clean == nil {
		return DefaultClean
	}
	return p.Clean
}

func (p *Plain) observationLine(observation Observation) string {
	age := FormatDuration(observation.Age)
	switch observation.Observation {
	case Reconnecting:
		return p.clean()("reconnecting: last known " + stateWord(observation.State) + " " + age + " ago")
	case Stale:
		return p.clean()("stale: last known " + stateWord(observation.State) + " " + age + " ago")
	case Live:
		if p.lastObservation == Reconnecting || p.lastObservation == Stale {
			return p.clean()("live: reconnected, operation " + stateWord(observation.State))
		}
	case Closed:
		return ""
	}
	return ""
}

func stateWord(state contract.State) string {
	if state == "" {
		return "unknown"
	}
	return string(state)
}

func formatEvent(event contract.Event, clean Cleaner) string {
	at := event.At
	if at.IsZero() {
		at = time.Now()
	}
	parts := []string{
		at.UTC().Format(time.RFC3339),
		fmt.Sprintf("%d", event.Sequence),
		clean(event.Kind),
	}
	if event.Stage != "" {
		parts = append(parts, clean(event.Stage))
	}
	line := strings.Join(parts, " ")
	if event.State != "" {
		line += " " + clean(string(event.State))
	}
	if event.Progress != nil {
		line += fmt.Sprintf(" %d/%d %s", event.Progress.Current, event.Progress.Total, clean(event.Progress.Unit))
	}
	if event.Message != "" {
		line += " " + clean(event.Message)
	}
	return line
}

func (p *Plain) line(text string) error {
	_, err := io.WriteString(p.Out, text+"\n")
	return err
}

// Finish writes the stable summary. A detached session prints the ID and a
// resume command; it never claims the operation was cancelled.
func (p *Plain) Finish(summary Summary, err error, resume string, now time.Time) error {
	if summary.Detached || errors.Is(err, context.Canceled) {
		line := "detached from " + string(summary.OperationID) + " after " + fmt.Sprintf("%d", summary.Events) + " events"
		if summary.State != "" {
			line += " · last known " + stateWord(summary.State)
		}
		if resume != "" {
			line += "\nresume: " + resume
		}
		return p.line(p.clean()(line))
	}
	if err != nil {
		message := p.clean()(err.Error())
		if summary.OperationID != "" {
			message += "\noperation: " + string(summary.OperationID)
		}
		if resume != "" {
			message += "\nresume: " + resume
		}
		_, _ = io.WriteString(p.Err, "error: "+message+"\n")
		return nil
	}
	elapsed := ""
	if duration, ok := summaryElapsed(summary, now); ok {
		elapsed = " · elapsed " + FormatDuration(duration)
	}
	code := ""
	if summary.ErrorCode != "" {
		code = " [" + string(summary.ErrorCode) + "]"
	}
	line := stateWord(summary.State) + code + " " + string(summary.OperationID) + elapsed + " · " + fmt.Sprintf("%d", summary.Events) + " events"
	return p.line(p.clean()(line))
}

func summaryElapsed(summary Summary, now time.Time) (time.Duration, bool) {
	created := summary.Operation.CreatedAt
	if created.IsZero() {
		return 0, false
	}
	end := now
	if summary.Operation.State.Terminal() && !summary.Operation.UpdatedAt.IsZero() {
		end = summary.Operation.UpdatedAt
	}
	if end.Before(created) {
		return 0, true
	}
	return end.Sub(created), true
}
