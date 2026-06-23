package tui

import (
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

func TestWindowSizeSetsViewportWithoutPanic(test *testing.T) {
	application := newTestApp("Services")
	// A tiny window must clamp the body height to >=1, not go negative.
	application.Update(tea.WindowSizeMsg{Width: 80, Height: 1})
	if application.width != 80 {
		test.Fatalf("width = %d, want 80", application.width)
	}
}
