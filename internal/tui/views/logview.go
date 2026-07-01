package views

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// LogStream is a live log subscription: Recv blocks for the next chunk (history first,
// then new entries), and Close stops it. Satisfied structurally by workspace.LogStream
// (the SDK-backed stream the parent wires in for the workspace log).
type LogStream interface {
	Recv(ctx context.Context) (string, error)
	Close() error
}

// LogStreamOpener opens a fresh stream for the CURRENT subject. When set on a LogView,
// the component STREAMS (push) instead of polling: it loads recent history then
// appends new entries as they arrive, with full scrollback. nil → poll mode.
type LogStreamOpener func(ctx context.Context) (LogStream, error)

// logStreamBufferCap bounds the in-memory streamed scrollback (trimmed on a line
// boundary), so a long-lived stream cannot grow without limit.
const logStreamBufferCap = 256 * 1024

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

// logViewStreamOpenedMsg carries the opened stream (or an open error), tagged with its
// generation. cancel cancels the stream's context (unblocking a pending Recv).
type logViewStreamOpenedMsg struct {
	stream     LogStream
	ctx        context.Context
	cancel     context.CancelFunc
	err        error
	generation int
}

// logViewStreamChunkMsg carries one Recv result from the stream, tagged with its
// generation so chunks from a previous activation/subject are discarded. raw is the
// accumulated (capped) scrollback; content is its normalized render — both computed
// in the Recv command goroutine so the (potentially heavy) normalize never runs on
// the bubbletea event loop.
type logViewStreamChunkMsg struct {
	raw        string
	content    string
	err        error
	generation int
}

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

	// Streaming mode (set when openStream != nil): the component opens ONE live log
	// stream per activation, loads history then appends new entries via re-armed Recv
	// commands — no polling. streamBuf is the accumulated raw scrollback (capped);
	// streamCtx/streamCancel govern the in-flight Recv so closeStream can unblock it.
	openStream   LogStreamOpener
	streamHandle LogStream
	streamCtx    context.Context
	streamCancel context.CancelFunc
	streamBuf    string

	// debugFilter identifies high-volume, low-signal "debug" lines (e.g. the
	// microsandbox agent-relay connect/disconnect churn) that are HIDDEN unless the
	// user toggles debug on with `d`. nil → no filtering / no toggle. content caches
	// the last full (unfiltered, normalized) text so a toggle re-renders without a
	// re-fetch, honouring the scroll freeze.
	debugFilter func(string) bool
	showDebug   bool
	content     string
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

// SetActive marks whether the embedding view is currently visible. While inactive the
// poll is PAUSED. In streaming mode, going inactive also CLOSES the live stream (it is
// reopened on the next Init when the view becomes visible) so no Recv lingers on a
// hidden tab; the embedder calls SetActive(true) then Init() on (re)activation.
func (view *LogView) SetActive(active bool) {
	view.paused = !active
	if !active && view.openStream != nil {
		view.closeStream()
	}
}

// closeStream cancels the in-flight Recv and closes the live stream handle
// (idempotent). It bumps the generation so any chunk a Recv returns AFTER close —
// even a successful one that races the cancel — is dropped by the generation guard
// and never re-arms a Recv on the now-nil handle.
func (view *LogView) closeStream() {
	view.generation++
	if view.streamCancel != nil {
		view.streamCancel()
		view.streamCancel = nil
	}
	view.streamCtx = nil
	if view.streamHandle != nil {
		_ = view.streamHandle.Close()
		view.streamHandle = nil
	}
}

// streaming reports whether this view is configured for push streaming.
func (view *LogView) streaming() bool { return view.openStream != nil }

// setContent caches the full (unfiltered) normalized content and renders the visible
// view (debug lines filtered unless toggled on), honouring the scroll freeze.
func (view *LogView) setContent(content string) {
	view.content = content
	view.renderContent()
}

// renderContent re-applies the debug filter + scroll freeze to the cached content.
func (view *LogView) renderContent() {
	display := view.visibleContent()
	view.empty = strings.TrimSpace(display) == ""
	if view.viewport.AtBottom() {
		// Following the tail: re-render + pin to the bottom.
		view.viewport.SetContent(display)
		view.viewport.GotoBottom()
		view.pending = ""
	} else {
		// Scrolled up to read/select: FREEZE (don't re-render, which would wipe a text
		// selection); buffer the latest and apply it when the user returns to the bottom.
		view.pending = display
	}
}

// visibleContent is the cached content with debug lines removed unless showDebug is on.
func (view *LogView) visibleContent() string {
	if view.debugFilter == nil || view.showDebug {
		return view.content
	}
	lines := strings.Split(view.content, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if view.debugFilter(line) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// issueLoad fires one poll and arms the in-flight guard so the tick cannot stack a
// second concurrent tailer call on top of a slow one.
func (view *LogView) issueLoad() tea.Cmd {
	view.polling = true
	return view.loadCmd(view.generation)
}

func (view *LogView) Hints() string {
	hint := "↑/↓ scroll · PgUp/PgDn page"
	if view.follow != nil {
		hint += " · f follow in terminal"
	}
	hint += " · r refresh"
	if view.debugFilter != nil {
		state := "off"
		if view.showDebug {
			state = "on"
		}
		hint += " · d debug (" + state + ")"
	}
	return hint
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
	if view.streaming() {
		view.closeStream()
		view.streamBuf = ""
	}
	view.content = ""
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

	// Streaming mode: open ONE live stream for this activation (history + follow). A
	// stopped subject shows nothing rather than a stale stream. Bumping the generation
	// above already orphans any previous stream's chunks; close its handle too.
	if view.streaming() {
		view.closeStream()
		view.streamBuf = ""
		view.notRunning = false
		if view.running != nil && !view.running() {
			view.loaded = true
			view.notRunning = true
			return nil
		}
		if view.paused {
			return nil // opened on (re)activation
		}
		return view.openStreamCmd(view.generation)
	}

	// Poll mode: start the heartbeat tick always; issue the first poll only when
	// visible (not paused) so a re-init while parked on another tab fires no call.
	if view.paused {
		return view.tickCmd(view.generation)
	}
	return tea.Batch(view.issueLoad(), view.tickCmd(view.generation))
}

// openStreamCmd opens the live stream off the event loop, under a fresh cancelable
// context so closeStream can unblock a pending Recv.
func (view *LogView) openStreamCmd(generation int) tea.Cmd {
	opener := view.openStream
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := opener(ctx)
		if err != nil {
			cancel()
			return logViewStreamOpenedMsg{err: err, generation: generation}
		}
		return logViewStreamOpenedMsg{stream: stream, ctx: ctx, cancel: cancel, generation: generation}
	}
}

// recvCmd blocks for the next chunk on the stream, appends it to buffer (the caller's
// current scrollback snapshot — chunks are processed one at a time through Update, so
// the snapshot is race-free), caps it, and normalizes — all off the event loop. ctx
// is the stream's cancelable context; cancelling it (closeStream) makes Recv return.
func (view *LogView) recvCmd(stream LogStream, ctx context.Context, buffer string, generation int) tea.Cmd {
	return func() tea.Msg {
		chunk, err := stream.Recv(ctx)
		if err != nil {
			return logViewStreamChunkMsg{err: err, generation: generation}
		}
		buffer = capLogBuffer(buffer + chunk)
		return logViewStreamChunkMsg{raw: buffer, content: normalizeTerminalOutput(buffer), generation: generation}
	}
}

// capLogBuffer bounds the streamed scrollback to logStreamBufferCap, trimming whole
// leading lines so the kept text starts on a clean line boundary.
func capLogBuffer(buffer string) string {
	if len(buffer) <= logStreamBufferCap {
		return buffer
	}
	trimmed := buffer[len(buffer)-logStreamBufferCap:]
	if index := strings.IndexByte(trimmed, '\n'); index >= 0 && index+1 <= len(trimmed) {
		trimmed = trimmed[index+1:]
	}
	return trimmed
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
			// Content is already normalized in the poll goroutine (loadCmd). setContent
			// applies the debug filter + scroll freeze; no heavy work on the event loop.
			view.setContent(message.content)
		}
		return nil
	case logViewStreamOpenedMsg:
		if message.generation != view.generation {
			// Stale (the view re-initialised or switched subject): close the orphan so
			// no Recv lingers and no relay/log connection leaks.
			if message.cancel != nil {
				message.cancel()
			}
			if message.stream != nil {
				_ = message.stream.Close()
			}
			return nil
		}
		view.loaded = true
		if message.err != nil {
			view.err = message.err
			return nil
		}
		view.err = nil
		view.streamHandle = message.stream
		view.streamCtx = message.ctx
		view.streamCancel = message.cancel
		// First Recv from an empty buffer; history streams in entry-by-entry, then live.
		return view.recvCmd(message.stream, message.ctx, "", view.generation)
	case logViewStreamChunkMsg:
		if message.generation != view.generation {
			return nil // a chunk from a previous activation/subject
		}
		view.loaded = true
		if message.err != nil {
			// EOF / context-cancel are normal endings (subject stopped, or we closed the
			// stream on switch); only a real error is surfaced. The buffer stays so the
			// last output remains visible and scrollable.
			if !errors.Is(message.err, io.EOF) && !errors.Is(message.err, context.Canceled) {
				view.err = message.err
			}
			return nil
		}
		view.streamBuf = message.raw
		view.setContent(message.content)
		return view.recvCmd(view.streamHandle, view.streamCtx, view.streamBuf, view.generation)
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
		case "d":
			// Toggle debug lines (e.g. the relay connect/disconnect churn) on/off.
			if view.debugFilter != nil {
				view.showDebug = !view.showDebug
				view.renderContent()
			}
			return nil
		case "r":
			if view.streaming() {
				// Reconnect: drop the current stream and re-open from history.
				if view.subject() == "" {
					return nil
				}
				view.closeStream()
				view.generation++
				view.streamBuf = ""
				view.loaded = false
				view.notRunning = false
				view.viewport.GotoBottom()
				if view.running != nil && !view.running() {
					view.loaded = true
					view.notRunning = true
					return nil
				}
				return view.openStreamCmd(view.generation)
			}
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
