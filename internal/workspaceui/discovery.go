package workspaceui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// CommandAction is one discoverable action with its exact noninteractive
// equivalent. CommandPath is the implemented CLI command subtree the equivalent
// resolves to; the catalog test checks it against `eregion commands --json`.
// Form identifies the guided form the interactive key opens; an empty form is a
// read-only command that the palette only documents.
type CommandAction struct {
	ID          string
	Name        string
	Keys        string
	Form        string
	CLI         string
	CommandPath string
	Destructive bool
}

// CommandActions is the single mapping table used by the palette, the help
// view, and docs/workspace.md. Only implemented actions belong here.
func CommandActions() []CommandAction {
	return []CommandAction{
		{
			ID: "deployment.deploy", Name: "Deploy an environment", Keys: "d", Form: "deploy",
			CLI:         "eregion deploy <app> <environment> --release <release> --idempotency-key <key> --json",
			CommandPath: "eregion deploy", Destructive: true,
		},
		{
			ID: "deployment.rollback", Name: "Roll back environment", Keys: "o", Form: "rollback",
			CLI: "eregion rollback <app> <environment> --confirm <app>/<environment> --idempotency-key <key> --json", CommandPath: "eregion rollback", Destructive: true,
		},
		{
			ID: "deployment.restart", Name: "Restart an environment", Keys: "", Form: "",
			CLI: "eregion restart <app> <environment> --idempotency-key <key> --json", CommandPath: "eregion restart",
		},
		{
			ID: "operations.get", Name: "Read an operation snapshot", Keys: "", Form: "",
			CLI: "eregion operations get <operation_id> --json", CommandPath: "eregion operations get",
		},
		{
			ID: "operations.follow", Name: "Follow an operation", Keys: "f", Form: "",
			CLI: "eregion operations follow <operation_id> --interactive", CommandPath: "eregion operations follow",
		},
	}
}

func (m *Model) openPalette() {
	m.paletteOpen = true
	m.paletteIndex = 0
	m.paletteFilter.SetValue("")
	m.paletteFilter.Focus()
}

func (m Model) visibleActions() []CommandAction {
	query := strings.ToLower(strings.TrimSpace(m.paletteFilter.Value()))
	if query == "" {
		return CommandActions()
	}
	actions := make([]CommandAction, 0, len(CommandActions()))
	for _, action := range CommandActions() {
		haystack := strings.ToLower(action.Name + " " + action.Keys + " " + action.CLI + " " + action.ID)
		if strings.Contains(haystack, query) {
			actions = append(actions, action)
		}
	}
	return actions
}

func (m Model) handlePaletteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	actions := m.visibleActions()
	index := m.paletteIndex
	switch message.String() {
	case "esc":
		m.paletteOpen = false
		m.paletteFilter.Blur()
		return m, nil
	case "up", "ctrl+p":
		if index > 0 {
			m.paletteIndex--
		}
		return m, nil
	case "down", "ctrl+n":
		if index+1 < len(actions) {
			m.paletteIndex++
		}
		return m, nil
	case "enter":
		if index < 0 || index >= len(actions) {
			return m, nil
		}
		action := actions[index]
		m.paletteOpen = false
		m.paletteFilter.Blur()
		if action.Form == "" {
			m.notice = "Noninteractive equivalent: " + action.CLI
			return m, nil
		}
		m.openForm(action.Form)
		return m, nil
	}
	m.paletteFilter.Update(message)
	m.paletteIndex = 0
	return m, nil
}

// openForm starts one guided form. A missing prerequisite is stated in the
// notice line instead of opening a form that cannot build a valid action.
func (m *Model) openForm(id string) {
	m.notice = ""
	if m.opts.API == nil {
		m.notice = "This workspace has no API client."
		return
	}
	var form *formState
	switch id {
	case "deploy":
		form = m.newDeployForm()
	case "rollback":
		form = m.newRollbackForm()
	default:
		m.notice = "This action has no guided form."
		return
	}
	if form == nil {
		return
	}
	m.form = form
}
