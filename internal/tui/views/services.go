// Package views holds the individual K9s-style screens of the `ai ui` TUI. Each
// view is a self-contained component (a pointer model with Init/Update/View +
// Title/Hints/SetSize) that the parent tui package composes and routes input to.
// Views take their data and side effects through injected funcs so they unit-test
// with fakes and the real package APIs are wired by the parent.
package views

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// ServiceFetcher returns the current service-tier status. Injected so the view is
// testable; the parent wires setup.ServicesStatus(deps).
type ServiceFetcher func() ([]setup.ServiceStatus, error)

// ServiceController applies a lifecycle action (start/stop/restart) to one named
// service. Injected; the parent wires setup.ControlService(deps, action, name).
type ServiceController func(action, service string) error

// ServiceUpdater re-pulls the latest images for one named service (or "" for all)
// and recreates its container. Injected; the parent wires
// setup.UpdateService(deps, service, …). It backs the `p` (pull/update) key.
type ServiceUpdater func(service string) error

// URLOpener opens a console URL in the host browser. Injected; the parent wires
// the OS opener (open / xdg-open).
type URLOpener func(url string) error

// LogTailer returns the recent log lines for a service. Injected; the parent
// wires it over internal/logs (Sources + Tail). Logs are viewed from the
// Services view (the `l` key) — there is no separate Logs tab.
type LogTailer func(service string) ([]string, error)

// ServicesRefreshInterval is how often the live view re-polls status;
// logsRefreshInterval is how often the open log pane re-tails its service.
const (
	ServicesRefreshInterval = 2 * time.Second
	logsRefreshInterval     = 1500 * time.Millisecond
)

type servicesRefreshedMsg struct {
	statuses []setup.ServiceStatus
	err      error
}
type servicesTickMsg struct{}
type logsTickMsg struct{}
type serviceActionDoneMsg struct {
	action  string
	service string
	err     error
}

// Services is the live view of the host service tier + their containers, with
// start/stop/restart and open-console actions on the selected row.
type Services struct {
	fetch      ServiceFetcher
	control    ServiceController
	update     ServiceUpdater
	open       URLOpener
	tail       LogTailer
	table      table.Model
	describe   describePane
	logs       describePane // full-pane log viewer for the selected service
	logService string       // the service whose logs the pane is following
	statuses   []setup.ServiceStatus
	flash      string
	err        error
	loaded     bool
}

// NewServices builds the services view over the injected status fetcher,
// lifecycle controller, image updater, URL opener, and log tailer.
func NewServices(fetch ServiceFetcher, control ServiceController, update ServiceUpdater, open URLOpener, tail LogTailer) *Services {
	columns := []table.Column{
		{Title: "SERVICE", Width: 20},
		{Title: "MODE", Width: 10},
		{Title: "STATE", Width: 14},
		{Title: "HEALTH", Width: 7},
		{Title: "ADDRESS", Width: 28},
	}
	built := table.New(table.WithColumns(columns), table.WithFocused(true))
	built.SetStyles(ui.TableStyles())
	return &Services{
		fetch: fetch, control: control, update: update, open: open, tail: tail,
		table: built, describe: newDescribePane(), logs: newDescribePane(),
	}
}

// Title is the view's name (used by the menu/header).
func (view *Services) Title() string { return "Services" }

// Hints are the context-sensitive key bindings shown in the footer.
func (view *Services) Hints() string {
	return "enter/d describe · s start · x stop · r restart · p update · e enable/disable · o console · l logs"
}

// SetSize fits the table + the describe/logs panes to the content area.
func (view *Services) SetSize(width, height int) {
	view.table.SetStyles(ui.TableStyles()) // pick up a live theme change
	view.table.SetWidth(width)
	if height > 0 {
		view.table.SetHeight(height)
	}
	view.describe.setSize(width, height)
	view.logs.setSize(width, height)
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
// tick; the tick triggers the next fetch; s/x/r/o act on the selected service;
// other keys drive table navigation.
func (view *Services) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case servicesRefreshedMsg:
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			view.statuses = message.statuses
			view.table.SetRows(serviceRows(message.statuses))
		}
		return servicesTick()
	case servicesTickMsg:
		return view.fetchCmd()
	case logsTickMsg:
		// Keep the open log pane live: re-tail and refresh until it is closed.
		if view.logs.active() && view.logService != "" {
			view.logs.refresh(view.serviceLogs(view.logService))
			return logsTick()
		}
		return nil
	case serviceActionDoneMsg:
		view.flash = actionFlash(message)
		return view.fetchCmd() // reflect the action immediately
	case tea.KeyMsg:
		// Menu keys stay live even while a pane is open, so the user can jump
		// straight from describe to logs (or run an action) without esc-ing out
		// first. Scroll keys + esc fall through to whichever pane is active.
		if cmd, handled := view.handleAction(message); handled {
			return cmd
		}
		if view.logs.active() {
			return view.logs.update(message)
		}
		if view.describe.active() {
			return view.describe.update(message)
		}
	}
	var cmd tea.Cmd
	view.table, cmd = view.table.Update(msg)
	return cmd
}

// handleAction maps the action keys to async commands; the bool reports whether
// the key was an action (so it is not also passed to the table for navigation).
func (view *Services) handleAction(key tea.KeyMsg) (tea.Cmd, bool) {
	service := view.selectedService()
	switch key.String() {
	case "s", "x", "r":
		if service == "" {
			return nil, true
		}
		// A disabled optional service must be enabled before it can be controlled.
		if status := view.statusByName(service); status.Optional && !status.Enabled() {
			view.flash = ui.Muted.Render(service + " is disabled — press e to enable it first")
			view.closePanes() // show the hint over the table
			return nil, true
		}
		action := map[string]string{"s": "start", "x": "stop", "r": "restart"}[key.String()]
		view.flash = ui.Muted.Render(action + "ing " + service + "…")
		view.closePanes() // surface the action's result over the table
		return view.controlCmd(action, service), true
	case "p":
		// Re-pull the latest images for the service and recreate its container.
		if service == "" {
			return nil, true
		}
		if status := view.statusByName(service); status.Optional && !status.Enabled() {
			view.flash = ui.Muted.Render(service + " is disabled — press e to enable it first")
			view.closePanes()
			return nil, true
		}
		view.flash = ui.Muted.Render("updating " + service + " (re-pulling images)…")
		view.closePanes()
		return view.updateCmd(service), true
	case "e":
		if service == "" {
			return nil, true
		}
		// enable/disable applies only to optional services — core services are
		// always on. Toggle based on the current state.
		status := view.statusByName(service)
		view.closePanes()
		if !status.Optional {
			view.flash = ui.Muted.Render(service + " is a core service — always enabled")
			return nil, true
		}
		action, gerund := "enable", "enabling"
		if status.Enabled() {
			action, gerund = "disable", "disabling"
		}
		view.flash = ui.Muted.Render(gerund + " " + service + "…")
		return view.controlCmd(action, service), true
	case "o":
		if service == "" {
			return nil, true
		}
		url := view.consoleURL(service)
		if url == "" {
			view.flash = ui.Muted.Render(service + " has no admin console")
			view.closePanes()
			return nil, true
		}
		return view.openCmd(service, url), true
	case "enter", "d":
		// enter / d both drill into the selected service's detail pane (closing the
		// logs pane if it was the one open).
		if service == "" {
			return nil, true
		}
		view.logs.close()
		view.describe.show(describeService(view.statusByName(service)))
		return nil, true
	case "l":
		// Switch to the live logs pane (closing the describe pane if it was open).
		if service == "" {
			return nil, true
		}
		view.describe.close()
		view.logService = service
		view.logs.showLive(view.serviceLogs(service))
		return logsTick(), true // start following the tail
	}
	return nil, false
}

// closePanes hides both the describe and logs panes so the table (and any flash)
// is visible again — used by the operation actions (start/stop/restart/enable).
func (view *Services) closePanes() {
	view.describe.close()
	view.logs.close()
}

func logsTick() tea.Cmd {
	return tea.Tick(logsRefreshInterval, func(time.Time) tea.Msg { return logsTickMsg{} })
}

// serviceLogs returns the tailed log content for the full-pane log viewer (or a
// friendly placeholder when there is nothing on disk / capture isn't wired yet).
func (view *Services) serviceLogs(service string) string {
	heading := ui.Heading.Render("logs · " + service)
	lines, err := view.tail(service)
	if err != nil {
		return heading + "\n" + ui.Failure.Render(ui.IconFail+" "+err.Error())
	}
	if len(lines) == 0 {
		return heading + "\n" + ui.Muted.Render("no logs on disk yet for "+service+
			" (live capture is wired during hardware bring-up)")
	}
	return heading + "\n" + strings.Join(lines, "\n")
}

// statusByName returns the cached status for a service (a name-only fallback if
// it is not in the latest fetch).
func (view *Services) statusByName(service string) setup.ServiceStatus {
	for _, status := range view.statuses {
		if status.Name == service {
			return status
		}
	}
	return setup.ServiceStatus{Name: service}
}

func (view *Services) controlCmd(action, service string) tea.Cmd {
	control := view.control
	return func() tea.Msg {
		return serviceActionDoneMsg{action: action, service: service, err: control(action, service)}
	}
}

func (view *Services) updateCmd(service string) tea.Cmd {
	update := view.update
	return func() tea.Msg {
		return serviceActionDoneMsg{action: "update", service: service, err: update(service)}
	}
}

func (view *Services) openCmd(service, url string) tea.Cmd {
	open := view.open
	return func() tea.Msg {
		return serviceActionDoneMsg{action: "open", service: service, err: open(url)}
	}
}

// selectedService returns the service name in the highlighted row (or "").
func (view *Services) selectedService() string {
	row := view.table.SelectedRow()
	if len(row) == 0 {
		return ""
	}
	return row[0]
}

// consoleURL returns the admin-console URL for the named service (or "").
func (view *Services) consoleURL(service string) string {
	for _, status := range view.statuses {
		if status.Name == service {
			return status.Console
		}
	}
	return ""
}

// View renders the full-pane logs or describe pane when open, else the table
// (with any flash). The panes fill the body; esc returns to the table.
func (view *Services) View() string {
	if view.logs.active() {
		return view.logs.view()
	}
	if view.describe.active() {
		return view.describe.view()
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading service status…")
	}
	if view.flash != "" {
		return view.flash + "\n" + view.table.View()
	}
	return view.table.View()
}

// describeService renders a service's full detail for the describe pane.
func describeService(status setup.ServiceStatus) string {
	health := "no"
	if status.Healthy {
		health = "yes"
	}
	var body strings.Builder
	body.WriteString(ui.Heading.Render(status.Name) + "\n")
	body.WriteString(field("mode", status.Mode))
	body.WriteString(field("state", status.State))
	if status.Optional {
		body.WriteString(field("optional", "yes (press e to enable/disable)"))
	}
	body.WriteString(field("healthy", health))
	body.WriteString(field("address", status.Address))
	body.WriteString(field("console", status.Console))
	body.WriteString(field("detail", status.Detail))
	return body.String()
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

// actionFlash renders the outcome of a lifecycle/console action.
func actionFlash(msg serviceActionDoneMsg) string {
	verbs := map[string]string{
		"start": "started", "stop": "stopped", "restart": "restarted", "open": "opened console for",
		"enable": "enabled", "disable": "disabled", "update": "updated",
	}
	if msg.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + msg.action + " " + msg.service + ": " + msg.err.Error())
	}
	return ui.Success.Render(ui.IconOK + " " + verbs[msg.action] + " " + msg.service)
}
