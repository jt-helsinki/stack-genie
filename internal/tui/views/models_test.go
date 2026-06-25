package views

import (
	"errors"
	"strconv"
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

// The routing section renders the LIVE served-model list (with provider/mode) that
// the status fetcher carries from the gateway — not a hardcoded list.
func TestModelsRoutingShowsLiveServedModels(test *testing.T) {
	status := litellm.StatusInfo{
		Healthy:   true,
		Default:   "gemma4",
		Providers: []string{"anthropic", "ollama"},
		BaseURL:   "http://localhost:14000",
		Models: []litellm.Model{
			{Name: "gemma4", Provider: "ollama", Mode: "chat"},
			{Name: "claude-opus", Provider: "anthropic"},
		},
	}
	view := NewModels(
		func() (litellm.StatusInfo, error) { return status, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		noLocalModels,
	)
	refreshModels(view)

	rendered := view.View()
	for _, want := range []string{"served models (live)", "gemma4", "claude-opus", "ollama, chat", "(anthropic)"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("routing section missing %q:\n%s", want, rendered)
		}
	}
}

// When the gateway is reachable but the model list could not be fetched, the
// routing section shows the note instead of erroring.
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
		noLocalModels,
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

// manyLocalModels returns a lister of count installed models with predictable names
// (model-00, model-01, …) so tests can assert which window is rendered.
func manyLocalModels(count int) LocalModelLister {
	return func() ([]ollama.Model, error) {
		models := make([]ollama.Model, 0, count)
		for index := 0; index < count; index++ {
			name := "model-0" + strconv.Itoa(index)
			if index >= 10 {
				name = "model-" + strconv.Itoa(index)
			}
			models = append(models, ollama.Model{Name: name, Size: 100, ParameterSize: "1B"})
		}
		return models, nil
	}
}

// loadedScrollView builds a Models view with count local models, a fixed pane size,
// and the local list loaded — ready to drive cursor movement.
func loadedScrollView(test *testing.T, count, width, height int) *Models {
	test.Helper()
	view := NewModels(
		func() (litellm.StatusInfo, error) { return litellm.StatusInfo{Healthy: true, Default: "gemma4"}, nil },
		func(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil },
		manyLocalModels(count),
	)
	refreshModels(view)
	_ = view.Update(view.listCmd()())
	view.SetSize(width, height)
	return view
}

func pressDown(view *Models) { _ = view.Update(tea.KeyMsg{Type: tea.KeyDown}) }
func pressUp(view *Models)   { _ = view.Update(tea.KeyMsg{Type: tea.KeyUp}) }

// Moving the cursor DOWN within the visible window does NOT change the offset; only
// crossing the bottom edge advances it (by one per step).
func TestModelsScrollEdgeRule(test *testing.T) {
	view := loadedScrollView(test, 30, 80, 12)
	visible := view.visibleRows()
	if visible < 2 || visible >= 30 {
		test.Fatalf("test needs a clipped window: visibleRows=%d of 30", visible)
	}

	// Move down to the LAST visible row — offset must stay 0 the whole way.
	for step := 0; step < visible-1; step++ {
		pressDown(view)
		if view.offset != 0 {
			test.Fatalf("offset moved to %d while cursor (%d) still inside the window", view.offset, view.cursor)
		}
	}
	if view.cursor != visible-1 {
		test.Fatalf("cursor = %d, want %d (last visible row)", view.cursor, visible-1)
	}

	// One more step crosses the bottom edge: offset advances by exactly one.
	pressDown(view)
	if view.offset != 1 {
		test.Fatalf("offset = %d after crossing the bottom edge, want 1", view.offset)
	}
	pressDown(view)
	if view.offset != 2 {
		test.Fatalf("offset = %d after a second cross, want 2", view.offset)
	}
}

// Scrolling back UP past the top edge moves the offset back to the cursor.
func TestModelsScrollUpEdgeRule(test *testing.T) {
	view := loadedScrollView(test, 30, 80, 12)
	visible := view.visibleRows()
	// Drive the cursor to the bottom so the window is scrolled down.
	for step := 0; step < 29; step++ {
		pressDown(view)
	}
	if view.cursor != 29 {
		test.Fatalf("cursor = %d, want 29 (bottom)", view.cursor)
	}
	bottomOffset := view.offset
	if bottomOffset != 30-visible {
		test.Fatalf("offset = %d at bottom, want %d", bottomOffset, 30-visible)
	}
	// Move up within the window — offset unchanged until we cross the top edge.
	pressUp(view)
	if view.offset != bottomOffset {
		test.Fatalf("offset changed to %d moving up within the window", view.offset)
	}
}

// The rendered local list never exceeds visibleRows, and the cursor is always within
// the rendered window — regardless of where the cursor sits.
func TestModelsViewWindowFitsAndKeepsCursorVisible(test *testing.T) {
	view := loadedScrollView(test, 30, 80, 12)
	visible := view.visibleRows()
	for target := 0; target < 30; target++ {
		// Move the cursor to `target`.
		for view.cursor < target {
			pressDown(view)
		}
		rendered := view.View()
		// Count rendered local rows (each prefixed with the padded name "model-").
		shown := strings.Count(rendered, "model-")
		if shown > visible {
			test.Fatalf("cursor=%d: rendered %d local rows, exceeds visibleRows=%d", target, shown, visible)
		}
		// The cursor's row must appear in the rendered window.
		name := view.rows[view.cursor].name
		if !strings.Contains(rendered, name) {
			test.Fatalf("cursor=%d: row %q not visible in window (offset=%d):\n%s", target, name, view.offset, rendered)
		}
	}
}

// When the list is clipped, the view shows a "↑/↓ more" affordance.
func TestModelsViewShowsScrollAffordanceWhenClipped(test *testing.T) {
	view := loadedScrollView(test, 30, 80, 12)
	if !strings.Contains(view.View(), "more") {
		test.Errorf("clipped list should show a scroll affordance:\n%s", view.View())
	}
}

// A short list that fits the pane is not clipped and renders every row.
func TestModelsViewNoClipWhenFits(test *testing.T) {
	view := loadedScrollView(test, 3, 80, 40)
	rendered := view.View()
	if strings.Count(rendered, "model-") != 3 {
		test.Fatalf("a fitting list should render all 3 rows:\n%s", rendered)
	}
	if strings.Contains(rendered, "↑/↓ more") {
		test.Errorf("a fitting list should not show the scroll affordance:\n%s", rendered)
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
