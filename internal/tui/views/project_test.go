package views

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/project"
)

// TestProjectDetailLifecycleKeyEmitsActionRequest: a lifecycle key emits a
// WorkspaceActionRequestedMsg so the parent runs it as a suspended subprocess
// (streaming msb output to the terminal), rather than calling msb on the alt-screen.
func TestProjectDetailLifecycleKeyEmitsActionRequest(test *testing.T) {
	view := NewProject(
		func(name string) (project.Entry, bool, error) {
			return project.Entry{Name: name, OS: "ubuntu", Status: "stopped"}, true, nil
		},
	)
	view.SetProject("app")
	_ = view.Update(view.Init()()) // refresh the summary

	for key, wantAction := range map[string]string{"s": "start", "x": "stop", "r": "restart", "d": "destroy"} {
		cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if cmd == nil {
			test.Fatalf("pressing %q must emit a workspace action request", key)
		}
		requested, ok := cmd().(WorkspaceActionRequestedMsg)
		if !ok || requested.Action != wantAction || requested.Project != "app" {
			test.Fatalf("key %q: want WorkspaceActionRequestedMsg{%s, app}, got %#v", key, wantAction, cmd())
		}
	}
}

func TestProjectDetailExecKeyRequestsShell(test *testing.T) {
	view := NewProject(
		func(name string) (project.Entry, bool, error) {
			return project.Entry{Name: name, OS: "ubuntu", Status: "started"}, true, nil
		},
	)
	view.SetProject("app")
	_ = view.Update(view.Init()()) // refresh the summary

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if cmd == nil {
		test.Fatal("pressing e with a project selected must emit an exec request command")
	}
	requested, ok := cmd().(ExecRequestedMsg)
	if !ok || requested.Project != "app" {
		test.Fatalf("want ExecRequestedMsg{app}, got %#v", cmd())
	}
}

func TestProjectDetailNoSelection(test *testing.T) {
	view := NewProject(nil)
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
