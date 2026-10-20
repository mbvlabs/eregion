package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/viewer"
)

type palette struct {
	text   lipgloss.Style
	dim    lipgloss.Style
	accent lipgloss.Style
	ok     lipgloss.Style
	warn   lipgloss.Style
	risk   lipgloss.Style
	info   lipgloss.Style
	bold   lipgloss.Style
	sel    lipgloss.Style
}

func newPalette(noColor bool) palette {
	if noColor {
		return palette{}
	}
	return palette{
		text:   lipgloss.NewStyle().Foreground(lipgloss.Color("#e3ecf5")),
		dim:    lipgloss.NewStyle().Foreground(lipgloss.Color("#92a6bb")),
		accent: lipgloss.NewStyle().Foreground(lipgloss.Color("#ffb547")),
		ok:     lipgloss.NewStyle().Foreground(lipgloss.Color("#6fd3a0")),
		warn:   lipgloss.NewStyle().Foreground(lipgloss.Color("#e8d36a")),
		risk:   lipgloss.NewStyle().Foreground(lipgloss.Color("#ff7b6b")),
		info:   lipgloss.NewStyle().Foreground(lipgloss.Color("#7cc0f0")),
		bold:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#e3ecf5")),
		sel:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffb547")),
	}
}

type glyphs struct {
	ascii bool
}

func (g glyphs) state(state contract.State) string {
	if g.ascii {
		switch state {
		case contract.Succeeded:
			return "OK"
		case contract.Failed:
			return "X"
		case contract.Canceled:
			return "--"
		case contract.Running:
			return ">"
		case contract.Cancelling:
			return "~"
		case contract.Queued:
			return ":"
		default:
			return "?"
		}
	}
	switch state {
	case contract.Succeeded:
		return "●"
	case contract.Failed:
		return "■"
	case contract.Canceled:
		return "×"
	case contract.Running:
		return "◐"
	case contract.Cancelling:
		return "◐"
	case contract.Queued:
		return "◇"
	default:
		return "?"
	}
}

func (g glyphs) stage(status viewer.StageStatus) string {
	if g.ascii {
		switch status {
		case viewer.StageCompleted:
			return "[x]"
		case viewer.StageRunning:
			return "[>]"
		case viewer.StageFailed:
			return "[!]"
		default:
			return "[-]"
		}
	}
	switch status {
	case viewer.StageCompleted:
		return "✓"
	case viewer.StageRunning:
		return "◐"
	case viewer.StageFailed:
		return "■"
	default:
		return "·"
	}
}

func (g glyphs) observation(state viewer.ObservationState) string {
	if g.ascii {
		switch state {
		case viewer.Live:
			return "*"
		case viewer.Stale:
			return "!"
		case viewer.Reconnecting:
			return "?"
		case viewer.Closed:
			return "#"
		default:
			return "..."
		}
	}
	switch state {
	case viewer.Live:
		return "●"
	case viewer.Stale:
		return "▲"
	case viewer.Reconnecting:
		return "?"
	case viewer.Closed:
		return "○"
	default:
		return "◌"
	}
}

func (g glyphs) separator() string {
	if g.ascii {
		return " | "
	}
	return " · "
}

func badgeText(state contract.State, observation viewer.ObservationState, ascii bool) string {
	g := glyphs{ascii: ascii}
	if state == "" {
		return g.observation(observation) + " " + strings.ToUpper(string(observation))
	}
	label := g.state(state) + " " + strings.ToUpper(string(state))
	if state.Terminal() {
		return label
	}
	switch observation {
	case viewer.Stale, viewer.Reconnecting:
		return g.observation(observation) + " LAST KNOWN: " + strings.ToUpper(string(state))
	default:
		return label
	}
}

func titleCase(text string) string {
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

func attemptText(attempt uint32, previous uint32, changed bool) string {
	if attempt == 0 {
		return "attempt not started"
	}
	if changed {
		return fmt.Sprintf("attempt %d (was %d)", attempt, previous)
	}
	return fmt.Sprintf("attempt %d", attempt)
}
