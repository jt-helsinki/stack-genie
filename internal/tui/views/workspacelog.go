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
// the live current project. With no current project it returns "", nil; when the
// workspace is not running it returns an error (surfaced in the pane).
type WorkspaceLogTailer func() (string, error)

// workspaceLogRefreshInterval is how often the tab re-polls the Workspace Log so new
// output "streams in" while the tab is open.
const workspaceLogRefreshInterval = 2 * time.Second

// workspaceLogLoadedMsg carries one poll result, tagged with the generation it was
// issued under so a result from a previous activation is discarded.
type workspaceLogLoadedMsg struct {
	content    string
	err        error
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
	project func() string

	viewport   viewport.Model
	loaded     bool
	empty      bool
	err        error
	generation int
	width      int
	height     int
	// pending holds the latest normalized content while the user has scrolled UP
	// (not at the bottom): the view is FROZEN so a 2s refresh does not re-render the
	// pane and wipe an in-progress text selection. It is applied when the user scrolls
	// back to the bottom.
	pending string
}

// NewWorkspaceLog builds the Workspace Log view over the injected tailer + current-
// project resolver.
func NewWorkspaceLog(tail WorkspaceLogTailer, project func() string) *WorkspaceLog {
	return &WorkspaceLog{tail: tail, project: project, viewport: viewport.New(0, 0)}
}

func (view *WorkspaceLog) Title() string { return "Workspace Log" }

func (view *WorkspaceLog) Hints() string {
	return "↑/↓ scroll · PgUp/PgDn page · f follow in terminal · r refresh"
}

// WorkspaceLogFollowRequestedMsg asks the parent to open a live `msb logs -f` follow in
// the user's REAL terminal (via tea.ExecProcess) — a non-embedded view that renders
// natively (selectable, and animated in place when the captured stream has the
// control codes), restoring the TUI on exit.
type WorkspaceLogFollowRequestedMsg struct{ Project string }

func (view *WorkspaceLog) SetSize(width, height int) {
	view.width = width
	view.height = height
	view.viewport.Width = width
	if height > 0 {
		view.viewport.Height = height
	}
}

// Init starts a fresh poll cycle for the current project. Re-init (returning to the
// tab, or a workspace change) bumps the generation so the previous tick chain stops.
func (view *WorkspaceLog) Init() tea.Cmd {
	if view.project() == "" {
		return nil
	}
	view.generation++
	view.loaded = false
	return tea.Batch(view.loadCmd(view.generation), view.tickCmd(view.generation))
}

func (view *WorkspaceLog) loadCmd(generation int) tea.Cmd {
	tail := view.tail
	return func() tea.Msg {
		content, err := tail()
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
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			// Apply the terminal control codes (\r / cursor moves / erase-line) so
			// progress redraws collapse IN PLACE.
			content := normalizeTerminalOutput(message.content)
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
		return tea.Batch(view.loadCmd(view.generation), view.tickCmd(view.generation))
	case tea.KeyMsg:
		switch message.String() {
		case "r":
			return view.loadCmd(view.generation)
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
