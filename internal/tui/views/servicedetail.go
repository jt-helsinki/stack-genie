package views

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// ServiceStatusFetcher returns the current status of ONE named service (and whether
// it was found). Injected; the parent resolves it from the live ServicesStatus set.
// It mirrors ProjectInfoFetcher for the workspace detail.
type ServiceStatusFetcher func(service string) (setup.ServiceStatus, bool)

// ServiceUpdateRequestedMsg is emitted when the user presses `p` (update) in the
// service detail: the parent runs `ai services update <service>` in the live
// embedded terminal overlay (a real PTY → the CLI's pull spinner renders), EXACTLY
// like views.ModelsPullRequestedMsg / AppActionRequestedMsg. Routing it through the
// overlay shows the image-pull progress for free, and the Services list/detail
// refresh when the overlay closes.
type ServiceUpdateRequestedMsg struct{ Service string }

// ServiceLogFollowRequestedMsg asks the parent to follow a service's container log
// live in the user's REAL terminal (via tea.ExecProcess) — the service-tier twin of
// WorkspaceLogFollowRequestedMsg. The parent runs `ai logs --service <svc> --follow`
// (setup.FollowServiceLogs → `<runtime> logs -f`), restoring the TUI on exit.
type ServiceLogFollowRequestedMsg struct{ Service string }

// serviceDetailRefreshedMsg carries a re-fetch of the open service's status.
type serviceDetailRefreshedMsg struct {
	status setup.ServiceStatus
	found  bool
}

// serviceActionDoneMsg reports the result of an in-place lifecycle action
// (start/stop/restart) run on the open service.
type serviceLifecycleDoneMsg struct {
	action  string
	service string
	err     error
}

// serviceSpinnerTickMsg advances the in-flight lifecycle spinner one frame.
type serviceSpinnerTickMsg struct{}

// serviceSpinnerInterval is how often the in-place lifecycle spinner animates.
const serviceSpinnerInterval = 120 * time.Millisecond

// serviceSummaryRows is the fixed height of the detail summary block above the
// embedded container log: name (1) + mode/state/health/address/console (5) + flash
// slot (1) + blank (1) + the "Container log" heading (1) = 9.
const serviceSummaryRows = 9

// ServiceDetail is the per-service drill-down: its summary (name / mode / state /
// health / address / console) on top with the live CONTAINER LOG embedded beneath
// it — mirroring views.Project (the workspace detail) with its embedded workspace
// log. The log is the generic LogView (logview.go) configured for a container, so it
// streams/scrolls/freezes/normalizes identically. In the detail, s/x/r act on THIS
// service IN PLACE (an animated status-line spinner while in flight, then a refresh —
// it does NOT pop back to the list) and `p` (update) routes through the parent's
// terminal overlay (ServiceUpdateRequestedMsg) so the CLI pull spinner shows.
type ServiceDetail struct {
	info    ServiceStatusFetcher
	control ServiceController
	open    URLOpener
	log     *LogView

	name   string
	status setup.ServiceStatus
	found  bool
	flash  string
	width  int
	height int
	// pending is the in-flight lifecycle action ("start"/"stop"/"restart"), shown as
	// an animated spinner on the state line; empty when idle. Mirrors Project's
	// pending/pendingFrame braille spinner, but the action runs as an in-view async
	// command (setup.ControlService) rather than a detached process the app polls.
	pending      string
	pendingFrame int
}

// NewServiceDetail builds the service-detail view over the injected per-service
// status fetcher, the lifecycle controller, the console opener, and the container
// log component shown beneath the summary.
func NewServiceDetail(info ServiceStatusFetcher, control ServiceController, open URLOpener, log *LogView) *ServiceDetail {
	return &ServiceDetail{info: info, control: control, open: open, log: log}
}

func (view *ServiceDetail) Title() string { return "Service" }

func (view *ServiceDetail) Hints() string {
	return "s start · x stop · r restart · p update · o console · ↑/↓ scroll log · f follow · esc back"
}

// Service is the name of the open service (or "" before SetService).
func (view *ServiceDetail) Service() string { return view.name }

// SetService points the detail at a service. The parent calls this, then Init, when
// the user drills into a row.
func (view *ServiceDetail) SetService(name string) { view.name = name }

// SetActive forwards visibility to the embedded log, so the 2s container-log poll
// only runs while the detail is the visible view (and not while backed out to the
// list or parked on another top-level tab).
func (view *ServiceDetail) SetActive(active bool) { view.log.SetActive(active) }

// SetSize splits the pane: a fixed summary block on top, the embedded container log
// fills the rest.
func (view *ServiceDetail) SetSize(width, height int) {
	view.width, view.height = width, height
	logHeight := height - serviceSummaryRows
	if logHeight < 1 {
		logHeight = 1
	}
	view.log.SetSize(width, logHeight)
}

// Init refreshes the open service AND starts the embedded log polling.
func (view *ServiceDetail) Init() tea.Cmd {
	if view.name == "" {
		return nil
	}
	return tea.Batch(view.refreshCmd(), view.log.Init())
}

func (view *ServiceDetail) refreshCmd() tea.Cmd {
	info := view.info
	name := view.name
	return func() tea.Msg {
		status, found := info(name)
		return serviceDetailRefreshedMsg{status: status, found: found}
	}
}

// Update advances the detail: a refresh fills the summary; s/x/r act on THIS service
// in place (with a spinner) and stay here; p emits the update request; o opens the
// console; every other key drives the embedded container log (scroll / follow).
func (view *ServiceDetail) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case serviceDetailRefreshedMsg:
		view.found = message.found
		if message.found {
			view.status = message.status
			if view.pending == "" {
				view.flash = "" // a fresh status supersedes any stale hint (unless mid-action)
			}
		}
		return nil
	case serviceLifecycleDoneMsg:
		view.pending = ""
		view.flash = serviceActionFlash(message.action, message.service, message.err)
		// Re-fetch the status + restart the log poll so the detail reflects the action
		// immediately — WITHOUT popping back to the list.
		return view.Init()
	case serviceSpinnerTickMsg:
		if view.pending == "" {
			return nil // the action landed — stop ticking
		}
		view.pendingFrame++
		return view.spinnerTick()
	case tea.KeyMsg:
		// Lifecycle/console/update keys act on the open service (unless one is already
		// in flight); every OTHER key drives the embedded log (scroll / follow), so the
		// container log is usable without a separate pane.
		if view.pending == "" {
			switch message.String() {
			case "s", "x", "r":
				return view.startLifecycle(map[string]string{"s": "start", "x": "stop", "r": "restart"}[message.String()])
			case "p":
				name := view.name
				return func() tea.Msg { return ServiceUpdateRequestedMsg{Service: name} }
			case "o":
				return view.openConsole()
			}
		}
		return view.log.Update(msg)
	default:
		// Async log messages (poll tick / loaded) drive the embedded log.
		return view.log.Update(msg)
	}
}

// startLifecycle runs the control action as an in-view async command and shows an
// animated spinner on the state line while it is in flight (it stays in the detail —
// it does NOT pop back to the list).
func (view *ServiceDetail) startLifecycle(action string) tea.Cmd {
	view.pending = action
	view.pendingFrame = 0
	view.flash = ""
	control := view.control
	name := view.name
	run := func() tea.Msg {
		return serviceLifecycleDoneMsg{action: action, service: name, err: control(action, name)}
	}
	return tea.Batch(run, view.spinnerTick())
}

func (view *ServiceDetail) spinnerTick() tea.Cmd {
	return tea.Tick(serviceSpinnerInterval, func(time.Time) tea.Msg { return serviceSpinnerTickMsg{} })
}

// openConsole opens the open service's admin console (if it has one).
func (view *ServiceDetail) openConsole() tea.Cmd {
	if view.status.Console == "" {
		view.flash = ui.Muted.Render(view.name + " has no admin console")
		return nil
	}
	open := view.open
	url := view.status.Console
	return func() tea.Msg {
		if err := open(url); err != nil {
			return serviceLifecycleDoneMsg{action: "open", service: view.name, err: err}
		}
		return serviceLifecycleDoneMsg{action: "open", service: view.name}
	}
}

func (view *ServiceDetail) View() string {
	if view.name == "" {
		return ui.Muted.Render("no service selected — open one from the Services list")
	}
	if !view.found {
		return ui.Muted.Render("loading " + view.name + "…")
	}
	// Fixed summary block (serviceSummaryRows lines), then the embedded container log
	// fills the rest. The flash slot is always emitted (blank when empty) so the
	// summary height is constant and the log below never shifts.
	health := "no"
	if view.status.Healthy {
		health = "yes"
	}
	var body strings.Builder
	body.WriteString(ui.Heading.Render(view.status.Name) + "\n") // 1
	body.WriteString(field("mode", view.status.Mode))            // 2
	if view.pending != "" {
		glyph := ui.Success.Render(spinnerFrames[view.pendingFrame%len(spinnerFrames)])
		body.WriteString(field("state", glyph+ui.Muted.Render(" "+view.pending+"ing…"))) // 3
	} else {
		body.WriteString(field("state", view.status.State)) // 3
	}
	body.WriteString(field("healthy", health))                  // 4
	body.WriteString(field("address", view.status.Address))     // 5
	body.WriteString(field("console", view.status.Console))     // 6
	body.WriteString(view.flash + "\n")                         // 7 (flash slot, blank when empty)
	body.WriteString("\n")                                      // 8
	body.WriteString(ui.Heading.Render("Container log") + "\n") // 9
	body.WriteString(view.log.View())
	return body.String()
}

// serviceActionFlash renders the outcome of an in-place lifecycle/console action.
func serviceActionFlash(action, service string, err error) string {
	verbs := map[string]string{
		"start": "started", "stop": "stopped", "restart": "restarted", "open": "opened console for",
	}
	if err != nil {
		return ui.Failure.Render(ui.IconFail + " " + action + " " + service + ": " + err.Error())
	}
	return ui.Success.Render(ui.IconOK + " " + verbs[action] + " " + service)
}
