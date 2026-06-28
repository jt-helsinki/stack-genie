package views

import (
	"errors"
	"strings"
	"testing"
)

// driveSandbox runs the view's Init/Update synchronously by executing the returned
// command and feeding its message back, mirroring the bubbletea loop for one step.
func TestWorkspaceLogLoadsAndShowsContent(test *testing.T) {
	project := "demo"
	view := NewWorkspaceLog(
		func() (string, error) { return "boot line 1\nboot line 2\n", nil },
		func() string { return project },
	)
	view.SetSize(80, 10)

	// Before any load the view reports loading.
	if got := view.View(); !strings.Contains(got, "loading workspace log") {
		test.Fatalf("expected loading state, got %q", got)
	}
	// Feed a load result for the current generation.
	view.generation = 1
	view.Update(workspaceLogLoadedMsg{content: "boot line 1\nboot line 2\n", generation: 1})
	if !view.loaded || view.empty {
		test.Fatalf("expected loaded non-empty after a content result")
	}
	if !strings.Contains(view.View(), "boot line") {
		test.Errorf("rendered view should contain the log content:\n%s", view.View())
	}
}

// TestWorkspaceLogStaleResultIgnored verifies a result tagged with an old generation
// (from a previous activation) is discarded.
func TestWorkspaceLogStaleResultIgnored(test *testing.T) {
	view := NewWorkspaceLog(func() (string, error) { return "", nil }, func() string { return "demo" })
	view.generation = 5
	view.Update(workspaceLogLoadedMsg{content: "stale", generation: 4})
	if view.loaded {
		test.Error("a stale-generation result must be ignored")
	}
}

// TestWorkspaceLogSurfacesError shows the error when the workspace is not running.
func TestWorkspaceLogSurfacesError(test *testing.T) {
	view := NewWorkspaceLog(
		func() (string, error) { return "", errors.New("workspace microVM is not running") },
		func() string { return "demo" },
	)
	view.SetSize(80, 10)
	view.generation = 1
	view.Update(workspaceLogLoadedMsg{err: errors.New("workspace microVM is not running"), generation: 1})
	if !strings.Contains(view.View(), "not running") {
		test.Errorf("expected the error surfaced in the view:\n%s", view.View())
	}
}

// TestWorkspaceLogNoProject shows the no-workspace hint and does not start a cycle.
func TestWorkspaceLogNoProject(test *testing.T) {
	view := NewWorkspaceLog(func() (string, error) { return "", nil }, func() string { return "" })
	if cmd := view.Init(); cmd != nil {
		test.Error("Init with no project should not start a poll cycle")
	}
	if !strings.Contains(view.View(), "no workspace selected") {
		test.Errorf("expected no-workspace hint:\n%s", view.View())
	}
}
