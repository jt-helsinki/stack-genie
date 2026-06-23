package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
)

func TestModelsPopulatesOnRefresh(test *testing.T) {
	status := litellm.StatusInfo{
		Healthy:   true,
		Default:   "ollama/gemma4:31b",
		Providers: []string{"ollama", "openai"},
		BaseURL:   "http://localhost:14000",
	}
	view := NewModels(
		func() (litellm.StatusInfo, error) { return status, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
	)

	_ = view.Update(view.Init()())

	if !view.loaded {
		test.Fatal("view should be loaded after a refresh")
	}
	rendered := view.View()
	if !strings.Contains(rendered, "ollama/gemma4:31b") {
		test.Error("rendered view should show the default model")
	}
	if !strings.Contains(rendered, "openai") {
		test.Error("rendered view should list providers")
	}
	if !strings.Contains(rendered, "http://localhost:14000") {
		test.Error("rendered view should show the base URL")
	}
}

func TestModelsSurfacesFetchError(test *testing.T) {
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{}, errors.New("gateway down") },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
	)
	_ = view.Update(view.Init()())

	if view.err == nil {
		test.Fatal("a fetch error must be recorded")
	}
	if !strings.Contains(view.View(), "gateway down") {
		test.Error("rendered view must surface the fetch error")
	}
}

func TestModelsTestActionInvokesTester(test *testing.T) {
	var tested string
	view := NewModels(
		func() (litellm.StatusInfo, error) {
			return litellm.StatusInfo{Healthy: true, Default: "claude-opus"}, nil
		},
		func(model string) (litellm.TestResult, error) {
			tested = model
			return litellm.TestResult{Model: model, OK: true, LatencyMS: 42}, nil
		},
	)
	_ = view.Update(view.Init()())

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if cmd == nil {
		test.Fatal("pressing t must return a test command")
	}
	done := cmd()
	if tested != "claude-opus" {
		test.Fatalf("tested model = %q, want claude-opus (the default)", tested)
	}
	_ = view.Update(done)
	if !strings.Contains(view.View(), "claude-opus reachable (42ms)") {
		test.Errorf("expected a success flash with latency, got view:\n%s", view.View())
	}
}

func TestModelsTestFlashesFailure(test *testing.T) {
	view := NewModels(
		func() (litellm.StatusInfo, error) {
			return litellm.StatusInfo{Default: "openai/gpt-5.5"}, nil
		},
		func(model string) (litellm.TestResult, error) {
			return litellm.TestResult{Model: model, OK: false, Status: 401, Error: "invalid key"}, nil
		},
	)
	_ = view.Update(view.Init()())

	done := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})()
	_ = view.Update(done)
	if !strings.Contains(view.View(), "invalid key") {
		test.Errorf("expected a failure flash, got view:\n%s", view.View())
	}
}
