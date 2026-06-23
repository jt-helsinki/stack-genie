package views

import (
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// LogServiceLister returns the host-service names a log scope can be picked from.
// Injected; the parent wires logs.Services.
type LogServiceLister func() []string

// LogTailer returns the trailing log lines for one service (resolving its source
// file and reading the tail). Injected; the parent wires logs.Sources + logs.Tail.
// An empty result is not an error — live capture is wired during hardware
// bring-up, so a service may simply have nothing on disk yet.
type LogTailer func(service string) ([]string, error)

type logsTailedMsg struct {
	service string
	lines   []string
	err     error
}

// Logs lets the user pick a host service and tail its log file in a scrollable
// pane. The service list (a table) sits on top; pressing enter tails the chosen
// service into the viewport below. Because live capture is wired during hardware
// bring-up, an empty tail renders a friendly "no logs yet" line rather than an
// error.
type Logs struct {
	list     LogServiceLister
	tail     LogTailer
	table    table.Model
	viewport viewport.Model
	width    int
	height   int
	selected string
	flash    string
	err      error
	tailing  bool
}

// tableHeight is how many rows of the content area the service list takes; the
// rest goes to the log viewport.
const logsTableHeight = 8

// NewLogs builds the logs view over the injected service lister and tailer.
func NewLogs(list LogServiceLister, tail LogTailer) *Logs {
	columns := []table.Column{{Title: "SERVICE", Width: 24}}
	built := table.New(table.WithColumns(columns), table.WithFocused(true))
	built.SetStyles(ui.TableStyles())
	rows := make([]table.Row, 0, len(list()))
	for _, service := range list() {
		rows = append(rows, table.Row{service})
	}
	built.SetRows(rows)
	return &Logs{list: list, tail: tail, table: built, viewport: viewport.New(0, 0)}
}

// Title is the view's name (used by the menu/header).
func (view *Logs) Title() string { return "Logs" }

// Hints are the context-sensitive key bindings shown in the footer.
func (view *Logs) Hints() string { return "enter view · ↑/↓ scroll · r refresh" }

// SetSize fits both the service table and the log viewport to the content area
// the parent allots: the table keeps a fixed height on top, the viewport fills
// the rest.
func (view *Logs) SetSize(width, height int) {
	view.width = width
	view.height = height
	view.table.SetWidth(width)
	tableHeight := logsTableHeight
	if height > 0 && tableHeight > height {
		tableHeight = height
	}
	view.table.SetHeight(tableHeight)
	viewportHeight := height - tableHeight - 1 // -1 for the divider line
	if viewportHeight < 1 {
		viewportHeight = 1
	}
	view.viewport.Width = width
	view.viewport.Height = viewportHeight
}

// Init populates the service list (rows were set at construction) — no fetch is
// needed until a service is selected.
func (view *Logs) Init() tea.Cmd { return nil }

func (view *Logs) tailCmd(service string) tea.Cmd {
	tail := view.tail
	return func() tea.Msg {
		lines, err := tail(service)
		return logsTailedMsg{service: service, lines: lines, err: err}
	}
}

// Update advances the view: enter tails the selected service (async); "r"
// refreshes the current tail; a tail result fills the viewport (or records an
// error / friendly-empty flash); other keys scroll the viewport or drive the
// table when nothing is tailing yet.
func (view *Logs) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case logsTailedMsg:
		view.tailing = true
		view.err = message.err
		view.selected = message.service
		if message.err != nil {
			view.flash = ui.Failure.Render(ui.IconFail + " read " + message.service + ": " + message.err.Error())
			return nil
		}
		view.flash = ""
		if len(message.lines) == 0 {
			view.viewport.SetContent(ui.Muted.Render("no logs on disk yet for " + message.service))
		} else {
			view.viewport.SetContent(joinLines(message.lines))
			view.viewport.GotoBottom()
		}
		return nil
	case tea.KeyMsg:
		switch message.String() {
		case "enter":
			service := view.selectedService()
			if service == "" {
				return nil
			}
			view.flash = ui.Muted.Render("reading " + service + "…")
			return view.tailCmd(service)
		case "r":
			service := view.selected
			if service == "" {
				service = view.selectedService()
			}
			if service == "" {
				return nil
			}
			view.flash = ui.Muted.Render("reading " + service + "…")
			return view.tailCmd(service)
		}
	}
	// Once a service is being tailed, scroll keys drive the viewport; before
	// that, navigation drives the service table.
	if view.tailing {
		var cmd tea.Cmd
		view.viewport, cmd = view.viewport.Update(msg)
		return cmd
	}
	var cmd tea.Cmd
	view.table, cmd = view.table.Update(msg)
	return cmd
}

// selectedService returns the service name in the highlighted table row (or "").
func (view *Logs) selectedService() string {
	row := view.table.SelectedRow()
	if len(row) == 0 {
		return ""
	}
	return row[0]
}

// View renders the service table on top, a divider, and the log viewport (or a
// prompt to pick a service) below, with the latest flash.
func (view *Logs) View() string {
	body := view.table.View() + "\n" + ui.Muted.Render(view.dividerLabel()) + "\n"
	if view.tailing {
		body += view.viewport.View()
	} else {
		body += ui.Muted.Render("select a service and press enter to tail its log")
	}
	if view.flash != "" {
		return view.flash + "\n" + body
	}
	return body
}

// dividerLabel names the source under the table.
func (view *Logs) dividerLabel() string {
	if view.selected != "" {
		return "── " + view.selected + " ──"
	}
	return "──"
}

// joinLines concatenates log lines with newlines.
func joinLines(lines []string) string {
	out := ""
	for index, line := range lines {
		if index > 0 {
			out += "\n"
		}
		out += line
	}
	return out
}
