package views

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/config"
)

// noMutator is a no-op egress mutator for tests that do not exercise add/remove.
func noMutator(string, string) error { return nil }

func TestNetworkPopulatesOnRefresh(test *testing.T) {
	network := config.NetworkConfig{
		Egress:      "public",
		AllowDomain: []string{"registry.npmjs.org"},
		DenyDomain:  []string{"evil.example.com"},
	}
	view := NewNetwork(
		func() (string, bool) { return "/proj", true },
		func() string { return "proj" },
		func(string) (config.NetworkConfig, error) { return network, nil },
		func(string, string) error { return nil },
		noMutator, noMutator,
	)

	_ = view.Update(view.Init()())

	if !view.loaded {
		test.Fatal("view should be loaded after a refresh")
	}
	rendered := view.View()
	if !strings.Contains(rendered, "public") {
		test.Error("rendered view should show the egress mode")
	}
	if !strings.Contains(rendered, "registry.npmjs.org") {
		test.Error("rendered view should show the allow-list")
	}
	if !strings.Contains(rendered, "evil.example.com") {
		test.Error("rendered view should show the deny-list")
	}
}

func TestNetworkNoProjectSelected(test *testing.T) {
	view := NewNetwork(
		func() (string, bool) { return "", false },
		func() string { return "" },
		func(string) (config.NetworkConfig, error) { return config.NetworkConfig{}, nil },
		func(string, string) error { return nil },
		noMutator, noMutator,
	)
	if cmd := view.Init(); cmd != nil {
		test.Error("Init with no project selected must be a no-op")
	}
	if !strings.Contains(view.View(), "no workspace selected") {
		test.Error("with no project, the view should say so")
	}
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")}); cmd != nil {
		test.Error("the toggle key must be inert with no project selected")
	}
}

// Pressing `i` flips the egress mode between "deny" (off) and "public" (on) directly
// — there is no persistent sandbox to restart, so the change applies the next time a
// nono profile is generated (the next `ai agent`/`ai shell`/`ai start`).
func TestNetworkToggleInternet(test *testing.T) {
	var calls [][2]string
	network := config.NetworkConfig{Egress: "public"}
	view := NewNetwork(
		func() (string, bool) { return "/proj", true },
		func() string { return "proj" },
		func(string) (config.NetworkConfig, error) { return network, nil },
		func(root, mode string) error { calls = append(calls, [2]string{root, mode}); return nil },
		noMutator, noMutator,
	)
	_ = view.Update(view.Init()())

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	if cmd == nil {
		test.Fatal("pressing i must return a toggle command")
	}
	done := cmd()
	if len(calls) != 1 || calls[0] != [2]string{"/proj", "deny"} {
		test.Fatalf("setter calls = %v, want [{/proj deny}] (public -> off)", calls)
	}

	refreshCmd := view.Update(done)
	if refreshCmd == nil {
		test.Fatal("the toggle result must return a refresh command")
	}
	if !strings.Contains(view.View(), "egress set to deny") {
		test.Errorf("expected a success flash, got view:\n%s", view.View())
	}
}

// Pressing `a` opens the allow prompt; typing a target + enter calls the allow
// mutator with the raw value, then refreshes.
func TestNetworkAllowPromptApplies(test *testing.T) {
	var got [2]string
	view := NewNetwork(
		func() (string, bool) { return "/proj", true },
		func() string { return "proj" },
		func(string) (config.NetworkConfig, error) { return config.NetworkConfig{Egress: "public"}, nil },
		func(string, string) error { return nil },
		func(root, raw string) error { got = [2]string{root, raw}; return nil },
		noMutator,
	)
	_ = view.Update(view.Init()())

	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}); cmd != nil {
		test.Fatal("pressing a should open the prompt, not run a command yet")
	}
	if view.inputMode != "allow" {
		test.Fatalf("a must enter allow input mode, got %q", view.inputMode)
	}
	view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("api.example.com")})
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		test.Fatal("enter must run the allow mutator")
	}
	_ = cmd()
	if got != [2]string{"/proj", "api.example.com"} {
		test.Fatalf("allow mutator called with %v, want [/proj api.example.com]", got)
	}
	if view.inputMode != "" {
		test.Fatal("input mode must close after enter")
	}
}
