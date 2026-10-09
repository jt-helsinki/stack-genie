package views

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
)

// SessionLister returns the current project's live nono sessions. Injected so the
// view is testable; the parent wires workspace.RealManager(...).ListSessions over
// application.currentProject.
type SessionLister func() ([]workspace.Session, error)

// SessionKiller stops a session (by id) in the current project's workspace.
// Injected; the parent wires workspace.RealManager(...).Stop.
type SessionKiller func(sessionID string) error

// AttachRequestedMsg is emitted when the user asks to attach to the selected
// session (`a`/`enter`). The parent app SUSPENDS the TUI and runs `ai attach
// <session> <name>` via tea.ExecProcess so the interactive shell runs in the
// user's real terminal (full keys, native selection, in-place output),
// restoring the TUI on exit.
type AttachRequestedMsg struct {
	Project string
	Session string
}

type sessionsRefreshedMsg struct {
	sessions []workspace.Session
	err      error
}
type sessionKilledMsg struct{ err error }

// sessionsRefreshMsg re-arms the steady auto-refresh that keeps the Shell tab LIVE
// while it is visible: it reflects sessions appearing/being stopped without a
// manual `r`. Generation-guarded so a stale chain (from a previous activation) dies.
type sessionsRefreshMsg struct{ generation int }

// sessionsRefreshInterval is how often the Shell tab re-lists while it is the active
// tab — so its state stays current. It only ticks while the tab is active (the hub
// pauses it on leave), so a background tab makes no calls.
const sessionsRefreshInterval = 3 * time.Second

// Sessions is the "Shell" tab: the per-workspace session manager over the live nono
// sessions backing `ai shell`/`ai agent`/`ai attach`. It lists sessions and, on the
// selected row, attaches (in the real terminal), starts a new shell, or stops one.
// The actual interactive shell runs via the parent's tea.ExecProcess, not an
// embedded emulator, so it behaves like a normal terminal.
type Sessions struct {
	list     SessionLister
	kill     SessionKiller
	project  func() string
	table    listTable
	sessions []workspace.Session
	flash    string
	err      error
	loaded   bool

	// active is set while this is the visible sub-tab; generation guards the
	// auto-retry chain so it stops when the tab is left (and on re-entry a fresh one
	// starts). Together they keep the recovery polling on the active tab only.
	active     bool
	generation int
}

// NewSessions builds the sessions view over the injected lister, killer, and
// current-project accessor.
func NewSessions(list SessionLister, kill SessionKiller, project func() string) *Sessions {
	columns := []listColumn{
		{title: "ID", width: 10},
		{title: "NAME", width: 16},
		{title: "STATUS", width: 10},
		{title: "ATTACHED", width: 10},
		{title: "STARTED", width: 22},
	}
	return &Sessions{list: list, kill: kill, project: project, table: newListTable(columns)}
}

// Title is the view's name (used by the header).
func (view *Sessions) Title() string { return "Shell" }

// Hints are the context-sensitive key bindings shown in the footer.
func (view *Sessions) Hints() string {
	return "enter/a attach · n new shell · d stop · r refresh"
}

// SetSize fits the table to the content area. One row is reserved for the flash slot
// (always rendered, blank when empty) so the table fills a FIXED height and its
// bottom never moves whether or not a flash/empty-hint shows.
func (view *Sessions) SetSize(width, height int) {
	if tableHeight := height - 1; tableHeight > 0 {
		view.table.SetSize(width, tableHeight)
	}
}

// Init kicks off a fresh session listing (bumping the generation so any pending
// auto-retry from a previous activation is discarded).
func (view *Sessions) Init() tea.Cmd {
	view.generation++
	return view.fetchCmd()
}

// SetActive marks whether the Shell tab is the visible one. The hub calls it on every
// sub-tab switch; leaving the tab stops the auto-retry chain (bump generation) so a
// background tab makes no calls.
func (view *Sessions) SetActive(active bool) {
	view.active = active
	if !active {
		view.generation++
	}
}

// viewFetchTimeout is the TUI's BACKSTOP for a session listing so a tab never hangs
// on "loading…".
const viewFetchTimeout = 12 * time.Second

func (view *Sessions) refreshTickCmd(generation int) tea.Cmd {
	return tea.Tick(sessionsRefreshInterval, func(time.Time) tea.Msg {
		return sessionsRefreshMsg{generation: generation}
	})
}

func (view *Sessions) fetchCmd() tea.Cmd {
	list := view.list
	return func() tea.Msg {
		done := make(chan sessionsRefreshedMsg, 1)
		go func() {
			sessions, err := list()
			done <- sessionsRefreshedMsg{sessions: sessions, err: err}
		}()
		select {
		case got := <-done:
			return got
		case <-time.After(viewFetchTimeout):
			return sessionsRefreshedMsg{err: errBackstopTimedOut("listing sessions")}
		}
	}
}

// Update advances the view: refresh results repopulate the table; a/enter attach,
// n starts a new shell session, d stops the selected session, r refreshes; other
// keys drive table navigation.
func (view *Sessions) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case sessionsRefreshedMsg:
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			view.sessions = message.sessions
			cursor := view.table.Cursor()
			view.table.SetRows(sessionRows(message.sessions))
			view.table.SetCursor(cursor) // keep the selection across the periodic refresh
		}
		// Keep the list LIVE while this tab is active: re-poll on a timer so it reflects
		// sessions created/stopped. Generation- and active-guarded (paused when the tab
		// is left), so only the visible tab makes calls.
		if view.active {
			return view.refreshTickCmd(view.generation)
		}
		return nil
	case sessionsRefreshMsg:
		if message.generation != view.generation || !view.active {
			return nil // stale chain or the tab was left
		}
		return view.fetchCmd()
	case sessionKilledMsg:
		if message.err != nil {
			view.flash = ui.Failure.Render(ui.IconFail + " stop: " + message.err.Error())
		} else {
			view.flash = ui.Success.Render(ui.IconOK + " session stopped")
		}
		return view.fetchCmd() // reflect the stop immediately
	case tea.KeyMsg:
		if cmd, handled := view.handleAction(message); handled {
			return cmd
		}
	}
	return view.table.Update(msg)
}

// handleAction maps the action keys; the bool reports whether the key was an
// action (so it is not also passed to the table for navigation).
func (view *Sessions) handleAction(key tea.KeyMsg) (tea.Cmd, bool) {
	project := view.project()
	if project == "" {
		// With no project selected there is nothing to act on.
		switch key.String() {
		case "a", "enter", "n", "d", "r":
			return nil, true
		}
		return nil, false
	}
	switch key.String() {
	case "a", "enter":
		session := view.selectedSessionID()
		if session == "" {
			return nil, true
		}
		return func() tea.Msg { return AttachRequestedMsg{Project: project, Session: session} }, true
	case "n":
		// nono shell sessions are anonymous (not named) — this always creates a
		// fresh one rather than prompting for a name. Reuses ExecRequestedMsg
		// (`ai shell <name>`), the same suspend-into-real-terminal flow the
		// Workspace tab's "e" key uses.
		return func() tea.Msg { return ExecRequestedMsg{Project: project} }, true
	case "d":
		session := view.selectedSessionID()
		if session == "" {
			return nil, true
		}
		view.flash = ui.Muted.Render("stopping " + session + "…")
		return view.killCmd(session), true
	case "r":
		return view.fetchCmd(), true
	}
	return nil, false
}

func (view *Sessions) killCmd(sessionID string) tea.Cmd {
	kill := view.kill
	return func() tea.Msg {
		return sessionKilledMsg{err: kill(sessionID)}
	}
}

// selectedSessionID returns the session id in the highlighted row (or "").
func (view *Sessions) selectedSessionID() string {
	row := view.table.SelectedRow()
	if len(row) == 0 {
		return ""
	}
	return row[0]
}

// View renders the sessions table (with any flash), a "no project selected" hint
// when none is current, or a fetch error.
func (view *Sessions) View() string {
	if view.project() == "" {
		return ui.Muted.Render("no workspace selected — open one from the Workspaces view")
	}
	if !view.loaded {
		return ui.Muted.Render("loading sessions…")
	}
	// Empty workspace (no sessions yet, not an error): show a clear, friendly
	// message instead of a blank table.
	if view.err == nil && len(view.sessions) == 0 {
		return ui.Heading.Render("No sessions yet") + "\n\n" +
			ui.Muted.Render("This workspace has no running shell or agent sessions.\n\nPress ") +
			ui.Primary.Render("n") + ui.Muted.Render(" to start a new shell session.")
	}
	// A listing error is shown in the flash slot, NOT in place of the view — so the
	// table + actions stay available and you can still press n to start a session.
	last := flashLine(view.flash)
	if view.err != nil {
		last = flashLine(ui.Failure.Render(ui.IconFail + " " + view.err.Error()))
	}
	return view.table.View() + "\n" + last
}

// errBackstopTimedOut is the shared error a view's fetch returns when a listing
// blocks past the TUI BACKSTOP (viewFetchTimeout).
func errBackstopTimedOut(action string) error {
	return fmt.Errorf("timed out %s — try again; press r to retry", action)
}

func sessionRows(sessions []workspace.Session) [][]string {
	rows := make([][]string, 0, len(sessions))
	for _, session := range sessions {
		name := session.Name
		if name == "" {
			name = "—"
		}
		attached := "no"
		if session.Attachment == "attached" {
			attached = "yes"
		}
		rows = append(rows, []string{shortSessionID(session.SessionID), name, session.Status, attached, session.Started})
	}
	return rows
}

// shortSessionID renders a nono session id's first 8 characters (enough to
// disambiguate in a table).
func shortSessionID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
