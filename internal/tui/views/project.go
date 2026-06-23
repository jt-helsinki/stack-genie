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

// WorkspaceController applies a workspace lifecycle action (start/stop/restart/
// destroy) to a project. Injected; the parent wires workspace.RealManager.
type WorkspaceController func(action, project string) error

type projectRefreshedMsg struct {
	entry project.Entry
	found bool
	err   error
}
type workspaceActionDoneMsg struct {
	action  string
	project string
	err     error
}

// Project is the detail view for the current project: its summary and workspace
// lifecycle (start/stop/restart/destroy).
type Project struct {
	info     ProjectInfoFetcher
	control  WorkspaceController
	name     string
	entry    project.Entry
	hasEntry bool
	flash    string
	err      error
}

// NewProject builds the project-detail view over the injected info fetcher and
// workspace controller.
func NewProject(info ProjectInfoFetcher, control WorkspaceController) *Project {
	return &Project{info: info, control: control}
}

func (view *Project) Title() string { return "Project" }
func (view *Project) Hints() string {
	if view.name == "" {
		return "open a project from the Projects view"
	}
	return "s start · x stop · r restart · d destroy"
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
		}
		return nil
	case workspaceActionDoneMsg:
		view.flash = workspaceActionFlash(message)
		return view.refreshCmd()
	case tea.KeyMsg:
		if view.name == "" {
			return nil
		}
		action, ok := map[string]string{"s": "start", "x": "stop", "r": "restart", "d": "destroy"}[message.String()]
		if !ok {
			return nil
		}
		view.flash = ui.Muted.Render(action + "ing workspace…")
		return view.controlCmd(action)
	}
	return nil
}

func (view *Project) controlCmd(action string) tea.Cmd {
	control := view.control
	name := view.name
	return func() tea.Msg {
		return workspaceActionDoneMsg{action: action, project: name, err: control(action, name)}
	}
}

func (view *Project) View() string {
	if view.name == "" {
		return ui.Muted.Render("no project selected — open one from the Projects view (menu: Projects)")
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

func workspaceActionFlash(msg workspaceActionDoneMsg) string {
	verbs := map[string]string{"start": "started", "stop": "stopped", "restart": "restarted", "destroy": "destroyed"}
	if msg.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + msg.action + " " + msg.project + ": " + msg.err.Error())
	}
	return ui.Success.Render(ui.IconOK + " " + verbs[msg.action] + " " + msg.project + " workspace")
}
