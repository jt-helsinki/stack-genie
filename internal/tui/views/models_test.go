package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
)

// noLocalModels is a lister stub for tests that don't exercise the local store.
func noLocalModels() ([]ollama.Model, error) { return nil, nil }

// refresh drives the view's gateway-status refresh synchronously: it dispatches a
// modelsRefreshedMsg via the fetch func (Init now batches refresh+list, so the old
// view.Update(view.Init()()) no longer yields a single message).
func refreshModels(view *Models) {
	_ = view.Update(view.refreshCmd()())
}

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
		noLocalModels,
	)

	refreshModels(view)

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
		noLocalModels,
	)
	refreshModels(view)

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
		noLocalModels,
	)
	refreshModels(view)

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
		noLocalModels,
	)
	refreshModels(view)

	done := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})()
	_ = view.Update(done)
	if !strings.Contains(view.View(), "invalid key") {
		test.Errorf("expected a failure flash, got view:\n%s", view.View())
	}
}

func TestModelsLocalListMergesAndPulls(test *testing.T) {
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true, Default: "gemma4"}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		func() ([]ollama.Model, error) {
			return []ollama.Model{{Name: "gemma4:31b", Size: 1610612736, ParameterSize: "31B"}}, nil
		},
	)
	refreshModels(view)
	_ = view.Update(view.listCmd()()) // dispatch the local-store list

	rendered := view.View()
	for _, want := range []string{"Local model store", "installed", "gemma4:31b"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("local list missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "available") {
		test.Errorf("local list should be installed-only now (no \"available\"):\n%s", rendered)
	}

	// "p" requests an interactive pull (handled by the parent via ExecProcess).
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if cmd == nil {
		test.Fatal("pressing p must return a pull-request command")
	}
	if _, ok := cmd().(ModelPullRequestedMsg); !ok {
		test.Fatalf("p must emit ModelPullRequestedMsg, got %T", cmd())
	}
}

func TestModelsRemoveSelectedInstalled(test *testing.T) {
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		func() ([]ollama.Model, error) {
			return []ollama.Model{{Name: "llama3.2:3b", Size: 100, ParameterSize: "3B"}}, nil
		},
	)
	refreshModels(view)
	_ = view.Update(view.listCmd()())

	// The cursor starts on the first row (installed, sorted first); "d" requests its
	// removal.
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if cmd == nil {
		test.Fatal("pressing d on an installed model must return a remove-request command")
	}
	msg, ok := cmd().(ModelRemoveRequestedMsg)
	if !ok {
		test.Fatalf("d must emit ModelRemoveRequestedMsg, got %T", cmd())
	}
	if msg.Name != "llama3.2:3b" {
		test.Fatalf("remove requested for %q, want llama3.2:3b", msg.Name)
	}
}

func TestModelsRemoveWithNoModelsIsNoOp(test *testing.T) {
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		noLocalModels, // nothing installed → no rows to remove
	)
	refreshModels(view)
	_ = view.Update(view.listCmd()())

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if cmd != nil {
		test.Fatalf("d with no installed model must be a no-op, got %T", cmd())
	}
}
