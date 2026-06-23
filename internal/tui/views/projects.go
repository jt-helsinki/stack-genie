package views

import (
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// ProjectLister returns all registered projects (the global switcher list).
// Injected; the parent wires project.List.
type ProjectLister func() ([]project.Entry, error)

// ProjectSelectedMsg is emitted when the user picks a project; the parent app
// makes it the current project and switches to the Project detail view.
type ProjectSelectedMsg struct {
	Name string
	Path string
}

// NewProjectRequestedMsg is emitted when the user asks to create a new project
// (the "n" key); the parent app switches to the Create (directory picker) view.
type NewProjectRequestedMsg struct{}

type projectsLoadedMsg struct {
	entries []project.Entry
	err     error
}

// Projects is the global project switcher: a list of every registered project
// (from ~/.ai-platform/config/projects.yaml); selecting one makes it current.
type Projects struct {
	list    ProjectLister
	table   table.Model
	entries []project.Entry
	err     error
	loaded  bool
}

// NewProjects builds the switcher over the injected project lister.
func NewProjects(list ProjectLister) *Projects {
	columns := []table.Column{
		{Title: "PROJECT", Width: 22},
		{Title: "OS", Width: 16},
		{Title: "WORKSPACE", Width: 12},
		{Title: "AGENTS", Width: 28},
	}
	built := table.New(table.WithColumns(columns), table.WithFocused(true))
	built.SetStyles(ui.TableStyles())
	return &Projects{list: list, table: built}
}

func (view *Projects) Title() string { return "Projects" }
func (view *Projects) Hints() string { return "enter open · n new · r refresh" }

func (view *Projects) SetSize(width, height int) {
	view.table.SetWidth(width)
	if height > 0 {
		view.table.SetHeight(height)
	}
}

func (view *Projects) Init() tea.Cmd { return view.listCmd() }

func (view *Projects) listCmd() tea.Cmd {
	list := view.list
	return func() tea.Msg {
		entries, err := list()
		return projectsLoadedMsg{entries: entries, err: err}
	}
}

// Update advances the switcher: a load result fills the table; enter selects the
// highlighted project (emits ProjectSelectedMsg); r refreshes.
func (view *Projects) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case projectsLoadedMsg:
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			view.entries = message.entries
			view.table.SetRows(projectRows(message.entries))
		}
		return nil
	case tea.KeyMsg:
		switch message.String() {
		case "enter":
			if selected, ok := view.selectedEntry(); ok {
				return func() tea.Msg { return ProjectSelectedMsg{Name: selected.Name, Path: selected.Path} }
			}
			return nil
		case "n":
			return func() tea.Msg { return NewProjectRequestedMsg{} }
		case "r":
			return view.listCmd()
		}
	}
	var cmd tea.Cmd
	view.table, cmd = view.table.Update(msg)
	return cmd
}

// selectedEntry returns the highlighted project entry (matched by name).
func (view *Projects) selectedEntry() (project.Entry, bool) {
	row := view.table.SelectedRow()
	if len(row) == 0 {
		return project.Entry{}, false
	}
	for _, entry := range view.entries {
		if entry.Name == row[0] {
			return entry, true
		}
	}
	return project.Entry{}, false
}

func (view *Projects) View() string {
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading projects…")
	}
	if len(view.entries) == 0 {
		return ui.Muted.Render("no projects yet — create one with `ai project create` in a directory")
	}
	return view.table.View()
}

func projectRows(entries []project.Entry) []table.Row {
	rows := make([]table.Row, 0, len(entries))
	for _, entry := range entries {
		agents := ""
		for index, agent := range entry.Agents {
			if index > 0 {
				agents += ","
			}
			agents += agent
		}
		rows = append(rows, table.Row{entry.Name, entry.OS, entry.Status, agents})
	}
	return rows
}
