package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestLogsListsServices: the service table is populated from the injected lister
// at construction.
func TestLogsListsServices(test *testing.T) {
	view := NewLogs(
		func() []string { return []string{"litellm", "ollama", "dns"} },
		func(string) ([]string, error) { return nil, nil },
	)
	view.SetSize(80, 24)

	if got := len(view.table.Rows()); got != 3 {
		test.Fatalf("table rows = %d, want 3", got)
	}
	if !strings.Contains(view.View(), "litellm") {
		test.Error("rendered view is missing a service name")
	}
}

// TestLogsTailsSelectedService: pressing enter calls the tailer and the content
// appears in the viewport.
func TestLogsTailsSelectedService(test *testing.T) {
	var asked string
	view := NewLogs(
		func() []string { return []string{"litellm", "ollama"} },
		func(service string) ([]string, error) {
			asked = service
			return []string{"line one", "boot complete"}, nil
		},
	)
	view.SetSize(80, 24)

	// litellm is the first (selected) row → enter tails it.
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		test.Fatal("pressing enter must return a tail command")
	}
	_ = view.Update(cmd()) // run the tailer, feed the result back

	if asked != "litellm" {
		test.Fatalf("tailer asked for %q, want litellm", asked)
	}
	if !view.tailing {
		test.Fatal("view should be tailing after a successful tail")
	}
	if !strings.Contains(view.View(), "boot complete") {
		test.Errorf("expected the log content in the view, got:\n%s", view.View())
	}
}

// TestLogsEmptyTailShowsFriendlyMessage: an empty tail renders the friendly
// "no logs yet" line, not an error.
func TestLogsEmptyTailShowsFriendlyMessage(test *testing.T) {
	view := NewLogs(
		func() []string { return []string{"headroom"} },
		func(string) ([]string, error) { return nil, nil },
	)
	view.SetSize(80, 24)

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = view.Update(cmd())

	if view.err != nil {
		test.Fatalf("an empty tail must not be an error, got %v", view.err)
	}
	if !strings.Contains(view.View(), "no logs on disk yet for headroom") {
		test.Errorf("expected the friendly empty message, got:\n%s", view.View())
	}
}

// TestLogsSurfacesTailError: a tail error is surfaced as an error flash.
func TestLogsSurfacesTailError(test *testing.T) {
	view := NewLogs(
		func() []string { return []string{"litellm"} },
		func(string) ([]string, error) { return nil, errors.New("permission denied") },
	)
	view.SetSize(80, 24)

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = view.Update(cmd())

	if view.err == nil {
		test.Fatal("a tail error must be recorded")
	}
	if !strings.Contains(view.View(), "permission denied") {
		test.Errorf("rendered view must surface the tail error, got:\n%s", view.View())
	}
}
