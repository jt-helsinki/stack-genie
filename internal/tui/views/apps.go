package views

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/apps"
	"github.com/jt-helsinki/stack-genie/internal/create"
	"github.com/jt-helsinki/stack-genie/internal/ui"
)

// AppLister returns the current workspace's apps + status. Injected so the view is
// testable; the parent wires the workspace AppManager's List over the live
// current project.
type AppLister func() ([]apps.Status, error)

// ToolRow is one AI tool's state for the Apps tab's merged table: the raw tool key
// (passed to `ai tools enable|disable`), its display name, and whether it is
// currently enabled. The description is looked up at render time (create.AIToolDescription)
// rather than stored here, so this package needn't cache it.
type ToolRow struct {
	Key     string
	Name    string
	Enabled bool
}

// ToolLister returns the current workspace's AI tools + enabled state. Injected so the
// view is testable; the parent wires it over the live current project's config.yaml.
type ToolLister func() ([]ToolRow, error)

// AppActionRequestedMsg is emitted when the user runs an apps lifecycle action on
// the selected APP row (add/remove/update/start/stop/restart). The parent app runs
// `ai apps <action> <app> <project>` live in the terminal overlay and refreshes
// the view on return.
type AppActionRequestedMsg struct {
	Project string
	App     string
	Action  string
}

// ToolActionRequestedMsg is emitted when the user toggles the selected AI-TOOL row
// (a=enable, x=disable — the same keys as an app's add/remove, since toggling an AI
// tool IS "adding"/"removing" it from the workspace). The parent app runs
// `ai tools <enable|disable> <tool> <project>` live in the terminal overlay (which may
// itself prompt to restart the workspace) and refreshes the view on return.
type ToolActionRequestedMsg struct {
	Project string
	Tool    string
	Action  string // "enable" or "disable"
}

type appsRefreshedMsg struct {
	apps  []apps.Status
	tools []ToolRow
	err   error
}

// appsRefreshMsg re-arms the steady auto-refresh that keeps the Apps tab LIVE while
// it is visible: it reflects containers starting/stopping and the workspace being
// stopped/started (and self-heals after a host sleep) without a manual `r`.
// Generation-guarded so a stale chain dies.
type appsRefreshMsg struct{ generation int }

// appsRefreshInterval is how often the Apps tab re-lists while it is the active tab.
// It only ticks while the tab is active (the hub pauses it on leave).
const appsRefreshInterval = 3 * time.Second

// Apps is the per-workspace view of the in-VM AI apps (Open WebUI) AND the opt-in AI
// tools (caveman, graphify, tokensave, …): one Services-style table NAME / STATUS / INFO
// — apps rows first, then tool rows — with keys to add/remove/update and
// start/stop/restart an app, or enable/disable a tool (the same a/x keys, since both
// read as "add/remove this from the workspace"). It honours the active theme.
type Apps struct {
	list     AppLister
	toolList ToolLister
	project  func() string
	table    listTable
	apps     []apps.Status
	tools    []ToolRow
	flash    string
	err      error
	loaded   bool

	// active marks the visible sub-tab; generation guards the auto-retry chain so it
	// runs only on the active tab and stops when the tab is left.
	active     bool
	generation int
}

// NewApps builds the apps view over the injected app lister, AI-tool lister, and
// current-project accessor.
func NewApps(list AppLister, toolList ToolLister, project func() string) *Apps {
	columns := []listColumn{
		{title: "NAME", width: 20},
		{title: "STATUS", width: 20},
		{title: "INFO", width: 32},
	}
	return &Apps{list: list, toolList: toolList, project: project, table: newListTable(columns)}
}

// Title is the view's name (used by the sub-tab bar/header).
func (view *Apps) Title() string { return "Apps" }

// Hints are the context-sensitive key bindings shown in the footer.
func (view *Apps) Hints() string {
	return "a add/enable · x remove/disable · u update · s start · t stop · R restart · r refresh"
}

// SetSize fits the table to the content area and re-picks live theme styles. One row
// is reserved for the flash slot (always rendered, blank when empty) so the table
// fills a FIXED height and its bottom never moves whether or not a flash shows.
func (view *Apps) SetSize(width, height int) {
	if tableHeight := height - 1; tableHeight > 0 {
		view.table.SetSize(width, tableHeight)
	}
}

// Init kicks off a fresh app+tool listing (bumping the generation so any pending
// auto-retry from a previous activation is discarded).
func (view *Apps) Init() tea.Cmd {
	view.generation++
	return view.fetchCmd()
}

// SetActive marks whether the Apps tab is the visible one; leaving it stops the
// auto-retry chain so a background tab makes no in-VM calls.
func (view *Apps) SetActive(active bool) {
	view.active = active
	if !active {
		view.generation++
	}
}

func (view *Apps) refreshTickCmd(generation int) tea.Cmd {
	return tea.Tick(appsRefreshInterval, func(time.Time) tea.Msg {
		return appsRefreshMsg{generation: generation}
	})
}

// listTools calls the injected ToolLister, tolerating a nil lister (e.g. in tests that
// only exercise the apps half) by returning an empty list.
func (view *Apps) listTools() ([]ToolRow, error) {
	if view.toolList == nil {
		return nil, nil
	}
	return view.toolList()
}

func (view *Apps) fetchCmd() tea.Cmd {
	list := view.list
	listTools := view.listTools
	return func() tea.Msg {
		done := make(chan appsRefreshedMsg, 1)
		go func() {
			statuses, err := list()
			if err != nil {
				done <- appsRefreshedMsg{apps: statuses, err: err}
				return
			}
			tools, err := listTools()
			done <- appsRefreshedMsg{apps: statuses, tools: tools, err: err}
		}()
		// Backstop the in-VM `nerdctl ps` read so the tab never hangs on "loading…".
		// The manager bounds + classifies the probe itself (stale/overloaded VM), so
		// this only fires if even that wedged; then degrade to a retryable error.
		select {
		case got := <-done:
			return got
		case <-time.After(viewFetchTimeout):
			return appsRefreshedMsg{err: errBackstopTimedOut("listing apps")}
		}
	}
}

// Update advances the view: refresh results repopulate the table; the action keys
// emit an AppActionRequestedMsg/ToolActionRequestedMsg for the selected row; r
// refreshes; other keys drive table navigation.
func (view *Apps) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case appsRefreshedMsg:
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			view.apps = message.apps
			view.tools = message.tools
			cursor := view.table.Cursor()
			view.table.SetRows(mergedRows(message.apps, message.tools))
			view.table.SetCursor(cursor) // keep the selection across the periodic refresh
		}
		// Keep the Apps list LIVE while this tab is active: re-poll on a timer so it
		// reflects containers starting/stopping and the workspace being stopped/started
		// (a stop returns Running=false with NO error, which the error-only path missed).
		// Generation- and active-guarded, so only the visible tab makes in-VM calls.
		if view.active {
			return view.refreshTickCmd(view.generation)
		}
		return nil
	case appsRefreshMsg:
		if message.generation != view.generation || !view.active {
			return nil // stale chain or the tab was left
		}
		return view.fetchCmd()
	case tea.KeyMsg:
		if cmd, handled := view.handleAction(message); handled {
			return cmd
		}
	}
	return view.table.Update(msg)
}

// actionForKey maps a key to its apps lifecycle action ("" when the key is not an
// action key). For a tool row, "add"/"remove" are reinterpreted as enable/disable by
// handleAction — the key mapping itself is shared between apps and tools.
func actionForKey(key string) string {
	switch key {
	case "a":
		return "add"
	case "x":
		return "remove"
	case "u":
		return "update"
	case "s":
		return "start"
	case "t":
		return "stop"
	case "R":
		return "restart"
	}
	return ""
}

// handleAction maps the action keys; the bool reports whether the key was an
// action (so it is not also passed to the table for navigation).
func (view *Apps) handleAction(key tea.KeyMsg) (tea.Cmd, bool) {
	if key.String() == "r" {
		return view.fetchCmd(), true
	}
	action := actionForKey(key.String())
	if action == "" {
		return nil, false
	}
	project := view.project()
	if project == "" {
		// With no workspace selected there is nothing to act on, but still swallow
		// the action key so it does not navigate the table.
		return nil, true
	}
	index := view.table.Cursor()
	switch {
	case index >= 0 && index < len(view.apps):
		app := view.apps[index].Key
		if app == "" {
			return nil, true
		}
		view.flash = ui.Muted.Render(action + " " + app + "…")
		return func() tea.Msg {
			return AppActionRequestedMsg{Project: project, App: app, Action: action}
		}, true
	case index >= len(view.apps) && index < len(view.apps)+len(view.tools):
		tool := view.tools[index-len(view.apps)]
		// u/s/t/R (update/start/stop/restart) make no sense for a non-container AI
		// tool — swallow them as no-ops rather than emit a nonsense message.
		var toolAction string
		switch action {
		case "add":
			toolAction = "enable"
		case "remove":
			toolAction = "disable"
		default:
			return nil, true
		}
		view.flash = ui.Muted.Render(toolAction + " " + tool.Name + "…")
		return func() tea.Msg {
			return ToolActionRequestedMsg{Project: project, Tool: tool.Key, Action: toolAction}
		}, true
	default:
		return nil, true
	}
}

// View renders the merged apps+tools table (with any flash), a "no workspace selected"
// hint when none is current, or a fetch error.
func (view *Apps) View() string {
	if view.project() == "" {
		return ui.Muted.Render("no workspace selected — open one from the Workspaces view")
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading apps…")
	}
	// Always emit the flash slot as the LAST line (blank when empty) so the table
	// above keeps its fixed height and the bottom sits at the constant margin.
	return view.table.View() + "\n" + flashLine(view.flash)
}

// mergedRows renders the combined table: app rows first (NAME/STATUS/INFO = name/
// lifecycle/URL), then AI-tool rows (NAME/STATUS/INFO = name/enabled-disabled/description).
func mergedRows(statuses []apps.Status, tools []ToolRow) [][]string {
	rows := make([][]string, 0, len(statuses)+len(tools))
	rows = append(rows, appRows(statuses)...)
	rows = append(rows, toolRows(tools)...)
	return rows
}

func appRows(statuses []apps.Status) [][]string {
	rows := make([][]string, 0, len(statuses))
	for _, status := range statuses {
		rows = append(rows, []string{status.Name, appStatusCell(status), orDashApps(status.URL)})
	}
	return rows
}

// toolRows renders each AI tool's NAME / STATUS (enabled/disabled) / INFO (description).
func toolRows(tools []ToolRow) [][]string {
	rows := make([][]string, 0, len(tools))
	for _, tool := range tools {
		rows = append(rows, []string{tool.Name, toolStatusCell(tool), create.AIToolDescription(tool.Key)})
	}
	return rows
}

// toolStatusCell renders an AI tool's enabled state for the table.
func toolStatusCell(tool ToolRow) string {
	if tool.Enabled {
		return "enabled"
	}
	return "disabled"
}

// appStatusCell renders one app's lifecycle state for the table.
func appStatusCell(status apps.Status) string {
	switch {
	case !status.Installed:
		return "not installed"
	case status.Running:
		return "running"
	default:
		return "installed (stopped)"
	}
}

// orDashApps renders an em dash for an empty cell.
func orDashApps(value string) string {
	if value == "" {
		return "—"
	}
	return value
}
