package views

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// Shell is the per-workspace "Shell" sub-tab: a launcher into the workspace's
// interactive shell. Pressing enter emits ExecRequestedMsg, which the app opens in
// the live terminal overlay — the SAME persistent, reattachable tmux shell that
// `ai shell` opens. It is a launcher rather than an embedded terminal because the
// live PTY pane is an app-level overlay (it owns the whole body and forwards every
// keystroke, including the ones the sub-tab bar would otherwise consume).
type Shell struct {
	project func() string
	width   int
	height  int
}

// NewShell builds the Shell launcher over the current-project resolver.
func NewShell(project func() string) *Shell { return &Shell{project: project} }

func (view *Shell) Title() string { return "Shell" }

func (view *Shell) Hints() string { return "enter open shell" }

func (view *Shell) SetSize(width, height int) { view.width, view.height = width, height }

func (view *Shell) Init() tea.Cmd { return nil }

// Update opens the shell on enter (or "a", matching the Sessions attach key).
func (view *Shell) Update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "enter", "a":
			project := view.project()
			if project == "" {
				return nil
			}
			return func() tea.Msg { return ExecRequestedMsg{Project: project} }
		}
	}
	return nil
}

func (view *Shell) View() string {
	if view.project() == "" {
		return ui.Muted.Render("no workspace selected — open one from the Workspaces view")
	}
	// Each line is rendered on its OWN row (styled segments joined horizontally, no
	// newline inside any single Render call) — a \n inside one lipgloss block pads the
	// line to the block width and shifts the following segment, so the text must be
	// assembled line-by-line and joined here.
	lines := []string{
		ui.Muted.Render("Press ") + ui.Primary.Render("enter") +
			ui.Muted.Render(" to open an interactive shell in this workspace."),
		ui.Muted.Render("It is a persistent, reattachable tmux session (the same as ") +
			ui.Primary.Render("ai shell") + ui.Muted.Render(")."),
		ui.Muted.Render("Detach with ") + ui.Primary.Render("ctrl+q") +
			ui.Muted.Render(" or ") + ui.Primary.Render("esc") + ui.Muted.Render("."),
	}
	return strings.Join(lines, "\n")
}
