package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
)

func noKill(string) error { return nil }

// The view lists the current project's sessions on refresh and renders the names.
func TestSessionsPopulatesTableOnRefresh(test *testing.T) {
	sessions := []workspace.Session{
		{SessionID: "abc12345", Name: "shell", Status: "running", Attachment: "attached"},
		{SessionID: "def67890", Name: "opencode", Status: "running", Attachment: "detached"},
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

// While the tab is ACTIVE the view re-arms a steady refresh after every result
// (success OR error) so it stays live (reflects stop/start, new/killed sessions, and
// recovers after a sleep); while INACTIVE it does not (no background calls).
func TestSessionsRefreshesWhileActive(test *testing.T) {
	view := NewSessions(
		func() ([]workspace.Session, error) { return nil, nil },
		noKill,
		func() string { return "app" },
	)
	view.SetActive(true)
	if cmd := view.Update(sessionsRefreshedMsg{sessions: nil}); cmd == nil {
		test.Fatal("an active tab must re-arm a refresh after a successful result")
	}
	if cmd := view.Update(sessionsRefreshedMsg{err: errors.New("not responding")}); cmd == nil {
		test.Fatal("an active tab must re-arm a refresh after an error (recovery)")
	}
	view.SetActive(false)
	if cmd := view.Update(sessionsRefreshedMsg{sessions: nil}); cmd != nil {
		test.Fatal("an inactive tab must NOT re-arm a refresh")
	}
}

// A refresh tick re-fetches only when its generation is current and the tab is
// active; a stale generation (the tab was left/re-entered) is dropped.
func TestSessionsRefreshTickGenerationGuard(test *testing.T) {
	view := NewSessions(
		func() ([]workspace.Session, error) { return nil, nil },
		noKill,
		func() string { return "app" },
	)
	view.SetActive(true)
	current := view.generation
	if cmd := view.Update(sessionsRefreshMsg{generation: current}); cmd == nil {
		test.Fatal("a current-generation refresh tick on an active tab must re-fetch")
	}
	if cmd := view.Update(sessionsRefreshMsg{generation: current - 1}); cmd != nil {
		test.Fatal("a stale-generation refresh tick must be dropped")
	}
}

// a/enter on the selected row emit an AttachRequestedMsg for that session's (short)
// id — the same id `ai sessions`/`ai attach` render and accept.
func TestSessionsAttachKeysEmitAttachRequest(test *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("a")},
		{Type: tea.KeyEnter},
	} {
		view := NewSessions(
			func() ([]workspace.Session, error) {
				return []workspace.Session{{SessionID: "abc12345xyz", Name: "shell", Status: "running"}}, nil
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
		if !ok || requested.Project != "app" || requested.Session != "abc12345" {
			test.Fatalf("want AttachRequestedMsg{app, abc12345}, got %#v", cmd())
		}
	}
}

// `n` emits an ExecRequestedMsg (`ai shell <name>`) directly — nono shell sessions
// are anonymous, so there is no name prompt to create one.
func TestSessionsNewKeyRequestsShell(test *testing.T) {
	view := NewSessions(
		func() ([]workspace.Session, error) { return nil, nil },
		noKill,
		func() string { return "app" },
	)
	_ = view.Update(view.Init()())
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if cmd == nil {
		test.Fatal("n must emit a shell request")
	}
	requested, ok := cmd().(ExecRequestedMsg)
	if !ok || requested.Project != "app" {
		test.Fatalf("want ExecRequestedMsg{app}, got %#v", cmd())
	}
}

// `d` stops the selected session via the injected killer, then refreshes.
func TestSessionsStopInvokesKiller(test *testing.T) {
	var stopped string
	view := NewSessions(
		func() ([]workspace.Session, error) {
			return []workspace.Session{{SessionID: "sess1", Name: "opencode", Status: "running"}}, nil
		},
		func(sessionID string) error { stopped = sessionID; return nil },
		func() string { return "app" },
	)
	_ = view.Update(view.Init()())

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if cmd == nil {
		test.Fatal("d must return a stop command")
	}
	done := cmd() // runs the killer
	if stopped != "sess1" {
		test.Fatalf("killer called with %q, want sess1", stopped)
	}
	if refresh := view.Update(done); refresh == nil {
		test.Fatal("after a stop the view must refresh")
	}
	if !strings.Contains(view.View(), "session stopped") {
		test.Errorf("expected a success flash, got:\n%s", view.View())
	}
}

// With no project selected the view shows a hint and action keys are inert.
func TestSessionsNoProjectSelected(test *testing.T) {
	view := NewSessions(
		func() ([]workspace.Session, error) { return nil, nil },
		noKill,
		func() string { return "" },
	)
	if !strings.Contains(view.View(), "no workspace selected") {
		test.Errorf("no-workspace view must hint: %q", view.View())
	}
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}); cmd != nil {
		test.Error("a must be a no-op with no project")
	}
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
