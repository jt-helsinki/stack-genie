package views

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/ollama"
	"github.com/mattn/go-runewidth"
)

// noShow is a Show fetcher stub for tests that don't open the describe pane.
func noShow(string) (ollama.ModelInfo, error) { return ollama.ModelInfo{}, nil }

// noTest is a tester stub for tests that don't exercise testing.
func noTest(model string) (litellm.TestResult, error) {
	return litellm.TestResult{Model: model, OK: true}, nil
}

// freshLibrary returns a library lister stub serving the given models, fresh.
// libTags builds library tags from short names (test convenience; the scraped
// size/context/input are exercised separately in the ollama package).
func libTags(names ...string) []ollama.LibraryTag {
	tags := make([]ollama.LibraryTag, len(names))
	for index, name := range names {
		tags[index] = ollama.LibraryTag{Name: name}
	}
	return tags
}

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
		nil, // refresh: nil → the `r` path reuses the library lister
		show,
		noTest,
		nil, // syncGway: not exercised here
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
			{Name: "qwen2.5", Description: "Qwen 2.5", Tags: libTags("7b", "72b"), RepoURL: "https://ollama.com/library/qwen2.5"},
			{Name: "llama3.2", Description: "Llama 3.2", Tags: libTags("1b", "3b"), RepoURL: "https://ollama.com/library/llama3.2"},
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
		[]ollama.LibraryModel{{Name: "llama3.2", Tags: libTags("1b"), RepoURL: "x"}},
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
		[]ollama.LibraryModel{{Name: "qwen2.5", Tags: libTags("7b", "72b"), RepoURL: "x"}},
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
	// enter opens the install-engine picker (it no longer pulls directly).
	if cmd := view.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		test.Fatalf("enter with selected tags must open the runtime picker (no cmd), got %T", cmd())
	}
	if view.runtime == nil {
		test.Fatal("enter with selected tags must open the runtime picker")
	}
	if view.drill != nil {
		test.Error("drill-down should close when the runtime picker opens")
	}
	// enter on the picker (default Ollama, cursor 0) emits the pull with the runtime.
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		test.Fatal("enter on the runtime picker must emit a pull command")
	}
	msg, ok := cmd().(ModelsPullRequestedMsg)
	if !ok {
		test.Fatalf("expected ModelsPullRequestedMsg, got %T", cmd())
	}
	want := []string{"qwen2.5:7b", "qwen2.5:72b"}
	if strings.Join(msg.Refs, ",") != strings.Join(want, ",") {
		test.Fatalf("pull refs = %v, want %v", msg.Refs, want)
	}
	if msg.Runtime != string(config.RuntimeOllama) {
		test.Fatalf("default runtime = %q, want %q", msg.Runtime, config.RuntimeOllama)
	}
	if view.runtime != nil {
		test.Error("runtime picker should close after a pull")
	}
}

// selecting Docker Model Runner in the picker threads that runtime onto the pull.
func TestLocalModelsRuntimePickerSelectsDMR(test *testing.T) {
	view := buildLocal(test,
		nil,
		[]ollama.LibraryModel{{Name: "qwen2.5", Tags: libTags("7b"), RepoURL: "x"}},
		noShow,
	)
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})                     // open drill (cursor on 7b)
	_ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")}) // tick 7b
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})                     // open runtime picker
	if view.runtime == nil {
		test.Fatal("runtime picker should be open")
	}
	// move down to Docker Model Runner (the second option), then enter.
	_ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		test.Fatal("enter on the runtime picker must emit a pull command")
	}
	msg, ok := cmd().(ModelsPullRequestedMsg)
	if !ok {
		test.Fatalf("expected ModelsPullRequestedMsg, got %T", cmd())
	}
	if msg.Runtime != string(config.RuntimeDockerModelRunner) {
		test.Fatalf("runtime = %q, want %q", msg.Runtime, config.RuntimeDockerModelRunner)
	}
}

// esc on the runtime picker restores the tag drill-down with the tick selection intact.
func TestLocalModelsRuntimePickerEscRestoresDrill(test *testing.T) {
	view := buildLocal(test,
		nil,
		[]ollama.LibraryModel{{Name: "qwen2.5", Tags: libTags("7b"), RepoURL: "x"}},
		noShow,
	)
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})                     // open drill
	_ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")}) // tick 7b
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})                     // open runtime picker
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEsc})                       // back out
	if view.runtime != nil {
		test.Fatal("esc should close the runtime picker")
	}
	if view.drill == nil {
		test.Fatal("esc should restore the tag drill-down")
	}
	if !view.drill.selected["7b"] {
		test.Error("the tick selection should survive the picker round-trip")
	}
}

// enter in the drill with no tags selected flashes the select-prompt.
func TestLocalModelsDrillNoSelectionFlashes(test *testing.T) {
	view := buildLocal(test,
		nil,
		[]ollama.LibraryModel{{Name: "qwen2.5", Tags: libTags("7b"), RepoURL: "x"}},
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
		[]ollama.LibraryModel{{Name: "qwen2.5", Tags: libTags("7b", "72b"), RepoURL: "x"}},
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
		freshLibrary([]ollama.LibraryModel{{Name: "qwen2.5", Tags: libTags("7b"), RepoURL: "x"}}),
		nil,
		noShow,
		func(model string) (litellm.TestResult, error) {
			tested = model
			return litellm.TestResult{Model: model, OK: true, LatencyMS: 12}, nil
		},
		nil, // syncGway
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
		[]ollama.LibraryModel{{Name: "qwen2.5", Tags: libTags("7b"), RepoURL: "x"}},
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
			return []ollama.LibraryModel{{Name: "qwen2.5", Tags: libTags("7b"), RepoURL: "x"}},
				ollama.SourceCached, errors.New("dns failure")
		},
		nil,
		noShow, noTest,
		nil,
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
		nil,
		noShow, noTest,
		nil,
	)
	view.SetSize(120, 40)
	drive(view, view.Init())
	if !strings.Contains(view.View(), "no cached copy") {
		test.Errorf("expected the no-cache warning:\n%s", view.View())
	}
}

// TestLocalModelsRefreshSyncsToGateway verifies the `r` refresh registers the installed
// models with the gateway (the syncer is invoked) and flashes the count, and that the
// concurrent display refresh does not clobber that flash.
func TestLocalModelsRefreshSyncsToGateway(test *testing.T) {
	var syncCalled bool
	view := NewLocalModels(
		func() ([]ollama.Model, error) {
			return []ollama.Model{{Name: "qwen3-coder:30b", Size: 100, ParameterSize: "30B"}}, nil
		},
		freshLibrary([]ollama.LibraryModel{{Name: "qwen3-coder", Tags: libTags("30b"), RepoURL: "x"}}),
		nil,
		noShow, noTest,
		func() ([]string, error) {
			syncCalled = true
			return []string{"ollama/qwen3-coder:30b"}, nil
		},
	)
	view.SetSize(120, 40)
	drive(view, view.Init())

	// `r` returns a batch: the display refresh AND the gateway sync. Run both.
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if batch, ok := cmd().(tea.BatchMsg); ok {
		for _, sub := range batch {
			if sub != nil {
				if msg := sub(); msg != nil {
					_ = view.Update(msg)
				}
			}
		}
	}
	if !syncCalled {
		test.Fatal("`r` must invoke the gateway syncer so installed models are registered")
	}
	if !strings.Contains(stripANSI(view.View()), "registered 1 local model") {
		test.Errorf("expected the register-count flash after refresh, got:\n%s", view.View())
	}
}

// renderedHeight is the number of lines a rendered block occupies (newlines + 1).
func renderedHeight(rendered string) int { return strings.Count(rendered, "\n") + 1 }

// bottomGap is the number of trailing blank (whitespace-only, ANSI-stripped) lines —
// the gap between the last visible content and the bottom of the rendered block.
func bottomGap(rendered string) int {
	lines := strings.Split(rendered, "\n")
	gap := 0
	for index := len(lines) - 1; index >= 0; index-- {
		if strings.TrimSpace(stripANSI(lines[index])) != "" {
			break
		}
		gap++
	}
	return gap
}

// The Local Models list must render a CONSTANT height regardless of scroll position,
// and must fill the content height (no growing blank gap above the bottom as you
// scroll down). This pins the windowing+padding fix: a short tail no longer shrinks
// the block, and the per-window section-header count no longer changes the row count.
func TestLocalModelsListFillsConstantHeightOnScroll(test *testing.T) {
	// Enough models that the list overflows a small pane and must scroll.
	library := make([]ollama.LibraryModel, 0, 30)
	for index := 0; index < 30; index++ {
		name := "model" + string(rune('a'+index%26)) + strconv.Itoa(index)
		library = append(library, ollama.LibraryModel{Name: name, Description: "desc", Tags: libTags("7b"), RepoURL: "x"})
	}
	view := buildLocal(test,
		[]ollama.Model{{Name: library[0].Name + ":7b", Size: 100, ParameterSize: "7B"}},
		library,
		noShow,
	)
	// A small pane forces a scroll window smaller than the model count.
	view.SetSize(120, 18)

	baseHeight := renderedHeight(view.View())
	baseGap := bottomGap(view.View())
	if baseGap > 1 {
		test.Fatalf("at top the list should fill the content (bottom gap %d, want <=1):\n%s", baseGap, view.View())
	}

	// Drive the cursor down through the whole list; the rendered height and the
	// bottom gap must stay invariant at every step.
	for step := 0; step < len(view.models)+5; step++ {
		_ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
		rendered := view.View()
		if got := renderedHeight(rendered); got != baseHeight {
			test.Fatalf("step %d: rendered height = %d, want constant %d:\n%s", step, got, baseHeight, rendered)
		}
		if got := bottomGap(rendered); got != baseGap {
			test.Fatalf("step %d: bottom gap = %d, want constant %d (the bottom moved on scroll):\n%s",
				step, got, baseGap, rendered)
		}
	}
}

// When the cursor scrolls down and then back up to the FIRST model row, the window
// must scroll to reveal the "Installed" section heading above it (not just the first
// model row) — i.e. it scrolls to the content, not the first selectable item.
func TestLocalModelsHeadingVisibleAfterScrollBackToTop(test *testing.T) {
	library := make([]ollama.LibraryModel, 0, 30)
	for index := 0; index < 30; index++ {
		name := "model" + string(rune('a'+index%26)) + strconv.Itoa(index)
		library = append(library, ollama.LibraryModel{Name: name, Description: "desc", Tags: libTags("7b"), RepoURL: "x"})
	}
	// Mark several models installed so the "Installed" section has multiple rows.
	installed := []ollama.Model{
		{Name: library[0].Name + ":7b", Size: 100},
		{Name: library[1].Name + ":7b", Size: 100},
		{Name: library[2].Name + ":7b", Size: 100},
	}
	view := buildLocal(test, installed, library, noShow)
	// A small pane forces a scroll window smaller than the model count.
	view.SetSize(120, 18)

	headingVisible := func() bool {
		for _, line := range strings.Split(view.View(), "\n") {
			if strings.Contains(stripANSI(line), "Installed") {
				return true
			}
		}
		return false
	}

	if !headingVisible() {
		test.Fatalf("the Installed heading should be visible at the top:\n%s", view.View())
	}
	// Scroll all the way down…
	for step := 0; step < len(view.models)+5; step++ {
		_ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	// …then all the way back up to the first model row.
	for step := 0; step < len(view.models)+5; step++ {
		_ = view.Update(tea.KeyMsg{Type: tea.KeyUp})
	}
	if view.window.Cursor() != 0 {
		test.Fatalf("cursor should be back on the first model, got %d", view.window.Cursor())
	}
	if !headingVisible() {
		test.Fatalf("after scrolling back to the top the Installed heading must be visible again:\n%s", view.View())
	}
}

// The PARTIAL-scroll case: scroll down just far enough that ONLY the header leaves the
// window (the first model row is still visible), then scroll back up. The window must
// scroll up far enough to re-show the heading once the cursor returns to the first
// model — even though the cursor row itself never left the window.
func TestLocalModelsHeadingRevealedOnPartialScrollBackUp(test *testing.T) {
	library := make([]ollama.LibraryModel, 0, 30)
	for index := 0; index < 30; index++ {
		name := "model" + string(rune('a'+index%26)) + strconv.Itoa(index)
		library = append(library, ollama.LibraryModel{Name: name, Description: "desc", Tags: libTags("7b"), RepoURL: "x"})
	}
	installed := []ollama.Model{
		{Name: library[0].Name + ":7b", Size: 100},
		{Name: library[1].Name + ":7b", Size: 100},
		{Name: library[2].Name + ":7b", Size: 100},
	}
	view := buildLocal(test, installed, library, noShow)
	view.SetSize(120, 18)

	headingVisible := func() bool {
		for _, line := range strings.Split(view.View(), "\n") {
			if strings.Contains(stripANSI(line), "Installed") {
				return true
			}
		}
		return false
	}

	// Step DOWN one row at a time only until the heading first scrolls off-screen.
	steps := 0
	for headingVisible() && steps < len(view.models) {
		_ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
		steps++
	}
	if headingVisible() {
		test.Fatal("the heading should have scrolled off after stepping down")
	}
	if steps >= len(view.models)-1 {
		test.Fatalf("the heading left the window only at the very bottom (steps=%d); expected a PARTIAL scroll", steps)
	}
	// Step back UP the same number of rows: the cursor returns to the first model and
	// the heading must be on-screen again.
	for index := 0; index < steps; index++ {
		_ = view.Update(tea.KeyMsg{Type: tea.KeyUp})
	}
	if view.window.Cursor() != 0 {
		test.Fatalf("cursor should be back on the first model, got %d", view.window.Cursor())
	}
	if !headingVisible() {
		test.Fatalf("scrolling back up must reveal the heading once the cursor returns to the first model:\n%s", view.View())
	}
}

// enter on an installed tag with no selection opens the /api/show describe pane.
func TestLocalModelsDrillEnterInstalledShowsDetail(test *testing.T) {
	var shown string
	view := buildLocal(test,
		[]ollama.Model{{Name: "qwen2.5:7b", Size: 100, ParameterSize: "7B"}},
		[]ollama.LibraryModel{{Name: "qwen2.5", Tags: libTags("7b"), RepoURL: "x"}},
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

// TestStripEmojiRemovesPictographsKeepsLetters verifies emoji (and the whitespace
// they leave) are stripped from descriptions while non-emoji letters/symbols stay.
func TestStripEmojiRemovesPictographsKeepsLetters(t *testing.T) {
	cases := map[string]string{
		"🌋 LLaVA is a model":  "LLaVA is a model",
		"🎩 Magicoder family":  "Magicoder family",
		"MathΣtral reasoning": "MathΣtral reasoning", // Greek Σ is NOT an emoji — kept
		"plain text":          "plain text",
		"a 🚀 b":               "a b", // collapse the gap the emoji left
	}
	for input, want := range cases {
		if got := stripEmoji(input); got != want {
			t.Errorf("stripEmoji(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestContentLineAlignsToPaneWidth verifies every rendered row is EXACTLY the pane's
// display width — the name never overruns its column and a wide-rune (emoji) or long
// name never pushes the line past the right border.
func TestContentLineAlignsToPaneWidth(t *testing.T) {
	view := &LocalModels{width: 80}
	rows := []localModel{
		{name: "phi", description: "short"},
		{name: "paraphrase-multilingual-very-long-name", description: "a long description that surely runs well past the available space and must be clipped"},
		{name: "llava", description: "🌋 vision model with emoji"},
	}
	for _, model := range rows {
		line := view.contentLine(model)
		if width := runewidth.StringWidth(line); width != view.width {
			t.Errorf("contentLine(%q) display width = %d, want %d", model.name, width, view.width)
		}
	}
}

// TestLocalModelsDrillDownScrolls verifies the per-model tag drill-down WINDOWS a
// long tag list so it scrolls to keep the cursor visible (some models have dozens
// of tags — the list must not run off the pane).
func TestLocalModelsDrillDownScrolls(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	// Zero-pad so the sorted (lexical) tag order matches numeric order: tag00 first,
	// tag29 last.
	tags := make([]ollama.LibraryTag, 0, 30)
	for index := 0; index < 30; index++ {
		suffix := strconv.Itoa(index)
		if len(suffix) == 1 {
			suffix = "0" + suffix
		}
		tags = append(tags, ollama.LibraryTag{Name: "tag" + suffix})
	}
	view := buildLocal(test, nil, []ollama.LibraryModel{{Name: "big", Tags: tags, RepoURL: "x"}}, nil)
	view.SetSize(80, 12) // a short pane forces windowing
	view.openDrill(view.models[0])

	// Move the cursor to the last tag (tag29).
	for index := 0; index < 29; index++ {
		view.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	out := view.drillView()

	if !strings.Contains(out, "↑ more") {
		test.Errorf("with the cursor at the bottom the drill must have scrolled (↑ more marker):\n%s", out)
	}
	if !strings.Contains(out, "] tag29") {
		test.Errorf("the selected (last) tag must be visible after scrolling:\n%s", out)
	}
	if strings.Contains(out, "] tag00") {
		test.Errorf("early tags should have scrolled off the top:\n%s", out)
	}
}

// The drill-down surfaces an installed tag's recorded serving runtime as a badge.
func TestLocalModelsDrillShowsRuntimeBadge(test *testing.T) {
	prev := modelRuntimeLookup
	modelRuntimeLookup = func(ref string) config.ModelRuntime {
		if ref == "qwen2.5:7b" {
			return config.RuntimeDockerModelRunner
		}
		return ""
	}
	test.Cleanup(func() { modelRuntimeLookup = prev })

	view := buildLocal(test,
		[]ollama.Model{{Name: "qwen2.5:7b", Size: 4700000000, ParameterSize: "7.6B"}},
		[]ollama.LibraryModel{{Name: "qwen2.5", Tags: libTags("7b", "72b"), RepoURL: "x"}},
		noShow,
	)
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter}) // open drill on qwen2.5
	if view.drill == nil {
		test.Fatal("enter should open the tag drill-down")
	}
	if !strings.Contains(view.View(), "dmr") {
		test.Fatalf("drill-down should badge the installed 7b tag's DMR runtime:\n%s", view.View())
	}
}
