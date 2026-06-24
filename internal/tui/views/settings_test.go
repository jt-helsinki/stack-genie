package views

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestSettingsAppliesThemeOnEnter: navigating to a theme and pressing enter
// applies+persists it (via the injected applier) and emits ThemeChangedMsg so the
// parent can recolor the whole UI.
func TestSettingsAppliesThemeOnEnter(test *testing.T) {
	applied := ""
	view := NewSettings(
		[]string{"one", "two", "three"},
		func() string { return applied },
		func(name string) error { applied = name; return nil },
		"standalone", "http://x:18787",
	)
	view.SetSize(80, 24)

	// Move to the second theme, then apply it.
	view.Update(tea.KeyMsg{Type: tea.KeyDown})
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if applied != "two" {
		test.Fatalf("applied theme = %q, want two", applied)
	}
	if cmd == nil {
		test.Fatal("applying a theme should emit a command (ThemeChangedMsg)")
	}
	if msg, ok := cmd().(ThemeChangedMsg); !ok || msg.Name != "two" {
		test.Fatalf("expected ThemeChangedMsg{two}, got %#v", cmd())
	}
}

// TestSettingsShowsPlatformInfo: the role and gateway are rendered for reference.
func TestSettingsShowsPlatformInfo(test *testing.T) {
	view := NewSettings([]string{"one"}, func() string { return "one" },
		func(string) error { return nil }, "server", "http://lan-host:18787")
	view.SetSize(80, 24)
	rendered := view.View()
	for _, want := range []string{"server", "http://lan-host:18787", "Theme", "Platform"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("settings view missing %q:\n%s", want, rendered)
		}
	}
}
