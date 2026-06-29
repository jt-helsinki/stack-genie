package views

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// LogTextTailer returns the recent captured output of the subject being followed —
// a workspace microVM (`msb logs --tail`) or a host service container
// (`<runtime> logs --tail`). Injected so the component is generic; the parent wires
// the concrete tailer (Manager.WorkspaceLogTail / setup.ServiceLogTail).
type LogTextTailer func() (string, error)

// LogSubjectRunning reports whether the subject is currently running. The log is
// only fetched/shown while it is — a stopped subject shows nothing rather than the
// stale captured output of a previous session. Injected; the parent wires it over
// the subject's live status. A nil running-check means "always fetch".
type LogSubjectRunning func() bool

// LogViewLabels are the subject-specific strings the generic LogView renders, so
// one component backs BOTH the embedded workspace log and the embedded service log
// (the only difference between them is wording + the follow-in-terminal message).
type LogViewLabels struct {
	// NoSubject is shown when there is no subject selected (e.g. no workspace open).
	// Empty means the subject is always present (the Services detail always has one).
	NoSubject string
	// NotRunning is shown when the subject is not running (its log only appears live).
	NotRunning string
	// Loading is shown before the first poll lands.
	Loading string
	// Empty is shown when the subject is running but has produced no output yet.
	Empty string
}

// logViewRefreshInterval is how often the component re-polls its tailer so new
// output "streams in" while it is visible.
const logViewRefreshInterval = 2 * time.Second

// logViewLoadedMsg carries one poll result, tagged with the generation it was issued
// under so a result from a previous activation is discarded. notRunning marks that
// the subject was not running at poll time (show nothing, not stale output).
type logViewLoadedMsg struct {
	content    string
	err        error
	notRunning bool
	generation int
}

// logViewTickMsg re-arms the auto-refresh. Its generation lets a stale tick chain
// (from a previous Init) die so only the latest activation keeps polling.
type logViewTickMsg struct{ generation int }

// LogView is the reusable scrollable, auto-refreshing log component embedded beneath
// a detail summary (the workspace log under the Workspace tab; the container log
// under the Services detail). It re-polls on a timer so new lines stream in, follows
// the tail unless the user has scrolled up, and is fully scrollable (PgUp/PgDn /
// arrows; the app does not capture the mouse, so host-terminal selection works).
// Polling is generation-guarded so re-entering the view starts a single fresh cycle
// rather than stacking timers, and PAUSES while the view is not visible (SetActive).
type LogView struct {
	tail    LogTextTailer
	running LogSubjectRunning
	// subject resolves the current subject identity (project name / service name).
	// Empty means "no subject" — the view shows NoSubject and runs no poll.
	subject func() string
	labels  LogViewLabels
	// follow builds the parent message asking to follow the log live in the REAL
	// terminal (msb logs -f / docker logs -f). nil disables the f/enter follow.
	follow func(subject string) tea.Msg

	viewport   viewport.Model
	loaded     bool
	empty      bool
	notRunning bool
	err        error
	generation int
	width      int
	height     int
	// paused is set while the embedding tab/view is NOT visible: the heartbeat tick
	// keeps running (cheap) but issues NO tailer call, so the background poll never
	// contends with the active view's calls. The embedder toggles it via SetActive.
	paused bool
	// polling guards against stacking: a slow tailer call must not let the tick fire a
	// second concurrent call on top of the first. True while a poll is in flight.
	polling bool
	// pending holds the latest normalized content while the user has scrolled UP (not
	// at the bottom): the view is FROZEN so a refresh does not re-render the pane and
	// wipe an in-progress text selection. Applied when the user scrolls back down.
	pending string
}

// NewLogView builds a generic log component over the injected tailer, an optional
// running-check, the subject resolver, the subject-specific labels, and an optional
// follow-in-terminal message factory.
func NewLogView(tail LogTextTailer, running LogSubjectRunning, subject func() string, labels LogViewLabels, follow func(subject string) tea.Msg) *LogView {
	return &LogView{
		tail: tail, running: running, subject: subject, labels: labels, follow: follow,
		viewport: viewport.New(0, 0),
	}
}

func (view *LogView) Title() string { return "Log" }

// SetActive marks whether the embedding view is currently visible. While inactive
// the poll is PAUSED — the heartbeat tick keeps running but issues no tailer call —
// so it does not contend with the active view's calls. It only sets the flag; the
// Init() the embedder calls right after does the immediate refresh.
func (view *LogView) SetActive(active bool) { view.paused = !active }

// issueLoad fires one poll and arms the in-flight guard so the tick cannot stack a
// second concurrent tailer call on top of a slow one.
func (view *LogView) issueLoad() tea.Cmd {
	view.polling = true
	return view.loadCmd(view.generation)
}

func (view *LogView) Hints() string {
	if view.follow == nil {
		return "↑/↓ scroll · PgUp/PgDn page · r refresh"
	}
	return "↑/↓ scroll · PgUp/PgDn page · f follow in terminal · r refresh"
}

// Reset clears the displayed log immediately and discards any in-flight poll (by
// bumping the generation). The embedder calls this when a lifecycle action starts so
// the PREVIOUS session's captured output does not linger while the subject is
// recreated — the view shows "loading…" and then streams the fresh log.
func (view *LogView) Reset() {
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

func (view *LogView) SetSize(width, height int) {
	view.width = width
	view.height = height
	view.viewport.Width = width
	if height > 0 {
		view.viewport.Height = height
	}
}

// Init starts a fresh poll cycle for the current subject. Re-init bumps the
// generation so the previous tick chain stops, and resets the viewport to FOLLOW the
// tail — so a re-activation never leaves the view stuck frozen at a stale position.
func (view *LogView) Init() tea.Cmd {
	if view.subject() == "" {
		return nil
	}
	view.generation++
	view.loaded = false
	view.pending = ""
	view.polling = false
	view.viewport.GotoBottom()
	// Start the heartbeat tick always; issue the first poll only when visible (not
	// paused) so a re-init while parked on another tab fires no tailer call.
	if view.paused {
		return view.tickCmd(view.generation)
	}
	return tea.Batch(view.issueLoad(), view.tickCmd(view.generation))
}

func (view *LogView) loadCmd(generation int) tea.Cmd {
	tail := view.tail
	running := view.running
	return func() tea.Msg {
		// Only read the log while the subject is running — a stopped subject must show
		// nothing, not the stale captured output of a previous session.
		if running != nil && !running() {
			return logViewLoadedMsg{notRunning: true, generation: generation}
		}
		content, err := tail()
		if err == nil {
			// Apply the terminal control codes HERE — in the async poll goroutine, NOT in
			// Update — so this (potentially heavy on a large TUI-redraw buffer) work never
			// runs on the bubbletea event loop and stalls input.
			content = normalizeTerminalOutput(content)
		}
		return logViewLoadedMsg{content: content, err: err, generation: generation}
	}
}

func (view *LogView) tickCmd(generation int) tea.Cmd {
	return tea.Tick(logViewRefreshInterval, func(time.Time) tea.Msg {
		return logViewTickMsg{generation: generation}
	})
}

func (view *LogView) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case logViewLoadedMsg:
		if message.generation != view.generation {
			return nil // a stale poll from a previous activation
		}
		view.polling = false // this poll landed — the tick may issue the next
		view.loaded = true
		if message.notRunning {
			// Subject not running: show nothing (not the previous session's log).
			view.notRunning = true
			view.err = nil
			view.empty = false
			return nil
		}
		view.notRunning = false
		view.err = message.err
		if message.err == nil {
			// Content is already normalized in the poll goroutine (loadCmd) — the terminal
			// control codes (\r / cursor moves / erase-line) have collapsed progress
			// redraws in place. Just place it; no heavy work on the event loop.
			content := message.content
			view.empty = strings.TrimSpace(content) == ""
			if view.viewport.AtBottom() {
				// Following the tail: re-render + pin to the bottom.
				view.viewport.SetContent(content)
				view.viewport.GotoBottom()
				view.pending = ""
			} else {
				// Scrolled up to read/select: FREEZE the visible content (don't re-render,
				// which would wipe a text selection); buffer the latest and apply it when
				// the user returns to the bottom.
				view.pending = content
			}
		}
		return nil
	case logViewTickMsg:
		if message.generation != view.generation {
			return nil // a stale tick chain
		}
		// Always re-arm the heartbeat. Issue a poll only when visible and no poll is
		// already in flight — so a parked view makes no tailer call and a slow call
		// never stacks a second.
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
			if view.follow == nil {
				return nil
			}
			if subject := view.subject(); subject != "" {
				follow := view.follow
				return func() tea.Msg { return follow(subject) }
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

func (view *LogView) View() string {
	if view.subject() == "" {
		if view.labels.NoSubject != "" {
			return ui.Muted.Render(view.labels.NoSubject)
		}
		return ui.Muted.Render(view.labels.Loading)
	}
	if view.notRunning {
		return ui.Muted.Render(view.labels.NotRunning)
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render(view.labels.Loading)
	}
	if view.empty {
		return ui.Muted.Render(view.labels.Empty)
	}
	return view.viewport.View()
}
