package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func sampleProviders() []APIKeyProvider {
	return []APIKeyProvider{
		{Provider: "openai", Name: "OpenAI", HasKey: true, Models: 2},
		{Provider: "google", Name: "Google", HasKey: false, Models: 1},
	}
}

// refreshAPIKeys drives the view's provider refresh synchronously.
func refreshAPIKeys(view *APIKeys) { _ = view.Update(view.refreshCmd()()) }

// pressKey sends a single rune keypress and returns the resulting command (if any).
func pressKey(view *APIKeys, key string) tea.Cmd {
	return view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
}

// TestAPIKeysRendersProviderRows verifies the table renders the routable providers
// with their keyed status + model count after a refresh.
func TestAPIKeysRendersProviderRows(test *testing.T) {
	view := NewAPIKeys(func() ([]APIKeyProvider, error) { return sampleProviders(), nil })
	view.SetSize(80, 20)
	refreshAPIKeys(view)

	if !view.loaded {
		test.Fatal("view should be loaded after a refresh")
	}
	rendered := view.View()
	for _, want := range []string{"openai", "OpenAI", "google", "yes"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("rendered view missing %q:\n%s", want, rendered)
		}
	}
}

// TestAPIKeysAddEmitsRequest verifies "a" on the selected (first) row emits an
// APIKeyAddRequestedMsg for that provider — the parent then runs `ai keys add` in
// the terminal overlay.
func TestAPIKeysAddEmitsRequest(test *testing.T) {
	view := NewAPIKeys(func() ([]APIKeyProvider, error) { return sampleProviders(), nil })
	view.SetSize(80, 20)
	refreshAPIKeys(view)

	cmd := pressKey(view, "a")
	if cmd == nil {
		test.Fatal("pressing a should emit a command")
	}
	msg, ok := cmd().(APIKeyAddRequestedMsg)
	if !ok {
		test.Fatalf("a emitted %T, want APIKeyAddRequestedMsg", cmd())
	}
	if msg.Provider != "openai" {
		test.Errorf("add request provider = %q, want openai (the selected row)", msg.Provider)
	}
}

// TestAPIKeysRemoveEmitsRequestWhenKeyed verifies "d" on a KEYED provider emits a
// remove request, and is a no-op (just a flash) on an unkeyed provider.
func TestAPIKeysRemoveEmitsRequestWhenKeyed(test *testing.T) {
	view := NewAPIKeys(func() ([]APIKeyProvider, error) { return sampleProviders(), nil })
	view.SetSize(80, 20)
	refreshAPIKeys(view)

	// First row (openai) is keyed → remove emits a request.
	cmd := pressKey(view, "d")
	if cmd == nil {
		test.Fatal("pressing d on a keyed provider should emit a command")
	}
	msg, ok := cmd().(APIKeyRemoveRequestedMsg)
	if !ok {
		test.Fatalf("d emitted %T, want APIKeyRemoveRequestedMsg", cmd())
	}
	if msg.Provider != "openai" {
		test.Errorf("remove request provider = %q, want openai", msg.Provider)
	}

	// Move to the second row (google, unkeyed) → remove is a no-op with a flash.
	_ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
	cmd = pressKey(view, "d")
	if cmd != nil {
		test.Error("pressing d on an unkeyed provider should not emit a request")
	}
	if !strings.Contains(view.flash, "no key") {
		test.Errorf("unkeyed remove should flash a hint, got %q", view.flash)
	}
}

// TestAPIKeysRefreshKey verifies "r" re-runs the lister.
func TestAPIKeysRefreshKey(test *testing.T) {
	calls := 0
	view := NewAPIKeys(func() ([]APIKeyProvider, error) {
		calls++
		return sampleProviders(), nil
	})
	view.SetSize(80, 20)
	refreshAPIKeys(view) // calls == 1

	cmd := pressKey(view, "r")
	if cmd == nil {
		test.Fatal("r should emit a refresh command")
	}
	_ = view.Update(cmd()) // executes the refresh
	if calls != 2 {
		test.Errorf("lister calls = %d, want 2 (initial + r)", calls)
	}
}

// TestAPIKeysListError surfaces a lister error in the view.
func TestAPIKeysListError(test *testing.T) {
	view := NewAPIKeys(func() ([]APIKeyProvider, error) { return nil, errors.New("gateway down") })
	view.SetSize(80, 20)
	refreshAPIKeys(view)
	if !strings.Contains(view.View(), "gateway down") {
		test.Errorf("view should surface the lister error:\n%s", view.View())
	}
}
