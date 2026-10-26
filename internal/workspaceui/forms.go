package workspaceui

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

type formPhase int

const (
	formEdit formPhase = iota
	formChecking
	formPreview
	formSubmitting
	formResult
)

type formField struct {
	label       string
	hint        string
	value       string
	placeholder string
	required    bool
}

type formState struct {
	kind          string
	title         string
	target        string
	confirmPhrase string
	fields        []*formField
	focus         int
	input         textField
	confirm       textField
	revision      int
	key           string
	phase         formPhase
	approved      *Action
	preview       *Preview
	planToken     string
	result        *SubmitResult
	errText       string
	failed        bool
	at            time.Time
}

func newForm(kind, title, target, confirmPhrase string, key string, fields []*formField) *formState {
	return &formState{
		kind: kind, title: title, target: target, confirmPhrase: confirmPhrase,
		fields: fields, input: newTextField(512), confirm: newTextField(128), key: key, phase: formEdit,
	}
}

func field(label, value, hint string, required bool) *formField {
	return &formField{label: label, value: value, hint: hint, placeholder: hint, required: required}
}

func (m Model) formContext() (app, environment, node string) {
	item, ok := m.selectedItem()
	if !ok {
		return "", "", ""
	}
	switch m.tab {
	case Fleet:
		node = item.Data["name"]
	case Apps:
		app = item.Data["name"]
	case Envs:
		app, environment, node = item.Data["app"], item.Data["environment"], item.Data["node"]
	}
	return app, environment, node
}

func (m Model) newDeployForm() *formState {
	app, environment, _ := m.formContext()
	release := ""
	if item, ok := m.selectedItem(); ok && m.tab == Envs {
		release = item.Data["desired_release"]
		if release == "" {
			release = item.Data["serving_release"]
		}
	}
	form := newForm(platform.KindDeploy, "Deploy release", "", environment, m.opts.Key(), []*formField{
		field("app", app, "api", true),
		field("environment", environment, "production", true),
		field("release", release, "rel_...", true),
	})
	form.input.Focus()
	return form
}

func (m Model) newRollbackForm() *formState {
	app, environment, _ := m.formContext()
	if m.tab != Envs || app == "" || environment == "" {
		m.notice = "Select an environment before planning a rollback."
		return nil
	}
	form := newForm(platform.KindRollback, "Roll back environment", "env/"+app+"/"+environment, app+"/"+environment, m.opts.Key(), []*formField{
		field("app", app, "api", true),
		field("environment", environment, "production", true),
		field("to release", "", "empty restores the last applied release", false),
	})
	form.input.Focus()
	return form
}

func (m Model) handleFormKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	form := m.form
	pressed := message.String()
	if pressed == "q" {
		switch form.phase {
		case formResult, formSubmitting, formChecking:
			return m.detach()
		case formPreview:
			if form.confirmPhrase == "" {
				return m.detach()
			}
		}
	}
	switch form.phase {
	case formResult:
		switch pressed {
		case "esc", "enter":
			m.form = nil
		case "f":
			command := m.followSelected()
			return m, command
		case "?":
			m.showHelp = true
		}
		return m, nil
	case formSubmitting:
		return m, nil
	case formPreview:
		return m.handlePreviewKey(message, pressed)
	case formChecking:
		if pressed == "esc" {
			form.errText = "wait for the dry-run to finish, or detach; the request cannot be cancelled"
		}
		return m, nil
	}
	// formEdit
	switch pressed {
	case "esc":
		m.form = nil
		return m, nil
	case "enter":
		if form.focus+1 < len(form.fields) {
			form.focus++
			form.setFocus()
			return m, nil
		}
		return m.startPreview()
	case "tab", "down":
		if form.focus+1 < len(form.fields) {
			form.focus++
			form.setFocus()
		}
		return m, nil
	case "shift+tab", "up":
		if form.focus > 0 {
			form.focus--
			form.setFocus()
		}
		return m, nil
	}
	previous := form.fields[form.focus].value
	form.input.Update(message)
	if form.input.Value() != previous {
		form.fields[form.focus].value = form.input.Value()
		form.revision++
		form.errText = ""
	}
	return m, nil
}

func (m Model) handlePreviewKey(message tea.KeyPressMsg, pressed string) (tea.Model, tea.Cmd) {
	form := m.form
	if form.confirmPhrase != "" && pressed != "esc" {
		if pressed == "enter" || (pressed == "s" && form.confirm.Value() == form.confirmPhrase) {
			if form.confirm.Value() != form.confirmPhrase {
				form.errText = "type " + form.confirmPhrase + " to confirm this action"
				return m, nil
			}
			return m.startSubmit()
		}
		form.confirm.Update(message)
		form.errText = ""
		return m, nil
	}
	switch pressed {
	case "esc":
		m.form = nil
		return m, nil
	case "e":
		form.phase = formEdit
		form.preview = nil
		form.approved = nil
		form.failed = false
		form.errText = ""
		form.setFocus()
		return m, nil
	case "s":
		if form.confirmPhrase != "" {
			if strings.TrimSpace(form.confirm.Value()) != form.confirmPhrase {
				form.errText = "type " + form.confirmPhrase + " to confirm this action"
				form.confirm.Focus()
				return m, nil
			}
		}
		return m.startSubmit()
	}
	if form.confirmPhrase != "" {
		form.confirm.Update(message)
		return m, nil
	}
	return m, nil
}

func (form *formState) setFocus() {
	form.input.SetValue(form.fields[form.focus].value)
	form.input.CursorEnd()
	form.input.Focus()
}

func (m Model) startPreview() (tea.Model, tea.Cmd) {
	form := m.form
	action, field, err := form.action()
	if err != nil {
		form.errText = m.clean(err.Error())
		if field >= 0 && field < len(form.fields) {
			form.focus = field
			form.setFocus()
		}
		return m, nil
	}
	form.target = action.Target
	form.phase = formChecking
	form.errText = ""
	form.revision++
	return m, tea.Batch(m.previewCommand(action, form.revision), m.spinCommand())
}

func (m Model) previewCommand(action Action, revision int) tea.Cmd {
	api, now := m.opts.API, m.opts.Now
	if api == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		raw, err := api.SubmitAction(ctx, action.Kind, action.Target, action.Input, "", "", true)
		if err != nil {
			return previewMsg{revision: revision, at: now(), err: err}
		}
		preview, err := DecodePreview(raw, action.Kind, action.Target)
		return previewMsg{revision: revision, action: action, preview: preview, err: err, at: now()}
	}
}

func (m Model) startSubmit() (tea.Model, tea.Cmd) {
	form := m.form
	if form.approved == nil {
		form.errText = "dry-run preview is required before submission"
		return m, nil
	}
	action := *form.approved
	if form.planToken != "" && action.PlanToken() != form.planToken {
		form.errText = "the previewed plan changed; run the dry run again"
		form.phase = formPreview
		return m, nil
	}
	if action.ConfirmPhrase != "" && strings.TrimSpace(form.confirm.Value()) != action.ConfirmPhrase {
		form.errText = "confirmation phrase does not match"
		return m, nil
	}
	form.phase = formSubmitting
	form.errText = ""
	return m, tea.Batch(m.submitCommand(action, form.revision), m.spinCommand())
}

func (m Model) submitCommand(action Action, revision int) tea.Cmd {
	api, now := m.opts.API, m.opts.Now
	if api == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		raw, err := api.SubmitAction(ctx, action.Kind, action.Target, action.Input, action.IdempotencyKey, action.Confirm, false)
		if err != nil {
			return submitMsg{revision: revision, at: now(), err: err}
		}
		result, err := DecodeSubmit(raw, action.Kind, action.Target)
		return submitMsg{revision: revision, result: result, err: err, at: now()}
	}
}

func (form *formState) action() (Action, int, error) {
	value := func(name string) string {
		for _, item := range form.fields {
			if item.label == name {
				return strings.TrimSpace(item.value)
			}
		}
		return ""
	}
	fieldIndex := func(name string) int {
		for index, item := range form.fields {
			if item.label == name {
				return index
			}
		}
		return -1
	}
	switch form.kind {
	case platform.KindDeploy:
		app, environment, release := value("app"), value("environment"), value("release")
		if app == "" || environment == "" || release == "" {
			for _, label := range []string{"app", "environment", "release"} {
				if value(label) == "" {
					return Action{}, fieldIndex(label), requiredValue(label, "", fieldIndex(label))
				}
			}
		}
		if err := platform.ValidateName(app); err != nil {
			return Action{}, fieldIndex("app"), err
		}
		if err := platform.ValidateName(environment); err != nil {
			return Action{}, fieldIndex("environment"), err
		}
		releaseID := platform.ReleaseID(release)
		if err := releaseID.Validate(); err != nil {
			return Action{}, fieldIndex("release"), err
		}
		raw, _ := json.Marshal(platform.DeployInput{App: app, Environment: environment, ReleaseID: releaseID})
		return Action{
			Kind: form.kind, Target: "env/" + app + "/" + environment, Input: raw, IdempotencyKey: form.key,
			Title: form.title, ConfirmPhrase: environment,
		}, -1, nil
	case platform.KindRollback:
		app, environment, release := value("app"), value("environment"), value("to release")
		if app == "" || environment == "" {
			for _, label := range []string{"app", "environment"} {
				if value(label) == "" {
					return Action{}, fieldIndex(label), requiredValue(label, "", fieldIndex(label))
				}
			}
		}
		if err := platform.ValidateName(app); err != nil {
			return Action{}, fieldIndex("app"), err
		}
		if err := platform.ValidateName(environment); err != nil {
			return Action{}, fieldIndex("environment"), err
		}
		input := platform.RollbackInput{App: app, Environment: environment}
		if release != "" {
			input.ToReleaseID = platform.ReleaseID(release)
			if err := input.ToReleaseID.Validate(); err != nil {
				return Action{}, fieldIndex("to release"), err
			}
		}
		raw, _ := json.Marshal(input)
		return Action{
			Kind: form.kind, Target: "env/" + app + "/" + environment, Input: raw, IdempotencyKey: form.key,
			Title: form.title, ConfirmPhrase: app + "/" + environment, Confirm: app + "/" + environment,
		}, -1, nil
	}
	return Action{}, -1, contract.NewFault(contract.Internal, "unsupported guided form", "Use a documented form; this is a binary defect.")
}

func requiredValue(label, value string, _ int) error {
	if value == "" {
		return contract.NewFault(contract.InputRequired, label+" is required", "Type a value for "+label+".")
	}
	return nil
}
