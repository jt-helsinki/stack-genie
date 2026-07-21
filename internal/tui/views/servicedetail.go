package views

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/stack-genie/internal/setup"
	"github.com/jt-helsinki/stack-genie/internal/ui"
)

// ServiceStatusFetcher returns the current status of ONE named service (and whether
// it was found). Injected; the parent resolves it from the live ServicesStatus set.
type ServiceStatusFetcher func(service string) (setup.ServiceStatus, bool)

// ServiceStatsFetcher returns the live per-container resource stats for ONE service
// (a service may own several containers). Injected; the parent wires
// setup.ServiceStats(deps, name). A nil fetcher (or an error) just means no metrics.
type ServiceStatsFetcher func(service string) ([]setup.ContainerStats, error)

// ServiceUpdateRequestedMsg is emitted when the user presses `p` (update) in the
// service detail: the parent runs `ai services update <service>` in the live
// embedded terminal overlay (a real PTY → the CLI's pull spinner renders). Routing it
// through the overlay shows the image-pull progress for free, and the Services
// list/detail refresh when the overlay closes.
type ServiceUpdateRequestedMsg struct{ Service string }

// ServiceLogFollowRequestedMsg asks the parent to follow a service's container log
// live in the user's REAL terminal (via tea.ExecProcess) — the service-tier twin of
// WorkspaceLogFollowRequestedMsg. The parent runs `ai logs --service <svc> --follow`.
type ServiceLogFollowRequestedMsg struct{ Service string }

// serviceDetailRefreshedMsg carries a periodic re-fetch of the open service's status +
// per-container stats. It is generation-tagged so a stale poll from a previous
// activation (or a different service) is dropped.
type serviceDetailRefreshedMsg struct {
	status     setup.ServiceStatus
	found      bool
	containers []setup.ContainerStats
	generation int
}

// serviceDetailTickMsg re-arms the Service sub-tab's status+stats poll (heartbeat). Its
// generation lets a stale tick chain (from a previous activation) die.
type serviceDetailTickMsg struct{ generation int }

// serviceLifecycleDoneMsg reports the result of an in-place lifecycle action
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

// serviceDetailRefreshInterval is how often the Service sub-tab re-polls status+stats
// so state + metrics update live.
const serviceDetailRefreshInterval = 2 * time.Second

// serviceSubTabRows is the height the sub-tab bar (+ a blank line) reserves.
const serviceSubTabRows = 2

// The service-detail sub-tabs.
const (
	serviceTabInfo = 0 // summary + live per-container metrics
	serviceTabLogs = 1 // the scrollable container log (LogView)
)

// serviceSubTabTitles labels the detail's sub-tab bar.
var serviceSubTabTitles = []string{"Service", "Logs"}

// ServiceDetail is the per-service drill-down, structured like the workspace hub: a
// SUB-TAB bar with a "Service" tab (summary + live per-container metrics, polled every
// 2s) and a "Logs" tab (the scrollable container LogView, identical to the sandbox
// log). Tab/←→ cycle the two; the parent (Services) owns esc → back to the list. On the
// Service tab, s/x/r act on THIS service IN PLACE (an animated spinner while in flight,
// then a refresh — it does NOT pop back to the list), `p` (update) routes through the
// parent's terminal overlay, and `o` opens the console.
type ServiceDetail struct {
	info    ServiceStatusFetcher
	stats   ServiceStatsFetcher
	control ServiceController
	open    URLOpener
	log     *LogView

	name       string
	status     setup.ServiceStatus
	found      bool
	containers []setup.ContainerStats
	flash      string
	width      int
	height     int

	subIndex int
	// active is true while the Services top-level tab is visible AND this detail is open.
	active bool

	// The Service sub-tab's status+stats poll mirrors LogView's generation-guarded,
	// always-re-arming heartbeat: the tick re-arms forever; a fetch is issued only while
	// active AND on the Service sub-tab, and a slow fetch never stacks (polling guard).
	generation int
	paused     bool
	polling    bool

	// pending is the in-flight lifecycle action ("start"/"stop"/"restart"), shown as an
	// animated spinner on the state line; empty when idle.
	pending      string
	pendingFrame int
}

// NewServiceDetail builds the service-detail view over the injected per-service status
// fetcher, the per-container stats fetcher, the lifecycle controller, the console
// opener, and the container log component shown on the Logs sub-tab.
func NewServiceDetail(info ServiceStatusFetcher, stats ServiceStatsFetcher, control ServiceController, open URLOpener, log *LogView) *ServiceDetail {
	return &ServiceDetail{info: info, stats: stats, control: control, open: open, log: log}
}

func (view *ServiceDetail) Title() string { return "Service" }

// Hints are sub-tab-aware: the log's own keys on the Logs tab, the lifecycle keys on
// the Service tab (the chrome adds tab/esc for the sub-tab nav).
func (view *ServiceDetail) Hints() string {
	if view.subIndex == serviceTabLogs {
		return view.log.Hints()
	}
	return "s start · x stop · r restart · p update · o console"
}

// Service is the name of the open service (or "" before SetService).
func (view *ServiceDetail) Service() string { return view.name }

// SetService points the detail at a service and resets it to the Service sub-tab. The
// parent calls this, then Init, when the user drills into a NEW row.
func (view *ServiceDetail) SetService(name string) {
	view.name = name
	view.subIndex = serviceTabInfo
}

// SetActive forwards visibility. The status+stats heartbeat keeps ticking (cheap) but
// issues no fetch while inactive; the log is active only when the Logs sub-tab shows.
func (view *ServiceDetail) SetActive(active bool) {
	view.active = active
	view.paused = !active
	view.log.SetActive(active && view.subIndex == serviceTabLogs)
}

// SetSize splits the pane: the sub-tab bar on top, the log (on its sub-tab) fills the
// rest. The Service sub-tab renders its summary+metrics directly beneath the bar.
func (view *ServiceDetail) SetSize(width, height int) {
	view.width, view.height = width, height
	logHeight := height - serviceSubTabRows
	if logHeight < 1 {
		logHeight = 1
	}
	view.log.SetSize(width, logHeight)
}

// Init (re)starts the CURRENT sub-tab's data cycle for the open service. On a fresh
// drill-in SetService has reset subIndex to the Service tab; on a tab re-entry the
// current sub-tab is preserved. It is idempotent (generation-guarded).
func (view *ServiceDetail) Init() tea.Cmd {
	if view.name == "" {
		return nil
	}
	view.found = false
	view.containers = nil
	if view.subIndex == serviceTabLogs {
		view.log.SetActive(view.active)
		return view.log.Init()
	}
	view.log.SetActive(false)
	return view.startStatsPoll()
}

// startStatsPoll retires any previous tick chain (bumping the generation) and issues a
// fresh status+stats fetch plus the heartbeat tick.
func (view *ServiceDetail) startStatsPoll() tea.Cmd {
	view.generation++
	view.polling = false
	if view.paused {
		return view.statsTick(view.generation) // heartbeat only; fetch resumes when visible
	}
	return tea.Batch(view.issueRefresh(), view.statsTick(view.generation))
}

func (view *ServiceDetail) issueRefresh() tea.Cmd {
	view.polling = true
	return view.refreshCmd(view.generation)
}

func (view *ServiceDetail) refreshCmd(generation int) tea.Cmd {
	info := view.info
	stats := view.stats
	name := view.name
	return func() tea.Msg {
		status, found := info(name)
		var containers []setup.ContainerStats
		if stats != nil {
			containers, _ = stats(name) // best-effort: metrics simply absent on error
		}
		return serviceDetailRefreshedMsg{status: status, found: found, containers: containers, generation: generation}
	}
}

func (view *ServiceDetail) statsTick(generation int) tea.Cmd {
	return tea.Tick(serviceDetailRefreshInterval, func(time.Time) tea.Msg {
		return serviceDetailTickMsg{generation: generation}
	})
}

// Update advances the detail: the periodic refresh fills the summary + metrics; the
// heartbeat re-polls; Tab/←→ cycle the two sub-tabs; on the Service tab s/x/r act on the
// service in place (spinner) and p/o update/console; on the Logs tab every other key
// drives the container log (scroll / follow).
func (view *ServiceDetail) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case serviceDetailRefreshedMsg:
		if message.generation != view.generation {
			return nil // a stale poll from a previous activation / service
		}
		view.polling = false
		view.found = message.found
		if message.found {
			view.status = message.status
			view.containers = message.containers
		}
		return nil
	case serviceDetailTickMsg:
		if message.generation != view.generation {
			return nil // a stale tick chain
		}
		// Always re-arm the heartbeat; issue a fetch only while visible, on the Service
		// sub-tab, and with no fetch already in flight.
		if view.paused || view.subIndex != serviceTabInfo || view.polling {
			return view.statsTick(view.generation)
		}
		return tea.Batch(view.issueRefresh(), view.statsTick(view.generation))
	case serviceLifecycleDoneMsg:
		view.pending = ""
		view.flash = serviceActionFlash(message.action, message.service, message.err)
		return view.startStatsPoll() // reflect the new state immediately (stays in the detail)
	case serviceSpinnerTickMsg:
		if view.pending == "" {
			return nil // the action landed — stop ticking
		}
		view.pendingFrame++
		return view.spinnerTick()
	case tea.KeyMsg:
		switch message.String() {
		case "tab", "right", "shift+tab", "left":
			return view.switchSub()
		}
		if view.subIndex == serviceTabLogs {
			return view.log.Update(msg)
		}
		// Service sub-tab: lifecycle/console/update keys act on the open service unless one
		// is already in flight.
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
		return nil
	default:
		// Async log messages (poll tick / loaded / stream chunks) drive the container log.
		return view.log.Update(msg)
	}
}

// switchSub toggles between the two sub-tabs, pausing the outgoing data cycle and
// starting the incoming one (so at most one is polling at a time).
func (view *ServiceDetail) switchSub() tea.Cmd {
	if view.subIndex == serviceTabInfo {
		view.subIndex = serviceTabLogs
		view.log.SetActive(view.active)
		return view.log.Init()
	}
	view.subIndex = serviceTabInfo
	view.log.SetActive(false)
	return view.startStatsPoll()
}

// ClickSubTab switches to the sub-tab under click column x (the bar's own
// coordinates). handled is false when no service is open or x hits no tab.
func (view *ServiceDetail) ClickSubTab(x int) (cmd tea.Cmd, handled bool) {
	if view.name == "" {
		return nil, false
	}
	index := subTabHitIndex(view.name, serviceSubTabTitles, x)
	if index < 0 {
		return nil, false
	}
	if index == view.subIndex {
		return nil, true // already there
	}
	return view.switchSub(), true // two tabs, so a switch is always a toggle
}

// startLifecycle runs the control action as an in-view async command and shows an
// animated spinner on the state line while it is in flight (it stays in the detail).
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
	name := view.name
	return func() tea.Msg {
		if err := open(url); err != nil {
			return serviceLifecycleDoneMsg{action: "open", service: name, err: err}
		}
		return serviceLifecycleDoneMsg{action: "open", service: name}
	}
}

func (view *ServiceDetail) View() string {
	if view.name == "" {
		return ui.Muted.Render("no service selected — open one from the Services list")
	}
	bar := renderSubTabBar(view.name, serviceSubTabTitles, view.subIndex)
	if view.subIndex == serviceTabLogs {
		return lipgloss.JoinVertical(lipgloss.Left, bar, "", view.log.View())
	}
	if !view.found {
		return lipgloss.JoinVertical(lipgloss.Left, bar, "", ui.Muted.Render("loading "+view.name+"…"))
	}
	return lipgloss.JoinVertical(lipgloss.Left, bar, "", view.infoBody())
}

// infoBody renders the Service sub-tab: the service summary, then one live metrics block
// per container, then the flash slot.
func (view *ServiceDetail) infoBody() string {
	health := "no"
	if view.status.Healthy {
		health = "yes"
	}
	var body strings.Builder
	body.WriteString(field("mode", view.status.Mode))
	if view.pending != "" {
		glyph := ui.Success.Render(spinnerFrames[view.pendingFrame%len(spinnerFrames)])
		body.WriteString(field("state", glyph+ui.Muted.Render(" "+view.pending+"ing…")))
	} else {
		body.WriteString(field("state", view.status.State))
	}
	body.WriteString(field("healthy", health))
	body.WriteString(field("address", view.status.Address))
	body.WriteString(field("console", view.status.Console))
	body.WriteString("\n")
	body.WriteString(ui.Heading.Render("Containers") + ui.Muted.Render("  · live, updates every 2s") + "\n")
	if len(view.containers) == 0 {
		body.WriteString(ui.Muted.Render("  collecting live metrics…") + "\n")
	}
	for _, container := range view.containers {
		body.WriteString(view.containerBlock(container))
	}
	body.WriteString(view.flash)
	return body.String()
}

// containerBlock renders one container's id/state/ports/uptime + live metrics under a
// "── <name> ──" heading (matching the log pane's per-container sections). A container
// that is not running shows just its state.
func (view *ServiceDetail) containerBlock(container setup.ContainerStats) string {
	var block strings.Builder
	block.WriteString("\n" + ui.Muted.Render("── ") + ui.Value.Render(container.Container) + ui.Muted.Render(" ──") + "\n")
	if !container.Found {
		block.WriteString(field("state", container.State))
		return block.String()
	}
	memory := container.MemUsage
	if container.MemPercent != "" {
		memory += " (" + container.MemPercent + ")"
	}
	block.WriteString(field("container id", container.ID))
	block.WriteString(field("state", container.State))
	block.WriteString(field("cpu", container.CPUPercent))
	block.WriteString(field("memory", memory))
	block.WriteString(field("net i/o", container.NetIO))
	block.WriteString(field("disk i/o", container.BlockIO))
	block.WriteString(field("ports", container.Ports))
	block.WriteString(field("started", container.StartedAt))
	block.WriteString(field("uptime", container.Uptime))
	return block.String()
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
