package workspaceui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/viewer"
)

// Tab identifies one inventory workspace.
type Tab int

const (
	Fleet Tab = iota
	Apps
	Envs
	Resources
	Backups
	tabCount
)

func (t Tab) String() string {
	switch t {
	case Fleet:
		return "Fleet"
	case Apps:
		return "Apps"
	case Envs:
		return "Envs"
	case Resources:
		return "Resources"
	case Backups:
		return "Backups"
	default:
		return "Unknown"
	}
}

// Collection is the platform inventory collection backing the tab.
func (t Tab) Collection() string {
	switch t {
	case Fleet:
		return "nodes"
	case Apps:
		return "apps"
	case Envs:
		return "environments"
	case Resources:
		return "resources"
	case Backups:
		return "backups"
	default:
		return ""
	}
}

// ParseTab accepts the stable lower-case tab names used by --tab.
func ParseTab(value string) (Tab, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "fleet":
		return Fleet, nil
	case "apps", "app":
		return Apps, nil
	case "envs", "env", "environments":
		return Envs, nil
	case "resources", "resource":
		return Resources, nil
	case "backups", "backup":
		return Backups, nil
	default:
		return Fleet, contract.NewFault(contract.Usage, "unknown workspace tab", "Use fleet, apps, envs, resources, or backups.")
	}
}

// Detail is one labelled field in a details view.
type Detail struct {
	Label string
	Value string
}

// Item is one sanitized inventory row.
type Item struct {
	Key        string
	Title      string
	Subtitle   string
	Status     string
	Details    []Detail
	SearchText string
	Data       map[string]string
}

type loadState int

const (
	loadIdle loadState = iota
	loadLoading
	loadReady
	loadFailed
)

type tabState struct {
	state     loadState
	items     []Item
	fetchedAt time.Time
	errText   string
	errCode   contract.Code
}

// Options configures one workspace session. All terminal flags are explicit;
// the UI never probes or prompts unless the caller has opened an interactive
// terminal deliberately.
type Options struct {
	API           API
	Host          string
	Secrets       *secrets.Registry
	ASCII         bool
	NoColor       bool
	ReducedMotion bool
	InitialTab    Tab
	Now           func() time.Time
	Key           func() string
	In            io.Reader
	Out           io.Writer
	Width         int
	Height        int
}

func (o Options) normalized() Options {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Key == nil {
		o.Key = newIdempotencyKey
	}
	if o.Width <= 0 {
		o.Width = 100
	}
	if o.Height <= 0 {
		o.Height = 30
	}
	return o
}

func newIdempotencyKey() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "ui:fallback-key"
	}
	return "ui:" + hex.EncodeToString(value[:])
}

// Summary is the outcome of one interactive session.
type Summary struct {
	Detached   bool
	Tab        string
	Operations []contract.OperationID
}

type inventoryMsg struct {
	tab   Tab
	items []Item
	at    time.Time
	err   error
}

type previewMsg struct {
	revision int
	action   Action
	preview  Preview
	err      error
	at       time.Time
}

type submitMsg struct {
	revision int
	result   SubmitResult
	err      error
	at       time.Time
}

type operationMsg struct {
	id       contract.OperationID
	snapshot contract.Operation
	events   []contract.Event
	err      error
	at       time.Time
}

type tickMsg time.Time

type opView struct {
	reducer  *viewer.Model
	id       contract.OperationID
	snapshot contract.Operation
	events   []contract.Event
	errText  string
	at       time.Time
	loading  bool
}

// Model is the pure workspace state. Update performs no I/O; Bubble Tea commands
// returned from Update own every API call.
type Model struct {
	opts Options
	now  time.Time

	width, height int
	tab           Tab
	tabs          [tabCount]tabState
	selected      [tabCount]int

	filter     textField
	filterText string
	filtering  bool

	detail       bool
	detailScroll int
	showHelp     bool

	form *formState
	op   *opView

	spinner spinner.Model
	help    help.Model
	keys    keyMap

	detached bool
	summary  Summary
}

// New constructs a workspace model. Init performs the first inventory read.
func New(opts Options) Model {
	opts = opts.normalized()
	filter := newTextField(128)
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	if opts.ASCII {
		spin = spinner.New(spinner.WithSpinner(spinner.Line))
	}
	helpModel := help.New()
	if opts.NoColor {
		helpModel.Styles = help.Styles{}
		helpModel.ShortSeparator = " | "
		helpModel.FullSeparator = "    "
	}
	model := Model{
		opts:    opts,
		now:     opts.Now(),
		width:   opts.Width,
		height:  opts.Height,
		tab:     opts.InitialTab,
		filter:  filter,
		spinner: spin,
		help:    helpModel,
		keys:    defaultKeys(),
	}
	model.tabs[opts.InitialTab].state = loadLoading
	model.summary.Tab = opts.InitialTab.String()
	return model
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.loadTabCommand(), tickCommand())
}

func (m Model) loadTabCommand() tea.Cmd {
	api, tab, now, registry := m.opts.API, m.tab, m.opts.Now, m.opts.Secrets
	if api == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		raw, err := api.PlatformInventory(ctx, tab.Collection(), url.Values{"limit": {fmt.Sprint(InventoryLimit)}})
		if err != nil {
			return inventoryMsg{tab: tab, at: now(), err: err}
		}
		items, decodeErr := decodeInventory(tab, raw, func(text string) string { return Sanitize(registry, text) })
		if decodeErr != nil {
			return inventoryMsg{tab: tab, at: now(), err: decodeErr}
		}
		return inventoryMsg{tab: tab, items: items, at: now()}
	}
}

func (m Model) clean(text string) string {
	return Sanitize(m.opts.Secrets, text)
}

func (m Model) spinnerActive() bool {
	for _, tab := range m.tabs {
		if tab.state == loadLoading {
			return true
		}
	}
	if m.form != nil && (m.form.phase == formChecking || m.form.phase == formSubmitting) {
		return true
	}
	return m.op != nil && m.op.loading
}

func (m Model) current() tabState { return m.tabs[m.tab] }

func (m *Model) setTab(tab Tab) tea.Cmd {
	if tab < 0 || tab >= tabCount || m.tab == tab {
		return nil
	}
	m.tab = tab
	m.summary.Tab = tab.String()
	m.detail = false
	m.detailScroll = 0
	if m.tabs[tab].state == loadIdle {
		m.tabs[tab].state = loadLoading
		return tea.Batch(m.loadTabCommand(), m.spinCommand())
	}
	return nil
}

func (m *Model) refresh() tea.Cmd {
	if m.current().state == loadLoading || m.opts.API == nil {
		return nil
	}
	m.tabs[m.tab].state = loadLoading
	return tea.Batch(m.loadTabCommand(), m.spinCommand())
}

func (m *Model) spinCommand() tea.Cmd {
	if m.opts.ReducedMotion {
		return nil
	}
	return m.spinner.Tick
}

func (m Model) visibleItems() []Item {
	items := m.current().items
	if m.filterText == "" {
		return items
	}
	query := strings.ToLower(m.filterText)
	filtered := make([]Item, 0, len(items))
	for _, item := range items {
		if strings.Contains(strings.ToLower(item.Title+" "+item.Subtitle+" "+item.Status+" "+item.SearchText), query) {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func (m Model) selectedItem() (Item, bool) {
	items := m.visibleItems()
	index := m.selected[m.tab]
	if index < 0 || index >= len(items) {
		return Item{}, false
	}
	return items[index], true
}

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = max(1, message.Width)
		m.height = max(1, message.Height)
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(message)
	case tickMsg:
		m.now = time.Time(message)
		if m.op != nil && m.op.reducer != nil {
			m.op.reducer.Tick(m.now)
		}
		if m.op != nil && !m.op.loading && (!m.op.snapshot.State.Terminal() || m.op.reducer.Cursor < m.op.snapshot.LastSequence) && m.now.Sub(m.op.at) >= 3*time.Second {
			m.op.loading = true
			return m, tea.Batch(m.operationCommand(m.op.id), m.spinCommand(), tickCommand())
		}
		return m, tickCommand()
	case spinner.TickMsg:
		var command tea.Cmd
		m.spinner, command = m.spinner.Update(message)
		if m.opts.ReducedMotion || !m.spinnerActive() {
			command = nil
		}
		return m, command
	case inventoryMsg:
		m.applyInventory(message)
		return m, nil
	case previewMsg:
		m.applyPreview(message)
		return m, nil
	case submitMsg:
		m.applySubmit(message)
		return m, nil
	case operationMsg:
		m.applyOperation(message)
		return m, nil
	}
	return m, nil
}

func (m *Model) applyInventory(message inventoryMsg) {
	state := &m.tabs[message.tab]
	if message.err != nil {
		state.state = loadFailed
		state.errText = ErrorText(message.err, m.clean)
		state.errCode = CodeOf(message.err)
	} else {
		state.fetchedAt = message.at
		state.state = loadReady
		state.items = message.items
		state.errText, state.errCode = "", ""
	}
	if m.selected[message.tab] >= len(state.items) {
		m.selected[message.tab] = max(0, len(state.items)-1)
	}
}

func (m *Model) applyPreview(message previewMsg) {
	if m.form == nil || message.revision != m.form.revision {
		return
	}
	m.form.at = message.at
	if message.err != nil {
		m.form.phase = formEdit
		m.form.errText = ErrorText(message.err, m.clean)
		m.form.preview = nil
		return
	}
	m.form.phase = formPreview
	m.form.preview = &message.preview
	// Preview input is display-only and may be redacted by the server. Submit
	// the exact action that was previewed, never a redacted display projection.
	approved := message.action
	m.form.approved = &approved
	m.form.target = approved.Target
	if approved.ConfirmPhrase != "" {
		m.form.confirm.Focus()
	}
	m.form.errText = ""
}

func (m *Model) applySubmit(message submitMsg) {
	if m.form == nil || message.revision != m.form.revision {
		return
	}
	m.form.at = message.at
	if message.err != nil {
		m.form.phase = formPreview
		m.form.errText = ErrorText(message.err, m.clean)
		m.form.failed = true
		return
	}
	m.form.phase = formResult
	m.form.result = &message.result
	m.form.errText = ""
	id := message.result.Operation.ID
	found := false
	for _, existing := range m.summary.Operations {
		if existing == id {
			found = true
			break
		}
	}
	if !found {
		m.summary.Operations = append(m.summary.Operations, id)
	}
}

func (m *Model) applyOperation(message operationMsg) {
	if m.op == nil || m.op.id != message.id {
		return
	}
	m.op.loading = false
	m.op.at = message.at
	if message.err != nil && message.snapshot.ID == "" {
		m.op.reducer.PollFailed(message.err, message.at)
		m.op.errText = ErrorText(message.err, m.clean)
		return
	}
	m.op.errText = ""
	if message.snapshot.ID != "" {
		m.op.reducer.Snapshot(message.snapshot, message.at)
		m.op.snapshot = message.snapshot
		m.op.snapshot.Kind = m.op.reducer.Kind
		m.op.snapshot.Target = m.op.reducer.Target
		m.op.snapshot.Actor = m.op.reducer.Actor
	}
	if err := m.op.reducer.Page(message.events, message.at); err != nil {
		m.op.errText = ErrorText(err, m.clean)
	}
	m.op.events = append([]contract.Event(nil), m.op.reducer.EventLines...)
	if message.err != nil {
		m.op.reducer.PollFailed(message.err, message.at)
		m.op.errText = ErrorText(message.err, m.clean)
	}
}

func (m Model) handleKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	pressed := message.String()
	if pressed == "ctrl+c" {
		return m.detach()
	}
	if m.form != nil {
		return m.handleFormKey(message)
	}
	if m.filtering {
		return m.handleFilterKey(message)
	}
	if pressed == "q" {
		return m.detach()
	}
	if m.showHelp {
		m.showHelp = false
		return m, nil
	}
	if m.op != nil {
		return m.handleOperationKey(pressed)
	}
	if m.detail {
		return m.handleDetailKey(pressed)
	}
	return m.handleListKey(message, pressed)
}

func (m Model) detach() (tea.Model, tea.Cmd) {
	m.detached = true
	m.summary.Detached = true
	return m, tea.Quit
}

func (m Model) handleListKey(message tea.KeyPressMsg, pressed string) (tea.Model, tea.Cmd) {
	items := m.visibleItems()
	index := m.selected[m.tab]
	switch pressed {
	case "1":
		command := m.setTab(Fleet)
		return m, command
	case "2":
		command := m.setTab(Apps)
		return m, command
	case "3":
		command := m.setTab(Envs)
		return m, command
	case "4":
		command := m.setTab(Resources)
		return m, command
	case "5":
		command := m.setTab(Backups)
		return m, command
	case "tab":
		command := m.setTab((m.tab + 1) % tabCount)
		return m, command
	case "shift+tab":
		command := m.setTab((m.tab + tabCount - 1) % tabCount)
		return m, command
	case "up", "k":
		if index > 0 {
			m.selected[m.tab]--
		}
	case "down", "j":
		if index+1 < len(items) {
			m.selected[m.tab]++
		}
	case "home", "g":
		m.selected[m.tab] = 0
	case "end", "G":
		if len(items) > 0 {
			m.selected[m.tab] = len(items) - 1
		}
	case "pgup", "ctrl+b":
		m.selected[m.tab] = max(0, index-max(1, m.listCapacity()/2))
	case "pgdown", "ctrl+f":
		if len(items) > 0 {
			m.selected[m.tab] = min(len(items)-1, index+max(1, m.listCapacity()/2))
		}
	case "enter", " ":
		if _, ok := m.selectedItem(); ok {
			m.detail = true
			m.detailScroll = 0
		}
	case "/":
		m.filtering = true
		m.filter.SetValue(m.filterText)
		m.filter.Focus()
		return m, nil
	case "esc":
		if m.filterText != "" {
			m.filterText = ""
			m.filter.SetValue("")
			m.selected[m.tab] = 0
		}
	case "r":
		command := m.refresh()
		return m, command
	case "a":
		m.form = m.newAppForm()
	case "e":
		m.form = m.newEnvironmentForm()
	case "i":
		m.form = m.newReleaseForm()
	case "d":
		m.form = m.newDeployForm()
	case "?":
		m.showHelp = true
	}
	return m, nil
}

func (m Model) handleFilterKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "esc", "enter":
		m.filtering = false
		m.filter.Blur()
		return m, nil
	case "ctrl+u":
		m.filter.SetValue("")
		m.filterText = ""
		m.selected[m.tab] = 0
		return m, nil
	}
	m.filter.Update(message)
	m.filterText = m.filter.Value()
	m.selected[m.tab] = 0
	return m, nil
}

func (m Model) handleDetailKey(pressed string) (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok {
		m.detail = false
		return m, nil
	}
	switch pressed {
	case "esc", "enter":
		m.detail = false
		m.detailScroll = 0
	case "up", "k":
		m.detailScroll = max(0, m.detailScroll-1)
	case "down", "j":
		m.detailScroll += 1
	case "g", "home":
		m.detailScroll = 0
	case "G", "end":
		m.detailScroll = max(0, len(item.Details))
	case "r":
		command := m.refresh()
		return m, command
	case "a":
		m.form = m.newAppForm()
	case "e":
		m.form = m.newEnvironmentForm()
	case "i":
		m.form = m.newReleaseForm()
	case "d":
		m.form = m.newDeployForm()
	case "?":
		m.showHelp = true
	}
	return m, nil
}

func (m Model) handleOperationKey(pressed string) (tea.Model, tea.Cmd) {
	switch pressed {
	case "esc":
		m.op = nil
	case "r":
		if !m.op.loading {
			m.op.loading = true
			return m, tea.Batch(m.operationCommand(m.op.id), m.spinCommand())
		}
	case "f":
		command := m.followSelected()
		return m, command
	case "up", "k":
		m.detailScroll = max(0, m.detailScroll-1)
	case "down", "j":
		m.detailScroll += 1
	case "g", "home":
		m.detailScroll = 0
	case "G", "end":
		m.detailScroll = max(0, len(m.op.events))
	case "?":
		m.showHelp = true
	}
	return m, nil
}

func (m *Model) followSelected() tea.Cmd {
	if m.form == nil || m.form.result == nil {
		return nil
	}
	id := m.form.result.Operation.ID
	m.op = &opView{id: id, snapshot: m.form.result.Operation, loading: true, at: m.now,
		reducer: viewer.New(id, viewer.Options{Clean: m.clean})}
	m.form = nil
	return tea.Batch(m.operationCommand(id), m.spinCommand())
}

func (m Model) operationCommand(id contract.OperationID) tea.Cmd {
	api, now, registry := m.opts.API, m.opts.Now, m.opts.Secrets
	var after uint64
	if m.op != nil && m.op.reducer != nil {
		after = m.op.reducer.Cursor
	}
	if api == nil {
		return nil
	}
	clean := func(text string) string { return Sanitize(registry, text) }
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		raw, err := api.Get(ctx, id)
		if err != nil {
			return operationMsg{id: id, at: now(), err: err}
		}
		var snapshot contract.Operation
		if json.Unmarshal(raw, &snapshot) != nil || snapshot.Validate() != nil || snapshot.ID != id {
			return operationMsg{id: id, at: now(), err: invalidAPI("operation snapshot is invalid")}
		}
		message := operationMsg{id: id, snapshot: snapshot, at: now()}
		eventsRaw, eventsErr := api.Events(ctx, contract.Cursor{OperationID: id, After: after}, InventoryLimit)
		if eventsErr != nil {
			message.err = eventsErr
			return message
		}
		var page struct {
			Events []contract.Event `json:"events"`
		}
		if json.Unmarshal(eventsRaw, &page) != nil {
			message.err = invalidAPI("operation events are invalid")
			return message
		}
		for _, event := range page.Events {
			if event.OperationID != id || event.Validate() != nil {
				message.err = invalidAPI("operation events are invalid")
				return message
			}
			message.events = append(message.events, sanitizeEvent(event, clean))
		}
		return message
	}
}

func sanitizeEvent(event contract.Event, clean func(string) string) contract.Event {
	// Event messages originate on a node or in a worker and are hostile input.
	event.Message = clean(event.Message)
	event.Stage = clean(event.Stage)
	event.Kind = clean(event.Kind)
	return event
}

func sanitizeText(registry *secrets.Registry, text string) string {
	if registry != nil {
		text = registry.Redact(text)
	}
	return Sanitize(nil, text)
}

func (m Model) listCapacity() int {
	// Header, tab bar, and footer are rendered outside the row list.
	return max(1, m.height-7)
}

func tickCommand() tea.Cmd {
	return tea.Tick(time.Second, func(now time.Time) tea.Msg { return tickMsg(now) })
}

func (m Model) freshness(tab tabState) (string, bool) {
	if tab.fetchedAt.IsZero() {
		return "never loaded", true
	}
	age := m.now.Sub(tab.fetchedAt)
	if age < 0 {
		age = 0
	}
	switch {
	case age < 15*time.Second:
		return "fresh", false
	case age < 60*time.Second:
		return "updated " + shortDuration(age) + " ago", false
	default:
		return "last known " + shortDuration(age) + " ago", true
	}
}

func shortDuration(value time.Duration) string {
	if value < time.Minute {
		return fmt.Sprintf("%ds", int(value.Seconds()))
	}
	if value < time.Hour {
		return fmt.Sprintf("%dm", int(value.Minutes()))
	}
	return fmt.Sprintf("%dh", int(value.Hours()))
}

func (m Model) stale() bool {
	_, stale := m.freshness(m.current())
	return stale
}

// Key bindings are also the help text; they never advertise a key that the
// update loop does not implement.
type keyMap struct {
	Nav       key.Binding
	Tabs      key.Binding
	Filter    key.Binding
	Details   key.Binding
	Refresh   key.Binding
	Actions   key.Binding
	Help      key.Binding
	Detach    key.Binding
	Form      key.Binding
	Follow    key.Binding
	Operation key.Binding
}

func defaultKeys() keyMap {
	return keyMap{
		Nav:       key.NewBinding(key.WithKeys("j", "k", "g", "G"), key.WithHelp("j/k g/G", "move")),
		Tabs:      key.NewBinding(key.WithKeys("tab", "1-5"), key.WithHelp("tab/1-5", "workspace")),
		Filter:    key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Details:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "details")),
		Refresh:   key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Actions:   key.NewBinding(key.WithKeys("a", "e", "i", "d"), key.WithHelp("a e i d", "app env image deploy")),
		Help:      key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Detach:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "detach")),
		Form:      key.NewBinding(key.WithKeys("enter", "tab", "esc"), key.WithHelp("enter/tab", "field")),
		Follow:    key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "follow")),
		Operation: key.NewBinding(key.WithKeys("r", "esc"), key.WithHelp("r/esc", "refresh/back")),
	}
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Nav, k.Tabs, k.Filter, k.Details, k.Refresh, k.Actions, k.Help, k.Detach}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Nav, k.Tabs, k.Filter, k.Details},
		{k.Refresh, k.Actions, k.Help, k.Detach},
		{k.Form, k.Follow, k.Operation},
	}
}
