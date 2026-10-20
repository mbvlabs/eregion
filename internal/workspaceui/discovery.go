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
			ID: "app.create", Name: "Create application", Keys: "a", Form: "app",
			CLI: "eregion platform apps create <app> --idempotency-key <key> --json", CommandPath: "eregion platform apps create",
		},
		{
			ID: "environment.create", Name: "Create environment", Keys: "e", Form: "env",
			CLI:         "eregion platform environments create <app> <environment> --node <node> --container-port <port> --host-port <port> --health-path <path> --idempotency-key <key> --json",
			CommandPath: "eregion platform environments create",
		},
		{
			ID: "release.create", Name: "Create image release", Keys: "i", Form: "release",
			CLI:         "eregion platform releases create <app> <environment> --image <image> --idempotency-key <key> --json",
			CommandPath: "eregion platform releases create",
		},
		{
			ID: "deployment.plan", Name: "Preview a deployment", Keys: "", Form: "",
			CLI:         "eregion platform deployments plan <app> <environment> --release-id <release> --json",
			CommandPath: "eregion platform deployments plan",
		},
		{
			ID: "deployment.deploy", Name: "Deploy release", Keys: "d", Form: "deploy",
			CLI:         "eregion platform deployments deploy <app> <environment> --release-id <release> --idempotency-key <key> --json",
			CommandPath: "eregion platform deployments deploy", Destructive: true,
		},
		{
			ID: "deployment.rollback", Name: "Roll back environment", Keys: "o", Form: "rollback",
			CLI: "eregion platform deployments rollback <app> <environment> --confirm <plan-token> --idempotency-key <key> --json", CommandPath: "eregion platform deployments rollback", Destructive: true,
		},
		{
			ID: "node.check", Name: "Check node runtime", Keys: "n", Form: "node-check",
			CLI: "eregion platform nodes check <node> --idempotency-key <key> --json", CommandPath: "eregion platform nodes check",
		},
		{
			ID: "resource.destroy", Name: "Destroy application resource", Keys: "x", Form: "resource-destroy",
			CLI:         "eregion platform submit --kind platform.resource.destroy --target node/<host> --input-file action.json --idempotency-key <key> --json",
			CommandPath: "eregion platform submit", Destructive: true,
		},
		{
			ID: "backup.create", Name: "Create resource backup", Keys: "b", Form: "backup-create",
			CLI:         "eregion platform submit --kind platform.backup.create --target node/<host> --input-file action.json --idempotency-key <key> --json",
			CommandPath: "eregion platform submit",
		},
		{
			ID: "backup.verify", Name: "Verify backup archive", Keys: "v", Form: "backup-verify",
			CLI:         "eregion platform submit --kind platform.backup.verify --target node/<host> --input-file action.json --idempotency-key <key> --json",
			CommandPath: "eregion platform submit",
		},
		{
			ID: "backup.restore", Name: "Restore verified backup", Keys: "R", Form: "backup-restore",
			CLI:         "eregion platform submit --kind platform.backup.restore --target node/<host> --input-file action.json --idempotency-key <key> --json",
			CommandPath: "eregion platform submit", Destructive: true,
		},
		{
			ID: "operations.get", Name: "Read an operation snapshot", Keys: "", Form: "",
			CLI: "eregion operations get <operation_id> --json", CommandPath: "eregion operations get",
		},
		{
			ID: "operations.follow", Name: "Follow an operation", Keys: "f", Form: "",
			CLI: "eregion operations follow <operation_id> --interactive", CommandPath: "eregion operations follow",
		},
		{
			ID: "platform.fleet", Name: "Read the fleet snapshot", Keys: "r", Form: "",
			CLI: "eregion platform fleet --json", CommandPath: "eregion platform fleet",
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
	case "app":
		form = m.newAppForm()
	case "env":
		form = m.newEnvironmentForm()
	case "release":
		form = m.newReleaseForm()
	case "deploy":
		form = m.newDeployForm()
	case "rollback":
		form = m.newRollbackForm()
	case "node-check":
		form = m.newNodeCheckForm()
	case "resource-destroy":
		form = m.newResourceDestroyForm()
	case "backup-create":
		form = m.newBackupCreateForm()
	case "backup-verify":
		form = m.newBackupVerifyForm()
	case "backup-restore":
		form = m.newBackupRestoreForm()
	default:
		m.notice = "This action has no guided form."
		return
	}
	if form == nil {
		return
	}
	m.form = form
}
