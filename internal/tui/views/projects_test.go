package views

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/project"
)

func TestProjectsLoadsAndSelectEmitsMsg(test *testing.T) {
	view := NewProjects(func() ([]project.Entry, error) {
		return []project.Entry{
			{Name: "app", Path: "/p/app", OS: "debian-trixie", Status: "started", Agents: []string{"opencode", "omp"}},
		}, nil
	})
	_ = view.Update(view.Init()()) // load

	if got := len(view.table.Rows()); got != 1 {
		test.Fatalf("rows = %d, want 1", got)
	}
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		test.Fatal("enter on a project must emit a selection command")
	}
	selected, ok := cmd().(ProjectSelectedMsg)
	if !ok || selected.Name != "app" || selected.Path != "/p/app" {
		test.Fatalf("want ProjectSelectedMsg{app,/p/app}, got %#v", cmd())
	}
}

func TestProjectsNewKeyRequestsCreate(test *testing.T) {
	view := NewProjects(func() ([]project.Entry, error) {
		return []project.Entry{{Name: "app", Path: "/p/app"}}, nil
	})
	_ = view.Update(view.Init()()) // load

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if cmd == nil {
		test.Fatal("pressing n must emit a new-project request command")
	}
	if _, ok := cmd().(NewProjectRequestedMsg); !ok {
		test.Fatalf("want NewProjectRequestedMsg, got %#v", cmd())
	}
}

func TestProjectsDescribeTogglesPane(test *testing.T) {
	view := NewProjects(func() ([]project.Entry, error) {
		return []project.Entry{{Name: "app", Path: "/p/app", OS: "ubuntu", Status: "started"}}, nil
	})
	_ = view.Update(view.Init()())
	view.SetSize(80, 20)

	_ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if !view.describe.active() {
		test.Fatal("d must open the describe pane")
	}
	if !strings.Contains(view.View(), "/p/app") {
		test.Errorf("describe pane should show the project path, got:\n%s", view.View())
	}
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if view.describe.active() {
		test.Fatal("esc must close the describe pane")
	}
}

func TestProjectsEmptyShowsHint(test *testing.T) {
	view := NewProjects(func() ([]project.Entry, error) { return nil, nil })
	_ = view.Update(view.Init()())
	if got := view.View(); !strings.Contains(got, "no workspaces yet") {
		test.Errorf("empty switcher should hint at creating a workspace, got: %q", got)
	}
}
