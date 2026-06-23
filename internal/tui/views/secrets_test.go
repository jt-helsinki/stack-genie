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
