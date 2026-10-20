package workspaceui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mbvlabs/eregion/internal/contract"
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

func (m Model) View() tea.View {
	view := tea.NewView(m.render())
	view.AltScreen = true
	view.WindowTitle = "eregion · workspaces"
	return view
}

func (m Model) render() string {
	width, height := max(1, m.width), max(1, m.height)
	if m.form != nil {
		return fitLines(m.renderForm(width, height), width, height)
	}
	if m.op != nil {
		return fitLines(m.renderOperation(width, height), width, height)
	}
	if m.showHelp {
		return fitLines(m.renderHelp(width, height), width, height)
	}
	if width < 40 || height < 9 {
		return fitLines(m.renderCompact(width, height), width, height)
	}
	header := m.renderHeader(width)
	tabs := m.renderTabs(width)
	footer := m.renderFooter(width)
	bodyHeight := height - lipgloss.Height(header) - lipgloss.Height(tabs) - lipgloss.Height(footer)
	if bodyHeight < 2 {
		return fitLines(m.renderCompact(width, height), width, height)
	}
	body := m.renderBody(width, bodyHeight)
	return fitLines(joinNonEmpty(header, tabs, body, footer), width, height)
}

func (m Model) paletteFor() palette { return newPalette(m.opts.NoColor) }

func (m Model) dim() lipgloss.Style    { return m.paletteFor().dim }
func (m Model) accent() lipgloss.Style { return m.paletteFor().accent }
func (m Model) okStyle() lipgloss.Style {
	return m.paletteFor().ok
}
func (m Model) warnStyle() lipgloss.Style { return m.paletteFor().warn }
func (m Model) riskStyle() lipgloss.Style { return m.paletteFor().risk }
func (m Model) infoStyle() lipgloss.Style { return m.paletteFor().info }
func (m Model) boldStyle() lipgloss.Style { return m.paletteFor().bold }
func (m Model) selStyle() lipgloss.Style  { return m.paletteFor().sel }

func (m Model) glyphSeparator() string {
	if m.opts.ASCII {
		return " | "
	}
	return " · "
}

func (m Model) glyphRule(width int) string {
	character := "─"
	if m.opts.ASCII {
		character = "-"
	}
	return m.dim().Render(strings.Repeat(character, max(1, width)))
}

func (m Model) spinnerText() string {
	if m.opts.ReducedMotion {
		return m.dim().Render("loading")
	}
	return m.spinner.View()
}

func (m Model) stateLabel() string {
	tab := m.current()
	switch tab.state {
	case loadLoading:
		return m.spinnerText() + " " + m.dim().Render("loading "+m.tab.Collection())
	case loadFailed:
		label := "unreachable"
		switch tab.errCode {
		case contract.Unauthenticated:
			label = "authentication required"
		case contract.Forbidden:
			label = "not authorized"
		case contract.ValidationFailed:
			if m.tab == Resources || m.tab == Backups {
				label = "inventory not available"
			}
		}
		style := m.riskStyle()
		if len(tab.items) > 0 {
			style = m.warnStyle()
		}
		return style.Render(label) + m.glyphSeparator() + m.dim().Render(tab.errText)
	case loadReady:
		if len(tab.items) == 0 {
			return m.dim().Render("empty")
		}
		freshness, stale := m.freshness(tab)
		style := m.okStyle()
		if stale {
			style = m.warnStyle()
		}
		return style.Render(freshness) + m.glyphSeparator() + m.dim().Render(fmt.Sprintf("%d rows", len(tab.items)))
	default:
		return m.dim().Render("not loaded")
	}
}

func (m Model) renderHeader(width int) string {
	host := m.clean(m.opts.Host)
	if host == "" {
		host = "API host unknown"
	}
	line1 := strings.Join([]string{m.boldStyle().Render("eregion"), m.dim().Render(host), m.boldStyle().Render(m.tab.String())}, m.glyphSeparator())
	status := m.stateLabel()
	if m.filterText != "" {
		status += m.glyphSeparator() + m.infoStyle().Render("/"+m.clean(m.filterText))
	}
	return fitLines(line1+"\n"+status, width, 2)
}

func (m Model) renderTabs(width int) string {
	parts := make([]string, 0, tabCount)
	for tab := Tab(0); tab < tabCount; tab++ {
		label := fmt.Sprintf("%d %s", int(tab)+1, tab.String())
		if tab == m.tab {
			label = "[" + label + "]"
			parts = append(parts, m.selStyle().Render(label))
			continue
		}
		parts = append(parts, m.dim().Render(label))
	}
	separator := "  "
	if m.opts.ASCII {
		separator = " | "
	}
	return fitLines(strings.Join(parts, separator), width, 1)
}

func (m Model) renderBody(width, height int) string {
	if m.detail {
		return m.renderDetail(width, height)
	}
	tab := m.current()
	items := m.visibleItems()
	if tab.state == loadFailed && len(tab.items) == 0 {
		return m.renderUnreachable(width, height)
	}
	if tab.state == loadLoading && len(tab.items) == 0 {
		lines := []string{
			m.spinnerText() + " " + m.boldStyle().Render("Loading "+m.tab.Collection()),
			m.dim().Render("The workspace keeps the previous view until the API responds."),
		}
		return fitLines(strings.Join(lines, "\n"), width, height)
	}
	if len(items) == 0 {
		return m.renderEmpty(width, height)
	}
	lines := make([]string, 0, height)
	capacity := height
	if width >= 56 && height >= 8 {
		header := fmt.Sprintf("%-*s %s", max(10, width-20), "NAME", "STATUS")
		lines = append(lines, m.dim().Render(truncate(header, width)))
		capacity--
	}
	selected := m.selected[m.tab]
	start := windowStart(selected, len(items), capacity)
	for index := start; index < len(items) && len(lines) < height; index++ {
		lines = append(lines, m.rowLine(items[index], index == selected, width))
	}
	if len(items) > capacity {
		lines = append(lines, m.dim().Render(fmt.Sprintf("%d–%d of %d · j/k to move", start+1, min(len(items), start+capacity), len(items))))
	}
	return fitLines(strings.Join(lines, "\n"), width, height)
}

func (m Model) rowLine(item Item, selected bool, width int) string {
	prefix := "  "
	titleStyle := m.boldStyle()
	if selected {
		prefix = "> "
		titleStyle = m.selStyle()
	}
	status := m.statusStyle(item.Status).Render(item.Status)
	titleWidth := max(8, width-lipgloss.Width(status)-4)
	title := truncate(item.Title, titleWidth)
	line := prefix + titleStyle.Render(title)
	if item.Subtitle != "" && width >= 72 {
		subtitle := truncate(item.Subtitle, max(4, titleWidth-lipgloss.Width(title)-1))
		if subtitle != "" {
			line += " " + m.dim().Render(subtitle)
		}
	}
	padding := max(1, width-lipgloss.Width(line)-lipgloss.Width(status))
	return line + strings.Repeat(" ", padding) + status
}

func (m Model) statusStyle(status string) lipgloss.Style {
	lower := strings.ToLower(status)
	switch {
	case strings.Contains(lower, "unreachable"), strings.Contains(lower, "failed"), strings.Contains(lower, "unhealthy"), strings.Contains(lower, "disabled"):
		return m.riskStyle()
	case strings.Contains(lower, "ready"), strings.Contains(lower, "healthy"), strings.Contains(lower, "succeeded"), strings.Contains(lower, "running"):
		return m.okStyle()
	case strings.Contains(lower, "stale"), strings.Contains(lower, "unknown"), strings.Contains(lower, "registered"):
		return m.warnStyle()
	default:
		return m.dim()
	}
}

func (m Model) renderUnreachable(width, height int) string {
	tab := m.current()
	title := "API unreachable"
	detail := tab.errText
	switch tab.errCode {
	case contract.Unauthenticated:
		title = "Authentication required"
		detail = "Set EREGION_API_TOKEN for the selected API before opening the workspace."
	case contract.ValidationFailed:
		if m.tab == Resources || m.tab == Backups {
			title = m.tab.String() + " inventory is not available"
			detail = "No rows are invented. The " + m.tab.Collection() + " collection is not registered."
		}
	}
	lines := []string{
		m.riskStyle().Render(title),
		m.dim().Render(truncate(detail, width)),
		"",
		m.dim().Render("Press r to retry; q detaches without changing any operation."),
	}
	return fitLines(strings.Join(lines, "\n"), width, height)
}

func (m Model) renderEmpty(width, height int) string {
	title := "No rows"
	hint := "Press r to refresh."
	if m.filterText != "" {
		title = "No rows match the filter"
		hint = "Press esc to clear the filter, or / to edit it."
	} else {
		switch m.tab {
		case Fleet:
			title = "No nodes registered"
			hint = "Register placement facts through the platform API; this view never invents a node."
		case Apps:
			title = "No applications yet"
			hint = "Press a to create an application."
		case Envs:
			title = "No environments yet"
			hint = "Press e to create an environment after an application exists."
		case Resources:
			title = "No resources reported"
			hint = "No resources inventory rows were returned by this control plane."
		case Backups:
			title = "No backups reported"
			hint = "No backups inventory rows were returned by this control plane."
		}
	}
	lines := append(wrapText(title, width), "", m.dim().Render(truncate(hint, width)))
	lines[0] = m.boldStyle().Render(lines[0])
	return fitLines(strings.Join(lines, "\n"), width, height)
}

func (m Model) renderDetail(width, height int) string {
	item, ok := m.selectedItem()
	if !ok {
		return fitLines(m.dim().Render("No row selected."), width, height)
	}
	lines := []string{
		m.dim().Render("DETAILS" + m.glyphSeparator() + m.tab.String()),
		m.boldStyle().Render(truncate(item.Title, width)),
	}
	if item.Status != "" {
		lines = append(lines, m.statusStyle(item.Status).Render(item.Status))
	}
	lines = append(lines, "")
	available := max(1, height-len(lines)-2)
	_ = available
	scroll := min(m.detailScroll, max(0, len(item.Details)-1))
	for index := scroll; index < len(item.Details) && len(lines) < height-2; index++ {
		detail := item.Details[index]
		label := m.dim().Render(detail.Label + ": ")
		value := m.clean(detail.Value)
		prefixWidth := lipgloss.Width(label)
		wrapped := wrapText(value, max(8, width-prefixWidth))
		if len(wrapped) == 0 {
			wrapped = []string{""}
		}
		lines = append(lines, label+truncate(wrapped[0], max(8, width-prefixWidth)))
		for _, extra := range wrapped[1:] {
			lines = append(lines, strings.Repeat(" ", prefixWidth)+truncate(extra, max(8, width-prefixWidth)))
		}
		if len(lines) >= height-2 {
			break
		}
	}
	if m.tab == Envs || m.tab == Resources || m.tab == Backups {
		lines = append(lines, "", m.dim().Render("Telemetry: no provider configured; no graph is shown."))
	}
	lines = append(lines, m.dim().Render("j/k scroll · r refresh · esc back · q detach"))
	return fitLines(strings.Join(lines, "\n"), width, height)
}

func (m Model) renderFooter(width int) string {
	freshness, stale := m.freshness(m.current())
	style := m.okStyle()
	if stale {
		style = m.warnStyle()
	}
	status := style.Render(freshness)
	if m.filterText != "" {
		status += m.glyphSeparator() + m.infoStyle().Render("filter: "+m.clean(m.filterText))
	}
	hints := "r refresh · / filter · enter details · a/e/i/d forms · ? help · q detach"
	if width < 72 {
		hints = "r / enter a e i d ? q"
	}
	helpModel := m.help
	helpModel.SetWidth(max(1, width))
	if width >= 96 {
		if rendered := helpModel.View(m.keys); rendered != "" {
			hints = rendered
		}
	}
	return fitLines(status+"\n"+m.dim().Render(truncate(hints, width)), width, 2)
}

func (m Model) renderCompact(width, height int) string {
	item, ok := m.selectedItem()
	lines := []string{
		m.boldStyle().Render("eregion") + " " + m.boldStyle().Render(m.tab.String()),
		m.dim().Render(truncate(m.clean(m.opts.Host), width)),
		m.stateLabel(),
	}
	if ok {
		lines = append(lines, m.selStyle().Render(truncate(item.Title, width)))
		if item.Status != "" {
			lines = append(lines, m.statusStyle(item.Status).Render(truncate(item.Status, width)))
		}
	}
	lines = append(lines, m.dim().Render(truncate("? help · q detach", width)))
	return fitLines(strings.Join(lines, "\n"), width, height)
}

func (m Model) renderHelp(width, height int) string {
	helpModel := m.help
	helpModel.ShowAll = true
	helpModel.SetWidth(max(1, width))
	lines := []string{
		m.boldStyle().Render("HELP"),
		m.dim().Render("Workspaces read platform inventory; every action is dry-run previewed, then submitted explicitly."),
		"",
		helpModel.View(m.keys),
		"",
		m.boldStyle().Render("Workspaces"),
		m.dim().Render("1 fleet · 2 apps · 3 envs · 4 resources · 5 backups · tab cycles"),
		m.boldStyle().Render("Guided forms"),
		m.dim().Render("a application · e environment · i image release · d deploy"),
		m.dim().Render("enter next field · tab next · s submit after preview · e edit preview · esc cancel"),
		m.boldStyle().Render("Safety"),
		m.dim().Render("Deploys require typing the environment name. A detached UI never cancels an operation."),
		m.dim().Render("No telemetry provider is configured, so no graph is drawn."),
	}
	return fitLines(strings.Join(lines, "\n"), width, height)
}

func (m Model) renderForm(width, height int) string {
	form := m.form
	lines := []string{
		m.boldStyle().Render(strings.ToUpper(form.title)),
		m.dim().Render("kind: " + form.kind),
	}
	if form.target != "" {
		lines = append(lines, m.dim().Render("target: "+m.clean(form.target)))
	}
	lines = append(lines, "")
	if form.errText != "" {
		lines = append(lines, m.riskStyle().Render("error: "+m.clean(form.errText)), "")
	}
	switch form.phase {
	case formResult:
		lines = append(lines, m.renderFormResult(form, width))
	case formSubmitting:
		lines = append(lines,
			m.spinnerText()+" "+m.boldStyle().Render("Submitting with idempotency key "+form.key),
			m.dim().Render("The operation is durable even if this interface detaches."),
		)
	case formPreview:
		lines = append(lines, m.renderPreview(form, width))
	case formChecking:
		lines = append(lines, m.spinnerText()+" "+m.boldStyle().Render("Running the API dry-run preview"))
		lines = append(lines, m.dim().Render("No operation, domain row, or job is created by a dry run."))
	default:
		lines = append(lines, m.renderFormFields(form, width))
	}
	if form.errText != "" {
		if form.failed && form.preview != nil {
			lines = append(lines, m.warnStyle().Render("Retry with the same key: "+form.key+". Do not start a new form after a timed-out submission."))
		}
	}
	lines = append(lines, "", m.dim().Render(m.formHint(form)))
	return fitLines(strings.Join(lines, "\n"), width, height)
}

func (m Model) renderFormFields(form *formState, width int) string {
	lines := make([]string, 0, len(form.fields)*2)
	for index, item := range form.fields {
		cursor := "  "
		labelStyle := m.dim()
		if index == form.focus {
			cursor = "> "
			labelStyle = m.accent()
		}
		required := ""
		if item.required {
			required = " *"
		}
		lines = append(lines, cursor+labelStyle.Render(item.label+required))
		value := item.value
		style := m.boldStyle()
		if value == "" {
			value = item.placeholder
			style = m.dim()
		}
		lines = append(lines, "  "+style.Render(truncate(value, max(8, width-4))))
		if index == form.focus && item.hint != "" && item.hint != value {
			lines = append(lines, "  "+m.dim().Render(truncate(item.hint, max(8, width-4))))
		}
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderPreview(form *formState, width int) string {
	lines := []string{m.okStyle().Render("DRY-RUN PREVIEW — nothing has been submitted.")}
	if form.preview != nil {
		if form.preview.Summary != "" {
			lines = append(lines, m.boldStyle().Render(form.preview.Summary))
		}
		lines = append(lines, m.dim().Render("kind: "+form.preview.Kind))
		lines = append(lines, m.dim().Render("target: "+m.clean(form.preview.Target)))
		if len(form.preview.Steps) > 0 {
			lines = append(lines, "", m.boldStyle().Render("Planned steps"))
			for index, step := range form.preview.Steps {
				lines = append(lines, fmt.Sprintf("  %d. %s", index+1, truncate(m.clean(step), max(8, width-5))))
			}
		}
		if len(form.preview.Warnings) > 0 {
			lines = append(lines, "", m.warnStyle().Render("Warnings"))
			for _, warning := range form.preview.Warnings {
				lines = append(lines, "  "+m.warnStyle().Render(truncate(m.clean(warning), max(8, width-2))))
			}
		}
	}
	if form.confirmPhrase != "" {
		lines = append(lines, "", m.warnStyle().Render("Type "+form.confirmPhrase+" to confirm this deploy."))
		value := form.confirm.Value()
		if value == "" {
			value = "_"
		}
		lines = append(lines, m.boldStyle().Render(value))
	}
	lines = append(lines, "", m.dim().Render("idempotency key: "+form.key))
	if form.approved != nil {
		lines = append(lines, "", m.boldStyle().Render("Noninteractive equivalent"))
		for _, command := range strings.Split(form.approved.Noninteractive(), "\n") {
			for _, line := range wrapText(command, max(8, width)) {
				lines = append(lines, m.dim().Render(truncate(line, width)))
			}
		}
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderFormResult(form *formState, width int) string {
	if form.result == nil {
		return m.dim().Render("No result.")
	}
	operation := form.result.Operation
	state := string(operation.State)
	style := m.infoStyle()
	switch {
	case operation.State == contract.Succeeded:
		style = m.okStyle()
	case operation.State == contract.Failed, operation.State == contract.Canceled:
		style = m.riskStyle()
	case operation.State == contract.Running:
		style = m.accent()
	}
	lines := []string{
		m.okStyle().Render("ACCEPTED — the operation is durable without this interface."),
		m.boldStyle().Render(string(operation.ID)),
		style.Render("state: " + state),
		m.dim().Render(fmt.Sprintf("kind: %s · target: %s · attempt: %d · last sequence: %d", operation.Kind, m.clean(operation.Target), operation.Attempt, operation.LastSequence)),
	}
	if operation.ErrorCode != "" {
		lines = append(lines, m.riskStyle().Render("error: "+string(operation.ErrorCode)))
	}
	if form.approved != nil {
		lines = append(lines, "", m.boldStyle().Render("Noninteractive equivalent"))
		for _, command := range strings.Split(form.approved.Noninteractive(), "\n") {
			for _, line := range wrapText(command, max(8, width)) {
				lines = append(lines, m.dim().Render(truncate(line, width)))
			}
		}
	}
	lines = append(lines, "", m.boldStyle().Render("Resume observation"))
	for _, line := range wrapText("eregion operations follow "+string(operation.ID)+" --interactive", max(8, width)) {
		lines = append(lines, m.dim().Render(truncate(line, width)))
	}
	lines = append(lines, m.dim().Render("eregion operations get "+string(operation.ID)+" --json"))
	lines = append(lines, "", m.dim().Render("Detaching never cancels the operation."))
	return strings.Join(lines, "\n")
}

func (m Model) formHint(form *formState) string {
	switch form.phase {
	case formResult:
		return "f follow · esc close · q detach"
	case formSubmitting:
		return "waiting for the API · ctrl+c detaches (the operation is not cancelled)"
	case formPreview:
		if form.confirmPhrase != "" {
			return "type confirmation · enter submit · esc cancel · ctrl+c detach"
		}
		return "s submit · e edit · esc cancel · ctrl+c detach"
	case formChecking:
		return "waiting for the dry-run · esc hides the error hint · ctrl+c detach"
	default:
		return "enter next · tab next · esc cancel · ctrl+c detach"
	}
}

func (m Model) renderOperation(width, height int) string {
	op := m.op
	lines := []string{m.boldStyle().Render("OPERATION " + string(op.id))}
	if op.snapshot.ID != "" {
		lines = append(lines,
			m.dim().Render(fmt.Sprintf("kind: %s · target: %s", op.snapshot.Kind, m.clean(op.snapshot.Target))),
			m.dim().Render(fmt.Sprintf("state: %s · attempt: %d · last sequence: %d", op.snapshot.State, op.snapshot.Attempt, op.snapshot.LastSequence)),
		)
	}
	if op.reducer != nil {
		lines = append(lines, m.dim().Render(m.clean(op.reducer.Freshness(m.now))))
	}
	if op.loading {
		lines = append(lines, m.spinnerText()+" "+m.dim().Render("refreshing"))
	}
	if op.errText != "" {
		lines = append(lines, m.warnStyle().Render("observation error: "+m.clean(op.errText)))
		if op.snapshot.ID != "" {
			lines = append(lines, m.dim().Render("Showing the last known snapshot; detaching does not cancel the operation."))
		}
	}
	lines = append(lines, "")
	available := max(1, height-len(lines)-2)
	start := windowStart(m.detailScroll, len(op.events), available)
	for index := start; index < len(op.events) && len(lines) < height-2; index++ {
		event := op.events[index]
		line := fmt.Sprintf("%4d %-8s", event.Sequence, event.Kind)
		if event.Stage != "" {
			line += " " + event.Stage
		}
		if event.Message != "" {
			line += " " + event.Message
		}
		lines = append(lines, truncate(m.clean(line), width))
	}
	if len(op.events) == 0 {
		lines = append(lines, m.dim().Render("No durable events observed yet. Press r to refresh."))
	}
	lines = append(lines, m.dim().Render("r refresh · j/k scroll · esc back · q detach (never cancels)"))
	return fitLines(strings.Join(lines, "\n"), width, height)
}

func windowStart(selected, total, capacity int) int {
	if total <= capacity || capacity <= 0 {
		return 0
	}
	start := selected - capacity/2
	if start < 0 {
		start = 0
	}
	if start > total-capacity {
		start = total - capacity
	}
	return start
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

func wrapText(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	if text == "" {
		return nil
	}
	var lines []string
	var line strings.Builder
	for _, word := range strings.Fields(text) {
		candidate := word
		if line.Len() > 0 {
			candidate = " " + word
		}
		if lipgloss.Width(line.String()+candidate) > width && line.Len() > 0 {
			lines = append(lines, line.String())
			line.Reset()
			candidate = word
		}
		line.WriteString(candidate)
	}
	if line.Len() > 0 {
		lines = append(lines, line.String())
	}
	return lines
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

func fitLines(text string, width, height int) string {
	width, height = max(1, width), max(1, height)
	lines := strings.Split(text, "\n")
	style := lipgloss.NewStyle().MaxWidth(width)
	for index := range lines {
		lines[index] = style.Render(lines[index])
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}
