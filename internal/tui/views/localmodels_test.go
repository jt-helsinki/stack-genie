package views

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/hf"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
)

// noTest is a tester stub for tests that don't exercise testing.
func noTest(model string) (litellm.TestResult, error) {
	return litellm.TestResult{Model: model, OK: true}, nil
}

// drive runs a command synchronously through the view's Update.
func drive(view *LocalModels, cmd tea.Cmd) {
	if cmd != nil {
		_ = view.Update(cmd())
	}
}

// renderedHeight is the number of lines a rendered view occupies (shared by the
// fixed-height layout tests).
func renderedHeight(rendered string) int {
	return strings.Count(rendered, "\n") + 1
}

// buildLocal builds + loads a LocalModels view from the given installed cache +
// curated list.
func buildLocal(test *testing.T, installed []hf.CachedModel, curated []hf.CuratedModel) *LocalModels {
	test.Helper()
	view := NewLocalModels(
		func() ([]hf.CachedModel, error) { return installed, nil },
		func() []hf.CuratedModel { return curated },
		noTest,
	)
	view.SetSize(120, 40)
	drive(view, view.Init())
	return view
}

// A downloaded repo lands in Installed; a curated-but-not-installed repo lands in
// Available. A curated repo that IS installed appears only in Installed.
func TestLocalModelsTwoSectionGrouping(test *testing.T) {
	view := buildLocal(test,
		[]hf.CachedModel{{Repo: "mlx-community/Qwen2.5-7B-Instruct-4bit", Size: "4.3 GB"}},
		[]hf.CuratedModel{
			{Name: "Qwen2.5-7B-Instruct-4bit", Repo: "mlx-community/Qwen2.5-7B-Instruct-4bit", Description: "Qwen"},
			{Name: "Llama-3.2-3B-Instruct-4bit", Repo: "mlx-community/Llama-3.2-3B-Instruct-4bit", Description: "Llama"},
		},
	)
	if len(view.models) != 2 {
		test.Fatalf("models = %d, want 2 (one installed, one available)", len(view.models))
	}
	if view.installedCount != 1 {
		test.Fatalf("installedCount = %d, want 1", view.installedCount)
	}
	if !view.models[0].installed || view.models[0].repo != "mlx-community/Qwen2.5-7B-Instruct-4bit" {
		test.Fatalf("first row = %+v, want the installed Qwen", view.models[0])
	}
	// The installed curated repo picked up its curated description.
	if view.models[0].description != "Qwen" {
		test.Errorf("installed row description = %q, want the curated one", view.models[0].description)
	}
	if view.models[1].installed || view.models[1].repo != "mlx-community/Llama-3.2-3B-Instruct-4bit" {
		test.Fatalf("second row = %+v, want the available Llama", view.models[1])
	}
}

// enter/p on an Available row emits a pull request for that repo.
func TestLocalModelsPullRequest(test *testing.T) {
	view := buildLocal(test, nil,
		[]hf.CuratedModel{{Name: "Llama-3.2-3B-Instruct-4bit", Repo: "mlx-community/Llama-3.2-3B-Instruct-4bit"}},
	)
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		test.Fatal("enter on an available row must emit a pull command")
	}
	msg, ok := cmd().(ModelsPullRequestedMsg)
	if !ok {
		test.Fatalf("want ModelsPullRequestedMsg, got %#v", cmd())
	}
	if len(msg.Refs) != 1 || msg.Refs[0] != "mlx-community/Llama-3.2-3B-Instruct-4bit" {
		test.Fatalf("pull refs = %v, want the repo id", msg.Refs)
	}
}

// d on an Installed row emits a remove request for that repo.
func TestLocalModelsRemoveRequest(test *testing.T) {
	view := buildLocal(test,
		[]hf.CachedModel{{Repo: "mlx-community/Qwen2.5-7B-Instruct-4bit"}},
		nil,
	)
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if cmd == nil {
		test.Fatal("d on an installed row must emit a remove command")
	}
	msg, ok := cmd().(ModelRemoveRequestedMsg)
	if !ok {
		test.Fatalf("want ModelRemoveRequestedMsg, got %#v", cmd())
	}
	if msg.Name != "mlx-community/Qwen2.5-7B-Instruct-4bit" {
		test.Fatalf("remove name = %q, want the repo id", msg.Name)
	}
}

// t on an Installed row tests the vllm/<alias> gateway handle (alias = repo base name).
func TestLocalModelsTestHandle(test *testing.T) {
	tested := ""
	view := NewLocalModels(
		func() ([]hf.CachedModel, error) {
			return []hf.CachedModel{{Repo: "mlx-community/Qwen2.5-7B-Instruct-4bit"}}, nil
		},
		func() []hf.CuratedModel { return nil },
		func(model string) (litellm.TestResult, error) {
			tested = model
			return litellm.TestResult{Model: model, OK: true}, nil
		},
	)
	view.SetSize(120, 40)
	drive(view, view.Init())
	drive(view, view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")}))
	if tested != "vllm/Qwen2.5-7B-Instruct-4bit" {
		test.Fatalf("tested handle = %q, want vllm/Qwen2.5-7B-Instruct-4bit", tested)
	}
}

// A d on an Available (not-installed) row does not emit a remove — it flashes a hint.
func TestLocalModelsRemoveOnAvailableNoOp(test *testing.T) {
	view := buildLocal(test, nil,
		[]hf.CuratedModel{{Name: "Llama-3.2-3B-Instruct-4bit", Repo: "mlx-community/Llama-3.2-3B-Instruct-4bit"}},
	)
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")}); cmd != nil {
		test.Fatalf("d on an available row must not emit a command, got %#v", cmd())
	}
}

// The content line renders the repo id and its description within the pane width.
func TestLocalModelsContentLine(test *testing.T) {
	view := buildLocal(test, nil,
		[]hf.CuratedModel{{Name: "Foo", Repo: "org/Foo", Description: "a foo model", Size: "1 GB"}},
	)
	line := view.contentLine(view.models[0])
	if !strings.Contains(line, "org/Foo") {
		test.Fatalf("content line missing repo id: %q", line)
	}
}
