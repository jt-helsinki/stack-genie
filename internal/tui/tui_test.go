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
