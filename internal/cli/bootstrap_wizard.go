package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/spf13/cobra"
)

// bootstrapWizardFlagNames is the noninteractive parity contract: every value
// the guided flow collects has the same flag on `eregion bootstrap`. The test
// below fails if a field is added without a flag.
var bootstrapWizardFlagNames = []string{
	"role", "node", "database-url-file", "operator-token-out",
	"automation-subject", "automation-scope", "automation-token-out",
	"binary", "node-binary", "api-listen", "api-tls-cert", "api-tls-key",
	"node-authorized-keys-file", "worker-concurrency", "commit-confirm-seconds",
}

type wizardField struct {
	Key      string
	Label    string
	Value    string
	Required bool
}

func bootstrapWizardFields(cfg host.Config) []wizardField {
	return []wizardField{
		{Key: "role", Label: "Role (main, application, builder, telemetry)", Value: string(cfg.Role), Required: true},
		{Key: "node", Label: "Node name (local and main name the local identity)", Value: cfg.Node, Required: true},
		{Key: "database-url-file", Label: "External database URL file (main role)", Value: ""},
		{Key: "operator-token-out", Label: "Operator token output on the host (main role)", Value: cfg.OperatorTokenOut, Required: true},
		{Key: "automation-subject", Label: "Optional automation subject", Value: cfg.AutomationSubject},
		{Key: "automation-scope", Label: "Optional automation scopes (comma separated)", Value: strings.Join(cfg.AutomationScopes, ",")},
		{Key: "automation-token-out", Label: "Optional automation token output", Value: cfg.AutomationTokenOut},
		{Key: "binary", Label: "Control-plane binary source", Value: cfg.BinaryPath},
		{Key: "node-binary", Label: "Node binary source (node roles)", Value: cfg.NodeBinaryPath},
		{Key: "api-listen", Label: "API listener", Value: cfg.APIListen, Required: true},
		{Key: "api-tls-cert", Label: "API TLS certificate source", Value: cfg.APITLSCert},
		{Key: "api-tls-key", Label: "API TLS private key source", Value: cfg.APITLSKey},
		{Key: "node-authorized-keys-file", Label: "Node authorized_keys source", Value: cfg.NodeAuthorizedKeysFile},
		{Key: "worker-concurrency", Label: "Worker concurrency (1–16)", Value: fmt.Sprint(cfg.WorkerConcurrency), Required: true},
		{Key: "commit-confirm-seconds", Label: "Rollback window seconds (30–900)", Value: fmt.Sprint(cfg.CommitConfirmSeconds), Required: true},
	}
}

type wizardPhase int

const (
	wizardEdit wizardPhase = iota
	wizardPlanning
	wizardReview
	wizardApplying
	wizardDone
	wizardFailed
)

type wizardPlanMsg struct {
	plan host.Plan
	err  error
}

type wizardApplyMsg struct {
	report host.ApplyReport
	err    error
}

type wizardModel struct {
	app    *app
	cmd    *cobra.Command
	fields []wizardField
	focus  int
	input  wizardInput
	phase  wizardPhase
	plan   host.Plan
	report host.ApplyReport
	err    error
	width  int
	height int
	quit   bool
}

type wizardInput struct {
	value  []rune
	cursor int
	limit  int
}

func newWizardInput(value string, limit int) wizardInput {
	runes := []rune(value)
	if limit <= 0 {
		limit = 1024
	}
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return wizardInput{value: runes, cursor: len(runes), limit: limit}
}

func (i *wizardInput) Set(value string) { *i = newWizardInput(value, i.limit) }
func (i wizardInput) Value() string     { return string(i.value) }

func (i *wizardInput) Update(message tea.KeyPressMsg) {
	switch message.Code {
	case tea.KeyLeft:
		i.cursor = max(0, i.cursor-1)
	case tea.KeyRight:
		i.cursor = min(len(i.value), i.cursor+1)
	case tea.KeyHome:
		i.cursor = 0
	case tea.KeyEnd:
		i.cursor = len(i.value)
	case tea.KeyBackspace:
		if i.cursor > 0 {
			i.value = append(i.value[:i.cursor-1], i.value[i.cursor:]...)
			i.cursor--
		}
	case tea.KeyDelete:
		if i.cursor < len(i.value) {
			i.value = append(i.value[:i.cursor], i.value[i.cursor+1:]...)
		}
	default:
		for _, r := range message.Text {
			if unicode.IsControl(r) || i.cursor >= i.limit {
				continue
			}
			i.value = append(i.value, 0)
			copy(i.value[i.cursor+1:], i.value[i.cursor:])
			i.value[i.cursor] = r
			i.cursor++
		}
	}
}

func (m wizardModel) Init() tea.Cmd { return nil }

func (m wizardModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, message.Width), max(1, message.Height)
		return m, nil
	case wizardPlanMsg:
		if message.err != nil {
			m.err, m.phase = message.err, wizardFailed
			return m, nil
		}
		m.plan, m.err, m.phase = message.plan, nil, wizardReview
		m.input = newWizardInput("", 32)
		return m, nil
	case wizardApplyMsg:
		m.report = message.report
		if message.err != nil {
			m.err, m.phase = message.err, wizardFailed
			return m, nil
		}
		m.err, m.phase = nil, wizardDone
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(message)
	}
	return m, nil
}

func (m wizardModel) handleKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	pressed := message.String()
	if pressed == "ctrl+c" {
		m.quit = true
		return m, tea.Quit
	}
	switch m.phase {
	case wizardPlanning, wizardApplying:
		return m, nil
	case wizardDone, wizardFailed:
		if pressed == "enter" || pressed == "q" || pressed == "esc" {
			m.quit = true
			return m, tea.Quit
		}
		return m, nil
	case wizardReview:
		switch pressed {
		case "esc":
			m.phase = wizardEdit
			m.focus = 0
			m.loadFocus()
			return m, nil
		case "enter":
			if m.input.Value() != "apply" {
				m.err = contract.NewFault(contract.ValidationFailed, "type apply to confirm", "This is an explicit confirmation, not a default.")
				return m, nil
			}
			m.phase = wizardApplying
			m.err = nil
			return m, m.applyCommand()
		}
		m.input.Update(message)
		return m, nil
	}
	// edit
	switch pressed {
	case "esc":
		m.quit = true
		return m, tea.Quit
	case "tab", "down":
		m.storeFocus()
		m.focus = (m.focus + 1) % len(m.fields)
		m.loadFocus()
		return m, nil
	case "shift+tab", "up":
		m.storeFocus()
		m.focus = (m.focus + len(m.fields) - 1) % len(m.fields)
		m.loadFocus()
		return m, nil
	case "enter":
		m.storeFocus()
		if m.focus+1 < len(m.fields) {
			m.focus++
			m.loadFocus()
			return m, nil
		}
		if err := m.validateEdit(); err != nil {
			m.err = err
			return m, nil
		}
		m.phase = wizardPlanning
		m.err = nil
		return m, m.planCommand()
	}
	m.input.Update(message)
	m.fields[m.focus].Value = m.input.Value()
	return m, nil
}

func (m *wizardModel) loadFocus() {
	if m.focus < 0 || m.focus >= len(m.fields) {
		return
	}
	m.input = newWizardInput(m.fields[m.focus].Value, 1024)
}

func (m *wizardModel) storeFocus() {
	if m.focus >= 0 && m.focus < len(m.fields) {
		m.fields[m.focus].Value = m.input.Value()
	}
}

func (m wizardModel) validateEdit() error {
	for _, field := range m.fields {
		if field.Required && strings.TrimSpace(field.Value) == "" {
			return contract.NewFault(contract.InputRequired, field.Label+" is required", "Give every required field a value; the same value is available as --"+field.Key+".")
		}
	}
	return nil
}

func (m wizardModel) applyConfig() (host.Config, error) {
	for _, field := range m.fields {
		if err := m.cmd.Flags().Set(field.Key, field.Value); err != nil {
			return host.Config{}, contract.NewFault(contract.ValidationFailed, "invalid value for --"+field.Key, "Use the same value you would pass on the command line.")
		}
	}
	return m.app.bootstrapConfig(m.cmd)
}

func (m wizardModel) planCommand() tea.Cmd {
	return func() tea.Msg {
		cfg, err := m.applyConfig()
		if err != nil {
			return wizardPlanMsg{err: err}
		}
		runner, err := host.NewRunner(cfg, m.app.secrets)
		if err != nil {
			return wizardPlanMsg{err: err}
		}
		plan, err := runner.Plan(context.Background())
		return wizardPlanMsg{plan: plan, err: err}
	}
}

func (m wizardModel) applyCommand() tea.Cmd {
	return func() tea.Msg {
		cfg, err := m.applyConfig()
		if err != nil {
			return wizardApplyMsg{err: err}
		}
		runner, err := host.NewRunner(cfg, m.app.secrets)
		if err != nil {
			return wizardApplyMsg{err: err}
		}
		runner.Reporter = bootstrapReporter(m.app, m.cmd)
		report, err := runner.Apply(context.Background(), m.plan.Confirmation.Token, false)
		return wizardApplyMsg{report: report, err: err}
	}
}

func (m wizardModel) View() tea.View {
	view := tea.NewView(m.render())
	view.WindowTitle = "eregion · bootstrap"
	return view
}

func (m wizardModel) render() string {
	width := m.width
	if width <= 0 {
		width = 100
	}
	style := lipgloss.NewStyle()
	dim := style.Foreground(lipgloss.Color("#92a6bb"))
	accent := style.Bold(true).Foreground(lipgloss.Color("#ffb547"))
	risk := style.Foreground(lipgloss.Color("#ff7b6b"))
	var out strings.Builder
	out.WriteString(accent.Render("eregion bootstrap") + dim.Render("  guided setup") + "\n\n")
	switch m.phase {
	case wizardEdit:
		out.WriteString("Enter the same inputs as the flags. Enter next, tab move, ctrl+c cancel.\n\n")
		for index, field := range m.fields {
			cursor := "  "
			if index == m.focus {
				cursor = "> "
			}
			value := field.Value
			if index == m.focus {
				value = m.input.Value() + "_"
			}
			fmt.Fprintf(&out, "%s%-28s %s\n", cursor, field.Label, value)
		}
	case wizardPlanning:
		out.WriteString("Planning (read-only)...\n")
	case wizardReview:
		out.WriteString("Plan and diff\n")
		fmt.Fprintf(&out, "  node: %s  role: %s  generation: %d\n", m.plan.Node, m.plan.Role, m.plan.Generation)
		fmt.Fprintf(&out, "  supported: %t  ready: %t  resumed: %t\n", m.plan.Supported, m.plan.Ready, m.plan.Resumed)
		for _, step := range m.plan.Steps {
			state := "ok"
			if !step.Complete {
				state = "change"
			}
			if step.Error != "" {
				state = "error: " + step.Error
			}
			fmt.Fprintf(&out, "  [%s] %s — %s\n", state, step.ID, step.Description)
			for _, line := range step.Diff {
				fmt.Fprintf(&out, "      %s\n", line)
			}
		}
		if len(m.plan.AccessChanges) > 0 {
			out.WriteString(risk.Render("\nAccess-changing apply; a rollback watchdog will revert unless confirmed:") + "\n")
			for _, change := range m.plan.AccessChanges {
				fmt.Fprintf(&out, "  - %s\n", change)
			}
		}
		out.WriteString("\nType apply and press enter to confirm. Esc goes back.\n> " + m.input.Value() + "_")
	case wizardApplying:
		out.WriteString("Applying. Step progress is written to stderr; do not interrupt.\n")
	case wizardDone:
		out.WriteString("Bootstrap completed.\n")
		out.WriteString(fmt.Sprintf("  operation: %s  generation: %d\n", m.report.OperationID, m.report.Generation))
		if m.report.Pending {
			out.WriteString(risk.Render("Access-changing apply is pending commit confirmation.") + "\n")
			out.WriteString("  confirm: eregion bootstrap confirm " + m.report.OperationID + "\n")
		}
		out.WriteString("\nEnter to close.")
	case wizardFailed:
		out.WriteString(risk.Render("Bootstrap did not complete.") + "\n")
		if m.err != nil {
			out.WriteString("  " + m.err.Error() + "\n")
		}
		if m.report.OperationID != "" {
			out.WriteString("  operation: " + m.report.OperationID + " (completed steps are resumable)\n")
		}
		out.WriteString("\nEnter to close; inspect the journal before retrying.")
	}
	if m.err != nil && m.phase != wizardFailed {
		out.WriteString("\n" + risk.Render(m.err.Error()) + "\n")
	}
	lines := strings.Split(out.String(), "\n")
	if m.height > 0 && len(lines) > m.height {
		lines = lines[len(lines)-m.height:]
	}
	return strings.Join(lines, "\n")
}

type bootstrapWizardSummary struct {
	Plan   host.Plan
	Report host.ApplyReport
	Error  error
}

func (a *app) bootstrapWizard(cmd *cobra.Command) (result, error) {
	jsonMode, _ := cmd.Flags().GetBool("json")
	outputMode, _ := cmd.Flags().GetString("output")
	if jsonMode || outputMode != "plain" {
		return result{}, contract.NewFault(contract.InvalidOutputMode, "the guided bootstrap cannot produce machine output", "Use the flag form with --json for noninteractive or agent use.")
	}
	streams := followConfig(cmd)
	if streams == nil || !isTerminal(streams.in) || !isTerminal(streams.out) {
		return result{}, contract.NewFault(contract.Usage, "the guided bootstrap requires a terminal", "Run from a TTY, or pass the same values as flags; every field has a flag equivalent.")
	}
	cfg, err := a.bootstrapConfig(cmd)
	if err != nil {
		return result{}, err
	}
	model := wizardModel{app: a, cmd: cmd, fields: bootstrapWizardFields(cfg), width: 100, height: 30}
	model.loadFocus()
	program := tea.NewProgram(model, tea.WithContext(cmd.Context()), tea.WithInput(streams.in), tea.WithOutput(streams.out))
	final, runErr := program.Run()
	summary := bootstrapWizardSummary{}
	if wizard, ok := final.(wizardModel); ok {
		summary.Plan, summary.Report, summary.Error = wizard.plan, wizard.report, wizard.err
	}
	if runErr != nil && !errors.Is(runErr, tea.ErrProgramKilled) && !errors.Is(runErr, tea.ErrInterrupted) {
		return result{}, contract.NewFault(contract.Internal, "the guided bootstrap stopped unexpectedly", "Use the flag form; completed steps remain in the journal.")
	}
	if summary.Error != nil {
		return hostResult(summary.Report), summary.Error
	}
	if summary.Report.OperationID == "" {
		return result{}, contract.NewFault(contract.Cancelled, "guided bootstrap cancelled before apply", "No host mutation was started. Re-run with the same values as flags if you intended to apply.")
	}
	return hostResult(summary.Report), nil
}
