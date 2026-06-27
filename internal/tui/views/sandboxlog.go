package views

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// SandboxLogTailer returns the recent captured output of the CURRENT workspace
// microVM (msb logs --tail). Injected; the parent wires Manager.SandboxLogTail for
// the live current project. With no current project it returns "", nil; when the
// workspace is not running it returns an error (surfaced in the pane).
type SandboxLogTailer func() (string, error)

// sandboxLogRefreshInterval is how often the tab re-polls the sandbox log so new
// output "streams in" while the tab is open.
const sandboxLogRefreshInterval = 2 * time.Second

// sandboxLogLoadedMsg carries one poll result, tagged with the generation it was
// issued under so a result from a previous activation is discarded.
type sandboxLogLoadedMsg struct {
	content    string
	err        error
	generation int
}

// sandboxLogTickMsg re-arms the auto-refresh. Its generation lets a stale tick chain
// (from a previous Init) die so only the latest activation keeps polling.
type sandboxLogTickMsg struct{ generation int }

// SandboxLog is the per-workspace "Sandbox log" sub-tab: a scrollable, auto-
// refreshing view of the microVM's captured output (msb logs). It re-polls on a
// timer so new lines stream in, follows the tail unless the user has scrolled up,
// and is fully scrollable (mouse wheel / PgUp/PgDn / arrows). Polling is generation-
// guarded so re-entering the tab (or switching workspace) starts a single fresh
// cycle rather than stacking timers.
type SandboxLog struct {
	tail    SandboxLogTailer
	project func() string

	viewport   viewport.Model
	loaded     bool
	empty      bool
	err        error
	generation int
	width      int
	height     int
}

// NewSandboxLog builds the Sandbox log view over the injected tailer + current-
// project resolver.
func NewSandboxLog(tail SandboxLogTailer, project func() string) *SandboxLog {
	return &SandboxLog{tail: tail, project: project, viewport: viewport.New(0, 0)}
}

func (view *SandboxLog) Title() string { return "Sandbox log" }

func (view *SandboxLog) Hints() string {
	return "↑/↓ scroll · PgUp/PgDn page · r refresh"
}

func (view *SandboxLog) SetSize(width, height int) {
	view.width = width
	view.height = height
	view.viewport.Width = width
	if height > 0 {
		view.viewport.Height = height
	}
}

// Init starts a fresh poll cycle for the current project. Re-init (returning to the
// tab, or a workspace change) bumps the generation so the previous tick chain stops.
func (view *SandboxLog) Init() tea.Cmd {
	if view.project() == "" {
		return nil
	}
	view.generation++
	view.loaded = false
	return tea.Batch(view.loadCmd(view.generation), view.tickCmd(view.generation))
}

func (view *SandboxLog) loadCmd(generation int) tea.Cmd {
	tail := view.tail
	return func() tea.Msg {
		content, err := tail()
		return sandboxLogLoadedMsg{content: content, err: err, generation: generation}
	}
}

func (view *SandboxLog) tickCmd(generation int) tea.Cmd {
	return tea.Tick(sandboxLogRefreshInterval, func(time.Time) tea.Msg {
		return sandboxLogTickMsg{generation: generation}
	})
}

func (view *SandboxLog) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case sandboxLogLoadedMsg:
		if message.generation != view.generation {
			return nil // a stale poll from a previous activation
		}
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			view.empty = strings.TrimSpace(message.content) == ""
			atBottom := view.viewport.AtBottom()
			view.viewport.SetContent(message.content)
			// Follow the tail unless the user has scrolled up to read history.
			if atBottom {
				view.viewport.GotoBottom()
			}
		}
		return nil
	case sandboxLogTickMsg:
		if message.generation != view.generation {
			return nil // a stale tick chain
		}
		return tea.Batch(view.loadCmd(view.generation), view.tickCmd(view.generation))
	case tea.KeyMsg:
		if message.String() == "r" {
			return view.loadCmd(view.generation)
		}
	}
	var cmd tea.Cmd
	view.viewport, cmd = view.viewport.Update(msg)
	return cmd
}

func (view *SandboxLog) View() string {
	if view.project() == "" {
		return ui.Muted.Render("no workspace selected — open one from the Workspaces view")
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading sandbox log…")
	}
	if view.empty {
		return ui.Muted.Render("no sandbox output captured yet — it streams in as the workspace runs")
	}
	return view.viewport.View()
}
