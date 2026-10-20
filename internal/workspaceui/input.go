package workspaceui

import (
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
)

// textField is the small single-line editor used by filters and guided forms.
// It deliberately has no clipboard, history, or completion side effects: every
// keystroke stays inside the model and is testable without a terminal.
type textField struct {
	value   []rune
	cursor  int
	limit   int
	focused bool
}

func newTextField(limit int) textField {
	if limit <= 0 {
		limit = 512
	}
	return textField{limit: limit}
}

func (f textField) Value() string { return string(f.value) }

func (f *textField) SetValue(value string) {
	runes := []rune(value)
	if len(runes) > f.limit {
		runes = runes[:f.limit]
	}
	f.value = runes
	f.cursor = len(runes)
}

func (f *textField) CursorEnd()   { f.cursor = len(f.value) }
func (f *textField) Focus()       { f.focused = true }
func (f *textField) Blur()        { f.focused = false }
func (f textField) Focused() bool { return f.focused }

// Update applies one key. Key handling for enter, escape, and field navigation
// happens in the workspace model before this method is called.
func (f *textField) Update(message tea.KeyPressMsg) {
	if message.Mod&(tea.ModCtrl|tea.ModAlt) != 0 || message.Text == "" {
		switch message.Code {
		case tea.KeyLeft:
			f.cursor = max(0, f.cursor-1)
		case tea.KeyRight:
			f.cursor = min(len(f.value), f.cursor+1)
		case tea.KeyHome:
			f.cursor = 0
		case tea.KeyEnd:
			f.cursor = len(f.value)
		case tea.KeyBackspace:
			if f.cursor > 0 {
				f.value = append(f.value[:f.cursor-1], f.value[f.cursor:]...)
				f.cursor--
			}
		case tea.KeyDelete:
			if f.cursor < len(f.value) {
				f.value = append(f.value[:f.cursor], f.value[f.cursor+1:]...)
			}
		}
		return
	}
	inserted := []rune(message.Text)
	for _, r := range inserted {
		if unicode.IsControl(r) || f.cursor >= f.limit {
			continue
		}
		f.value = append(f.value, 0)
		copy(f.value[f.cursor+1:], f.value[f.cursor:])
		f.value[f.cursor] = r
		f.cursor++
	}
}

func (f textField) View() string {
	value := string(f.value)
	if f.focused {
		value += "_"
	}
	return value
}

func (f textField) SearchString() string { return strings.ToLower(strings.TrimSpace(string(f.value))) }
