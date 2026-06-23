package views

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/project"
)

func TestProjectsLoadsAndSelectEmitsMsg(test *testing.T) {
	view := NewProjects(func() ([]project.Entry, error) {
		return []project.Entry{
			{Name: "app", Path: "/p/app", OS: "debian-trixie", Status: "started", Agents: []string{"opencode", "pi"}},
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

func TestProjectsEmptyShowsHint(test *testing.T) {
	view := NewProjects(func() ([]project.Entry, error) { return nil, nil })
	_ = view.Update(view.Init()())
	if got := view.View(); !strings.Contains(got, "no projects yet") {
		test.Errorf("empty switcher should hint at creating a project, got: %q", got)
	}
}
