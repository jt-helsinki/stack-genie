package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
)

// noShow is a Show fetcher stub for tests that don't open the describe pane.
func noShow(string) (ollama.ModelInfo, error) { return ollama.ModelInfo{}, nil }

// noTest is a tester stub for tests that don't exercise testing.
func noTest(model string) (litellm.TestResult, error) {
	return litellm.TestResult{Model: model, OK: true}, nil
}

// freshLibrary returns a library lister stub serving the given models, fresh.
func freshLibrary(models []ollama.LibraryModel) LibraryLister {
	return func() ([]ollama.LibraryModel, ollama.Source, error) {
		return models, ollama.SourceFresh, nil
	}
}

// drive runs a command synchronously through the view's Update.
func drive(view *LocalModels, cmd tea.Cmd) {
	if cmd != nil {
		_ = view.Update(cmd())
	}
}

// buildLocal builds + loads a LocalModels view from the given installed + library.
func buildLocal(test *testing.T, installed []ollama.Model, library []ollama.LibraryModel, show ModelShowFetcher) *LocalModels {
	test.Helper()
	view := NewLocalModels(
		func() ([]ollama.Model, error) { return installed, nil },
		freshLibrary(library),
		show,
		noTest,
	)
	view.SetSize(120, 40)
	drive(view, view.Init())
	return view
}

// A library model with ≥1 installed tag lands in the Installed section; one with no
// installed tag lands in Installable.
func TestLocalModelsTwoSectionGrouping(test *testing.T) {
	view := buildLocal(test,
		[]ollama.Model{{Name: "qwen2.5:7b", Size: 4700000000, ParameterSize: "7.6B"}},
		[]ollama.LibraryModel{
			{Name: "qwen2.5", Description: "Qwen 2.5", Tags: []string{"7b", "72b"}, RepoURL: "https://ollama.com/library/qwen2.5"},
			{Name: "llama3.2", Description: "Llama 3.2", Tags: []string{"1b", "3b"}, RepoURL: "https://ollama.com/library/llama3.2"},
		},
		noShow,
	)

	if len(view.models) != 2 {
		test.Fatalf("models = %d, want 2", len(view.models))
	}
	// qwen2.5 has an installed tag → Installed (first); llama3.2 → Installable.
	if view.models[0].name != "qwen2.5" || !view.models[0].anyInstalled() {
		test.Fatalf("row 0 should be installed qwen2.5, got %+v", view.models[0])
	}
	if view.models[1].name != "llama3.2" || view.models[1].anyInstalled() {
		test.Fatalf("row 1 should be installable llama3.2, got %+v", view.models[1])
	}
	rendered := view.View()
	for _, want := range []string{"Installed", "Installable", "qwen2.5", "llama3.2", "Qwen 2.5"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("rendered view missing %q:\n%s", want, rendered)
		}
	}
}

// An installed custom model not in the library is synthesized into the Installed
// section.
func TestLocalModelsSynthesizesInstalledCustom(test *testing.T) {
	view := buildLocal(test,
		[]ollama.Model{{Name: "mycustom:latest", Size: 100, ParameterSize: "1B"}},
		[]ollama.LibraryModel{{Name: "llama3.2", Tags: []string{"1b"}, RepoURL: "x"}},
		noShow,
	)
	var found bool
	for _, model := range view.models {
		if model.name == "mycustom" && model.anyInstalled() {
			found = true
		}
	}
	if !found {
		test.Fatalf("installed custom mycustom should be synthesized as an Installed row, got %+v", view.models)
	}
}

// enter opens the tag drill-down; space selects a not-installed tag; enter pulls the
// selected tags as name:tag refs.
func TestLocalModelsDrillSelectsAndPulls(test *testing.T) {
	view := buildLocal(test,
		nil,
		[]ollama.LibraryModel{{Name: "qwen2.5", Tags: []string{"7b", "72b"}, RepoURL: "x"}},
		noShow,
	)
	// enter the drill-down on the (only) model.
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if view.drill == nil {
		test.Fatal("enter should open the tag drill-down")
	}
	if !strings.Contains(view.View(), "7b") || !strings.Contains(view.View(), "72b") {
		test.Errorf("drill-down should list the tags:\n%s", view.View())
	}
	// space selects the cursor tag (7b); move down + space selects 72b.
	_ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	_ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
	_ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	// enter pulls.
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		test.Fatal("enter with selected tags must emit a pull command")
	}
	msg, ok := cmd().(ModelsPullRequestedMsg)
	if !ok {
		test.Fatalf("expected ModelsPullRequestedMsg, got %T", cmd())
	}
	want := []string{"qwen2.5:7b", "qwen2.5:72b"}
	if strings.Join(msg.Refs, ",") != strings.Join(want, ",") {
		test.Fatalf("pull refs = %v, want %v", msg.Refs, want)
	}
	if view.drill != nil {
		test.Error("drill-down should close after a pull")
	}
}

// enter in the drill with no tags selected flashes the select-prompt.
func TestLocalModelsDrillNoSelectionFlashes(test *testing.T) {
	view := buildLocal(test,
		nil,
		[]ollama.LibraryModel{{Name: "qwen2.5", Tags: []string{"7b"}, RepoURL: "x"}},
		noShow,
	)
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter}) // open drill (cursor on not-installed 7b)
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		test.Fatalf("enter with nothing selected (cursor on not-installed) must be a no-op, got %T", cmd())
	}
	if !strings.Contains(view.View(), "select 1+ tags") {
		test.Errorf("expected a select-tags flash:\n%s", view.View())
	}
}

// d in the drill removes the cursor tag when installed.
func TestLocalModelsDrillRemovesInstalledTag(test *testing.T) {
	view := buildLocal(test,
		[]ollama.Model{{Name: "qwen2.5:7b", Size: 100, ParameterSize: "7B"}},
		[]ollama.LibraryModel{{Name: "qwen2.5", Tags: []string{"7b", "72b"}, RepoURL: "x"}},
		noShow,
	)
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter}) // drill on qwen2.5 (cursor on 7b, installed)
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if cmd == nil {
		test.Fatal("d on an installed tag must emit a remove command")
	}
	msg, ok := cmd().(ModelRemoveRequestedMsg)
	if !ok || msg.Name != "qwen2.5:7b" {
		test.Fatalf("d must remove qwen2.5:7b, got %+v (ok=%v)", msg, ok)
	}
}

// t in the drill tests the cursor tag when installed.
func TestLocalModelsDrillTestsInstalledTag(test *testing.T) {
	var tested string
	view := NewLocalModels(
		func() ([]ollama.Model, error) {
			return []ollama.Model{{Name: "qwen2.5:7b", Size: 100, ParameterSize: "7B"}}, nil
		},
		freshLibrary([]ollama.LibraryModel{{Name: "qwen2.5", Tags: []string{"7b"}, RepoURL: "x"}}),
		noShow,
		func(model string) (litellm.TestResult, error) {
			tested = model
			return litellm.TestResult{Model: model, OK: true, LatencyMS: 12}, nil
		},
	)
	view.SetSize(120, 40)
	drive(view, view.Init())

	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter}) // drill (cursor on installed 7b)
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if cmd == nil {
		test.Fatal("t on an installed tag must emit a test command")
	}
	done := cmd()
	if tested != "qwen2.5:7b" {
		test.Fatalf("tested = %q, want qwen2.5:7b", tested)
	}
	_ = view.Update(done)
	if !strings.Contains(view.View(), "reachable") {
		test.Errorf("expected a test-success flash:\n%s", view.View())
	}
}

// esc backs out of the drill to the list.
func TestLocalModelsDrillEscBacksOut(test *testing.T) {
	view := buildLocal(test, nil,
		[]ollama.LibraryModel{{Name: "qwen2.5", Tags: []string{"7b"}, RepoURL: "x"}},
		noShow)
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if view.drill == nil {
		test.Fatal("enter should open the drill")
	}
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if view.drill != nil {
		test.Fatal("esc should close the drill")
	}
}

// A live-fetch failure WITH a cached copy shows the cached-copy warning.
func TestLocalModelsSourceCachedFlash(test *testing.T) {
	view := NewLocalModels(
		func() ([]ollama.Model, error) { return nil, nil },
		func() ([]ollama.LibraryModel, ollama.Source, error) {
			return []ollama.LibraryModel{{Name: "qwen2.5", Tags: []string{"7b"}, RepoURL: "x"}},
				ollama.SourceCached, errors.New("dns failure")
		},
		noShow, noTest,
	)
	view.SetSize(120, 40)
	drive(view, view.Init())
	if !strings.Contains(view.View(), "showing the cached copy") {
		test.Errorf("expected the cached-copy warning:\n%s", view.View())
	}
}

// A live-fetch failure WITH NO cached copy shows the no-cache warning.
func TestLocalModelsSourceNoCacheFlash(test *testing.T) {
	view := NewLocalModels(
		func() ([]ollama.Model, error) { return nil, nil },
		func() ([]ollama.LibraryModel, ollama.Source, error) {
			return nil, ollama.SourceCached, errors.New("dns failure")
		},
		noShow, noTest,
	)
	view.SetSize(120, 40)
	drive(view, view.Init())
	if !strings.Contains(view.View(), "no cached copy") {
		test.Errorf("expected the no-cache warning:\n%s", view.View())
	}
}

// enter on an installed tag with no selection opens the /api/show describe pane.
func TestLocalModelsDrillEnterInstalledShowsDetail(test *testing.T) {
	var shown string
	view := buildLocal(test,
		[]ollama.Model{{Name: "qwen2.5:7b", Size: 100, ParameterSize: "7B"}},
		[]ollama.LibraryModel{{Name: "qwen2.5", Tags: []string{"7b"}, RepoURL: "x"}},
		func(ref string) (ollama.ModelInfo, error) {
			shown = ref
			return ollama.ModelInfo{Name: ref, Family: "qwen", ParameterSize: "7.6B"}, nil
		},
	)
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter}) // drill (cursor on installed 7b)
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter}) // no selection → describe
	if shown != "qwen2.5:7b" {
		test.Fatalf("Show fetched %q, want qwen2.5:7b", shown)
	}
	if !view.describe.active() {
		test.Fatal("enter on an installed tag with no selection should open the describe pane")
	}
	if !strings.Contains(view.View(), "qwen") {
		test.Errorf("describe pane should show the /api/show detail:\n%s", view.View())
	}
}
