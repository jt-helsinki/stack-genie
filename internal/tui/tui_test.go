package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/jt-helsinki/ideal-robot/internal/tui/views"
)

// fakeView is a minimal View for exercising the app's routing/menu logic.
type fakeView struct{ title string }

func (view *fakeView) Init() tea.Cmd          { return nil }
func (view *fakeView) Update(tea.Msg) tea.Cmd { return nil }
func (view *fakeView) View() string           { return "fake-" + view.title }
func (view *fakeView) Title() string          { return view.title }
func (view *fakeView) Hints() string          { return "" }
func (view *fakeView) SetSize(int, int)       {}

func newTestApp(titles ...string) *app {
	views := make([]View, 0, len(titles))
	for _, title := range titles {
		views = append(views, &fakeView{title: title})
	}
	application := &app{views: views}
	application.buildPalette()
	return application
}

func colon() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(":")} }
func enter() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEnter} }
func qKey() tea.KeyMsg  { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")} }
func esc() tea.KeyMsg   { return tea.KeyMsg{Type: tea.KeyEsc} }

func TestColonOpensMenuAndEscCloses(test *testing.T) {
	application := newTestApp("Services")
	application.Update(colon())
	if !application.paletteOpen {
		test.Fatal(": must open the menu")
	}
	application.Update(esc())
	if application.paletteOpen {
		test.Fatal("esc must close the menu")
	}
}

func TestMenuExitSetsQuitting(test *testing.T) {
	application := newTestApp("Services")
	application.Update(colon())
	// The palette is [Services, Exit]; select the last entry (Exit).
	application.paletteCursor = len(application.palette) - 1
	application.Update(enter())
	if !application.quitting {
		test.Fatal("choosing the Exit menu item must set quitting")
	}
}

func TestMenuSwitchesView(test *testing.T) {
	application := newTestApp("Services", "Project")
	application.Update(colon())
	application.paletteCursor = 1 // "Project"
	application.Update(enter())
	if application.current != 1 {
		test.Fatalf("current view = %d, want 1", application.current)
	}
	if application.paletteOpen {
		test.Error("menu should close after a selection")
	}
}

func TestPaletteTypeToFilter(test *testing.T) {
	application := newTestApp("Services", "Projects", "Models")
	application.Update(colon())
	// Typing "p" filters to items whose label contains it (only "Projects").
	application.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if filtered := application.filteredPalette(); len(filtered) != 1 || filtered[0].label != "Projects" {
		test.Fatalf("filter 'p' = %v, want [Projects]", filtered)
	}
	application.Update(enter())
	if application.current != 1 {
		test.Fatalf("after filter+enter current = %d, want 1 (Projects)", application.current)
	}
	if application.paletteOpen {
		test.Error("enter should close the palette")
	}
}

func TestQuitKey(test *testing.T) {
	application := newTestApp("Services")
	application.Update(qKey())
	if !application.quitting {
		test.Fatal("q must set quitting")
	}
}

func TestProjectSelectedSwitchesToDetail(test *testing.T) {
	detail := views.NewProject(
		func(string) (project.Entry, bool, error) { return project.Entry{Name: "app"}, true, nil },
		func(string, string) error { return nil },
	)
	application := &app{
		views:              []View{&fakeView{title: "Services"}, &fakeView{title: "Projects"}, detail},
		projectDetail:      detail,
		projectDetailIndex: 2,
	}
	application.buildPalette()

	application.Update(views.ProjectSelectedMsg{Name: "app", Path: "/p/app"})
	if application.currentProject != "app" {
		test.Fatalf("currentProject = %q, want app", application.currentProject)
	}
	if application.current != 2 {
		test.Fatalf("current view = %d, want 2 (project detail)", application.current)
	}
}

func TestNewProjectRequestedOpensCreateOverlay(test *testing.T) {
	application := &app{cwd: test.TempDir(), views: []View{&fakeView{title: "Projects"}}}
	application.buildPalette()

	application.Update(views.NewProjectRequestedMsg{})
	if application.createView == nil {
		test.Fatal("NewProjectRequestedMsg must open the create overlay")
	}
}

func TestCreateCancelledClosesOverlay(test *testing.T) {
	application := &app{cwd: test.TempDir(), views: []View{&fakeView{title: "Projects"}}}
	application.createView = views.NewCreate(application.cwd)

	application.Update(views.CreateCancelledMsg{})
	if application.createView != nil {
		test.Fatal("CreateCancelledMsg must close the overlay")
	}
}

func TestCreateConfirmedClosesOverlayAndRunsWizard(test *testing.T) {
	application := &app{cwd: test.TempDir(), projectsIndex: 0, views: []View{&fakeView{title: "Projects"}}}
	application.createView = views.NewCreate(application.cwd)

	_, cmd := application.Update(views.CreateConfirmedMsg{Dir: test.TempDir()})
	if application.createView != nil {
		test.Fatal("CreateConfirmedMsg must close the overlay")
	}
	if cmd == nil {
		test.Fatal("CreateConfirmedMsg must return a command (the create wizard subprocess)")
	}
}

func TestExecRequestedReturnsCommand(test *testing.T) {
	application := &app{
		views:              []View{&fakeView{title: "Project"}},
		projectDetail:      views.NewProject(func(string) (project.Entry, bool, error) { return project.Entry{}, false, nil }, func(string, string) error { return nil }),
		projectDetailIndex: 0,
	}
	if _, cmd := application.Update(views.ExecRequestedMsg{Project: "app"}); cmd == nil {
		test.Fatal("ExecRequestedMsg must return a command (the in-workspace shell)")
	}
}

func TestHelpToggle(test *testing.T) {
	application := newTestApp("Services")

	application.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	if !application.helpOpen {
		test.Fatal("? must open the help overlay")
	}
	if !strings.Contains(application.helpView(), "Global") {
		test.Error("help should list the global key bindings")
	}
	// Any key dismisses it.
	application.Update(qKey())
	if application.helpOpen {
		test.Fatal("any key must close the help overlay")
	}
}

func TestWindowSizeSetsViewportWithoutPanic(test *testing.T) {
	application := newTestApp("Services")
	// A tiny window must clamp the body height to >=1, not go negative.
	application.Update(tea.WindowSizeMsg{Width: 80, Height: 1})
	if application.width != 80 {
		test.Fatalf("width = %d, want 80", application.width)
	}
}

func tabKey() tea.KeyMsg      { return tea.KeyMsg{Type: tea.KeyTab} }
func shiftTabKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyShiftTab} }
func rightKey() tea.KeyMsg    { return tea.KeyMsg{Type: tea.KeyRight} }
func leftKey() tea.KeyMsg     { return tea.KeyMsg{Type: tea.KeyLeft} }
func digit(value string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)}
}

// TestHeaderShowsLogoAndCommands: the header has the ASCII logo and the active
// context's command grid (incl. the global keys).
func TestHeaderShowsLogoAndCommands(test *testing.T) {
	application := newTestApp("Services")
	application.width = 120
	header := application.header()
	if !strings.Contains(header, "█") {
		test.Error("header should contain the ASCII logo")
	}
	if !strings.Contains(header, "menu") || !strings.Contains(header, "quit") {
		test.Errorf("header command grid missing global keys: %q", header)
	}
}

// TestFooterShowsContext: the status line (moved down from the header) carries
// role / gateway / scope.
func TestFooterShowsContext(test *testing.T) {
	application := &app{views: []View{&fakeView{title: "Services"}}, role: "standalone", gateway: "http://x:18787"}
	footer := application.footer()
	if !strings.Contains(footer, "role:standalone") || !strings.Contains(footer, "scope:server") {
		test.Errorf("footer missing context: %q", footer)
	}
}

// TestTabBarListsAllTitles confirms every view's Title appears in the tab bar.
func TestTabBarListsAllTitles(test *testing.T) {
	titles := []string{"Services", "Projects", "Project", "Network", "Context"}
	application := newTestApp(titles...)
	bar := application.tabBar()
	for _, title := range titles {
		if !strings.Contains(bar, title) {
			test.Errorf("tab bar %q missing title %q", bar, title)
		}
	}
}

// TestTabBarHighlightsActive checks the active tab is bracketed (the accent marker)
// while the others are not.
func TestTabBarHighlightsActive(test *testing.T) {
	application := newTestApp("Services", "Projects", "Models")
	application.current = 1 // Projects
	bar := application.tabBar()
	if !strings.Contains(bar, "[Projects]") {
		test.Errorf("active tab not bracketed in %q", bar)
	}
	if strings.Contains(bar, "[Services]") || strings.Contains(bar, "[Models]") {
		test.Errorf("inactive tabs should not be bracketed: %q", bar)
	}
}

// TestTabKeysCycle checks Tab/Shift+Tab and →/← move application.current, wrapping.
func TestTabKeysCycle(test *testing.T) {
	application := newTestApp("Services", "Projects", "Models")

	application.Update(tabKey())
	if application.current != 1 {
		test.Fatalf("after tab current = %d, want 1", application.current)
	}
	application.Update(rightKey())
	if application.current != 2 {
		test.Fatalf("after right current = %d, want 2", application.current)
	}
	// Tab past the end wraps to the first tab.
	application.Update(tabKey())
	if application.current != 0 {
		test.Fatalf("tab past end should wrap to 0, got %d", application.current)
	}
	// Shift+Tab / ← from the first tab wrap to the last.
	application.Update(shiftTabKey())
	if application.current != 2 {
		test.Fatalf("shift+tab from 0 should wrap to 2, got %d", application.current)
	}
	application.Update(leftKey())
	if application.current != 1 {
		test.Fatalf("after left current = %d, want 1", application.current)
	}
}

// TestNumberKeysJumpToTab checks 1-9 jump to that tab (1-based) and out-of-range
// numbers are ignored.
func TestNumberKeysJumpToTab(test *testing.T) {
	application := newTestApp("Services", "Projects", "Models")

	application.Update(digit("3"))
	if application.current != 2 {
		test.Fatalf("'3' should select index 2, got %d", application.current)
	}
	application.Update(digit("1"))
	if application.current != 0 {
		test.Fatalf("'1' should select index 0, got %d", application.current)
	}
	// '9' is out of range (only 3 views) — must be a no-op.
	application.Update(digit("9"))
	if application.current != 0 {
		test.Fatalf("out-of-range '9' should not move; got %d", application.current)
	}
}

// TestTinyWindowClampsBodySize checks a tiny window yields a body size of >=1 on both
// axes without panicking, and that the chrome still renders.
func TestTinyWindowClampsBodySize(test *testing.T) {
	application := newTestApp("Services", "Projects")
	application.Update(tea.WindowSizeMsg{Width: 2, Height: 2})
	width, height := application.bodyContentSize()
	if width < 1 || height < 1 {
		test.Fatalf("body size = %dx%d, both must be >=1", width, height)
	}
	// View() must not panic on a degenerate window.
	if application.View() == "" {
		test.Error("View() should render chrome even on a tiny window")
	}
}
