package views

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
)

// SessionLister returns the current project's workspace tmux sessions. Injected
// so the view is testable; the parent wires workspace.RealManager(...).ListSessions
// over application.currentProject.
type SessionLister func() ([]workspace.Session, error)

// SessionKiller kills a named session in the current project's workspace.
// Injected; the parent wires workspace.RealManager(...).KillSession.
type SessionKiller func(session string) error

// AttachRequestedMsg is emitted when the user asks to attach to (or create) a
// workspace session — `a`/`enter` attach the selected one, `n` creates a new named
// one (`tmux new-session -A` creates it on attach). The parent app SUSPENDS the TUI
// and runs `ai attach <session> <name>` via tea.ExecProcess so the interactive shell
// runs in the user's real terminal (full keys, native selection, in-place output),
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

// Sessions is the "Shell" tab: the per-workspace session manager over the tmux
// sessions backing `ai shell`/`ai agent`/`ai attach`. It lists sessions and, on the
// selected row, attaches (in the real terminal), creates a new named session, and
// kills. The actual interactive shell runs via the parent's tea.ExecProcess, not an
// embedded emulator, so it behaves like a normal terminal.
type Sessions struct {
	list     SessionLister
	kill     SessionKiller
	project  func() string
	table    table.Model
	sessions []workspace.Session
	flash    string
	err      error
	loaded   bool

	// creating + nameInput drive the inline "new session" name prompt (n): while
	// creating, keystrokes edit the name and enter attaches/creates it.
	creating  bool
	nameInput string
}

// NewSessions builds the sessions view over the injected lister, killer, and
// current-project accessor.
func NewSessions(list SessionLister, kill SessionKiller, project func() string) *Sessions {
	columns := []table.Column{
		{Title: "NAME", Width: 24},
		{Title: "ATTACHED", Width: 10},
		{Title: "IDLE", Width: 10},
	}
	built := table.New(table.WithColumns(columns), table.WithFocused(true))
	built.SetStyles(ui.TableStyles())
	return &Sessions{list: list, kill: kill, project: project, table: built}
}

// Title is the view's name (used by the header).
func (view *Sessions) Title() string { return "Shell" }

// CapturingInput reports whether the inline new-session name prompt is open, so the
// app routes all keys here (not the global shortcuts) while the user types a name.
func (view *Sessions) CapturingInput() bool { return view.creating }

// Hints are the context-sensitive key bindings shown in the footer.
func (view *Sessions) Hints() string {
	return "enter/a attach · n new · d kill · r refresh"
}

// SetSize fits the table to the content area. One row is reserved for the flash slot
// (always rendered, blank when empty) so the table fills a FIXED height and its
// bottom never moves whether or not a flash/empty-hint shows.
func (view *Sessions) SetSize(width, height int) {
	view.table.SetStyles(ui.TableStyles()) // pick up a live theme change
	view.table.SetWidth(width)
	if tableHeight := height - 1; tableHeight > 0 {
		view.table.SetHeight(tableHeight)
	}
}

// Init kicks off the first session listing.
func (view *Sessions) Init() tea.Cmd { return view.fetchCmd() }

// sessionFetchTimeout bounds the session listing so the tab never hangs on
// "loading…" when the in-VM `msb exec tmux list-sessions` is slow or blocked (e.g.
// while the workspace is busy pulling an image); it degrades to an error the user can
// retry with `r`.
const sessionFetchTimeout = 8 * time.Second

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
		case <-time.After(sessionFetchTimeout):
			return sessionsRefreshedMsg{err: errTimedOut("listing sessions")}
		}
	}
}

// Update advances the view: refresh results repopulate the table; a/enter attach,
// n starts a default agent session, k kills the selected session, r refreshes;
// other keys drive table navigation.
func (view *Sessions) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case sessionsRefreshedMsg:
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			view.sessions = message.sessions
			view.table.SetRows(sessionRows(message.sessions))
		}
		return nil
	case sessionKilledMsg:
		if message.err != nil {
			view.flash = ui.Failure.Render(ui.IconFail + " kill: " + message.err.Error())
		} else {
			view.flash = ui.Success.Render(ui.IconOK + " session killed")
		}
		return view.fetchCmd() // reflect the kill immediately
	case tea.KeyMsg:
		// The inline "new session" name prompt owns the keyboard while open.
		if view.creating {
			return view.handleCreateKey(message)
		}
		if cmd, handled := view.handleAction(message); handled {
			return cmd
		}
	}
	var cmd tea.Cmd
	view.table, cmd = view.table.Update(msg)
	return cmd
}

// handleAction maps the action keys; the bool reports whether the key was an
// action (so it is not also passed to the table for navigation).
func (view *Sessions) handleAction(key tea.KeyMsg) (tea.Cmd, bool) {
	project := view.project()
	if project == "" {
		// With no project selected there is nothing to act on.
		switch key.String() {
		case "a", "enter", "n", "d", "k", "r":
			return nil, true
		}
		return nil, false
	}
	switch key.String() {
	case "a", "enter":
		session := view.selectedSession()
		if session == "" {
			return nil, true
		}
		return func() tea.Msg { return AttachRequestedMsg{Project: project, Session: session} }, true
	case "n":
		// Begin the inline new-session name prompt; the next keystrokes edit the name.
		view.creating = true
		view.nameInput = ""
		view.flash = ""
		return nil, true
	case "d", "k":
		session := view.selectedSession()
		if session == "" {
			return nil, true
		}
		view.flash = ui.Muted.Render("killing " + session + "…")
		return view.killCmd(session), true
	case "r":
		return view.fetchCmd(), true
	}
	return nil, false
}

// handleCreateKey edits the inline new-session name: runes append, backspace
// deletes, enter attaches/creates the named session (in the real terminal via the
// parent), and esc cancels. Spaces/dots are dropped (not valid tmux session names).
func (view *Sessions) handleCreateKey(key tea.KeyMsg) tea.Cmd {
	switch key.Type {
	case tea.KeyEsc:
		view.creating = false
		view.nameInput = ""
		return nil
	case tea.KeyEnter:
		name := strings.TrimSpace(view.nameInput)
		view.creating = false
		view.nameInput = ""
		project := view.project()
		if name == "" || project == "" {
			return nil
		}
		return func() tea.Msg { return AttachRequestedMsg{Project: project, Session: name} }
	case tea.KeyBackspace:
		runes := []rune(view.nameInput)
		if len(runes) > 0 {
			view.nameInput = string(runes[:len(runes)-1])
		}
		return nil
	case tea.KeyRunes:
		for _, char := range key.Runes {
			if char == ' ' || char == '.' || char == ':' {
				continue
			}
			view.nameInput += string(char)
		}
		return nil
	}
	return nil
}

func (view *Sessions) killCmd(session string) tea.Cmd {
	kill := view.kill
	return func() tea.Msg {
		return sessionKilledMsg{err: kill(session)}
	}
}

// selectedSession returns the session name in the highlighted row (or "").
func (view *Sessions) selectedSession() string {
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
	// Empty workspace (no tmux server yet, not an error): show a clear, friendly
	// message instead of a blank table. The new-session prompt still takes precedence
	// so n works from here.
	if !view.creating && view.err == nil && len(view.sessions) == 0 {
		return ui.Heading.Render("No sessions yet") + "\n\n" +
			ui.Muted.Render("This workspace has no running shell or agent sessions.\n\nPress ") +
			ui.Primary.Render("n") + ui.Muted.Render(" to start a new shell session.")
	}
	// A listing error (e.g. the in-VM read timed out while the workspace was busy) is
	// shown in the flash slot, NOT in place of the view — so the table + actions stay
	// available and you can still press n to create or attach a session.
	last := flashLine(view.flash)
	switch {
	case view.creating:
		last = ui.Heading.Render("new session: ") + view.nameInput + "▏" +
			ui.Muted.Render("  (enter create · esc cancel)")
	case view.err != nil:
		last = flashLine(ui.Failure.Render(ui.IconFail + " " + view.err.Error()))
	}
	return view.table.View() + "\n" + last
}

// errTimedOut is the shared error a view's fetch returns when an in-VM read blocks
// past its timeout, so the pane shows a retryable message instead of hanging.
func errTimedOut(action string) error {
	return fmt.Errorf("timed out %s — the workspace may be busy (e.g. pulling an image); press r to retry", action)
}

func sessionRows(sessions []workspace.Session) []table.Row {
	rows := make([]table.Row, 0, len(sessions))
	for _, session := range sessions {
		attached := "no"
		if session.Attached {
			attached = "yes"
		}
		rows = append(rows, table.Row{session.Name, attached, sessionIdle(session.Activity)})
	}
	return rows
}

// sessionIdle renders how long a session has been idle from tmux's raw
// last-activity epoch string (relative to now), or a dash when unparseable.
func sessionIdle(activity string) string {
	if activity == "" {
		return "-"
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(activity), 10, 64)
	if err != nil {
		return "-"
	}
	idle := time.Since(time.Unix(epoch, 0))
	if idle < 0 {
		idle = 0
	}
	switch {
	case idle < time.Minute:
		return strconv.Itoa(int(idle.Seconds())) + "s"
	case idle < time.Hour:
		return strconv.Itoa(int(idle.Minutes())) + "m"
	default:
		return strconv.Itoa(int(idle.Hours())) + "h"
	}
}
