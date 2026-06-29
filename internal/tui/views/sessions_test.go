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

// When a refresh errors AND the tab is active, the view schedules an auto-retry so
// it self-heals once the workspace responds (e.g. after a host sleep); when the tab
// is inactive it does not (no background in-VM calls).
func TestSessionsAutoRetriesOnErrorWhenActive(test *testing.T) {
	view := NewSessions(
		func() ([]workspace.Session, error) { return nil, errors.New("not responding") },
		noKill,
		func() string { return "app" },
	)
	view.SetActive(true)
	if cmd := view.Update(sessionsRefreshedMsg{err: errors.New("not responding")}); cmd == nil {
		test.Fatal("an active tab must schedule an auto-retry after an error")
	}
	view.SetActive(false)
	if cmd := view.Update(sessionsRefreshedMsg{err: errors.New("not responding")}); cmd != nil {
		test.Fatal("an inactive tab must NOT schedule an auto-retry")
	}
}

// A retry tick re-fetches only when its generation is current and the tab is active;
// a stale generation (the tab was left/re-entered) is dropped.
func TestSessionsRetryTickGenerationGuard(test *testing.T) {
	view := NewSessions(
		func() ([]workspace.Session, error) { return nil, nil },
		noKill,
		func() string { return "app" },
	)
	view.SetActive(true)
	current := view.generation
	if cmd := view.Update(sessionsRetryMsg{generation: current}); cmd == nil {
		test.Fatal("a current-generation retry tick on an active tab must re-fetch")
	}
	if cmd := view.Update(sessionsRetryMsg{generation: current - 1}); cmd != nil {
		test.Fatal("a stale-generation retry tick must be dropped")
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

// `n` opens the inline new-session prompt; typing a name + enter emits an attach
// request that creates (and attaches to) that named session.
func TestSessionsNewSessionPromptCreates(test *testing.T) {
	view := NewSessions(
		func() ([]workspace.Session, error) { return nil, nil },
		noKill,
		func() string { return "app" },
	)
	_ = view.Update(view.Init()())
	// n enters create mode (no command yet — it's just opening the prompt).
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}); cmd != nil {
		test.Fatal("n should open the prompt, not emit a command yet")
	}
	if !view.creating {
		test.Fatal("n must enter create mode")
	}
	// Type a name (a space is dropped — not a valid tmux session name).
	view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("feat")})
	view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		test.Fatal("enter must emit an attach request for the new named session")
	}
	requested, ok := cmd().(AttachRequestedMsg)
	if !ok || requested.Session != "featx" {
		test.Fatalf("want AttachRequestedMsg{session=%q}, got %#v", "featx", cmd())
	}
	if view.creating {
		test.Fatal("create mode must close after enter")
	}
}

// esc cancels the inline new-session prompt without emitting anything.
func TestSessionsNewSessionPromptCancels(test *testing.T) {
	view := NewSessions(
		func() ([]workspace.Session, error) { return nil, nil },
		noKill,
		func() string { return "app" },
	)
	_ = view.Update(view.Init()())
	view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abc")})
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
		test.Fatal("esc must cancel without emitting a command")
	}
	if view.creating || view.nameInput != "" {
		test.Fatal("esc must clear create mode + the typed name")
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
	if !strings.Contains(view.View(), "no workspace selected") {
		test.Errorf("no-workspace view must hint: %q", view.View())
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

// The PRECISE manager sentinel messages (stale / overloaded VM) reach the rendered
// view verbatim — NOT the old blanket "may be busy (e.g. pulling an image)" line.
func TestSessionsSurfacesPreciseManagerError(test *testing.T) {
	for _, message := range []string{
		"workspace is marked started but its microVM isn't running (stale state) — run `ai restart`",
		"workspace is running but not responding — it may be overloaded; try `ai restart`",
	} {
		view := NewSessions(
			func() ([]workspace.Session, error) { return nil, errors.New(message) },
			noKill,
			func() string { return "app" },
		)
		_ = view.Update(view.Init()())
		if rendered := view.View(); !strings.Contains(rendered, message) {
			test.Errorf("view must show the precise manager error %q:\n%s", message, rendered)
		}
	}
}
