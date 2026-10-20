package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mbvlabs/eregion/internal/viewer"
)

func (m Model) View() tea.View {
	view := tea.NewView(m.render())
	view.AltScreen = true
	view.WindowTitle = "eregion · " + string(m.view.OperationID)
	return view
}

func (m Model) render() string {
	width := max(1, m.width)
	height := max(1, m.height)
	if width < 36 || height < 10 {
		return fitLines(m.renderCompact(width, height), width, height)
	}
	header := m.renderHeader(width)
	footer := m.renderFooter(width)
	bodyHeight := height - lipgloss.Height(header) - lipgloss.Height(footer)
	if bodyHeight < 3 {
		compact := m.renderCompact(width, max(1, height-lipgloss.Height(footer)))
		return fitLines(joinNonEmpty(header, compact, footer), width, height)
	}
	var body string
	if width >= 100 && bodyHeight >= 8 {
		body = m.renderWide(width, bodyHeight)
	} else {
		body = m.renderStandard(width, bodyHeight)
	}
	return fitLines(joinNonEmpty(header, body, footer), width, height)
}

func (m Model) renderHeader(width int) string {
	identity := m.bold().Render("eregion")
	host := m.dim().Render(viewer.DefaultClean(m.opts.Host))
	if host == "" {
		host = m.dim().Render("API host unknown")
	}
	kind := m.dim().Render(m.view.Kind)
	line1 := strings.Join(compactParts(identity, host, kind), m.glyphs.separator())

	target := m.bold().Render(m.view.Target)
	if target == "" {
		target = m.dim().Render("no target observed")
	}
	id := m.dim().Render(string(m.view.OperationID))
	line2 := target

	spin := m.spinnerView()
	badge := badgeText(m.view.State, m.view.ObservationState, m.opts.ASCII)
	if spin != "" {
		label := strings.ToUpper(string(m.view.State))
		if label == "" {
			label = "CONNECTING"
		}
		badge = spin + " " + label
	}
	badgeStyle := m.stateStyle(m.view.State)
	if !m.view.State.Terminal() {
		badgeStyle = m.observationStyle(m.view.ObservationState)
		if m.view.State != "" && m.view.ObservationState != viewer.Stale && m.view.ObservationState != viewer.Reconnecting {
			badgeStyle = m.stateStyle(m.view.State)
		}
	}
	meta := []string{m.dim().Render(attemptText(m.view.Attempt, m.view.PreviousAttempt, m.view.AttemptChanged))}
	if elapsed, ok := m.view.Elapsed(m.now); ok {
		meta = append(meta, m.dim().Render("elapsed "+viewer.FormatDuration(elapsed)))
	}
	if queued, ok := m.view.Queued(m.now); ok && (m.view.State == "" || m.view.State == "queued") {
		meta = append(meta, m.dim().Render("queued "+viewer.FormatDuration(queued)))
	}
	if m.view.CancelRequested {
		if m.view.CancelAcknowledged {
			meta = append(meta, m.dim().Render("cancel acknowledged"))
		} else {
			meta = append(meta, m.warning().Render("cancel requested"))
		}
	}
	if m.view.ErrorCode != "" && !m.opts.NoColor {
		meta = append(meta, m.risk().Render("["+string(m.view.ErrorCode)+"]"))
	} else if m.view.ErrorCode != "" {
		meta = append(meta, "["+string(m.view.ErrorCode)+"]")
	}
	line3 := strings.Join(append([]string{badgeStyle.Render(badge)}, meta...), m.glyphs.separator())
	serving := m.dim().Render(viewer.DefaultClean(m.view.ServingLine()))
	return fitLines(strings.Join([]string{line1, line2, id, line3, serving}, "\n"), width, 5)
}

func (m Model) renderWide(width, height int) string {
	leftWidth := min(38, max(24, width/3))
	separator := m.dim().Render(" │ ")
	if m.opts.ASCII {
		separator = " | "
	}
	rightWidth := max(10, width-leftWidth-3)
	insightHeight := 0
	if len(m.view.Targets) > 0 || len(m.view.Health) > 0 {
		insightHeight = min(max(3, len(m.view.Targets)+len(m.view.Health)+3), max(3, height/2))
	}
	left := joinNonEmpty(
		m.renderStagesPane(leftWidth, max(4, height-insightHeight)),
		m.renderInsightsPane(leftWidth, insightHeight),
	)
	right := m.renderEventsPane(rightWidth, height)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, separator, right)
}

func (m Model) renderStandard(width, height int) string {
	eventsHeight := min(max(3, height/3), max(3, height-4))
	insightHeight := 0
	if len(m.view.Targets) > 0 || len(m.view.Health) > 0 {
		insightHeight = min(max(2, len(m.view.Targets)/2+len(m.view.Health)+2), max(2, (height-eventsHeight)/2))
	}
	stagesHeight := max(2, height-eventsHeight-insightHeight-1)
	stages := m.renderStagesPane(width, stagesHeight)
	insights := m.renderInsightsPane(width, insightHeight)
	rule := m.rule(width)
	events := m.renderEventsPane(width, eventsHeight)
	return joinNonEmpty(stages, insights, rule, events)
}

func (m Model) renderStagesPane(width, height int) string {
	lines := []string{m.dim().Render("EXECUTION STAGES")}
	if len(m.view.Stages) == 0 {
		lines = append(lines, m.dim().Render("No stage observations yet."))
	}
	for index, stage := range m.view.Stages {
		lines = append(lines, m.stageView(index, stage))
	}
	completed, active, failed := m.view.StageCounts()
	counts := fmt.Sprintf("%d completed%s%d active", completed, m.glyphs.separator(), active)
	if failed > 0 {
		counts += fmt.Sprintf("%s%d failed", m.glyphs.separator(), failed)
	}
	lines = append(lines, "", m.dim().Render(counts))
	if progress := m.progressView(width); progress != "" {
		lines = append(lines, m.info().Render(progress))
	}
	if activeStage, ok := m.view.ActiveStage(); ok {
		lines = append(lines, m.accent().Render("active: "+activeStage.Name))
	}
	return fitLines(strings.Join(lines, "\n"), width, height)
}

func (m Model) renderInsightsPane(width, height int) string {
	if height <= 0 {
		return ""
	}
	lines := []string{m.dim().Render("TARGETS")}
	for _, target := range m.view.Targets {
		symbol := m.glyphs.stage(target.Status)
		label := target.Role.String()
		if target.Label != "" && target.Label != string(target.Role) {
			label = target.Label
		}
		line := fmt.Sprintf("%s %-22s %s", symbol, truncate(label, 22), target.Status)
		lines = append(lines, m.stateStyle(stageState(target.Status)).Render(line))
	}
	if len(m.view.Health) > 0 {
		lines = append(lines, "", m.dim().Render("HEALTH OBSERVATIONS"))
		for _, observation := range m.view.Health {
			state := "passed"
			style := m.okStyle()
			if !observation.Passed {
				state, style = "failed", m.risk()
			}
			probes := "checks not reported"
			if observation.Checks > 0 {
				probes = fmt.Sprintf("%d probe(s)", observation.Checks)
			}
			detail := observation.Detail
			if detail == "" {
				detail = "no detail"
			}
			line := fmt.Sprintf("%s %s%s%s%s%s", state, probes, m.glyphs.separator(), detail, m.glyphs.separator(), observation.At.UTC().Format("15:04:05"))
			lines = append(lines, style.Render(truncate(line, width)))
		}
	}
	return fitLines(strings.Join(lines, "\n"), width, height)
}

func (m Model) renderEventsPane(width, height int) string {
	if m.showHelp {
		helpModel := m.help
		helpModel.ShowAll = true
		helpModel.SetWidth(max(1, width))
		return fitLines(m.dim().Render("HELP")+"\n"+helpModel.View(m.keys), width, height)
	}
	label := "EVENTS"
	if m.paused {
		label += m.glyphs.separator() + "scroll paused"
		if m.newEvents > 0 {
			label += fmt.Sprintf("%s%d new", m.glyphs.separator(), m.newEvents)
		}
	}
	if total := len(m.view.EventLines); total > 0 {
		label += fmt.Sprintf("%s%d shown%s%d observed", m.glyphs.separator(), total, m.glyphs.separator(), m.view.EventCount)
	}
	if len(m.view.BuildLog) > 0 {
		label += fmt.Sprintf("%sbuild log %d", m.glyphs.separator(), len(m.view.BuildLog))
	}
	if m.view.Gap {
		label += m.glyphs.separator() + "sequence gap"
	}
	m.events.SetWidth(max(1, width))
	m.events.SetHeight(max(1, height-1))
	if !m.paused {
		m.events.GotoBottom()
	}
	content := m.events.View()
	return fitLines(m.dim().Render(label)+"\n"+content, width, height)
}

func (m Model) renderFooter(width int) string {
	freshness := m.observationStyle(m.view.ObservationState).Render(
		m.glyphs.observation(m.view.ObservationState) + " " + m.view.Freshness(m.now),
	)
	mode := "following"
	switch {
	case m.paused:
		mode = "scroll paused"
	case m.done:
		mode = "complete"
	case m.view.Terminal():
		mode = "terminal" + m.glyphs.separator() + "closing"
	}
	status := strings.Join([]string{
		freshness,
		m.dim().Render(mode),
		m.dim().Render(fmt.Sprintf("%d events", m.view.EventCount)),
	}, m.glyphs.separator())
	if m.summaryLine() != "" {
		status = m.dim().Render(viewer.DefaultClean(m.summaryLine()))
	}
	helpModel := m.help
	helpModel.SetWidth(max(1, width))
	hints := helpModel.View(m.keys)
	if hints == "" {
		hints = m.dim().Render("? help" + m.glyphs.separator() + "q detach")
	}
	return fitLines(joinNonEmpty(status, hints), width, 2)
}

func (m Model) renderCompact(width, height int) string {
	badge := badgeText(m.view.State, m.view.ObservationState, m.opts.ASCII)
	badgeStyle := m.stateStyle(m.view.State)
	if !m.view.State.Terminal() {
		badgeStyle = m.observationStyle(m.view.ObservationState)
	}
	lines := []string{
		m.bold().Render("eregion") + " " + badgeStyle.Render(badge),
		m.dim().Render(string(m.view.OperationID)),
	}
	meta := []string{m.bold().Render(m.view.Target)}
	if elapsed, ok := m.view.Elapsed(m.now); ok {
		meta = append(meta, m.dim().Render("elapsed "+viewer.FormatDuration(elapsed)))
	}
	lines = append(lines, strings.Join(compactParts(meta...), m.glyphs.separator()))
	if stage, ok := m.view.ActiveStage(); ok {
		lines = append(lines, m.accent().Render("active: "+stage.Name))
	} else if len(m.view.Stages) > 0 {
		completed, _, _ := m.view.StageCounts()
		lines = append(lines, m.dim().Render(fmt.Sprintf("%d stages observed%s%d completed", len(m.view.Stages), m.glyphs.separator(), completed)))
	}
	lines = append(lines, m.dim().Render(viewer.DefaultClean(m.view.ServingLine())))
	if len(m.view.Health) > 0 {
		last := m.view.Health[len(m.view.Health)-1]
		state := "health passed"
		if !last.Passed {
			state = "health failed"
		}
		probes := ""
		if last.Checks > 0 {
			probes = fmt.Sprintf("%s%d probe(s)", m.glyphs.separator(), last.Checks)
		}
		lines = append(lines, m.dim().Render(state+probes))
	}
	if len(m.view.EventLines) > 0 {
		last := m.view.EventLines[len(m.view.EventLines)-1]
		message := last.Message
		if message == "" {
			message = string(last.State)
		}
		if message == "" {
			message = last.Kind
		}
		lines = append(lines, m.dim().Render("last: "+message))
	}
	lines = append(lines, m.observationStyle(m.view.ObservationState).Render(
		m.glyphs.observation(m.view.ObservationState)+" "+m.view.Freshness(m.now),
	))
	if m.showHelp {
		lines = append(lines, m.dim().Render("? toggles help"+m.glyphs.separator()+"tab pane"+m.glyphs.separator()+"space pause"+m.glyphs.separator()+"q detach"))
	} else {
		lines = append(lines, m.dim().Render("? help"+m.glyphs.separator()+"q detach"))
	}
	return fitLines(strings.Join(lines, "\n"), width, height)
}

func (m Model) rule(width int) string {
	character := "─"
	if m.opts.ASCII {
		character = "-"
	}
	return m.dim().Render(strings.Repeat(character, max(1, width)))
}

func (m Model) warning() lipgloss.Style {
	if m.opts.NoColor {
		return lipgloss.NewStyle()
	}
	return m.palette.warn
}

func compactParts(parts ...string) []string {
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			filtered = append(filtered, part)
		}
	}
	return filtered
}

func fitLines(text string, width, height int) string {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	lines := strings.Split(text, "\n")
	for index := range lines {
		lines[index] = lipgloss.NewStyle().MaxWidth(width).Render(lines[index])
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func truncate(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(text) <= width {
		return text
	}
	if width <= 3 {
		return strings.Repeat(".", width)
	}
	target := width - 3
	var out strings.Builder
	for _, r := range text {
		if lipgloss.Width(out.String()+string(r)) > target {
			break
		}
		out.WriteRune(r)
	}
	return out.String() + "..."
}
