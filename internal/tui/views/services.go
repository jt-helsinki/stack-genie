// Package views holds the individual K9s-style screens of the `ai ui` TUI. Each
// view is a self-contained component (a pointer model with Init/Update/View +
// Title/Hints/SetSize) that the parent tui package composes and routes input to.
// Views take their data and side effects through injected funcs so they unit-test
// with fakes and the real package APIs are wired by the parent.
package views

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/setup"
	"github.com/jt-helsinki/stack-genie/internal/ui"
)

// ServiceFetcher returns the current service-tier status. Injected so the view is
// testable; the parent wires setup.ServicesStatus(deps).
type ServiceFetcher func() ([]setup.ServiceStatus, error)

// ServiceController applies a lifecycle action (start/stop/restart/enable/disable)
// to one named service. Injected; the parent wires setup.ControlService(deps,
// action, name).
type ServiceController func(action, service string) error

// URLOpener opens a console URL in the host browser. Injected; the parent wires the
// OS opener (open / xdg-open).
type URLOpener func(url string) error

// ServicesRefreshInterval is how often the live LIST re-polls status.
const ServicesRefreshInterval = 2 * time.Second

type servicesRefreshedMsg struct {
	statuses []setup.ServiceStatus
	err      error
}
type servicesTickMsg struct{}
type servicesToggleDoneMsg struct {
	action  string
	service string
	err     error
}

// servicesRestartDoneMsg reports the result of a list-level restart — one service
// (target = its name) or ALL of them (target = "").
type servicesRestartDoneMsg struct {
	target string
	err    error
}

// Services is the live LIST of the host service tier + their containers. It behaves
// like the Workspaces hub's switcher: `enter`/`d` drills into a per-service DETAIL
// (ServiceDetail — summary + embedded container log + in-place lifecycle), and `esc`
// backs out to the list. `e` toggles an optional service from the list (core
// services are always on). The list auto-refreshes on a 2s tick; the detail owns its
// own (paused-when-hidden) container-log poll.
type Services struct {
	fetch    ServiceFetcher
	control  ServiceController
	detail   *ServiceDetail
	table    listTable
	statuses []setup.ServiceStatus
	flash    string
	err      error
	loaded   bool
	// drilled is true while a service detail is open (the list is hidden). esc backs
	// out to the list, mirroring the Workspaces hub's open/switcher levels.
	drilled bool
}

// NewServices builds the services list over the injected status fetcher + lifecycle
// controller, and the per-service detail drilled into on enter/d (built in tui.go,
// like Project, so the docker-logs LogView + console opener are wired by the parent).
func NewServices(fetch ServiceFetcher, control ServiceController, detail *ServiceDetail) *Services {
	columns := []listColumn{
		{title: "SERVICE", width: 20},
		{title: "MODE", width: 10},
		{title: "STATE", width: 14},
		{title: "HEALTH", width: 7},
		{title: "ADDRESS", width: 50},
	}
	return &Services{fetch: fetch, control: control, detail: detail, table: newListTable(columns)}
}

// Title is the view's name (used by the menu/header).
func (view *Services) Title() string { return "Services" }

// Hints are the context-sensitive key bindings shown in the footer — the detail's
// own keys while drilled in, else the list keys.
func (view *Services) Hints() string {
	if view.drilled {
		return view.detail.Hints()
	}
	return "enter/d open · r restart · a restart all · e enable/disable"
}

// CapturesNav reports whether the view wants Tab/←→/esc for itself — true while a
// service detail is open, so the app cycles WITHIN the detail (esc backs out) rather
// than switching top-level tabs. Mirrors ProjectsHub.CapturesNav.
func (view *Services) CapturesNav() bool { return view.drilled }

// SetActive forwards the top-level tab's visibility to the open detail's embedded
// container log, so the 2s `<runtime> logs` poll only runs while the Services tab is
// visible AND a detail is open (paused on the list or while another top-level tab is
// shown). A no-op when no detail is open.
func (view *Services) SetActive(active bool) {
	if view.drilled {
		view.detail.SetActive(active)
	}
}

// SetSize fits the list table (when showing the list) or the detail (when drilled).
// One row is reserved for the flash slot so the table fills a FIXED height and its
// bottom never moves whether or not a flash shows.
func (view *Services) SetSize(width, height int) {
	if tableHeight := height - 1; tableHeight > 0 {
		view.table.SetSize(width, tableHeight)
	}
	view.detail.SetSize(width, height)
}

// Init kicks off the list fetch and, when a detail is open (e.g. re-entering the
// Services top-level tab while drilled in), restarts the detail's status/metrics/log
// poll too — otherwise its tick chain (dropped while another tab was focused) would
// not resume and the detail would freeze.
func (view *Services) Init() tea.Cmd {
	if view.drilled {
		return tea.Batch(view.fetchCmd(), view.detail.Init())
	}
	return view.fetchCmd()
}

// RefreshActive re-loads whichever level is visible: the open detail (so a
// `services update` overlay closing reflects in the detail + restarts its log poll)
// when drilled in, else the list. Used by the app after the embedded terminal
// overlay closes, mirroring ProjectsHub.RefreshActive.
func (view *Services) RefreshActive() tea.Cmd {
	if view.drilled {
		return view.detail.Init()
	}
	return view.fetchCmd()
}

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

// Update advances the view. While drilled into a detail it owns esc (back to the
// list) and delegates everything else to the detail; on the list a refresh
// repopulates the table + re-arms the tick, `enter`/`d` drills in, and `e` toggles an
// optional service. Async list messages (refresh/tick) are always handled so the
// list stays current even while the detail is open.
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
	case servicesToggleDoneMsg:
		view.flash = toggleFlash(message)
		return view.fetchCmd() // reflect the toggle immediately
	case servicesRestartDoneMsg:
		view.flash = restartFlash(message)
		return view.fetchCmd() // reflect the new state immediately
	case tea.KeyMsg:
		if view.drilled {
			if message.String() == "esc" {
				// Back out to the list (mirrors the Workspaces hub's esc → switcher). Pause
				// the detail's container-log poll so it does not run while hidden.
				view.detail.SetActive(false)
				view.drilled = false
				return view.fetchCmd()
			}
			return view.detail.Update(msg)
		}
		if cmd, handled := view.handleListKey(message); handled {
			return cmd
		}
	default:
		// Async messages (the detail's log poll/tick, lifecycle/refresh results) drive
		// the open detail.
		if view.drilled {
			return view.detail.Update(msg)
		}
	}
	return view.table.Update(msg)
}

// handleListKey maps the list-level keys; the bool reports whether the key was
// handled (so it is not also passed to the table for navigation).
func (view *Services) handleListKey(key tea.KeyMsg) (tea.Cmd, bool) {
	switch key.String() {
	case "enter", "d":
		return view.drillIn(), true
	case "e":
		return view.handleToggle(), true
	case "r":
		return view.restartSelected(), true
	case "a":
		return view.restartAll(), true
	}
	return nil, false
}

// restartSelected restarts the highlighted service in place (the list keeps
// auto-polling, so no manual refresh is needed — that key is gone).
func (view *Services) restartSelected() tea.Cmd {
	service := view.selectedService()
	if service == "" {
		return nil
	}
	view.flash = ui.Muted.Render("restarting " + service + "…")
	return view.restartCmd(service)
}

// restartAll restarts every service (control with an empty target).
func (view *Services) restartAll() tea.Cmd {
	view.flash = ui.Muted.Render("restarting all services…")
	return view.restartCmd("")
}

func (view *Services) restartCmd(target string) tea.Cmd {
	control := view.control
	return func() tea.Msg {
		return servicesRestartDoneMsg{target: target, err: control("restart", target)}
	}
}

// drillIn opens the selected service's detail view (summary + embedded container
// log + in-place lifecycle), mirroring ProjectsHub.OpenProject.
func (view *Services) drillIn() tea.Cmd {
	service := view.selectedService()
	if service == "" {
		return nil
	}
	view.drilled = true
	view.flash = ""
	view.detail.SetService(service)
	view.detail.SetActive(true)
	return view.detail.Init()
}

// handleToggle enables/disables the selected service (optional services only — core
// services are always on). Lifecycle (start/stop/restart) + update now live in the
// detail; the list keeps only the enable/disable toggle.
func (view *Services) handleToggle() tea.Cmd {
	service := view.selectedService()
	if service == "" {
		return nil
	}
	status := view.statusByName(service)
	if !status.Optional {
		view.flash = ui.Muted.Render(service + " is a core service — always enabled")
		return nil
	}
	action, gerund := "enable", "enabling"
	if status.Enabled() {
		action, gerund = "disable", "disabling"
	}
	view.flash = ui.Muted.Render(gerund + " " + service + "…")
	return view.toggleCmd(action, service)
}

// statusByName returns the cached status for a service (a name-only fallback if it is
// not in the latest fetch).
func (view *Services) statusByName(service string) setup.ServiceStatus {
	for _, status := range view.statuses {
		if status.Name == service {
			return status
		}
	}
	return setup.ServiceStatus{Name: service}
}

func (view *Services) toggleCmd(action, service string) tea.Cmd {
	control := view.control
	return func() tea.Msg {
		return servicesToggleDoneMsg{action: action, service: service, err: control(action, service)}
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

// ClickSubTab forwards a sub-tab-bar click to the drilled service detail (whose
// bar is the first row of this view's content). Not handled while the list shows.
func (view *Services) ClickSubTab(x int) (cmd tea.Cmd, handled bool) {
	if !view.drilled {
		return nil, false
	}
	return view.detail.ClickSubTab(x)
}

// View renders the open service detail when drilled in, else the list table (with
// any flash).
func (view *Services) View() string {
	if view.drilled {
		return view.detail.View()
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading service status…")
	}
	// Always emit the flash slot as the LAST line (blank when empty) so the table above
	// keeps its fixed height and the bottom sits at the constant margin.
	return view.table.View() + "\n" + flashLine(view.flash)
}

func serviceRows(statuses []setup.ServiceStatus) [][]string {
	rows := make([][]string, 0, len(statuses))
	for _, status := range statuses {
		health := ui.IconFail
		if status.Healthy {
			health = ui.IconOK
		}
		rows = append(rows, []string{status.Name, status.Mode, status.State, health, status.Address})
	}
	return rows
}

// toggleFlash renders the outcome of an enable/disable toggle.
func toggleFlash(msg servicesToggleDoneMsg) string {
	verbs := map[string]string{"enable": "enabled", "disable": "disabled"}
	if msg.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + msg.action + " " + msg.service + ": " + msg.err.Error())
	}
	return ui.Success.Render(ui.IconOK + " " + verbs[msg.action] + " " + msg.service)
}

// restartFlash renders the outcome of a list-level restart (one service or all).
func restartFlash(msg servicesRestartDoneMsg) string {
	label := msg.target
	if label == "" {
		label = "all services"
	}
	if msg.err != nil {
		return ui.Failure.Render(ui.IconFail + " restart " + label + ": " + msg.err.Error())
	}
	return ui.Success.Render(ui.IconOK + " restarted " + label)
}
