// Package views holds the individual K9s-style screens of the `ai ui` TUI. Each
// view is a self-contained component (a pointer model with Init/Update/View +
// Title/SetSize) that the parent tui package composes and routes input to. Views
// take their data through injected fetcher/action funcs so they unit-test with
// fakes and the real package APIs are wired by the parent.
package views

import (
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// ServiceFetcher returns the current service-tier status. Injected so the view is
// testable; the parent wires setup.ServicesStatus(deps).
type ServiceFetcher func() ([]setup.ServiceStatus, error)

// ServicesRefreshInterval is how often the live view re-polls status.
const ServicesRefreshInterval = 2 * time.Second

type servicesRefreshedMsg struct {
	statuses []setup.ServiceStatus
	err      error
}
type servicesTickMsg struct{}

// Services is the live view of the host service tier + their containers.
type Services struct {
	fetch  ServiceFetcher
	table  table.Model
	err    error
	loaded bool
}

// NewServices builds the services view over the given status fetcher.
func NewServices(fetch ServiceFetcher) *Services {
	columns := []table.Column{
		{Title: "SERVICE", Width: 20},
		{Title: "MODE", Width: 10},
		{Title: "STATE", Width: 14},
		{Title: "HEALTH", Width: 7},
		{Title: "ADDRESS", Width: 28},
	}
	built := table.New(table.WithColumns(columns), table.WithFocused(true))
	return &Services{fetch: fetch, table: built}
}

// Title is the view's name (used by the menu/header).
func (view *Services) Title() string { return "Services" }

// SetSize fits the table to the content area the parent allots it.
func (view *Services) SetSize(width, height int) {
	view.table.SetWidth(width)
	if height > 0 {
		view.table.SetHeight(height)
	}
}

// Init kicks off the first status fetch.
func (view *Services) Init() tea.Cmd { return view.fetchCmd() }

func (view *Services) fetchCmd() tea.Cmd {
	fetch := view.fetch
	return func() tea.Msg {
		statuses, err := fetch()
		return servicesRefreshedMsg{statuses: statuses, err: err}
	}
}

func servicesTick() tea.Cmd {
	return tea.Tick(ServicesRefreshInterval, func(time.Time) tea.Msg { return servicesTickMsg{} })
}

// Update advances the view: refresh results repopulate the table and re-arm the
// tick; the tick triggers the next fetch; other messages drive table navigation.
func (view *Services) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case servicesRefreshedMsg:
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			view.table.SetRows(serviceRows(message.statuses))
		}
		return servicesTick()
	case servicesTickMsg:
		return view.fetchCmd()
	}
	var cmd tea.Cmd
	view.table, cmd = view.table.Update(msg)
	return cmd
}

// View renders the table (or a load/error line).
func (view *Services) View() string {
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading service status…")
	}
	return view.table.View()
}

func serviceRows(statuses []setup.ServiceStatus) []table.Row {
	rows := make([]table.Row, 0, len(statuses))
	for _, status := range statuses {
		health := ui.IconFail
		if status.Healthy {
			health = ui.IconOK
		}
		rows = append(rows, table.Row{status.Name, status.Mode, status.State, health, status.Address})
	}
	return rows
}
