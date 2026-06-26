package views

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

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
// (start/stop/restart/destroy). The parent app suspends the TUI and runs
// `ai <action> <name>` via tea.ExecProcess, so msb's image-build /
// boot progress streams to the REAL terminal (not the alt-screen, which it would
// otherwise corrupt) and the TUI is restored — and refreshed — on return.
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
// lifecycle (start/stop/restart/destroy).
type Project struct {
	info     ProjectInfoFetcher
	name     string
	entry    project.Entry
	hasEntry bool
	flash    string
	err      error
}

// NewProject builds the project-detail view over the injected info fetcher.
func NewProject(info ProjectInfoFetcher) *Project {
	return &Project{info: info}
}

func (view *Project) Title() string { return "Workspace" }
func (view *Project) Hints() string {
	if view.name == "" {
		return "open a workspace from the Workspaces view"
	}
	return "s start · x stop · r restart · d destroy · e shell"
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
		action, ok := map[string]string{"s": "start", "x": "stop", "r": "restart", "d": "destroy"}[message.String()]
		if !ok {
			return nil
		}
		// Lifecycle runs as a suspended subprocess (the app handles this msg), so
		// msb's progress streams to the terminal instead of corrupting the TUI.
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
	body.WriteString(field("workspace", view.entry.Status))
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
