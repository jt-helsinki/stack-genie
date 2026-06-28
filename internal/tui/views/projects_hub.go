// ProjectsHub is the two-level "Projects" tab. It opens on the project SWITCHER
// (the project list); selecting a project drops INTO that project, revealing a
// sub-tab bar — Project · Network · Context · Sessions · Apps — for it. While a
// project is open, Tab/←→ cycle the sub-tabs and the focused sub-view's pane is
// acted on directly; esc backs UP one level (sub-pane → switcher), matching the
// app-wide "esc goes up" convention. The chrome (tui/layout.go) only ever shows
// the three TOP-level tabs (Services · Projects · Models); the per-project sub-tab
// bar is drawn here, inside the hub's body.
package views

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// Screen is the structural twin of tui.View — the hub composes sub-views through
// it without importing the parent package (which would be a cycle). Every view in
// this package satisfies it.
type Screen interface {
	Init() tea.Cmd
	Update(tea.Msg) tea.Cmd
	View() string
	Title() string
	Hints() string
	SetSize(width, height int)
}

// projectAware is implemented by sub-views that must be re-pointed when the open
// project changes. Only the Project detail view needs it; the others resolve the
// live current project through their injected closures.
type projectAware interface{ SetProject(name string) }

// escConsumer is implemented by a sub-view that has an open overlay (e.g. a
// describe pane) which should absorb esc itself — so esc closes that overlay
// before the hub uses esc to back out of the open project (esc goes up one level:
// overlay → sub-tab → switcher).
type escConsumer interface{ WantsEsc() bool }

// subTabRows is the height the per-project sub-tab bar (plus a blank line) reserves
// from the body when a project is open, so the sub-view's pane fits the remainder.
const subTabRows = 2

// ProjectsHub is one top-level View that owns the switcher and the per-project
// sub-views.
type ProjectsHub struct {
	switcher  Screen
	subViews  []Screen
	subTitles []string

	open     bool // false: showing the switcher; true: inside a project
	subIndex int
	project  string

	width, height int
}

// NewProjectsHub wires the switcher (the project list) and the per-project
// sub-views (in display order) into a single hub. subTitles labels the sub-tab bar
// and must be the same length as subViews.
func NewProjectsHub(switcher Screen, subViews []Screen, subTitles []string) *ProjectsHub {
	return &ProjectsHub{switcher: switcher, subViews: subViews, subTitles: subTitles}
}

// Title is the top-level tab label.
func (hub *ProjectsHub) Title() string { return "Workspaces" }

// CapturesNav reports whether the hub wants Tab/←→/esc for itself — true while a
// project is open (so the app cycles sub-tabs instead of top-level tabs).
func (hub *ProjectsHub) CapturesNav() bool { return hub.open }

// inputCapturer is a sub-view with an inline text prompt open (Shell new-session,
// Network allow/publish) that must receive every key while the prompt is up.
type inputCapturer interface{ CapturingInput() bool }

// CapturingInput reports whether the focused sub-view (in an open project) has an
// inline prompt open, so the app routes every key to the hub (and on to that
// sub-view) rather than the global shortcuts or the hub's own nav keys.
func (hub *ProjectsHub) CapturingInput() bool {
	if !hub.open {
		return false
	}
	capturer, ok := hub.active().(inputCapturer)
	return ok && capturer.CapturingInput()
}

// OpenProject drops into a project: it points the project-aware sub-views at it and
// returns the batch of sub-view refreshes. The caller (the app) is responsible for
// having set the live current-project state first, so the closure-driven sub-views
// (Network/Context/Sessions) resolve the right project when they refetch.
func (hub *ProjectsHub) OpenProject(name string) tea.Cmd {
	hub.project = name
	hub.open = true
	hub.subIndex = 0
	commands := make([]tea.Cmd, 0, len(hub.subViews))
	for _, sub := range hub.subViews {
		if aware, ok := sub.(projectAware); ok {
			aware.SetProject(name)
		}
		commands = append(commands, sub.Init())
	}
	return tea.Batch(commands...)
}

// Reset backs out to the switcher and refreshes it (used after the create wizard
// returns, so a newly created project appears).
func (hub *ProjectsHub) Reset() tea.Cmd {
	hub.open = false
	return hub.switcher.Init()
}

// Init initialises the switcher and every sub-view so switching is instant.
func (hub *ProjectsHub) Init() tea.Cmd {
	commands := make([]tea.Cmd, 0, len(hub.subViews)+1)
	commands = append(commands, hub.switcher.Init())
	for _, sub := range hub.subViews {
		commands = append(commands, sub.Init())
	}
	return tea.Batch(commands...)
}

// active is the sub-view under the current sub-tab.
func (hub *ProjectsHub) active() Screen { return hub.subViews[hub.subIndex] }

// Update routes input. While a project is open the hub owns Tab/←→ (sub-tab cycle)
// and esc (back to the switcher); everything else delegates to the focused
// sub-view. In switcher mode all input goes to the switcher (the app handles
// top-level Tab before delegating here).
func (hub *ProjectsHub) Update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		if hub.open {
			// While the focused sub-view has an inline prompt open, it owns EVERY key
			// (so typed characters aren't taken by the hub's tab/esc navigation).
			if hub.CapturingInput() {
				return hub.active().Update(msg)
			}
			switch key.String() {
			case "tab", "right":
				hub.subIndex = (hub.subIndex + 1) % len(hub.subViews)
				return hub.active().Init()
			case "shift+tab", "left":
				count := len(hub.subViews)
				hub.subIndex = ((hub.subIndex-1)%count + count) % count
				return hub.active().Init()
			case "esc":
				// If the active sub-view has an open overlay (e.g. a describe pane),
				// let it consume esc first; only back out to the switcher once the
				// sub-view has nothing left to close.
				if consumer, ok := hub.active().(escConsumer); ok && consumer.WantsEsc() {
					return hub.active().Update(msg)
				}
				hub.open = false
				return hub.switcher.Init()
			}
			return hub.active().Update(msg)
		}
		return hub.switcher.Update(msg)
	}
	// Non-key messages (ticks, async refresh results) are delivered to EVERY sub-view,
	// not just the focused one. OpenProject pre-fetches all sub-views at once, so a
	// result (e.g. the Sessions/Apps listing) routinely arrives while a DIFFERENT
	// sub-tab is focused; routing only to hub.active() dropped it and left that view
	// stuck on "loading…" forever. Each sub-view's Update ignores message types it
	// does not recognise, so broadcasting is safe.
	if hub.open {
		commands := make([]tea.Cmd, 0, len(hub.subViews))
		for _, sub := range hub.subViews {
			commands = append(commands, sub.Update(msg))
		}
		return tea.Batch(commands...)
	}
	return hub.switcher.Update(msg)
}

// View shows the switcher, or — inside a project — the sub-tab bar above the
// focused sub-view's pane.
func (hub *ProjectsHub) View() string {
	if !hub.open {
		return hub.switcher.View()
	}
	return lipgloss.JoinVertical(lipgloss.Left, hub.subTabBar(), "", hub.active().View())
}

// SubTabBar exposes the per-project sub-tab bar so the app can keep it visible above
// the live terminal overlay when that overlay was launched from a sub-tab (so the
// tabs don't disappear while, e.g., a shell is open). Empty when no project is open.
func (hub *ProjectsHub) SubTabBar() string {
	if !hub.open {
		return ""
	}
	return hub.subTabBar()
}

// subTabBar renders "<project> ▸ [Project] Network Context Sessions Apps" with
// the active sub-tab filled in the theme accent (mirroring the chrome's tab bar).
func (hub *ProjectsHub) subTabBar() string {
	activeStyle := lipgloss.NewStyle().Bold(true).
		Foreground(lipgloss.Color("0")).Background(ui.Accent()).Padding(0, 1)
	inactiveStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("252")).Background(lipgloss.Color("238")).Padding(0, 1)

	cells := make([]string, 0, len(hub.subTitles)+1)
	cells = append(cells, ui.Heading.Render(hub.project)+ui.Muted.Render("  ▸ "))
	for index, title := range hub.subTitles {
		if index == hub.subIndex {
			cells = append(cells, activeStyle.Render(title))
			continue
		}
		cells = append(cells, inactiveStyle.Render(title))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cells...)
}

// Hints feeds the chrome's command grid: the switcher's keys when showing the list,
// else the focused sub-view's keys (the chrome adds tab/esc for the sub-tab nav,
// recognising the hub as a nav-capturer).
func (hub *ProjectsHub) Hints() string {
	if !hub.open {
		return hub.switcher.Hints()
	}
	return hub.active().Hints()
}

// SetSize gives the switcher the full body and the sub-views the body minus the
// sub-tab bar, so a pane never overflows the border when a project is open.
func (hub *ProjectsHub) SetSize(width, height int) {
	hub.width, hub.height = width, height
	hub.switcher.SetSize(width, height)
	subHeight := height - subTabRows
	if subHeight < 1 {
		subHeight = 1
	}
	for _, sub := range hub.subViews {
		sub.SetSize(width, subHeight)
	}
}
