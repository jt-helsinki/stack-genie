package views

import (
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

// AttachRequestedMsg is emitted when the user asks to attach to a workspace
// session (the `a`/`enter` keys) or to start a default agent session (`n`). The
// parent app suspends the TUI and runs `ai workspace attach <session> <project>`
// via tea.ExecProcess, restoring the TUI on exit.
type AttachRequestedMsg struct {
	Project string
	Session string
}

// defaultAgentSession is the session `n` (new agent) attaches/creates for v1 —
// the default agent CLI's session. Attaching to it creates it if absent.
const defaultAgentSession = "opencode"

type sessionsRefreshedMsg struct {
	sessions []workspace.Session
	err      error
}
type sessionKilledMsg struct{ err error }

// Sessions is the view of the current project's persistent workspace sessions
// (the tmux sessions backing `ai shell`/`ai agent`/`ai attach`). It attaches to,
// starts, and kills sessions on the selected row.
type Sessions struct {
	list     SessionLister
	kill     SessionKiller
	project  func() string
	table    table.Model
	sessions []workspace.Session
	flash    string
	err      error
	loaded   bool
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

// Title is the view's name (used by the menu/header).
func (view *Sessions) Title() string { return "Sessions" }

// Hints are the context-sensitive key bindings shown in the footer.
func (view *Sessions) Hints() string {
	return "a/enter attach · n new agent · k kill · r refresh"
}

// SetSize fits the table to the content area.
func (view *Sessions) SetSize(width, height int) {
	view.table.SetStyles(ui.TableStyles()) // pick up a live theme change
	view.table.SetWidth(width)
	if height > 0 {
		view.table.SetHeight(height)
	}
}

// Init kicks off the first session listing.
func (view *Sessions) Init() tea.Cmd { return view.fetchCmd() }

func (view *Sessions) fetchCmd() tea.Cmd {
	list := view.list
	return func() tea.Msg {
		sessions, err := list()
		return sessionsRefreshedMsg{sessions: sessions, err: err}
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
		return nil, key.String() == "a" || key.String() == "n" || key.String() == "k" || key.String() == "r"
	}
	switch key.String() {
	case "a", "enter":
		session := view.selectedSession()
		if session == "" {
			return nil, true
		}
		return func() tea.Msg { return AttachRequestedMsg{Project: project, Session: session} }, true
	case "n":
		// Start (attach-or-create) the default agent session. v1 keeps it minimal:
		// it attaches the default agent CLI's session, creating it if absent.
		return func() tea.Msg { return AttachRequestedMsg{Project: project, Session: defaultAgentSession} }, true
	case "k":
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
		return ui.Muted.Render("no project selected — open one from the Projects view")
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading sessions…")
	}
	body := view.table.View()
	if len(view.sessions) == 0 {
		body = ui.Muted.Render("no sessions yet — press n to start an agent, or a to open a shell") + "\n" + body
	}
	if view.flash != "" {
		return view.flash + "\n" + body
	}
	return body
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
