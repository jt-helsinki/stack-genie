package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// errSave is the injected persist failure for the mouse-toggle error test.
var errSave = errors.New("save failed")

// TestSettingsAppliesThemeOnEnter: navigating to a theme and pressing enter
// applies+persists it (via the injected applier) and emits ThemeChangedMsg so the
// parent can recolor the whole UI.
func TestSettingsAppliesThemeOnEnter(test *testing.T) {
	applied := ""
	view := NewSettings(
		[]string{"one", "two", "three"},
		func() string { return applied },
		func(name string) error { applied = name; return nil },
		true, func(bool) error { return nil },
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

// TestSettingsShowsPlatformInfo: the role and gateway are rendered for reference,
// and the mouse toggle's current state shows.
func TestSettingsShowsPlatformInfo(test *testing.T) {
	view := NewSettings([]string{"one"}, func() string { return "one" },
		func(string) error { return nil }, true, func(bool) error { return nil },
		"server", "http://lan-host:18787")
	view.SetSize(80, 24)
	rendered := view.View()
	for _, want := range []string{"server", "http://lan-host:18787", "Theme", "Platform", "Mouse", "tab clicking"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("settings view missing %q:\n%s", want, rendered)
		}
	}
}

// TestSettingsTogglesMouseOnM: pressing `m` flips the mouse-capture setting,
// persists it via the injected setter, and emits MouseToggledMsg so the parent
// switches mouse reporting live. A second press toggles it back on.
func TestSettingsTogglesMouseOnM(test *testing.T) {
	persisted := []bool{}
	view := NewSettings([]string{"one"}, func() string { return "one" },
		func(string) error { return nil },
		true, func(enabled bool) error { persisted = append(persisted, enabled); return nil },
		"standalone", "http://x:18787")
	view.SetSize(80, 24)

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	if cmd == nil {
		test.Fatal("toggling the mouse should emit a command (MouseToggledMsg)")
	}
	if msg, ok := cmd().(MouseToggledMsg); !ok || msg.Enabled {
		test.Fatalf("expected MouseToggledMsg{Enabled: false}, got %#v", cmd())
	}
	if !strings.Contains(view.View(), "off") {
		test.Errorf("view should show the mouse toggle off:\n%s", view.View())
	}

	cmd = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	if cmd == nil {
		test.Fatal("re-toggling the mouse should emit a command")
	}
	if msg, ok := cmd().(MouseToggledMsg); !ok || !msg.Enabled {
		test.Fatalf("expected MouseToggledMsg{Enabled: true}, got %#v", cmd())
	}

	if len(persisted) != 2 || persisted[0] != false || persisted[1] != true {
		test.Fatalf("persisted sequence = %v, want [false true]", persisted)
	}
}

// TestSettingsMouseToggleKeepsSettingOnPersistError: a failed save leaves the
// in-view state unchanged and emits no message (the flash reports the error).
func TestSettingsMouseToggleKeepsSettingOnPersistError(test *testing.T) {
	view := NewSettings([]string{"one"}, func() string { return "one" },
		func(string) error { return nil },
		true, func(bool) error { return errSave },
		"standalone", "http://x:18787")
	view.SetSize(80, 24)

	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}}); cmd != nil {
		test.Fatalf("a failed persist must not emit MouseToggledMsg, got %#v", cmd())
	}
	if !view.mouse {
		test.Fatal("a failed persist must leave the setting unchanged (still on)")
	}
}
