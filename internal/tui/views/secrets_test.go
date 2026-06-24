package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/secrets"
)

func TestSecretsPopulatesTableOnRefresh(test *testing.T) {
	entries := []secrets.Entry{
		{Name: "openai", EnvVar: "OPENAI_API_KEY"},
		{Name: "anthropic", EnvVar: "ANTHROPIC_API_KEY"},
	}
	view := NewSecrets(
		func() ([]secrets.Entry, error) { return entries, nil },
		func(string) error { return nil },
	)

	_ = view.Update(view.Init()())

	if !view.loaded {
		test.Fatal("view should be loaded after a refresh")
	}
	if got := len(view.table.Rows()); got != 2 {
		test.Fatalf("table rows = %d, want 2", got)
	}
	if !strings.Contains(view.View(), "openai") {
		test.Error("rendered view is missing a credential name")
	}
}

func TestSecretsSurfacesListError(test *testing.T) {
	view := NewSecrets(
		func() ([]secrets.Entry, error) { return nil, errors.New("gateway unreachable") },
		func(string) error { return nil },
	)
	_ = view.Update(view.Init()())

	if view.err == nil {
		test.Fatal("a list error must be recorded")
	}
	if !strings.Contains(view.View(), "gateway unreachable") {
		test.Error("rendered view must surface the list error")
	}
}

// TestSecretsEnterDescribesAndKeysStayLive: enter drills into the selected
// credential's detail (name + env var, never the value), and the menu keys remain
// live while the pane is open (esc backs out).
func TestSecretsEnterDescribesAndKeysStayLive(test *testing.T) {
	view := NewSecrets(
		func() ([]secrets.Entry, error) {
			return []secrets.Entry{{Name: "openai", EnvVar: "OPENAI_API_KEY"}}, nil
		},
		func(string) error { return nil },
	)
	_ = view.Update(view.Init()())
	view.SetSize(80, 20)

	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !view.describe.active() {
		test.Fatal("enter must open the credential describe pane")
	}
	rendered := view.View()
	if !strings.Contains(rendered, "openai") || !strings.Contains(rendered, "OPENAI_API_KEY") {
		test.Errorf("describe pane should show name + env var, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "never on platform disk") {
		test.Error("describe pane must note the value is not on platform disk")
	}
	// esc backs out to the table.
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if view.describe.active() {
		test.Fatal("esc must close the describe pane")
	}
}

func TestSecretsDeleteActionInvokesRemover(test *testing.T) {
	var removed []string
	view := NewSecrets(
		func() ([]secrets.Entry, error) {
			return []secrets.Entry{{Name: "openai", EnvVar: "OPENAI_API_KEY"}}, nil
		},
		func(name string) error { removed = append(removed, name); return nil },
	)
	_ = view.Update(view.Init()()) // load rows so one is selected

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if cmd == nil {
		test.Fatal("pressing d must return a remove command")
	}
	done := cmd()
	if len(removed) != 1 || removed[0] != "openai" {
		test.Fatalf("remover calls = %v, want [openai]", removed)
	}
	_ = view.Update(done)
	if !strings.Contains(view.View(), "deleted openai") {
		test.Errorf("expected a success flash, got view:\n%s", view.View())
	}
}
