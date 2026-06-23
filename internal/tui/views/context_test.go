package views

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/contextopt"
)

func TestContextPopulatesOnRefresh(test *testing.T) {
	status := contextopt.Status{Strategy: "balanced", CavemanLevel: "full", CavemanInstalled: true}
	view := NewContext(
		func() (string, bool) { return "/proj", true },
		func(string) (contextopt.Status, error) { return status, nil },
		func(string, string) error { return nil },
		func(string, string) error { return nil },
	)

	_ = view.Update(view.Init()())

	if !view.loaded {
		test.Fatal("view should be loaded after a refresh")
	}
	rendered := view.View()
	if !strings.Contains(rendered, "balanced") {
		test.Error("rendered view should show the strategy")
	}
	if !strings.Contains(rendered, "full") {
		test.Error("rendered view should show the caveman level")
	}
}

func TestContextNoProjectSelected(test *testing.T) {
	view := NewContext(
		func() (string, bool) { return "", false },
		func(string) (contextopt.Status, error) { return contextopt.Status{}, nil },
		func(string, string) error { return nil },
		func(string, string) error { return nil },
	)
	if cmd := view.Init(); cmd != nil {
		test.Error("Init with no project selected must be a no-op")
	}
	if !strings.Contains(view.View(), "no project selected") {
		test.Error("with no project, the view should say so")
	}
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")}); cmd != nil {
		test.Error("the strategy key must be inert with no project selected")
	}
}

func TestContextCycleStrategyInvokesSetter(test *testing.T) {
	var calls [][2]string
	status := contextopt.Status{Strategy: "conservative", CavemanLevel: "lite"}
	view := NewContext(
		func() (string, bool) { return "/proj", true },
		func(string) (contextopt.Status, error) { return status, nil },
		func(root, strategy string) error { calls = append(calls, [2]string{root, strategy}); return nil },
		func(string, string) error { return nil },
	)
	_ = view.Update(view.Init()())

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd == nil {
		test.Fatal("pressing s must return a strategy command")
	}
	done := cmd()
	// conservative → balanced (next in contextopt.Strategies).
	if len(calls) != 1 || calls[0] != [2]string{"/proj", "balanced"} {
		test.Fatalf("strategy setter calls = %v, want [{/proj balanced}]", calls)
	}
	_ = view.Update(done)
	if !strings.Contains(view.View(), "strategy set to balanced") {
		test.Errorf("expected a success flash, got view:\n%s", view.View())
	}
}

func TestContextCycleCavemanInvokesSetter(test *testing.T) {
	var calls [][2]string
	status := contextopt.Status{Strategy: "balanced", CavemanLevel: "lite"}
	view := NewContext(
		func() (string, bool) { return "/proj", true },
		func(string) (contextopt.Status, error) { return status, nil },
		func(string, string) error { return nil },
		func(root, level string) error { calls = append(calls, [2]string{root, level}); return nil },
	)
	_ = view.Update(view.Init()())

	done := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})()
	// lite → full (next in contextopt.CavemanLevels).
	if len(calls) != 1 || calls[0] != [2]string{"/proj", "full"} {
		test.Fatalf("caveman setter calls = %v, want [{/proj full}]", calls)
	}
	_ = view.Update(done)
	if !strings.Contains(view.View(), "caveman set to full") {
		test.Errorf("expected a success flash, got view:\n%s", view.View())
	}
}
