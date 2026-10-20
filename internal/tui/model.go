// Package tui renders the operation viewer with Bubble Tea v2, Bubbles v2, and
// Lip Gloss v2. It consumes the plain reducer in internal/viewer; it never
// submits, cancels, or executes work.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/viewer"
)

type Options struct {
	Host          string
	ASCII         bool
	ReducedMotion bool
	NoColor       bool
	Now           func() time.Time
	In            io.Reader
	Out           io.Writer
	Width         int
	Height        int
}

func (o Options) normalized() Options {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Width <= 0 {
		o.Width = 80
	}
	if o.Height <= 0 {
		o.Height = 24
	}
	return o
}

// Producer follows one operation and emits reducer observations. It must stop
// when the context ends and must never request cancellation.
type Producer func(context.Context, func(viewer.Observation) error) (viewer.Summary, error)

type observationMsg struct{ observation viewer.Observation }
type doneMsg struct {
	summary viewer.Summary
	err     error
}
type tickMsg time.Time

type pane int

const (
	paneStages pane = iota
	paneEvents
)

// Keys is the complete key map. Help uses the same bindings as navigation.
type Keys struct {
	Up       key.Binding
	Down     key.Binding
	Top      key.Binding
	Bottom   key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Tab      key.Binding
	Pause    key.Binding
	Help     key.Binding
	Detach   key.Binding
}

func DefaultKeys() Keys {
	return Keys{
		Up:       key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("k/↑", "previous")),
		Down:     key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/↓", "next")),
		Top:      key.NewBinding(key.WithKeys("g", "home"), key.WithHelp("g", "oldest")),
		Bottom:   key.NewBinding(key.WithKeys("G", "end"), key.WithHelp("G", "newest")),
		PageUp:   key.NewBinding(key.WithKeys("pgup", "ctrl+b"), key.WithHelp("pgup", "page up")),
		PageDown: key.NewBinding(key.WithKeys("pgdown", "ctrl+f"), key.WithHelp("pgdn", "page down")),
		Tab:      key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "pane")),
		Pause:    key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "pause scroll")),
		Help:     key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Detach:   key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "detach")),
	}
}

func (k Keys) ShortHelp() []key.Binding {
	return []key.Binding{k.Pause, k.Tab, k.Help, k.Detach}
}

func (k Keys) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Top, k.Bottom, k.PageUp, k.PageDown},
		{k.Tab, k.Pause, k.Help, k.Detach},
	}
}

type Model struct {
	view   viewer.Model
	now    time.Time
	width  int
	height int

	opts      Options
	palette   palette
	glyphs    glyphs
	keys      Keys
	spinner   spinner.Model
	progress  progress.Model
	events    viewport.Model
	help      help.Model
	focus     pane
	stage     int
	paused    bool
	showHelp  bool
	newEvents int

	done    bool
	summary viewer.Summary
	err     error
}

func newModel(initial viewer.Model, opts Options) Model {
	opts = opts.normalized()
	now := opts.Now()
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	if opts.ASCII {
		spin = spinner.New(spinner.WithSpinner(spinner.Line))
	}
	prog := progress.New(progress.WithWidth(30), progress.WithoutPercentage())
	if opts.ASCII {
		prog.Full = '#'
		prog.Empty = '-'
	}
	if opts.NoColor {
		prog.FullColor = nil
		prog.EmptyColor = nil
	}
	events := viewport.New(viewport.WithWidth(opts.Width), viewport.WithHeight(8))
	events.SoftWrap = true
	events.FillHeight = true
	helpModel := help.New()
	if opts.NoColor {
		helpModel.Styles = help.Styles{}
		helpModel.ShortSeparator = glyphs{ascii: opts.ASCII}.separator()
		helpModel.FullSeparator = "    "
	}
	m := Model{
		view:     initial,
		now:      now,
		width:    opts.Width,
		height:   opts.Height,
		opts:     opts,
		palette:  newPalette(opts.NoColor),
		glyphs:   glyphs{ascii: opts.ASCII},
		keys:     DefaultKeys(),
		spinner:  spin,
		progress: prog,
		events:   events,
		help:     helpModel,
	}
	m.refreshContent()
	return m
}

func (m Model) Init() tea.Cmd {
	commands := []tea.Cmd{tickCommand()}
	if !m.opts.ReducedMotion && !m.view.Terminal() {
		commands = append(commands, m.spinner.Tick)
	}
	return tea.Batch(commands...)
}

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = max(1, message.Width)
		m.height = max(1, message.Height)
		m.refreshContent()
		return m, nil
	case tickMsg:
		m.now = time.Time(message)
		m.view.Tick(m.now)
		if m.done {
			return m, nil
		}
		return m, tickCommand()
	case spinner.TickMsg:
		var command tea.Cmd
		m.spinner, command = m.spinner.Update(message)
		if m.opts.ReducedMotion || m.view.Terminal() || m.view.ObservationState == viewer.Stale || m.view.ObservationState == viewer.Closed || m.done {
			command = nil
		}
		return m, command
	case observationMsg:
		m.applyObservation(message.observation)
		return m, nil
	case doneMsg:
		m.done = true
		m.summary = message.summary
		m.err = message.err
		m.view.Tick(m.now)
		m.refreshContent()
		return m, tea.Quit
	case tea.KeyPressMsg:
		return m.handleKey(message)
	}
	return m, nil
}

func (m *Model) applyObservation(observation viewer.Observation) {
	m.now = observation.At
	switch observation.Kind {
	case viewer.ObservationSnapshot:
		if observation.Snapshot != nil {
			m.view.Snapshot(*observation.Snapshot, observation.At)
		}
	case viewer.ObservationEvents:
		if err := m.view.Page(observation.Events, observation.At); err == nil {
			if !m.paused {
				m.newEvents = 0
			} else {
				m.newEvents += len(observation.Events)
			}
		}
	case viewer.ObservationFailed:
		m.view.PollFailed(observation.Err, observation.At)
	case viewer.ObservationSucceeded:
		m.view.PollSucceeded(observation.At)
	case viewer.ObservationTick:
		m.view.Tick(observation.At)
	case viewer.ObservationClosed:
		m.view.Close(observation.At)
	}
	m.refreshContent()
}

func (m Model) handleKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(message, m.keys.Detach):
		return m, tea.Quit
	case key.Matches(message, m.keys.Help):
		m.showHelp = !m.showHelp
		return m, nil
	case key.Matches(message, m.keys.Tab):
		if m.focus == paneStages {
			m.focus = paneEvents
		} else {
			m.focus = paneStages
		}
		return m, nil
	case key.Matches(message, m.keys.Pause):
		m.paused = !m.paused
		if !m.paused {
			m.newEvents = 0
			m.events.GotoBottom()
		}
		return m, nil
	}
	if m.focus == paneStages {
		switch {
		case key.Matches(message, m.keys.Up):
			if m.stage > 0 {
				m.stage--
			}
		case key.Matches(message, m.keys.Down):
			if m.stage+1 < len(m.view.Stages) {
				m.stage++
			}
		case key.Matches(message, m.keys.Top):
			m.stage = 0
		case key.Matches(message, m.keys.Bottom):
			if len(m.view.Stages) > 0 {
				m.stage = len(m.view.Stages) - 1
			}
		}
		return m, nil
	}
	switch {
	case key.Matches(message, m.keys.Up):
		m.events.ScrollUp(1)
	case key.Matches(message, m.keys.Down):
		m.events.ScrollDown(1)
	case key.Matches(message, m.keys.Top):
		m.events.GotoTop()
	case key.Matches(message, m.keys.Bottom):
		m.events.GotoBottom()
	case key.Matches(message, m.keys.PageUp):
		m.events.PageUp()
	case key.Matches(message, m.keys.PageDown):
		m.events.PageDown()
	}
	return m, nil
}

func tickCommand() tea.Cmd {
	return tea.Tick(time.Second, func(now time.Time) tea.Msg { return tickMsg(now) })
}

func (m *Model) refreshContent() {
	lines := make([]string, 0, len(m.view.EventLines))
	for _, event := range m.view.EventLines {
		lines = append(lines, m.eventLine(event))
	}
	if len(lines) == 0 {
		lines = []string{m.dim().Render("Waiting for the first durable event.")}
	}
	atBottom := m.events.AtBottom()
	m.events.SetContent(strings.Join(lines, "\n"))
	if !m.paused {
		m.events.GotoBottom()
	} else if atBottom {
		m.events.GotoBottom()
	}
}

func (m Model) eventLine(event contract.Event) string {
	at := event.At.UTC().Format("15:04:05")
	parts := []string{
		m.dim().Render(at),
		m.dim().Render(fmt.Sprintf("%5d", event.Sequence)),
		m.kindStyle(event.Kind).Render(event.Kind),
	}
	if event.Stage != "" {
		parts = append(parts, m.accent().Render(event.Stage))
	}
	if event.Progress != nil {
		parts = append(parts, m.info().Render(fmt.Sprintf("%d/%d %s", event.Progress.Current, event.Progress.Total, event.Progress.Unit)))
	}
	line := strings.Join(parts, " ")
	if event.Message != "" {
		line += " " + event.Message
	}
	return line
}

func (m Model) kindStyle(kind string) lipgloss.Style {
	if m.opts.NoColor {
		return lipgloss.NewStyle()
	}
	switch kind {
	case "warning":
		return m.palette.warn
	case "state":
		return m.palette.ok
	case "step":
		return m.palette.accent
	default:
		return m.palette.dim
	}
}

func (m Model) dim() lipgloss.Style     { return m.palette.dim }
func (m Model) accent() lipgloss.Style  { return m.palette.accent }
func (m Model) info() lipgloss.Style    { return m.palette.info }
func (m Model) bold() lipgloss.Style    { return m.palette.bold }
func (m Model) risk() lipgloss.Style    { return m.palette.risk }
func (m Model) okStyle() lipgloss.Style { return m.palette.ok }

func (m Model) stateStyle(state contract.State) lipgloss.Style {
	if m.opts.NoColor {
		return lipgloss.NewStyle()
	}
	switch state {
	case contract.Succeeded:
		return m.palette.ok
	case contract.Failed:
		return m.palette.risk
	case contract.Canceled:
		return m.palette.warn
	case contract.Running, contract.Cancelling:
		return m.palette.accent
	case contract.Queued:
		return m.palette.info
	default:
		return m.palette.dim
	}
}

func (m Model) observationStyle(observation viewer.ObservationState) lipgloss.Style {
	if m.opts.NoColor {
		return lipgloss.NewStyle()
	}
	switch observation {
	case viewer.Live:
		return m.palette.ok
	case viewer.Stale, viewer.Reconnecting:
		return m.palette.warn
	default:
		return m.palette.dim
	}
}

func (m Model) spinnerView() string {
	if m.opts.ReducedMotion || m.view.Terminal() || m.view.ObservationState == viewer.Stale || m.view.ObservationState == viewer.Reconnecting || m.view.ObservationState == viewer.Closed {
		return ""
	}
	if m.view.ObservationState != viewer.Live && m.view.ObservationState != viewer.Connecting {
		return ""
	}
	return m.spinner.View()
}

func (m Model) stageView(index int, stage viewer.Stage) string {
	selected := m.focus == paneStages && index == m.stage
	prefix := "  "
	if selected {
		prefix = "> "
	}
	symbol := m.glyphs.stage(stage.Status)
	status := string(stage.Status)
	if stage.Status == viewer.StageRunning && stage.Attempt > 0 {
		status += fmt.Sprintf("%sattempt %d", m.glyphs.separator(), stage.Attempt)
	}
	line := fmt.Sprintf("%s%s %-18s %s", prefix, symbol, stage.Name, status)
	if selected {
		return m.palette.sel.Render(line)
	}
	return m.stateStyle(stageState(stage.Status)).Render(line)
}

func stageState(status viewer.StageStatus) contract.State {
	switch status {
	case viewer.StageCompleted:
		return contract.Succeeded
	case viewer.StageFailed:
		return contract.Failed
	case viewer.StageRunning:
		return contract.Running
	default:
		return contract.Canceled
	}
}

func (m Model) progressView(width int) string {
	if m.view.Progress == nil || m.view.Progress.Total == 0 {
		return ""
	}
	percent := float64(m.view.Progress.Current) / float64(m.view.Progress.Total)
	if percent < 0 {
		percent = 0
	}
	if percent > 1 {
		percent = 1
	}
	m.progress.SetWidth(max(6, min(width, 40)))
	bar := m.progress.ViewAs(percent)
	return fmt.Sprintf("%d/%d %s %s", m.view.Progress.Current, m.view.Progress.Total, m.view.Progress.Unit, bar)
}

func (m Model) summaryLine() string {
	if m.err != nil {
		return "follow ended: " + m.err.Error()
	}
	if m.done {
		return "terminal result observed"
	}
	return ""
}

func (m Model) maxWidth(width int) string {
	return lipgloss.NewStyle().MaxWidth(max(1, width)).Render("")
}

func joinNonEmpty(parts ...string) string {
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			filtered = append(filtered, part)
		}
	}
	return strings.Join(filtered, "\n")
}

func isQuit(err error) bool {
	return errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, tea.ErrInterrupted)
}
