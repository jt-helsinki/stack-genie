package views

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/project"
	"github.com/jt-helsinki/stack-genie/internal/ui"
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
	list     ProjectLister
	table    listTable
	describe describePane
	entries  []project.Entry
	err      error
	loaded   bool
}

// NewProjects builds the switcher over the injected project lister.
func NewProjects(list ProjectLister) *Projects {
	columns := []listColumn{
		{title: "NAME", width: 22},
		{title: "OS", width: 16},
		{title: "STATUS", width: 12},
		{title: "AGENTS", width: 28},
	}
	return &Projects{list: list, table: newListTable(columns), describe: newDescribePane()}
}

func (view *Projects) Title() string { return "Workspaces" }
func (view *Projects) Hints() string { return "enter open · n new · d describe · r refresh" }

func (view *Projects) SetSize(width, height int) {
	if height > 0 {
		view.table.SetSize(width, height)
	}
	view.describe.setSize(width, height)
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
		// Menu keys stay live even while the describe pane is open (so enter opens
		// the project, r refreshes, etc.); scroll keys + esc fall through to the
		// pane.
		switch message.String() {
		case "enter":
			if selected, ok := view.selectedEntry(); ok {
				return func() tea.Msg { return ProjectSelectedMsg{Name: selected.Name, Path: selected.Path} }
			}
			return nil
		case "n":
			return func() tea.Msg { return NewProjectRequestedMsg{} }
		case "d":
			if selected, ok := view.selectedEntry(); ok {
				view.describe.show(describeProject(selected))
			}
			return nil
		case "r":
			return view.listCmd()
		}
		if view.describe.active() {
			return view.describe.update(message)
		}
	}
	return view.table.Update(msg)
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
	if view.describe.active() {
		return view.describe.view()
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading workspaces…")
	}
	if len(view.entries) == 0 {
		return ui.Muted.Render("no workspaces yet — create one with `ai create` in a directory")
	}
	return view.table.View()
}

// describeProject renders a project's full detail for the describe pane.
func describeProject(entry project.Entry) string {
	var body strings.Builder
	body.WriteString(ui.Heading.Render(entry.Name) + "\n")
	body.WriteString(field("os", entry.OS))
	body.WriteString(field("workspace", entry.Status))
	body.WriteString(field("path", entry.Path))
	body.WriteString(field("agents", strings.Join(entry.Agents, ", ")))
	return body.String()
}

func projectRows(entries []project.Entry) [][]string {
	rows := make([][]string, 0, len(entries))
	for _, entry := range entries {
		agents := ""
		for index, agent := range entry.Agents {
			if index > 0 {
				agents += ","
			}
			agents += agent
		}
		rows = append(rows, []string{entry.Name, entry.OS, entry.Status, agents})
	}
	return rows
}
