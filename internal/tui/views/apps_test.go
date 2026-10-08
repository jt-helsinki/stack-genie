package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/apps"
)

func sampleStatuses() []apps.Status {
	return []apps.Status{
		{Key: "openwebui", Name: "Open WebUI", Installed: true, Running: true, Port: 21000, URL: "http://localhost:21000"},
		{Key: "webui-alt", Name: "WebUI Alt"},
	}
}

func sampleTools() []ToolRow {
	return []ToolRow{
		{Key: "caveman", Name: "caveman", Enabled: true},
		{Key: "tokensave", Name: "tokensave", Enabled: false},
	}
}

func TestAppsPopulatesTableOnRefresh(test *testing.T) {
	view := NewApps(
		func() ([]apps.Status, error) { return sampleStatuses(), nil },
		func() ([]ToolRow, error) { return sampleTools(), nil },
		func() string { return "demo" },
	)
	_ = view.Update(view.Init()())
	if !view.loaded {
		test.Fatal("view should be loaded after refresh")
	}
	// 2 app rows + 2 tool rows merged into one table.
	if got := len(view.table.Rows()); got != 4 {
		test.Fatalf("table rows = %d, want 4", got)
	}
	rendered := view.View()
	for _, want := range []string{
		"Open WebUI", "running", "http://localhost:21000", "WebUI Alt", "not installed",
		"caveman", "enabled", "tokensave", "disabled",
	} {
		if !strings.Contains(rendered, want) {
			test.Errorf("rendered view missing %q:\n%s", want, rendered)
		}
	}
}

// Each action key emits an AppActionRequestedMsg for the selected APP row with the
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
			func() ([]ToolRow, error) { return nil, nil },
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

// On a TOOL row, "a"/"x" emit a ToolActionRequestedMsg with enable/disable (not
// add/remove), and the container-only keys (u/s/t/R) are swallowed as no-ops.
func TestAppsToolRowActionKeys(test *testing.T) {
	newView := func() *Apps {
		view := NewApps(
			func() ([]apps.Status, error) { return sampleStatuses(), nil }, // 2 app rows
			func() ([]ToolRow, error) { return sampleTools(), nil },        // 2 tool rows after
			func() string { return "demo" },
		)
		_ = view.Update(view.Init()())
		view.table.SetCursor(2) // first tool row ("caveman")
		return view
	}

	test.Run("a enables", func(test *testing.T) {
		view := newView()
		cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
		if cmd == nil {
			test.Fatal("expected a ToolActionRequestedMsg")
		}
		requested, ok := cmd().(ToolActionRequestedMsg)
		if !ok {
			test.Fatalf("got %#v, want ToolActionRequestedMsg", cmd())
		}
		if requested.Project != "demo" || requested.Tool != "caveman" || requested.Action != "enable" {
			test.Fatalf("got %#v, want {demo, caveman, enable}", requested)
		}
	})

	test.Run("x disables", func(test *testing.T) {
		view := newView()
		cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
		requested, ok := cmd().(ToolActionRequestedMsg)
		if !ok {
			test.Fatalf("got %#v, want ToolActionRequestedMsg", cmd())
		}
		if requested.Action != "disable" {
			test.Fatalf("got action %q, want disable", requested.Action)
		}
	})

	for _, key := range []string{"u", "s", "t", "R"} {
		test.Run("key "+key+" is a no-op on a tool row", func(test *testing.T) {
			view := newView()
			cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
			if cmd != nil {
				test.Fatalf("key %q on a tool row must not emit a request, got %#v", key, cmd())
			}
		})
	}
}

// r refreshes (re-invokes both listers).
func TestAppsRefreshKeyRefetches(test *testing.T) {
	calls := 0
	view := NewApps(
		func() ([]apps.Status, error) { calls++; return sampleStatuses(), nil },
		func() ([]ToolRow, error) { return nil, nil },
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
		func() ([]ToolRow, error) { return nil, nil },
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
		func() ([]ToolRow, error) { return nil, nil },
		func() string { return "demo" },
	)
	_ = view.Update(view.Init()())
	if !strings.Contains(view.View(), "boom") {
		test.Fatalf("view should render the fetch error:\n%s", view.View())
	}
}

func TestAppsTitleAndHints(test *testing.T) {
	view := NewApps(
		func() ([]apps.Status, error) { return nil, nil },
		func() ([]ToolRow, error) { return nil, nil },
		func() string { return "" },
	)
	if view.Title() != "Apps" {
		test.Fatalf("Title = %q, want Apps", view.Title())
	}
	if !strings.Contains(view.Hints(), "add") {
		test.Fatalf("Hints missing add: %q", view.Hints())
	}
}
