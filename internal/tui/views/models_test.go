package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/catalog"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
)

// noLocalModels is a lister stub for tests that don't exercise the local store.
func noLocalModels() ([]ollama.Model, error) { return nil, nil }

// noPopular is a catalog stub for tests that don't exercise the installable list.
func noPopular() ([]ollama.PopularModel, error) { return nil, nil }

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
		noLocalModels, noPopular, noShow,
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
		noLocalModels, noPopular, noShow,
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
		noLocalModels, noPopular, noShow,
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
		noLocalModels, noPopular, noShow,
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
		noLocalModels, noPopular, noShow,
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
		noLocalModels, noPopular, noShow,
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
		noLocalModels, noPopular, noShow,
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
		noPopular, noShow,
	)
	view.SetSize(80, 30)
	refreshModels(view)
	listLocal(view)

	rendered := view.View()
	for _, want := range []string{"local (Ollama)", "NAME", "PARAMETERS", "SIZE", "STATUS", "gemma4:31b", "31B", "installed"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("local table missing %q:\n%s", want, rendered)
		}
	}

	// "p" pulls the SELECTED row's exact reference (handled by the parent via ExecProcess).
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if cmd == nil {
		test.Fatal("pressing p must return a pull-request command")
	}
	msg, ok := cmd().(ModelPullRequestedMsg)
	if !ok {
		test.Fatalf("p must emit ModelPullRequestedMsg, got %T", cmd())
	}
	if msg.Name != "gemma4:31b" {
		test.Fatalf("p must pull the selected row %q, got %q", "gemma4:31b", msg.Name)
	}
}

func TestModelsRemoveSelectedInstalled(test *testing.T) {
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		func() ([]ollama.Model, error) {
			return []ollama.Model{{Name: "llama3.2:3b", Size: 100, ParameterSize: "3B"}}, nil
		},
		noPopular, noShow,
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
		noPopular, noShow,
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
		noPopular,
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
		noPopular,
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
		noLocalModels, noPopular, noShow, // nothing installed → no rows to remove
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

// installedAndAvailable wires one installed model plus a popular catalog with a
// matching variant (deduped → installed) and a non-installed variant (→ available).
func installedAndAvailable(test *testing.T, show ModelShowFetcher) *Models {
	test.Helper()
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true, Default: "gemma4"}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		func() ([]ollama.Model, error) {
			return []ollama.Model{{Name: "qwen2.5:7b", Size: 4700000000, ParameterSize: "7.6B"}}, nil
		},
		func() ([]ollama.PopularModel, error) {
			return []ollama.PopularModel{
				{Name: "qwen2.5:7b", Parameters: "7b", DownloadSize: 4700000000, RepoURL: "https://ollama.com/library/qwen2.5"}, // installed → deduped
				{Name: "llama3.2:1b", Parameters: "1b", DownloadSize: 1300000000, RepoURL: "https://ollama.com/library/llama3.2", PullCount: "5M"},
			}, nil
		},
		show,
	)
	view.SetSize(100, 40)
	refreshModels(view)
	listLocal(view)
	return view
}

// The merged table shows installed first (with on-disk size + /api/tags params) then
// available (with download size + catalog params); STATUS reflects each.
func TestModelsMergesInstalledAndAvailable(test *testing.T) {
	view := installedAndAvailable(test, noShow)

	if len(view.models) != 2 {
		test.Fatalf("merged rows = %d, want 2 (1 installed + 1 available, the matching variant deduped)", len(view.models))
	}
	// Installed first.
	if view.models[0].name != "qwen2.5:7b" || !view.models[0].installed() {
		test.Fatalf("row 0 should be installed qwen2.5:7b, got %+v", view.models[0])
	}
	if view.models[1].name != "llama3.2:1b" || view.models[1].installed() {
		test.Fatalf("row 1 should be available llama3.2:1b, got %+v", view.models[1])
	}
	// The installed row's params come from /api/tags (7.6B), not the catalog (7b).
	if view.models[0].params != "7.6B" {
		test.Errorf("installed params = %q, want 7.6B (from /api/tags)", view.models[0].params)
	}
	// The available row's params + size come from the catalog.
	if view.models[1].params != "1b" {
		test.Errorf("available params = %q, want 1b (from the catalog)", view.models[1].params)
	}
	if view.models[1].size != 1300000000 {
		test.Errorf("available size = %d, want the catalog download size", view.models[1].size)
	}

	rendered := view.View()
	for _, want := range []string{"qwen2.5:7b", "installed", "llama3.2:1b", "available"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("merged table missing %q:\n%s", want, rendered)
		}
	}
}

// A popular variant that is also installed appears exactly once, as installed.
func TestModelsDedupesInstalledPopularVariant(test *testing.T) {
	view := installedAndAvailable(test, noShow)

	count := 0
	var found localModel
	for _, model := range view.models {
		if model.name == "qwen2.5:7b" {
			count++
			found = model
		}
	}
	if count != 1 {
		test.Fatalf("qwen2.5:7b appears %d times, want 1 (deduped to installed)", count)
	}
	if !found.installed() {
		test.Errorf("the deduped qwen2.5:7b row must be installed, got %q", found.status)
	}
}

// p on an available row emits a pull request for THAT row's exact ref.
func TestModelsPullAvailableRow(test *testing.T) {
	view := installedAndAvailable(test, noShow)

	// Move to the available row (row 1).
	_ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
	selected, ok := view.selectedModel()
	if !ok || selected.name != "llama3.2:1b" {
		test.Fatalf("selected row = %q (ok=%v), want llama3.2:1b", selected.name, ok)
	}
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	msg, ok := cmd().(ModelPullRequestedMsg)
	if !ok || msg.Name != "llama3.2:1b" {
		test.Fatalf("p must pull the selected available ref, got %+v (ok=%v)", msg, ok)
	}
}

// enter on an available row shows CATALOG detail and never calls Show().
func TestModelsEnterAvailableShowsCatalogNotShow(test *testing.T) {
	showCalled := false
	view := installedAndAvailable(test, func(string) (ollama.ModelInfo, error) {
		showCalled = true
		return ollama.ModelInfo{}, nil
	})

	_ = view.Update(tea.KeyMsg{Type: tea.KeyDown}) // to the available row
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if showCalled {
		test.Fatal("enter on an available row must NOT call /api/show")
	}
	if !view.describe.active() {
		test.Fatal("enter should open the describe pane")
	}
	rendered := view.View()
	for _, want := range []string{"llama3.2:1b", "not installed", "press p to pull", "catalog", "1b", "ollama.com/library/llama3.2"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("available describe pane missing %q:\n%s", want, rendered)
		}
	}
}

// enter on an installed row DOES call Show() (the full /api/show detail path).
func TestModelsEnterInstalledCallsShow(test *testing.T) {
	var shown string
	view := installedAndAvailable(test, func(name string) (ollama.ModelInfo, error) {
		shown = name
		return ollama.ModelInfo{Name: name, Family: "qwen"}, nil
	})

	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter}) // row 0 is installed
	if shown != "qwen2.5:7b" {
		test.Fatalf("enter on installed should Show %q, got %q", "qwen2.5:7b", shown)
	}
	if !strings.Contains(view.View(), "qwen") {
		test.Errorf("installed describe pane should show the /api/show detail:\n%s", view.View())
	}
}

// d on an available row is a no-op (flash only), and never a remove request.
func TestModelsRemoveAvailableIsNoOp(test *testing.T) {
	view := installedAndAvailable(test, noShow)

	_ = view.Update(tea.KeyMsg{Type: tea.KeyDown}) // to the available row
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if cmd != nil {
		test.Fatalf("d on an available row must be a no-op, got %T", cmd())
	}
	if !strings.Contains(view.View(), "not installed") {
		test.Errorf("expected a 'not installed' flash, got:\n%s", view.View())
	}
}

// When Popular() fails, the view degrades to installed-only (no available rows).
func TestModelsDegradesToInstalledOnlyWhenPopularFails(test *testing.T) {
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		func() ([]ollama.Model, error) {
			return []ollama.Model{{Name: "gemma4:31b", Size: 100, ParameterSize: "31B"}}, nil
		},
		func() ([]ollama.PopularModel, error) { return nil, errors.New("snapshot broken") },
		noShow,
	)
	view.SetSize(80, 30)
	refreshModels(view)
	listLocal(view)

	if len(view.models) != 1 || !view.models[0].installed() {
		test.Fatalf("expected installed-only degrade, got %+v", view.models)
	}
}

// When Ollama is unreachable (List fails) the offline catalog still renders as
// available rows, with an unreachable note.
func TestModelsShowsCatalogWhenOllamaUnreachable(test *testing.T) {
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		func() ([]ollama.Model, error) { return nil, errors.New("connection refused") },
		func() ([]ollama.PopularModel, error) {
			return []ollama.PopularModel{{Name: "llama3.2:1b", Parameters: "1b", DownloadSize: 1300000000, RepoURL: "x"}}, nil
		},
		noShow,
	)
	view.SetSize(80, 30)
	refreshModels(view)
	listLocal(view)

	if len(view.models) != 1 || view.models[0].installed() {
		test.Fatalf("expected the offline catalog as one available row, got %+v", view.models)
	}
	rendered := view.View()
	for _, want := range []string{"Ollama unreachable", "llama3.2:1b", "available"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("unreachable view missing %q:\n%s", want, rendered)
		}
	}
}

// --- cloud catalog (Phase E) ------------------------------------------------

// cloudTestCatalogJSON has two cloud models: an openai model and a google model,
// each with release/last-updated/limits/modalities, so the enriched table + detail
// pane can be asserted.
const cloudTestCatalogJSON = `{
	"models": {
		"openai/gpt-5.5": {
			"id": "openai/gpt-5.5", "name": "GPT-5.5", "family": "gpt",
			"release_date": "2026-01-15", "last_updated": "2026-03-01",
			"limit": {"context": 400000, "output": 128000},
			"modalities": {"input": ["text", "image"], "output": ["text"]}
		},
		"google/gemini-3.1-pro": {
			"id": "google/gemini-3.1-pro", "name": "Gemini 3.1 Pro", "family": "gemini",
			"release_date": "2026-02-20", "last_updated": "2026-04-10",
			"limit": {"context": 1000000, "output": 65000},
			"modalities": {"input": ["text", "audio"], "output": ["text"]}
		}
	},
	"providers": {
		"openai": {"id": "openai", "name": "OpenAI"},
		"google": {"id": "google", "name": "Google"}
	}
}`

func cloudTestCatalog(test *testing.T) *catalog.Catalog {
	test.Helper()
	cat, err := catalog.Parse([]byte(cloudTestCatalogJSON))
	if err != nil {
		test.Fatalf("parse cloud test catalog: %v", err)
	}
	return cat
}

// loadCatalogCloud drives the cloud-catalog load synchronously.
func loadCatalogCloud(view *Models) {
	if cmd := view.catalogCmd(); cmd != nil {
		_ = view.Update(cmd())
	}
}

// withCloud wires the catalog side onto a freshly-built models view: the cloud
// catalog, the registered (live) set, and a refresher whose call count is recorded.
func withCloud(test *testing.T, registered []litellm.LiveModel, refresh func() error) *Models {
	test.Helper()
	cat := cloudTestCatalog(test)
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true, Default: "gemma4"}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		noLocalModels, noPopular, noShow,
	).WithCatalog(
		func() (*catalog.Catalog, error) { return cat, nil },
		func() ([]litellm.LiveModel, error) { return registered, nil },
		refresh,
	)
	view.SetSize(120, 40)
	refreshModels(view)
	listLocal(view)
	loadCatalogCloud(view)
	return view
}

// The merged table renders cloud catalog rows with NAME · CONTEXT · STATUS, where a
// gateway-served (registered) model is "registered" and an unkeyed one "available".
func TestModelsRendersCloudCatalogRows(test *testing.T) {
	// openai/gpt-5.5 is served by the gateway → registered; the gemini model is not.
	view := withCloud(test,
		[]litellm.LiveModel{{Name: "openai/gpt-5.5", Provider: "openai"}},
		nil)

	if len(view.cloudModels) != 2 {
		test.Fatalf("cloud rows = %d, want 2", len(view.cloudModels))
	}
	rendered := view.View()
	for _, want := range []string{"openai/gpt-5.5", "google/gemini-3.1-pro", "registered", "available", "CONTEXT", "400K", "1M"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("cloud table missing %q:\n%s", want, rendered)
		}
	}
	// Registered sorts before available.
	if !view.cloudModels[0].registered() {
		test.Errorf("registered row should sort first, got %+v", view.cloudModels[0])
	}
}

// enter on a cloud row opens the describe pane with the models.dev metadata
// (release date, last updated, context/output limits, input/output modalities) and
// never calls /api/show.
func TestModelsEnterCloudShowsCatalogDetail(test *testing.T) {
	showCalled := false
	cat := cloudTestCatalog(test)
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		noLocalModels, noPopular,
		func(string) (ollama.ModelInfo, error) { showCalled = true; return ollama.ModelInfo{}, nil },
	).WithCatalog(
		func() (*catalog.Catalog, error) { return cat, nil },
		func() ([]litellm.LiveModel, error) { return nil, nil },
		nil,
	)
	view.SetSize(120, 40)
	refreshModels(view)
	listLocal(view)
	loadCatalogCloud(view)

	// With no Ollama rows, the first table row is a cloud model.
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if showCalled {
		test.Fatal("enter on a cloud row must NOT call /api/show")
	}
	if !view.describe.active() {
		test.Fatal("enter should open the describe pane")
	}
	rendered := view.View()
	for _, want := range []string{
		"catalog (models.dev)", "release date", "last updated",
		"context limit", "output limit", "input modalities", "output modalities",
	} {
		if !strings.Contains(rendered, want) {
			test.Errorf("cloud describe pane missing label %q:\n%s", want, rendered)
		}
	}
	// The first cloud row (alpha) is google/gemini-3.1-pro — assert ITS values.
	for _, want := range []string{"2026-02-20", "2026-04-10", "1M tokens", "65K tokens", "text, audio"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("cloud describe pane missing value %q:\n%s", want, rendered)
		}
	}
}

// enter on an INSTALLED Ollama row still shows the /api/show detail (size/params),
// proving the Ollama and cloud detail paths coexist.
func TestModelsEnterOllamaShowsSizeParamsAlongsideCloud(test *testing.T) {
	cat := cloudTestCatalog(test)
	var shown string
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		func() ([]ollama.Model, error) {
			return []ollama.Model{{Name: "llama3.2:3b", Size: 2147483648, ParameterSize: "3.2B"}}, nil
		},
		noPopular,
		func(name string) (ollama.ModelInfo, error) {
			shown = name
			return ollama.ModelInfo{Name: name, Family: "llama", ParameterSize: "3.2B"}, nil
		},
	).WithCatalog(
		func() (*catalog.Catalog, error) { return cat, nil },
		func() ([]litellm.LiveModel, error) { return nil, nil },
		nil,
	)
	view.SetSize(120, 40)
	refreshModels(view)
	listLocal(view)
	loadCatalogCloud(view)

	// Row 0 is the installed Ollama model (Ollama rows precede cloud rows).
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if shown != "llama3.2:3b" {
		test.Fatalf("enter on the installed row should Show %q, got %q", "llama3.2:3b", shown)
	}
	rendered := view.View()
	for _, want := range []string{"parameter size", "3.2B", "llama"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("ollama describe pane missing %q:\n%s", want, rendered)
		}
	}
}

// Pressing r triggers the catalog refresh + gateway resync (the injected refresher
// runs), then reloads the displayed data.
func TestModelsRefreshTriggersCatalogResync(test *testing.T) {
	resyncs := 0
	view := withCloud(test, nil, func() error { resyncs++; return nil })

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		test.Fatal("pressing r must return a resync command")
	}
	msg := cmd() // runs the injected refresher off the UI thread
	if resyncs != 1 {
		test.Fatalf("refresher called %d times, want 1", resyncs)
	}
	done, ok := msg.(catalogResyncedMsg)
	if !ok {
		test.Fatalf("r must produce a catalogResyncedMsg, got %T", msg)
	}
	if done.err != nil {
		test.Fatalf("resync should have succeeded, got %v", done.err)
	}
	// The completion message reloads the displayed data (a batch command).
	if reload := view.Update(done); reload == nil {
		test.Fatal("the resync completion should reload the displayed data")
	}
	if !strings.Contains(view.View(), "resynced") {
		test.Errorf("expected a success flash after the resync:\n%s", view.View())
	}
}

// A resync error is surfaced as a flash, not a fatal.
func TestModelsRefreshResyncErrorFlashes(test *testing.T) {
	view := withCloud(test, nil, func() error { return errors.New("gateway down") })

	msg := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})()
	_ = view.Update(msg)
	if !strings.Contains(view.View(), "gateway down") {
		test.Errorf("expected the resync error in a flash:\n%s", view.View())
	}
}

// With NO catalog side wired, r still refreshes the displayed data (no resync).
func TestModelsRefreshWithoutCatalogSideStillRefreshes(test *testing.T) {
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		noLocalModels, noPopular, noShow,
	)
	refreshModels(view)
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		test.Fatal("r should still return a data-refresh command when no catalog is wired")
	}
}

// p on a cloud row is a no-op (cloud models are keyed, not pulled).
func TestModelsPullCloudRowIsNoOp(test *testing.T) {
	view := withCloud(test, nil, nil) // no Ollama rows → row 0 is a cloud model
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if cmd != nil {
		test.Fatalf("p on a cloud row must be a no-op, got %T", cmd())
	}
	if !strings.Contains(view.View(), "cloud model") {
		test.Errorf("expected a 'cloud model' flash, got:\n%s", view.View())
	}
}
