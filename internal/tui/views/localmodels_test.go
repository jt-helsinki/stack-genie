package views

import (
	"errors"
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

// drive runs a command synchronously through the view's Update, unwrapping a
// tea.Batch (Init now batches the store list + the whoami probe) so each batched
// command's message is delivered.
func drive(view *LocalModels, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, batched := range msg {
			drive(view, batched)
		}
	default:
		_ = view.Update(msg)
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
		func() (string, error) { return "", nil },
		nil,
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
		func() (string, error) { return "", nil },
		nil,
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

// e on an enabled installed row emits a disable request; on a disabled installed row
// it emits an enable request instead — the toggle mirrors the model's current
// recorded state (via the injected ModelDisabledFn), and the disabled row also
// renders a "[disabled]" marker.
func TestLocalModelsToggleEnableDisable(test *testing.T) {
	repo := "mlx-community/Qwen2.5-7B-Instruct-4bit"
	disabled := false
	view := NewLocalModels(
		func() ([]hf.CachedModel, error) { return []hf.CachedModel{{Repo: repo}}, nil },
		func() []hf.CuratedModel { return nil },
		noTest,
		func() (string, error) { return "", nil },
		func(candidate string) bool { return candidate == repo && disabled },
	)
	view.SetSize(120, 40)
	drive(view, view.Init())

	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if cmd == nil {
		test.Fatal("e on an enabled installed row must emit a request")
	}
	disableMsg, ok := cmd().(ModelDisableRequestedMsg)
	if !ok || disableMsg.Name != repo {
		test.Fatalf("got %#v, want ModelDisableRequestedMsg{%q}", cmd(), repo)
	}
	if strings.Contains(view.View(), "[disabled]") {
		test.Errorf("an enabled row must not show [disabled]:\n%s", view.View())
	}

	// Simulate the model now being disabled (as the parent's list refresh would
	// reflect after `ai models disable` runs) and re-init to rebuild the rows.
	disabled = true
	drive(view, view.Init())
	if !strings.Contains(view.View(), "[disabled]") {
		test.Errorf("a disabled row should show [disabled]:\n%s", view.View())
	}
	cmd = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if cmd == nil {
		test.Fatal("e on a disabled installed row must emit a request")
	}
	enableMsg, ok := cmd().(ModelEnableRequestedMsg)
	if !ok || enableMsg.Name != repo {
		test.Fatalf("got %#v, want ModelEnableRequestedMsg{%q}", cmd(), repo)
	}
}

// e on an Available (not-installed) row does not emit a request — it flashes a hint.
func TestLocalModelsToggleOnAvailableNoOp(test *testing.T) {
	view := buildLocal(test, nil,
		[]hf.CuratedModel{{Name: "Llama-3.2-3B-Instruct-4bit", Repo: "mlx-community/Llama-3.2-3B-Instruct-4bit"}},
	)
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")}); cmd != nil {
		test.Fatalf("e on an available row must not emit a command, got %#v", cmd())
	}
}

// l emits a login request; o emits a logout request.
func TestLocalModelsLoginRequest(test *testing.T) {
	view := buildLocal(test, nil,
		[]hf.CuratedModel{{Name: "Llama", Repo: "meta-llama/Llama-3.2-3B-Instruct"}},
	)
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if cmd == nil {
		test.Fatal("l must emit a login command")
	}
	if _, ok := cmd().(ModelsLoginRequestedMsg); !ok {
		test.Fatalf("want ModelsLoginRequestedMsg, got %#v", cmd())
	}
}

func TestLocalModelsLogoutRequest(test *testing.T) {
	view := buildLocal(test, nil,
		[]hf.CuratedModel{{Name: "Llama", Repo: "meta-llama/Llama-3.2-3B-Instruct"}},
	)
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if cmd == nil {
		test.Fatal("o must emit a logout command")
	}
	if _, ok := cmd().(ModelsLogoutRequestedMsg); !ok {
		test.Fatalf("want ModelsLogoutRequestedMsg, got %#v", cmd())
	}
}

// The whoami line shows the logged-in user when whoami succeeds, else a not-logged-in
// fallback. A whoami error never breaks the view.
func TestLocalModelsWhoamiLine(test *testing.T) {
	loggedIn := NewLocalModels(
		func() ([]hf.CachedModel, error) { return nil, nil },
		func() []hf.CuratedModel { return nil },
		noTest,
		func() (string, error) { return "alice", nil },
		nil,
	)
	loggedIn.SetSize(120, 40)
	drive(loggedIn, loggedIn.Init())
	if got := loggedIn.View(); !strings.Contains(got, "logged in as alice") {
		test.Fatalf("logged-in view missing user, got:\n%s", got)
	}

	loggedOut := NewLocalModels(
		func() ([]hf.CachedModel, error) { return nil, nil },
		func() []hf.CuratedModel { return nil },
		noTest,
		func() (string, error) { return "", errors.New("Not logged in") },
		nil,
	)
	loggedOut.SetSize(120, 40)
	drive(loggedOut, loggedOut.Init())
	if got := loggedOut.View(); !strings.Contains(got, "not logged in") {
		test.Fatalf("logged-out view missing fallback, got:\n%s", got)
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

// n opens the inline "pull by name" prompt (focusing the input); typing an arbitrary
// Hugging Face repo id then enter emits a ModelsPullRequestedMsg for exactly that repo.
func TestLocalModelsPullByNamePrompt(test *testing.T) {
	view := buildLocal(test, nil, nil)
	if view.CapturingInput() {
		test.Fatal("the pull-by-name prompt must start closed")
	}
	_ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if !view.CapturingInput() {
		test.Fatal("n must open the pull-by-name prompt (CapturingInput)")
	}
	if !view.pullInput.Focused() {
		test.Fatal("opening the prompt must focus the input")
	}
	repo := "some-org/My-Model.v2-4bit"
	_ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(repo)})
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		test.Fatal("enter must emit a pull command for the typed repo")
	}
	msg, ok := cmd().(ModelsPullRequestedMsg)
	if !ok {
		test.Fatalf("want ModelsPullRequestedMsg, got %#v", cmd())
	}
	if len(msg.Refs) != 1 || msg.Refs[0] != repo {
		test.Fatalf("pull refs = %v, want [%q]", msg.Refs, repo)
	}
	if view.CapturingInput() {
		test.Fatal("the prompt must close after submit")
	}
}

// esc cancels the pull-by-name prompt: it closes with no pull command emitted.
func TestLocalModelsPullByNameCancel(test *testing.T) {
	view := buildLocal(test, nil, nil)
	_ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	_ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("foo/bar")})
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
		test.Fatalf("esc must emit no command, got %#v", cmd())
	}
	if view.CapturingInput() {
		test.Fatal("esc must close the prompt")
	}
}

// An empty submit closes the prompt without emitting a pull.
func TestLocalModelsPullByNameEmpty(test *testing.T) {
	view := buildLocal(test, nil, nil)
	_ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		test.Fatalf("empty submit must emit nothing, got %#v", cmd())
	}
	if view.CapturingInput() {
		test.Fatal("empty submit must close the prompt")
	}
}
