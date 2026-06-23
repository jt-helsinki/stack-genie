package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
)

// noControl / noOpen are stubs for tests that don't exercise actions.
func noControl(string, string) error { return nil }
func noOpen(string) error            { return nil }

func TestServicesPopulatesTableOnRefresh(test *testing.T) {
	statuses := []setup.ServiceStatus{
		{Name: "litellm", Mode: "container", State: "running", Healthy: true, Address: "127.0.0.1:14000"},
		{Name: "ollama", Mode: "container", State: "stopped", Healthy: false},
	}
	view := NewServices(func() ([]setup.ServiceStatus, error) { return statuses, nil }, noControl, noOpen)

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
	}, noControl, noOpen)

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
		noControl, noOpen,
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

func TestServicesOpenConsoleUsesURL(test *testing.T) {
	var opened string
	view := NewServices(
		func() ([]setup.ServiceStatus, error) {
			return []setup.ServiceStatus{{Name: "litellm", Console: "http://localhost:14000/ui"}}, nil
		},
		noControl,
		func(url string) error { opened = url; return nil },
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
