package views

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/project"
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

// TestProjectStatusLabels: a running workspace shows "running", a lifecycle action in
// flight shows the progress verb ("starting"/"restarting"), and other states map.
func TestProjectStatusLabels(test *testing.T) {
	if got := workspaceStatusLabel("started"); !strings.Contains(got, "running") {
		test.Errorf("started → %q, want 'running'", got)
	}
	if got := workspaceStatusLabel("running"); !strings.Contains(got, "running") {
		test.Errorf("running → %q, want 'running'", got)
	}
	if got := workspaceStatusLabel("stopped"); !strings.Contains(got, "stopped") {
		test.Errorf("stopped → %q, want 'stopped'", got)
	}
	if got := workspaceStatusLabel("none"); !strings.Contains(got, "not created") {
		test.Errorf("none → %q, want 'not created'", got)
	}
	if pendingVerb("start") != "starting" || pendingVerb("restart") != "restarting" || pendingVerb("stop") != "stopping" {
		test.Errorf("pendingVerb: got %q/%q/%q", pendingVerb("start"), pendingVerb("restart"), pendingVerb("stop"))
	}

	// Rendered: a started workspace shows "running"; a pending restart shows "restarting".
	view := NewProject(func(name string) (project.Entry, bool, error) {
		return project.Entry{Name: name, OS: "ubuntu", Status: "started"}, true, nil
	}, nil)
	view.SetProject("app")
	_ = view.Update(view.Init()())
	if !strings.Contains(view.View(), "running") {
		test.Errorf("started workspace view should show 'running':\n%s", view.View())
	}
	view.StartPending("restart")
	if out := view.View(); !strings.Contains(out, "restarting") {
		test.Errorf("pending restart view should show 'restarting':\n%s", out)
	}
}

// TestProjectConfigScrollsWhenSized: a config taller than the pane is scrollable.
func TestProjectConfigScrollsWhenSized(test *testing.T) {
	fields := make([]ConfigField, 0, 30)
	for index := 0; index < 30; index++ {
		fields = append(fields, ConfigField{Label: fmt.Sprintf("k%d", index), Value: "v"})
	}
	view := NewProject(
		func(name string) (project.Entry, bool, error) {
			return project.Entry{Name: name, OS: "ubuntu", Status: "started"}, true, nil
		},
		func(name string) ([]ConfigField, error) { return fields, nil },
	)
	view.SetProject("app")
	view.SetSize(80, 8) // small pane → the 30-field config overflows → must scroll
	_ = view.Update(view.Init()())
	_ = view.View() // render into the viewport
	if view.viewport.TotalLineCount() <= view.viewport.Height {
		test.Fatalf("expected overflowing content (%d lines > height %d)", view.viewport.TotalLineCount(), view.viewport.Height)
	}
	before := view.viewport.YOffset
	view.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if view.viewport.YOffset == before {
		test.Errorf("PgDown should scroll the config viewport (offset stayed %d)", before)
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
