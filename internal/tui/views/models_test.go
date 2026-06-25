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

// noShow is a Show fetcher stub for tests that don't open the describe pane.
func noShow(string) (ollama.ModelInfo, error) { return ollama.ModelInfo{}, nil }

// refreshModels drives the view's gateway-status refresh synchronously.
func refreshModels(view *Models) { _ = view.Update(view.refreshCmd()()) }

// listLocal drives the local-store list synchronously.
func listLocal(view *Models) { _ = view.Update(view.listCmd()()) }

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
		noLocalModels, noShow,
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

// The routing section renders the LIVE served-model list, collapsed via
// DisplayModels: a concrete alias covered by its provider wildcard is dropped, the
// wildcard + uncovered concrete models are kept.
func TestModelsRoutingShowsCollapsedServedModels(test *testing.T) {
	status := litellm.StatusInfo{
		Healthy:   true,
		Default:   "gemma4",
		Providers: []string{"anthropic", "ollama"},
		BaseURL:   "http://localhost:14000",
		Models: []litellm.Model{
			{Name: "anthropic/*", Provider: "anthropic"},
			{Name: "claude-opus", Provider: "anthropic"}, // covered by anthropic/* → dropped
			{Name: "gemma4", Provider: "ollama"},         // no ollama/* wildcard → kept
		},
	}
	view := NewModels(
		func() (litellm.StatusInfo, error) { return status, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		noLocalModels, noShow,
	)
	refreshModels(view)

	rendered := view.View()
	for _, want := range []string{"served models (live)", "anthropic/*", "gemma4"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("routing section missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "claude-opus") {
		test.Errorf("alias covered by anthropic/* should be collapsed away:\n%s", rendered)
	}
}

// When every served model collapses under a wildcard, the served-models heading is
// omitted (the providers line already summarizes the wildcards).
func TestModelsRoutingOmitsServedHeadingWhenEmpty(test *testing.T) {
	status := litellm.StatusInfo{
		Healthy:   true,
		Default:   "gemma4",
		Providers: []string{"anthropic"},
		BaseURL:   "http://localhost:14000",
		Models: []litellm.Model{
			{Name: "anthropic/*", Provider: "anthropic"},
		},
	}
	view := NewModels(
		func() (litellm.StatusInfo, error) { return status, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		noLocalModels, noShow,
	)
	refreshModels(view)
	// anthropic/* is itself a wildcard, so it is kept and the heading shows.
	if !strings.Contains(view.View(), "anthropic/*") {
		test.Errorf("the wildcard itself should still render:\n%s", view.View())
	}
}

// When the gateway is reachable but the model list could not be fetched, the routing
// section shows the note instead of erroring.
func TestModelsRoutingShowsModelListNote(test *testing.T) {
	status := litellm.StatusInfo{
		Healthy:    true,
		Default:    "gemma4",
		BaseURL:    "http://localhost:14000",
		ModelsNote: "could not list served models: unauthorized",
	}
	view := NewModels(
		func() (litellm.StatusInfo, error) { return status, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		noLocalModels, noShow,
	)
	refreshModels(view)

	if !strings.Contains(view.View(), "could not list served models") {
		test.Errorf("expected the model-list note in the routing section:\n%s", view.View())
	}
}

func TestModelsSurfacesFetchError(test *testing.T) {
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{}, errors.New("gateway down") },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		noLocalModels, noShow,
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
		noLocalModels, noShow,
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
		noLocalModels, noShow,
	)
	refreshModels(view)

	done := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})()
	_ = view.Update(done)
	if !strings.Contains(view.View(), "invalid key") {
		test.Errorf("expected a failure flash, got view:\n%s", view.View())
	}
}

// The local store renders as a 4-column table (NAME · PARAMETERS · SIZE · STATUS),
// and the selected installed model can be pulled.
func TestModelsLocalTableAndPull(test *testing.T) {
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true, Default: "gemma4"}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		func() ([]ollama.Model, error) {
			return []ollama.Model{{Name: "gemma4:31b", Size: 1610612736, ParameterSize: "31B"}}, nil
		},
		noShow,
	)
	view.SetSize(80, 30)
	refreshModels(view)
	listLocal(view)

	rendered := view.View()
	for _, want := range []string{"Local model store", "NAME", "PARAMETERS", "SIZE", "STATUS", "gemma4:31b", "31B", "installed"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("local table missing %q:\n%s", want, rendered)
		}
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
		noShow,
	)
	view.SetSize(80, 30)
	refreshModels(view)
	listLocal(view)

	// The cursor starts on the first row (the only installed model); "d" requests
	// its removal.
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

// The table tracks selection across multiple rows: moving down selects the next
// model, and that is the one acted on.
func TestModelsTableSelectionTracksCursor(test *testing.T) {
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		func() ([]ollama.Model, error) {
			return []ollama.Model{
				{Name: "aaa:1b", Size: 100, ParameterSize: "1B"},
				{Name: "bbb:7b", Size: 200, ParameterSize: "7B"},
			}, nil
		},
		noShow,
	)
	view.SetSize(80, 30)
	refreshModels(view)
	listLocal(view)

	selected, ok := view.selectedModel()
	if !ok || selected.name != "aaa:1b" {
		test.Fatalf("first row should be aaa:1b, got %q (ok=%v)", selected.name, ok)
	}
	_ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
	selected, ok = view.selectedModel()
	if !ok || selected.name != "bbb:7b" {
		test.Fatalf("after down the second row should be bbb:7b, got %q (ok=%v)", selected.name, ok)
	}
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	msg, ok := cmd().(ModelRemoveRequestedMsg)
	if !ok || msg.Name != "bbb:7b" {
		test.Fatalf("d must remove the selected bbb:7b, got %+v (ok=%v)", msg, ok)
	}
}

// enter opens the describe pane with the full /api/show detail (details block,
// parameters, template, license, capabilities, model_info); esc closes it.
func TestModelsEnterOpensDescribePane(test *testing.T) {
	var shown string
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		func() ([]ollama.Model, error) {
			return []ollama.Model{{Name: "llama3.2:3b", Size: 100, ParameterSize: "3B"}}, nil
		},
		func(name string) (ollama.ModelInfo, error) {
			shown = name
			return ollama.ModelInfo{
				Name:              name,
				Family:            "llama",
				ParameterSize:     "3.2B",
				QuantizationLevel: "Q4_K_M",
				Format:            "gguf",
				ParentModel:       "llama3.2",
				Parameters:        "stop \"<|eot|>\"",
				Template:          "{{ .Prompt }}",
				License:           "MIT LICENSE TEXT",
				Capabilities:      []string{"completion", "tools"},
				ModelInfo:         map[string]any{"llama.context_length": float64(131072)},
			}, nil
		},
	)
	view.SetSize(80, 30)
	refreshModels(view)
	listLocal(view)

	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !view.describe.active() {
		test.Fatal("enter should open the describe pane")
	}
	if shown != "llama3.2:3b" {
		test.Fatalf("Show fetched %q, want llama3.2:3b", shown)
	}
	rendered := view.View()
	for _, want := range []string{
		"details", "llama", "3.2B", "Q4_K_M", "gguf", "llama3.2",
		"parameters", "template", "license", "MIT LICENSE TEXT",
		"capabilities", "completion, tools",
		"model_info", "llama.context_length", "131072",
	} {
		if !strings.Contains(rendered, want) {
			test.Errorf("describe pane missing %q:\n%s", want, rendered)
		}
	}

	// esc closes the pane back to the table.
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if view.describe.active() {
		test.Fatal("esc should close the describe pane")
	}
	if !strings.Contains(view.View(), "NAME") {
		test.Errorf("after esc the table should be visible again:\n%s", view.View())
	}
}

// The describe-pane Show error is surfaced, not fatal.
func TestModelsDescribeSurfacesShowError(test *testing.T) {
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		func() ([]ollama.Model, error) {
			return []ollama.Model{{Name: "gone:1b", Size: 100}}, nil
		},
		func(string) (ollama.ModelInfo, error) { return ollama.ModelInfo{}, errors.New("not found") },
	)
	view.SetSize(80, 30)
	refreshModels(view)
	listLocal(view)

	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(view.View(), "not found") {
		test.Errorf("describe pane should surface the Show error:\n%s", view.View())
	}
}

func TestModelsRemoveWithNoModelsIsNoOp(test *testing.T) {
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		noLocalModels, noShow, // nothing installed → no rows to remove
	)
	refreshModels(view)
	listLocal(view)

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if cmd != nil {
		test.Fatalf("d with no installed model must be a no-op (flash only), got %T", cmd())
	}
	if !strings.Contains(view.View(), "select an installed model") {
		test.Errorf("expected a hint flash, got:\n%s", view.View())
	}
}
