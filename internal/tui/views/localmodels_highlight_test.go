package views

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/muesli/termenv"
)

// forceTrueColor forces lipgloss to emit truecolor ANSI even on the non-TTY test
// runner, so the Selected-row background is actually present in the rendered output
// (otherwise lipgloss strips styling and the assertion can't see the highlight).
func forceTrueColor(test *testing.T) {
	test.Helper()
	lipgloss.SetColorProfile(termenv.TrueColor)
	test.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
}

// selectedBackgroundANSI is the truecolor SGR for the default theme accent
// (#FF66FF), used as the Selected row Background by ui.TableStyles().
const selectedBackgroundANSI = "48;2;255;102;255"

// cursorRow returns the table-row index of the cursor and the models[] index it maps
// to (-1 for a header row).
func cursorRow(view *LocalModels) (int, int) {
	cursor := view.table.Cursor()
	if cursor < 0 || cursor >= len(view.rowModel) {
		return cursor, -2
	}
	return cursor, view.rowModel[cursor]
}

// The active/selected model row must be visibly highlighted: the cursor sits on a
// MODEL row (never a header) after load and after ↓, and the rendered output carries
// the Selected background on the highlighted model row's line.
func TestLocalModelsSelectedRowHighlighted(test *testing.T) {
	forceTrueColor(test)
	view := buildLocal(test,
		[]ollama.Model{{Name: "qwen2.5:7b", Size: 4700000000, ParameterSize: "7.6B"}},
		[]ollama.LibraryModel{
			{Name: "qwen2.5", Description: "Qwen 2.5", Tags: []string{"7b", "72b"}},
			{Name: "gemma3", Description: "Gemma 3", Tags: []string{"1b", "4b"}},
			{Name: "llama3.2", Description: "Llama 3.2", Tags: []string{"1b", "3b"}},
		},
		noShow,
	)

	// (a) After load the cursor is on a model row, not a header.
	_, modelIndex := cursorRow(view)
	if modelIndex < 0 {
		test.Fatalf("after load the cursor must sit on a model row, got rowModel index %d", modelIndex)
	}

	// (b) The rendered output carries the Selected background on the highlighted
	// model row's line, and ONLY there among the model/header lines.
	assertHighlightOnCursorLine(test, view, "after load")

	// (c) ↓ moves the highlight to the next model row, still highlighted.
	before, _ := cursorRow(view)
	_ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
	after, afterModel := cursorRow(view)
	if after == before {
		test.Fatalf("↓ should move the cursor, stayed at row %d", before)
	}
	if afterModel < 0 {
		test.Fatalf("after ↓ the cursor must sit on a model row, got rowModel index %d", afterModel)
	}
	assertHighlightOnCursorLine(test, view, "after ↓")
}

// assertHighlightOnCursorLine renders the view and asserts the Selected background is
// present on the table line at the cursor and on no other model/header line.
func assertHighlightOnCursorLine(test *testing.T, view *LocalModels, when string) {
	test.Helper()
	cursor := view.table.Cursor()
	rendered := view.View()
	if !strings.Contains(rendered, selectedBackgroundANSI) {
		test.Fatalf("%s: the Selected background %q is absent from the rendered view:\n%s",
			when, selectedBackgroundANSI, rendered)
	}
	// Locate the table body lines (the rows after the header chrome). The table's
	// own lines are those rendering a model name / section header; find the one that
	// carries the highlight and confirm its content matches the cursor row.
	lines := strings.Split(rendered, "\n")
	highlighted := 0
	for _, line := range lines {
		if strings.Contains(line, selectedBackgroundANSI) {
			highlighted++
		}
	}
	if highlighted != 1 {
		test.Fatalf("%s: expected exactly one highlighted line, got %d:\n%s", when, highlighted, rendered)
	}
	// The highlighted line must contain the cursor row's model name AND the
	// highlight must span the full row — the closing reset is the LAST escape on the
	// line (no unstyled trailing pad spaces after it), so the background bar reaches
	// the right edge rather than stopping short and looking ragged.
	index := view.rowModel[cursor]
	wantName := view.models[index].name
	for _, line := range lines {
		if !strings.Contains(line, selectedBackgroundANSI) {
			continue
		}
		if !strings.Contains(line, wantName) {
			test.Fatalf("%s: highlighted line does not contain cursor model %q:\n%s", when, wantName, line)
		}
		if trailing := line[strings.LastIndex(line, "\x1b[0m")+len("\x1b[0m"):]; strings.TrimSpace(trailing) == "" && trailing != "" {
			test.Fatalf("%s: highlight stops short — %d unstyled trailing pad chars after the reset:\n%q",
				when, len(trailing), line)
		}
	}
}
