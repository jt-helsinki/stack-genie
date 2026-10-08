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
		Egress:            "public",
		AllowHostServices: []config.HostService{{Host: "registry.npmjs.org", Port: 443}},
		PublishPorts:      []config.PortMapping{{Guest: 3000, Host: 8080}},
	}
	view := NewNetwork(
		func() (string, bool) { return "/proj", true },
		func() string { return "proj" },
		func(string) (config.NetworkConfig, error) { return network, nil },
		func(string, string) error { return nil },
		noMutator, noMutator, noMutator, noMutator,
	)

	_ = view.Update(view.Init()())

	if !view.loaded {
		test.Fatal("view should be loaded after a refresh")
	}
	rendered := view.View()
	if !strings.Contains(rendered, "public") {
		test.Error("rendered view should show the egress mode")
	}
	if !strings.Contains(rendered, "registry.npmjs.org:443") {
		test.Error("rendered view should show the allow-list")
	}
	if !strings.Contains(rendered, "8080") {
		test.Error("rendered view should show published ports")
	}
}

func TestNetworkNoProjectSelected(test *testing.T) {
	view := NewNetwork(
		func() (string, bool) { return "", false },
		func() string { return "" },
		func(string) (config.NetworkConfig, error) { return config.NetworkConfig{}, nil },
		func(string, string) error { return nil },
		noMutator, noMutator, noMutator, noMutator,
	)
	if cmd := view.Init(); cmd != nil {
		test.Error("Init with no project selected must be a no-op")
	}
	if !strings.Contains(view.View(), "no project selected") {
		test.Error("with no project, the view should say so")
	}
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")}); cmd != nil {
		test.Error("the mode key must be inert with no project selected")
	}
}

func TestNetworkCycleModeInvokesSetter(test *testing.T) {
	var calls [][2]string
	network := config.NetworkConfig{Egress: "deny"}
	view := NewNetwork(
		func() (string, bool) { return "/proj", true },
		func() string { return "proj" },
		func(string) (config.NetworkConfig, error) { return network, nil },
		func(root, mode string) error { calls = append(calls, [2]string{root, mode}); return nil },
		noMutator, noMutator, noMutator, noMutator,
	)
	_ = view.Update(view.Init()())

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	if cmd == nil {
		test.Fatal("pressing m must return a mode command")
	}
	done := cmd()
	// deny → public (next in config.EgressModes).
	if len(calls) != 1 || calls[0] != [2]string{"/proj", "public"} {
		test.Fatalf("setter calls = %v, want [{/proj public}]", calls)
	}
	_ = view.Update(done)
	if !strings.Contains(view.View(), "egress set to public") {
		test.Errorf("expected a success flash, got view:\n%s", view.View())
	}
}

// Pressing `i` flips the egress mode between "deny" (off) and "public" (on), and
// once persisted, bubbles a WorkspaceNetworkToggleRequestedMsg so the parent restarts
// the running workspace (net-rules only apply at create/restart).
func TestNetworkToggleInternet(test *testing.T) {
	var calls [][2]string
	network := config.NetworkConfig{Egress: "public"}
	view := NewNetwork(
		func() (string, bool) { return "/proj", true },
		func() string { return "proj" },
		func(string) (config.NetworkConfig, error) { return network, nil },
		func(root, mode string) error { calls = append(calls, [2]string{root, mode}); return nil },
		noMutator, noMutator, noMutator, noMutator,
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

	restartCmd := view.Update(done)
	if restartCmd == nil {
		test.Fatal("the toggle result must return a command to restart the workspace")
	}
	// The view batches a refresh alongside the bubbled message (tea.Batch yields a
	// BatchMsg of sub-commands for the runtime to run individually).
	batch, ok := restartCmd().(tea.BatchMsg)
	if !ok {
		test.Fatalf("expected a tea.BatchMsg, got %T", restartCmd())
	}
	var message WorkspaceNetworkToggleRequestedMsg
	var found bool
	for _, sub := range batch {
		if result, ok := sub().(WorkspaceNetworkToggleRequestedMsg); ok {
			message, found = result, true
		}
	}
	if !found {
		test.Fatal("expected one batched command to yield WorkspaceNetworkToggleRequestedMsg")
	}
	if message.Project != "proj" || message.Mode != "deny" {
		test.Fatalf("got %+v, want {Project:proj Mode:deny}", message)
	}
	if !strings.Contains(view.View(), "internet off") {
		test.Errorf("expected an 'internet off' success flash, got view:\n%s", view.View())
	}
}

func TestNextEgressModeWraps(test *testing.T) {
	last := config.EgressModes[len(config.EgressModes)-1]
	if got := nextEgressMode(last); got != config.EgressModes[0] {
		test.Errorf("nextEgressMode(%q) = %q, want wrap to %q", last, got, config.EgressModes[0])
	}
	if got := nextEgressMode("nonsense"); got != config.EgressModes[0] {
		test.Errorf("nextEgressMode of unknown = %q, want %q", got, config.EgressModes[0])
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
		noMutator, noMutator, noMutator,
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
