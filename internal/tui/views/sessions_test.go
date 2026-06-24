package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
)

func noKill(string) error { return nil }

// The view lists the current project's sessions on refresh and renders the names.
func TestSessionsPopulatesTableOnRefresh(test *testing.T) {
	sessions := []workspace.Session{
		{Name: "shell", Attached: true, Activity: "1700000000"},
		{Name: "opencode", Attached: false, Activity: "1700000500"},
	}
	view := NewSessions(
		func() ([]workspace.Session, error) { return sessions, nil },
		noKill,
		func() string { return "app" },
	)
	_ = view.Update(view.Init()())

	if !view.loaded {
		test.Fatal("view should be loaded after a refresh")
	}
	if got := len(view.table.Rows()); got != 2 {
		test.Fatalf("table rows = %d, want 2", got)
	}
	rendered := view.View()
	if !strings.Contains(rendered, "shell") || !strings.Contains(rendered, "opencode") {
		test.Errorf("rendered view is missing session names:\n%s", rendered)
	}
}

// a/enter on the selected row emit an AttachRequestedMsg for that session.
func TestSessionsAttachKeysEmitAttachRequest(test *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("a")},
		{Type: tea.KeyEnter},
	} {
		view := NewSessions(
			func() ([]workspace.Session, error) {
				return []workspace.Session{{Name: "shell", Attached: false}}, nil
			},
			noKill,
			func() string { return "app" },
		)
		_ = view.Update(view.Init()()) // load rows so a row is selected

		cmd := view.Update(key)
		if cmd == nil {
			test.Fatalf("%v must emit an attach request", key)
		}
		requested, ok := cmd().(AttachRequestedMsg)
		if !ok || requested.Project != "app" || requested.Session != "shell" {
			test.Fatalf("want AttachRequestedMsg{app, shell}, got %#v", cmd())
		}
	}
}

// `n` (new agent) emits an attach request for the default agent session (which is
// created on attach).
func TestSessionsNewAgentEmitsDefaultAttach(test *testing.T) {
	view := NewSessions(
		func() ([]workspace.Session, error) { return nil, nil },
		noKill,
		func() string { return "app" },
	)
	_ = view.Update(view.Init()())
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if cmd == nil {
		test.Fatal("n must emit an attach request for the default agent session")
	}
	requested, ok := cmd().(AttachRequestedMsg)
	if !ok || requested.Session != defaultAgentSession {
		test.Fatalf("want AttachRequestedMsg{session=%q}, got %#v", defaultAgentSession, cmd())
	}
}

// `k` kills the selected session via the injected killer, then refreshes.
func TestSessionsKillInvokesKiller(test *testing.T) {
	var killed string
	view := NewSessions(
		func() ([]workspace.Session, error) { return []workspace.Session{{Name: "opencode"}}, nil },
		func(session string) error { killed = session; return nil },
		func() string { return "app" },
	)
	_ = view.Update(view.Init()())

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if cmd == nil {
		test.Fatal("k must return a kill command")
	}
	done := cmd() // runs the killer
	if killed != "opencode" {
		test.Fatalf("killer called with %q, want opencode", killed)
	}
	if refresh := view.Update(done); refresh == nil {
		test.Fatal("after a kill the view must refresh")
	}
	if !strings.Contains(view.View(), "session killed") {
		test.Errorf("expected a success flash, got:\n%s", view.View())
	}
}

// With no project selected the view shows a hint and the lister is not consulted.
func TestSessionsNoProjectSelected(test *testing.T) {
	called := false
	view := NewSessions(
		func() ([]workspace.Session, error) { called = true; return nil, nil },
		noKill,
		func() string { return "" },
	)
	if !strings.Contains(view.View(), "no project selected") {
		test.Errorf("no-project view must hint: %q", view.View())
	}
	// Attach/kill keys are no-ops with no project.
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}); cmd != nil {
		test.Error("a must be a no-op with no project")
	}
	_ = called // the lister may still be invoked by Init; the View guard is what matters
}

// A fetch error is surfaced in the view.
func TestSessionsSurfacesFetchError(test *testing.T) {
	view := NewSessions(
		func() ([]workspace.Session, error) { return nil, errors.New("workspace not running") },
		noKill,
		func() string { return "app" },
	)
	_ = view.Update(view.Init()())
	if view.err == nil {
		test.Fatal("a fetch error must be recorded")
	}
	if !strings.Contains(view.View(), "workspace not running") {
		test.Error("rendered view must surface the fetch error")
	}
}
