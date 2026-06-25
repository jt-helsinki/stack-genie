package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/apps"
)

func sampleStatuses() []apps.Status {
	return []apps.Status{
		{Key: "openwebui", Name: "Open WebUI", Installed: true, Running: true, Port: 21000, URL: "http://localhost:21000"},
		{Key: "anythingllm", Name: "AnythingLLM"},
	}
}

func TestAppsPopulatesTableOnRefresh(test *testing.T) {
	view := NewApps(
		func() ([]apps.Status, error) { return sampleStatuses(), nil },
		func() string { return "demo" },
	)
	_ = view.Update(view.Init()())
	if !view.loaded {
		test.Fatal("view should be loaded after refresh")
	}
	if got := len(view.table.Rows()); got != 2 {
		test.Fatalf("table rows = %d, want 2", got)
	}
	rendered := view.View()
	for _, want := range []string{"Open WebUI", "running", "http://localhost:21000", "AnythingLLM", "not installed"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("rendered view missing %q:\n%s", want, rendered)
		}
	}
}

// Each action key emits an AppActionRequestedMsg for the selected row with the
// right action.
func TestAppsActionKeysEmitRequests(test *testing.T) {
	cases := map[string]string{
		"a": "add",
		"x": "remove",
		"u": "update",
		"s": "start",
		"t": "stop",
		"R": "restart",
	}
	for key, action := range cases {
		view := NewApps(
			func() ([]apps.Status, error) { return sampleStatuses(), nil },
			func() string { return "demo" },
		)
		_ = view.Update(view.Init()()) // load rows; cursor on first row (openwebui)
		cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if cmd == nil {
			test.Fatalf("key %q (%s) must emit an action request", key, action)
		}
		requested, ok := cmd().(AppActionRequestedMsg)
		if !ok {
			test.Fatalf("key %q produced %#v, want AppActionRequestedMsg", key, cmd())
		}
		if requested.Project != "demo" || requested.App != "openwebui" || requested.Action != action {
			test.Fatalf("key %q -> %#v, want {demo, openwebui, %s}", key, requested, action)
		}
	}
}

// r refreshes (re-invokes the lister).
func TestAppsRefreshKeyRefetches(test *testing.T) {
	calls := 0
	view := NewApps(
		func() ([]apps.Status, error) { calls++; return sampleStatuses(), nil },
		func() string { return "demo" },
	)
	_ = view.Update(view.Init()())
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); cmd != nil {
		_ = view.Update(cmd())
	}
	if calls < 2 {
		test.Fatalf("lister called %d times, want >=2 (initial + refresh)", calls)
	}
}

// With no workspace selected, action keys are swallowed (no request) and the view
// shows a hint.
func TestAppsNoWorkspaceSwallowsActions(test *testing.T) {
	view := NewApps(
		func() ([]apps.Status, error) { return nil, nil },
		func() string { return "" },
	)
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if cmd != nil {
		test.Fatalf("with no workspace, action key must not emit a request, got %#v", cmd())
	}
	if !strings.Contains(view.View(), "no workspace selected") {
		test.Fatalf("view should hint at selecting a workspace:\n%s", view.View())
	}
}

func TestAppsRendersFetchError(test *testing.T) {
	view := NewApps(
		func() ([]apps.Status, error) { return nil, errors.New("boom") },
		func() string { return "demo" },
	)
	_ = view.Update(view.Init()())
	if !strings.Contains(view.View(), "boom") {
		test.Fatalf("view should render the fetch error:\n%s", view.View())
	}
}

func TestAppsTitleAndHints(test *testing.T) {
	view := NewApps(func() ([]apps.Status, error) { return nil, nil }, func() string { return "" })
	if view.Title() != "Apps" {
		test.Fatalf("Title = %q, want Apps", view.Title())
	}
	if !strings.Contains(view.Hints(), "add") {
		test.Fatalf("Hints missing add: %q", view.Hints())
	}
}
