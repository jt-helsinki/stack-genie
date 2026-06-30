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
		nil,
	)
	view.SetProject("app")
	_ = view.Update(view.Init()()) // refresh the summary

	for key, wantAction := range map[string]string{"s": "start", "x": "stop", "r": "restart", "d": "delete"} {
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
		nil,
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

// TestProjectDetailShellGuardedWhenNotRunning: pressing e on a not-running
// workspace shows an inline hint and does NOT emit an exec request (which would
// suspend the TUI into a doomed subprocess).
func TestProjectDetailShellGuardedWhenNotRunning(test *testing.T) {
	view := NewProject(
		func(name string) (project.Entry, bool, error) {
			return project.Entry{Name: name, OS: "ubuntu", Status: "none"}, true, nil
		},
		nil,
	)
	view.SetProject("app")
	_ = view.Update(view.Init()())

	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")}); cmd != nil {
		test.Fatal("e on a not-running workspace must not emit an exec request")
	}
	if !strings.Contains(view.View(), "not running") {
		test.Errorf("expected a 'not running' hint, got:\n%s", view.View())
	}
}

func TestProjectDetailNoSelection(test *testing.T) {
	view := NewProject(nil, nil)
	if cmd := view.Init(); cmd != nil {
		test.Error("Init with no project selected must be a no-op")
	}
	if !strings.Contains(view.View(), "no workspace selected") {
		test.Error("with no workspace, the view should prompt to open one")
	}
	// Action keys are inert until a project is selected.
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")}); cmd != nil {
		test.Error("action keys must be inert with no project selected")
	}
}

// While a lifecycle action is pending, the workspace status line shows an animated
// spinner + "<action>ing…", and clearing it restores the real status.
func TestProjectPendingShowsSpinner(test *testing.T) {
	view := NewProject(func(name string) (project.Entry, bool, error) {
		return project.Entry{Name: name, OS: "ubuntu", Status: "none"}, true, nil
	}, nil)
	view.SetProject("app")
	_ = view.Update(view.Init()())

	view.StartPending("start")
	view.TickSpinner()
	out := view.View()
	if !strings.Contains(out, "starting…") {
		test.Errorf("pending view should show 'starting…':\n%s", out)
	}
	// While pending, lifecycle keys are ignored (no new action emitted).
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")}); cmd != nil {
		test.Error("lifecycle keys must be ignored while an action is pending")
	}
	view.ClearPending()
	if strings.Contains(view.View(), "starting…") {
		test.Error("clearing pending should remove the spinner")
	}
}

// TestProjectShowsSandboxConfiguration: the live sandbox config fetched alongside the
// summary is rendered under the "Sandbox Configuration" heading.
func TestProjectShowsSandboxConfiguration(test *testing.T) {
	view := NewProject(
		func(name string) (project.Entry, bool, error) {
			return project.Entry{Name: name, OS: "ubuntu", Status: "started"}, true, nil
		},
		func(name string) ([]ConfigField, error) {
			return []ConfigField{{Label: "image", Value: "aip-app:local"}, {Label: "memory", Value: "4096 MiB"}}, nil
		},
	)
	view.SetProject("app")
	_ = view.Update(view.Init()())
	out := view.View()
	if !strings.Contains(out, "Sandbox Configuration") {
		test.Errorf("expected the Sandbox Configuration heading:\n%s", out)
	}
	if !strings.Contains(out, "aip-app:local") || !strings.Contains(out, "4096 MiB") {
		test.Errorf("expected the live config fields rendered:\n%s", out)
	}
}
