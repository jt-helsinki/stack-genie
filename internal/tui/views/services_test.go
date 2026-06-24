package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
)

// noControl / noOpen / noTail are stubs for tests that don't exercise actions.
func noControl(string, string) error  { return nil }
func noOpen(string) error             { return nil }
func noTail(string) ([]string, error) { return nil, nil }

func TestServicesPopulatesTableOnRefresh(test *testing.T) {
	statuses := []setup.ServiceStatus{
		{Name: "litellm", Mode: "container", State: "running", Healthy: true, Address: "127.0.0.1:14000"},
		{Name: "ollama", Mode: "container", State: "stopped", Healthy: false},
	}
	view := NewServices(func() ([]setup.ServiceStatus, error) { return statuses, nil }, noControl, noOpen, noTail)

	// Run the fetch command Init returns, then feed its message back in.
	_ = view.Update(view.Init()())

	if !view.loaded {
		test.Fatal("view should be loaded after a refresh")
	}
	if got := len(view.table.Rows()); got != 2 {
		test.Fatalf("table rows = %d, want 2", got)
	}
	if !strings.Contains(view.View(), "litellm") {
		test.Error("rendered view is missing a service name")
	}
}

func TestServicesSurfacesFetchError(test *testing.T) {
	view := NewServices(func() ([]setup.ServiceStatus, error) {
		return nil, errors.New("docker is not running")
	}, noControl, noOpen, noTail)

	_ = view.Update(view.Init()())

	if view.err == nil {
		test.Fatal("a fetch error must be recorded")
	}
	if !strings.Contains(view.View(), "docker is not running") {
		test.Error("rendered view must surface the fetch error")
	}
}

func TestServicesStartActionInvokesController(test *testing.T) {
	var calls []string
	view := NewServices(
		func() ([]setup.ServiceStatus, error) {
			return []setup.ServiceStatus{{Name: "litellm", State: "stopped"}}, nil
		},
		func(action, service string) error { calls = append(calls, action+":"+service); return nil },
		noOpen,
		noTail,
	)
	_ = view.Update(view.Init()()) // load rows so a row is selected

	// Press "s" on the selected service → an async control command.
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd == nil {
		test.Fatal("pressing s must return a control command")
	}
	done := cmd() // runs the controller
	if len(calls) != 1 || calls[0] != "start:litellm" {
		test.Fatalf("controller calls = %v, want [start:litellm]", calls)
	}
	// Feeding the done message back sets a success flash.
	_ = view.Update(done)
	if !strings.Contains(view.View(), "started litellm") {
		test.Errorf("expected a success flash, got view:\n%s", view.View())
	}
}

func TestServicesDescribeTogglesPane(test *testing.T) {
	view := NewServices(
		func() ([]setup.ServiceStatus, error) {
			return []setup.ServiceStatus{{Name: "litellm", State: "running", Console: "http://localhost:14000/ui"}}, nil
		},
		noControl, noOpen, noTail,
	)
	_ = view.Update(view.Init()())
	view.SetSize(80, 20) // give the describe viewport room to render

	_ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if !view.describe.active() {
		test.Fatal("d must open the describe pane")
	}
	if !strings.Contains(view.View(), "http://localhost:14000/ui") {
		test.Errorf("describe pane should show the console URL, got:\n%s", view.View())
	}
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if view.describe.active() {
		test.Fatal("esc must close the describe pane")
	}
}

func TestServicesLogsPaneOpensAndEscReturns(test *testing.T) {
	view := NewServices(
		func() ([]setup.ServiceStatus, error) {
			return []setup.ServiceStatus{{Name: "litellm", State: "running"}}, nil
		},
		noControl, noOpen,
		func(service string) ([]string, error) { return []string{"line one", "line two"}, nil },
	)
	_ = view.Update(view.Init()())
	view.SetSize(80, 20)

	// `l` opens the full-pane log viewer for the selected service.
	_ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if !view.logs.active() {
		test.Fatal("l must open the logs pane")
	}
	rendered := view.View()
	if !strings.Contains(rendered, "logs · litellm") || !strings.Contains(rendered, "line one") {
		test.Errorf("logs pane should fill the view with the tailed logs, got:\n%s", rendered)
	}
	if strings.Contains(rendered, "SERVICE") {
		test.Error("the service table should be hidden while the logs pane is open")
	}
	// esc returns to the table.
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if view.logs.active() {
		test.Fatal("esc must return from the logs pane to the table")
	}
}

// TestServicesEnableKeyTogglesOptional: `e` enables a disabled optional service
// (and would disable an enabled one), driving the control func with enable/disable.
func TestServicesEnableKeyTogglesOptional(test *testing.T) {
	var controlled string
	view := NewServices(
		func() ([]setup.ServiceStatus, error) {
			return []setup.ServiceStatus{{Name: "open-webui", State: "disabled", Optional: true}}, nil
		},
		func(action, service string) error { controlled = action + ":" + service; return nil },
		noOpen, noTail,
	)
	_ = view.Update(view.Init()())

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if cmd == nil {
		test.Fatal("e on a disabled optional must return an enable command")
	}
	cmd()
	if controlled != "enable:open-webui" {
		test.Fatalf("e should enable a disabled optional, got %q", controlled)
	}
}

// TestServicesEnableKeyRejectsCore: `e` on a core service is a no-op with a hint
// (core services are always on).
func TestServicesEnableKeyRejectsCore(test *testing.T) {
	view := NewServices(
		func() ([]setup.ServiceStatus, error) {
			return []setup.ServiceStatus{{Name: "litellm", State: "running"}}, nil
		}, noControl, noOpen, noTail)
	_ = view.Update(view.Init()())

	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")}); cmd != nil {
		test.Fatal("e on a core service must not control anything")
	}
	if !strings.Contains(view.View(), "core service") {
		test.Errorf("expected a core-service hint, got:\n%s", view.View())
	}
}

// TestServicesStartGatedOnDisabledOptional: start/stop/restart are blocked on a
// disabled optional service (the user must enable it first) — no control call.
func TestServicesStartGatedOnDisabledOptional(test *testing.T) {
	controlled := false
	view := NewServices(
		func() ([]setup.ServiceStatus, error) {
			return []setup.ServiceStatus{{Name: "odysseus", State: "disabled", Optional: true}}, nil
		},
		func(string, string) error { controlled = true; return nil },
		noOpen, noTail,
	)
	_ = view.Update(view.Init()())

	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")}); cmd != nil {
		test.Fatal("start on a disabled optional must not return a control command")
	}
	if controlled {
		test.Fatal("start on a disabled optional must not control anything")
	}
	if !strings.Contains(view.View(), "disabled") {
		test.Errorf("expected a 'press e to enable' hint, got:\n%s", view.View())
	}
}

// TestServicesMenuKeysWorkWhilePaneOpen is the regression for the pane-trap bug:
// once a service's describe/logs pane is open, the menu keys must still work — `l`
// jumps from describe to logs (and `d` back), and an operation key closes the pane
// and acts — without needing esc first.
func TestServicesMenuKeysWorkWhilePaneOpen(test *testing.T) {
	var controlled string
	view := NewServices(
		func() ([]setup.ServiceStatus, error) {
			return []setup.ServiceStatus{{Name: "litellm", State: "running", Console: "http://x/ui"}}, nil
		},
		func(action, service string) error { controlled = action + ":" + service; return nil },
		noOpen,
		func(string) ([]string, error) { return []string{"a log line"}, nil },
	)
	_ = view.Update(view.Init()())
	view.SetSize(80, 20)

	// Open describe, then jump straight to logs with `l` (no esc).
	_ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	_ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if !view.logs.active() || view.describe.active() {
		test.Fatalf("l from describe must switch to the logs pane (logs=%v describe=%v)",
			view.logs.active(), view.describe.active())
	}
	// `d` jumps back to describe, closing logs.
	_ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if !view.describe.active() || view.logs.active() {
		test.Fatalf("d from logs must switch back to describe (logs=%v describe=%v)",
			view.logs.active(), view.describe.active())
	}
	// An operation key (restart) works while a pane is open: it closes the pane and
	// runs the action so the result is visible over the table.
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if view.describe.active() || view.logs.active() {
		test.Fatal("an operation key must close the open pane")
	}
	if cmd == nil {
		test.Fatal("restart must return a control command")
	}
	cmd()
	if controlled != "restart:litellm" {
		test.Fatalf("restart should control the service, got %q", controlled)
	}
}

func TestServicesOpenConsoleUsesURL(test *testing.T) {
	var opened string
	view := NewServices(
		func() ([]setup.ServiceStatus, error) {
			return []setup.ServiceStatus{{Name: "litellm", Console: "http://localhost:14000/ui"}}, nil
		},
		noControl,
		func(url string) error { opened = url; return nil },
		noTail,
	)
	_ = view.Update(view.Init()())

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if cmd == nil {
		test.Fatal("pressing o on a service with a console must return a command")
	}
	cmd()
	if opened != "http://localhost:14000/ui" {
		test.Fatalf("opened URL = %q, want the console URL", opened)
	}
}
