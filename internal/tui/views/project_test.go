package views

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/project"
)

func TestProjectDetailStartActionInvokesController(test *testing.T) {
	var calls []string
	view := NewProject(
		func(name string) (project.Entry, bool, error) {
			return project.Entry{Name: name, OS: "ubuntu", Status: "stopped"}, true, nil
		},
		func(action, projectName string) error { calls = append(calls, action+":"+projectName); return nil },
	)
	view.SetProject("app")
	_ = view.Update(view.Init()()) // refresh the summary

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd == nil {
		test.Fatal("pressing s must return a workspace control command")
	}
	done := cmd()
	if len(calls) != 1 || calls[0] != "start:app" {
		test.Fatalf("controller calls = %v, want [start:app]", calls)
	}
	_ = view.Update(done)
	if !strings.Contains(view.View(), "started app") {
		test.Errorf("expected a success flash, got view:\n%s", view.View())
	}
}

func TestProjectDetailNoSelection(test *testing.T) {
	view := NewProject(nil, nil)
	if cmd := view.Init(); cmd != nil {
		test.Error("Init with no project selected must be a no-op")
	}
	if !strings.Contains(view.View(), "no project selected") {
		test.Error("with no project, the view should prompt to open one")
	}
	// Action keys are inert until a project is selected.
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")}); cmd != nil {
		test.Error("action keys must be inert with no project selected")
	}
}
