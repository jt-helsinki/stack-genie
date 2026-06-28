package views

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// WorkspaceLogTailer returns the recent captured output of the CURRENT workspace
// microVM (msb logs --tail). Injected; the parent wires Manager.WorkspaceLogTail for
// the live current project.
type WorkspaceLogTailer func() (string, error)

// WorkspaceRunning reports whether the CURRENT workspace microVM is running. The log
// is only fetched/shown while it is — a stopped workspace shows nothing rather than
// the stale captured output of a previous session. Injected; the parent wires it
// over the project's live status.
type WorkspaceRunning func() bool

// workspaceLogRefreshInterval is how often the tab re-polls the Workspace Log so new
// output "streams in" while the tab is open.
const workspaceLogRefreshInterval = 2 * time.Second

// workspaceLogLoadedMsg carries one poll result, tagged with the generation it was
// issued under so a result from a previous activation is discarded. notRunning marks
// that the workspace was not running at poll time (show nothing, not stale output).
type workspaceLogLoadedMsg struct {
	content    string
	err        error
	notRunning bool
	generation int
}

// workspaceLogTickMsg re-arms the auto-refresh. Its generation lets a stale tick chain
// (from a previous Init) die so only the latest activation keeps polling.
type workspaceLogTickMsg struct{ generation int }

// WorkspaceLog is the per-workspace "Workspace Log" sub-tab: a scrollable, auto-
// refreshing view of the microVM's captured output (msb logs). It re-polls on a
// timer so new lines stream in, follows the tail unless the user has scrolled up,
// and is fully scrollable (mouse wheel / PgUp/PgDn / arrows). Polling is generation-
// guarded so re-entering the tab (or switching workspace) starts a single fresh
// cycle rather than stacking timers.
type WorkspaceLog struct {
	tail    WorkspaceLogTailer
	running WorkspaceRunning
	project func() string

	viewport   viewport.Model
	loaded     bool
	empty      bool
	notRunning bool
	err        error
	generation int
	width      int
	height     int
	// paused is set while the Workspace sub-tab is NOT the visible one: the 2s
	// heartbeat tick keeps running (cheap) but issues NO `msb logs` call, so the
	// background poll never contends with the active tab's in-VM exec calls
	// (sessions/apps/shell). The hub toggles it via SetActive on every sub-tab
	// switch. Resuming issues an immediate refresh.
	paused bool
	// polling guards against stacking: a slow `msb logs` (VM busy) must not let the
	// 2s tick fire a second concurrent call on top of the first. True while a poll
	// is in flight; the tick skips issuing another until the result lands.
	polling bool
	// pending holds the latest normalized content while the user has scrolled UP
	// (not at the bottom): the view is FROZEN so a 2s refresh does not re-render the
	// pane and wipe an in-progress text selection. It is applied when the user scrolls
	// back to the bottom.
	pending string
}

// NewWorkspaceLog builds the Workspace Log view over the injected tailer, a
// running-check (the log is only fetched/shown while the workspace is running), and
// the current-project resolver.
func NewWorkspaceLog(tail WorkspaceLogTailer, running WorkspaceRunning, project func() string) *WorkspaceLog {
	return &WorkspaceLog{tail: tail, running: running, project: project, viewport: viewport.New(0, 0)}
}

func (view *WorkspaceLog) Title() string { return "Workspace Log" }

// SetActive marks whether the Workspace sub-tab is currently the visible one. While
// inactive the poll is PAUSED — the heartbeat tick keeps running but issues no
// `msb logs` call — so it does not contend with the active tab's in-VM exec calls.
// It only sets the flag; the Init() the hub calls right after does the immediate
// refresh (and re-follows the tail).
func (view *WorkspaceLog) SetActive(active bool) { view.paused = !active }

// issueLoad fires one poll and arms the in-flight guard so the 2s tick cannot stack
// a second concurrent `msb logs` on top of a slow one.
func (view *WorkspaceLog) issueLoad() tea.Cmd {
	view.polling = true
	return view.loadCmd(view.generation)
}

func (view *WorkspaceLog) Hints() string {
	return "↑/↓ scroll · PgUp/PgDn page · f follow in terminal · r refresh"
}

// WorkspaceLogFollowRequestedMsg asks the parent to open a live `msb logs -f` follow in
// the user's REAL terminal (via tea.ExecProcess) — a non-embedded view that renders
// natively (selectable, and animated in place when the captured stream has the
// control codes), restoring the TUI on exit.
type WorkspaceLogFollowRequestedMsg struct{ Project string }

// Reset clears the displayed log immediately and discards any in-flight poll
// (by bumping the generation). The parent calls this when a lifecycle action
// starts (start/restart/stop) so the PREVIOUS session's captured output does not
// linger on screen while the microVM is recreated — the view shows "loading…" and
// then streams the fresh log.
func (view *WorkspaceLog) Reset() {
	view.generation++
	view.loaded = false
	view.empty = false
	view.notRunning = false
	view.err = nil
	view.pending = ""
	view.polling = false
	view.viewport.SetContent("")
	view.viewport.GotoTop()
}

func (view *WorkspaceLog) SetSize(width, height int) {
	view.width = width
	view.height = height
	view.viewport.Width = width
	if height > 0 {
		view.viewport.Height = height
	}
}

// Init starts a fresh poll cycle for the current project. Re-init (returning to the
// tab, a workspace change, or after a suspended shell) bumps the generation so the
// previous tick chain stops, and resets the viewport to FOLLOW the tail — so a
// re-activation never leaves the view stuck frozen at a stale scroll position.
func (view *WorkspaceLog) Init() tea.Cmd {
	if view.project() == "" {
		return nil
	}
	view.generation++
	view.loaded = false
	view.pending = ""
	view.polling = false
	view.viewport.GotoBottom()
	// Start the heartbeat tick always; issue the first poll only when the tab is
	// visible (not paused) so a re-init while parked on another sub-tab does not
	// fire a `msb logs` call.
	if view.paused {
		return view.tickCmd(view.generation)
	}
	return tea.Batch(view.issueLoad(), view.tickCmd(view.generation))
}

func (view *WorkspaceLog) loadCmd(generation int) tea.Cmd {
	tail := view.tail
	running := view.running
	return func() tea.Msg {
		// Only read the log while the workspace is running — a stopped workspace must
		// show nothing, not the stale captured output of a previous session.
		if running != nil && !running() {
			return workspaceLogLoadedMsg{notRunning: true, generation: generation}
		}
		content, err := tail()
		if err == nil {
			// Apply the terminal control codes HERE — in the async poll goroutine, NOT
			// in Update — so this (potentially heavy on a large TUI-redraw buffer) work
			// never runs on the bubbletea event loop and stalls input.
			content = normalizeTerminalOutput(content)
		}
		return workspaceLogLoadedMsg{content: content, err: err, generation: generation}
	}
}

func (view *WorkspaceLog) tickCmd(generation int) tea.Cmd {
	return tea.Tick(workspaceLogRefreshInterval, func(time.Time) tea.Msg {
		return workspaceLogTickMsg{generation: generation}
	})
}

func (view *WorkspaceLog) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case workspaceLogLoadedMsg:
		if message.generation != view.generation {
			return nil // a stale poll from a previous activation
		}
		view.polling = false // this poll landed — the tick may issue the next
		view.loaded = true
		if message.notRunning {
			// Workspace not running: show nothing (not the previous session's log).
			view.notRunning = true
			view.err = nil
			view.empty = false
			return nil
		}
		view.notRunning = false
		view.err = message.err
		if message.err == nil {
			// Content is already normalized in the poll goroutine (loadCmd) — the
			// terminal control codes (\r / cursor moves / erase-line) have collapsed
			// progress redraws in place. Just place it; no heavy work on the event loop.
			content := message.content
			view.empty = strings.TrimSpace(content) == ""
			if view.viewport.AtBottom() {
				// Following the tail: re-render + pin to the bottom.
				view.viewport.SetContent(content)
				view.viewport.GotoBottom()
				view.pending = ""
			} else {
				// Scrolled up to read/select: FREEZE the visible content (don't
				// re-render, which would wipe a text selection); buffer the latest and
				// apply it when the user returns to the bottom.
				view.pending = content
			}
		}
		return nil
	case workspaceLogTickMsg:
		if message.generation != view.generation {
			return nil // a stale tick chain
		}
		// Always re-arm the heartbeat. Issue a poll only when visible and no poll is
		// already in flight — so a parked tab makes no `msb logs` call and a slow
		// call never stacks a second.
		if view.paused || view.polling {
			return view.tickCmd(view.generation)
		}
		return tea.Batch(view.issueLoad(), view.tickCmd(view.generation))
	case tea.KeyMsg:
		switch message.String() {
		case "r":
			if view.polling {
				return nil // a poll is already in flight
			}
			return view.issueLoad()
		case "f", "enter":
			if project := view.project(); project != "" {
				return func() tea.Msg { return WorkspaceLogFollowRequestedMsg{Project: project} }
			}
			return nil
		}
	}
	var cmd tea.Cmd
	view.viewport, cmd = view.viewport.Update(msg)
	// Returning to the bottom resumes following: apply the content buffered while the
	// view was frozen (scrolled up).
	if view.pending != "" && view.viewport.AtBottom() {
		view.viewport.SetContent(view.pending)
		view.viewport.GotoBottom()
		view.pending = ""
	}
	return cmd
}

func (view *WorkspaceLog) View() string {
	if view.project() == "" {
		return ui.Muted.Render("no workspace selected — open one from the Workspaces view")
	}
	if view.notRunning {
		return ui.Muted.Render("workspace not running — its log appears here while it is running (press s to start)")
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading workspace log…")
	}
	if view.empty {
		return ui.Muted.Render("no workspace output captured yet — it streams in as the workspace runs")
	}
	return view.viewport.View()
}
