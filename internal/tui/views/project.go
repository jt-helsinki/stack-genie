package views

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// spinnerFrames are the braille spinner glyphs for the in-flight lifecycle status.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// ProjectInfoFetcher returns the current state of one project (name, OS, agents,
// workspace status). Injected; the parent wires it over project.List.
type ProjectInfoFetcher func(name string) (project.Entry, bool, error)

// ExecRequestedMsg is emitted when the user asks to open an interactive shell in
// the current project's workspace (the "e" key). The parent app suspends the TUI
// and tea.ExecProcess an interactive shell via `ai shell`.
type ExecRequestedMsg struct {
	Project string
}

// WorkspaceActionRequestedMsg is emitted for a workspace lifecycle action
// (start/stop/restart/delete). For start/stop/restart the parent app runs
// `ai <action> <name>` DETACHED (it keeps running even if `ai ui` is closed) and
// shows a spinner + status here while polling — the TUI stays navigable and no log
// is shown. delete runs in the confirm overlay.
type WorkspaceActionRequestedMsg struct {
	Action  string
	Project string
}

type projectRefreshedMsg struct {
	entry project.Entry
	found bool
	err   error
}

// Project is the detail view for the current project: its summary and workspace
// lifecycle (start/stop/restart/delete).
type Project struct {
	info     ProjectInfoFetcher
	name     string
	entry    project.Entry
	hasEntry bool
	flash    string
	err      error
	// pending is the in-flight lifecycle action ("start"/"stop"/"restart"), shown as
	// an animated spinner on the workspace status line; empty when idle. The parent
	// sets/clears it (StartPending/ClearPending) and advances the frame (TickSpinner)
	// while it polls the detached action — so the spinner animates regardless of which
	// tab is focused.
	pending      string
	pendingFrame int
}

// NewProject builds the project-detail view over the injected info fetcher.
func NewProject(info ProjectInfoFetcher) *Project {
	return &Project{info: info}
}

// StartPending shows the "<action>ing…" spinner on the workspace status line; the
// parent calls this when it kicks off a detached lifecycle action.
func (view *Project) StartPending(action string) {
	view.pending = action
	view.pendingFrame = 0
	view.flash = ""
}

// TickSpinner advances the pending spinner one frame (driven by the parent's poll).
func (view *Project) TickSpinner() { view.pendingFrame++ }

// ClearPending stops the spinner (the action finished or timed out).
func (view *Project) ClearPending() { view.pending = "" }

// SetFlash shows a one-line message under the summary (e.g. a failure to launch a
// detached lifecycle action).
func (view *Project) SetFlash(message string) { view.flash = message }

func (view *Project) Title() string { return "Workspace" }
func (view *Project) Hints() string {
	if view.name == "" {
		return "open a workspace from the Workspaces view"
	}
	return "s start · x stop · r restart · d delete · e shell"
}
func (view *Project) SetSize(int, int) {}

// SetProject points the view at a project (the parent calls this, then Init, when
// the user selects one in the switcher).
func (view *Project) SetProject(name string) { view.name = name }

// Init refreshes the current project (no-op until one is selected).
func (view *Project) Init() tea.Cmd {
	if view.name == "" {
		return nil
	}
	return view.refreshCmd()
}

func (view *Project) refreshCmd() tea.Cmd {
	info := view.info
	name := view.name
	return func() tea.Msg {
		entry, found, err := info(name)
		return projectRefreshedMsg{entry: entry, found: found, err: err}
	}
}

// Update advances the detail view: a refresh fills the summary; s/x/r/d act on the
// workspace; an action result flashes and re-refreshes.
func (view *Project) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case projectRefreshedMsg:
		view.err = message.err
		view.hasEntry = message.found
		if message.err == nil && message.found {
			view.entry = message.entry
			view.flash = "" // a fresh status supersedes any stale hint
		}
		return nil
	case tea.KeyMsg:
		// While a lifecycle action is in flight, ignore further lifecycle keys.
		if view.pending != "" {
			return nil
		}
		if view.name == "" {
			return nil
		}
		name := view.name
		if message.String() == "e" {
			// A shell only works once the workspace is running; otherwise opening it
			// would suspend the TUI to a subprocess that immediately fails. Hint
			// inline instead.
			if view.entry.Status != "started" {
				view.flash = ui.Muted.Render("workspace not running — press s to start it first")
				return nil
			}
			return func() tea.Msg { return ExecRequestedMsg{Project: name} }
		}
		action, ok := map[string]string{"s": "start", "x": "stop", "r": "restart", "d": "delete"}[message.String()]
		if !ok {
			return nil
		}
		// The app handles this: start/stop/restart run DETACHED with a spinner here
		// (TUI stays navigable, no log); delete confirms in the terminal overlay.
		return func() tea.Msg { return WorkspaceActionRequestedMsg{Action: action, Project: name} }
	}
	return nil
}

func (view *Project) View() string {
	if view.name == "" {
		return ui.Muted.Render("no workspace selected — open one from the Workspaces view (menu: Workspaces)")
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.hasEntry {
		return ui.Muted.Render("loading " + view.name + "…")
	}
	var body strings.Builder
	body.WriteString(ui.Heading.Render(view.entry.Name) + "\n")
	body.WriteString(field("OS", view.entry.OS))
	body.WriteString(field("agents", strings.Join(view.entry.Agents, ", ")))
	if view.pending != "" {
		glyph := ui.Success.Render(spinnerFrames[view.pendingFrame%len(spinnerFrames)])
		body.WriteString(field("workspace", glyph+ui.Muted.Render(" "+view.pending+"ing…")))
	} else {
		body.WriteString(field("workspace", view.entry.Status))
	}
	body.WriteString(field("path", view.entry.Path))
	if view.flash != "" {
		body.WriteString("\n" + view.flash)
	}
	return body.String()
}

func field(label, value string) string {
	if value == "" {
		value = ui.Muted.Render("—")
	}
	return "  " + ui.Muted.Render(label+":") + " " + value + "\n"
}
