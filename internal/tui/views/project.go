package views

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// spinnerFrames are the braille spinner glyphs for the in-flight lifecycle status.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// ProjectInfoFetcher returns the current state of one project (name, OS, agents,
// workspace status). Injected; the parent wires it over project.List.
type ProjectInfoFetcher func(name string) (project.Entry, bool, error)

// ExecRequestedMsg is emitted when the user asks to open an interactive shell in
// the current project's workspace (the "e" key). The parent app suspends the TUI
// and tea.ExecProcess an interactive shell via `ai shell`.
type ExecRequestedMsg struct {
	Project string
}

// WorkspaceActionRequestedMsg is emitted for a workspace lifecycle action
// (start/stop/restart/delete). For start/stop/restart the parent app runs
// `ai <action> <name>` DETACHED (it keeps running even if `ai ui` is closed) and
// shows a spinner + status here while polling — the TUI stays navigable and no log
// is shown. delete runs in the confirm overlay.
type WorkspaceActionRequestedMsg struct {
	Action  string
	Project string
}

type projectRefreshedMsg struct {
	entry project.Entry
	found bool
	err   error
}

// summaryRows is the fixed height of the detail summary block above the embedded
// workspace log: name (1) + OS/agents/workspace/path (4) + flash slot (1) + blank
// (1) + the "Workspace log" heading (1) = 8.
const summaryRows = 8

// Project is the detail view for the current project: its summary + workspace
// lifecycle (start/stop/restart/delete), with the workspace LOG embedded below the
// summary (the log is not a separate tab).
type Project struct {
	info     ProjectInfoFetcher
	log      *WorkspaceLog
	name     string
	entry    project.Entry
	hasEntry bool
	flash    string
	err      error
	width    int
	height   int
	// pending is the in-flight lifecycle action ("start"/"stop"/"restart"), shown as
	// an animated spinner on the workspace status line; empty when idle. The parent
	// sets/clears it (StartPending/ClearPending) and advances the frame (TickSpinner)
	// while it polls the detached action — so the spinner animates regardless of which
	// tab is focused.
	pending      string
	pendingFrame int
}

// NewProject builds the project-detail view over the injected info fetcher and the
// workspace-log component shown beneath the summary.
func NewProject(info ProjectInfoFetcher, log *WorkspaceLog) *Project {
	return &Project{info: info, log: log}
}

// StartPending shows the "<action>ing…" spinner on the workspace status line; the
// parent calls this when it kicks off a detached lifecycle action.
func (view *Project) StartPending(action string) {
	view.pending = action
	view.pendingFrame = 0
	view.flash = ""
}

// TickSpinner advances the pending spinner one frame (driven by the parent's poll).
func (view *Project) TickSpinner() { view.pendingFrame++ }

// ClearPending stops the spinner (the action finished or timed out).
func (view *Project) ClearPending() { view.pending = "" }

// SetFlash shows a one-line message under the summary (e.g. a failure to launch a
// detached lifecycle action).
func (view *Project) SetFlash(message string) { view.flash = message }

func (view *Project) Title() string { return "Workspace" }
func (view *Project) Hints() string {
	if view.name == "" {
		return "open a workspace from the Workspaces view"
	}
	return "s start · x stop · r restart · d delete · e shell · ↑/↓ scroll log · f follow"
}

// SetSize splits the pane: a fixed summary block on top, the embedded workspace log
// fills the rest.
func (view *Project) SetSize(width, height int) {
	view.width, view.height = width, height
	logHeight := height - summaryRows
	if logHeight < 1 {
		logHeight = 1
	}
	view.log.SetSize(width, logHeight)
}

// SetProject points the view at a project (the parent calls this, then Init, when
// the user selects one in the switcher).
func (view *Project) SetProject(name string) { view.name = name }

// Init refreshes the current project AND starts the embedded log polling (no-op
// until a workspace is selected).
func (view *Project) Init() tea.Cmd {
	if view.name == "" {
		return nil
	}
	return tea.Batch(view.refreshCmd(), view.log.Init())
}

func (view *Project) refreshCmd() tea.Cmd {
	info := view.info
	name := view.name
	return func() tea.Msg {
		entry, found, err := info(name)
		return projectRefreshedMsg{entry: entry, found: found, err: err}
	}
}

// Update advances the detail view: a refresh fills the summary; s/x/r/d act on the
// workspace; an action result flashes and re-refreshes.
func (view *Project) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case projectRefreshedMsg:
		view.err = message.err
		view.hasEntry = message.found
		if message.err == nil && message.found {
			view.entry = message.entry
			view.flash = "" // a fresh status supersedes any stale hint
		}
		return nil
	case tea.KeyMsg:
		name := view.name
		// Lifecycle keys act on the workspace (unless one is already in flight or no
		// workspace is selected); every OTHER key drives the embedded log (scroll /
		// follow), so the log is usable without a separate tab.
		if name != "" && view.pending == "" {
			if message.String() == "e" {
				if view.entry.Status != "started" {
					view.flash = ui.Muted.Render("workspace not running — press s to start it first")
					return nil
				}
				return func() tea.Msg { return ExecRequestedMsg{Project: name} }
			}
			if action, ok := map[string]string{"s": "start", "x": "stop", "r": "restart", "d": "delete"}[message.String()]; ok {
				// The app handles this: start/stop/restart run DETACHED with a spinner
				// here (TUI stays navigable); delete confirms in the terminal overlay.
				return func() tea.Msg { return WorkspaceActionRequestedMsg{Action: action, Project: name} }
			}
		}
		return view.log.Update(msg)
	default:
		// Async log messages (poll tick / loaded) drive the embedded log.
		return view.log.Update(msg)
	}
}

func (view *Project) View() string {
	if view.name == "" {
		return ui.Muted.Render("no workspace selected — open one from the Workspaces view (menu: Workspaces)")
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.hasEntry {
		return ui.Muted.Render("loading " + view.name + "…")
	}
	// Fixed summary block (summaryRows lines), then the embedded workspace log fills
	// the rest of the pane. The flash slot is always emitted (blank when empty) so the
	// summary height is constant and the log below never shifts.
	var body strings.Builder
	body.WriteString(ui.Heading.Render(view.entry.Name) + "\n")              // 1
	body.WriteString(field("OS", view.entry.OS))                             // 2
	body.WriteString(field("agents", strings.Join(view.entry.Agents, ", "))) // 3
	if view.pending != "" {
		glyph := ui.Success.Render(spinnerFrames[view.pendingFrame%len(spinnerFrames)])
		body.WriteString(field("workspace", glyph+ui.Muted.Render(" "+view.pending+"ing…"))) // 4
	} else {
		body.WriteString(field("workspace", view.entry.Status)) // 4
	}
	body.WriteString(field("path", view.entry.Path))            // 5
	body.WriteString(view.flash + "\n")                         // 6 (flash slot, blank when empty)
	body.WriteString("\n")                                      // 7
	body.WriteString(ui.Heading.Render("Workspace log") + "\n") // 8
	body.WriteString(view.log.View())
	return body.String()
}

func field(label, value string) string {
	if value == "" {
		value = ui.Muted.Render("—")
	}
	return "  " + ui.Muted.Render(label+":") + " " + value + "\n"
}
