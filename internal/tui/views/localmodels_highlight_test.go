package views

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/stack-genie/internal/ollama"
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
// (#FF66FF), used as the Selected row Background by the highlight style.
const selectedBackgroundANSI = "48;2;255;102;255"

// secondaryForegroundANSI is the truecolor SGR for the default theme secondary
// (#33FFFF) as a FOREGROUND — what the Installed/Installable section headers use.
const secondaryForegroundANSI = "38;2;51;255;255"

// The active/selected model row must be visibly highlighted: the cursor sits on a
// MODEL row (never a header) after load and after ↓, and the rendered output carries
// the Selected background on the highlighted model row's line.
func TestLocalModelsSelectedRowHighlighted(test *testing.T) {
	forceTrueColor(test)
	view := buildLocal(test,
		[]ollama.Model{{Name: "qwen2.5:7b", Size: 4700000000, ParameterSize: "7.6B"}},
		[]ollama.LibraryModel{
			{Name: "qwen2.5", Description: "Qwen 2.5", Tags: libTags("7b", "72b")},
			{Name: "gemma3", Description: "Gemma 3", Tags: libTags("1b", "4b")},
			{Name: "llama3.2", Description: "Llama 3.2", Tags: libTags("1b", "3b")},
		},
		noShow,
	)

	// (a) After load the cursor is on a model row.
	if view.window.Cursor() < 0 || view.window.Cursor() >= len(view.models) {
		test.Fatalf("after load the cursor must sit on a model row, got cursor %d of %d models", view.window.Cursor(), len(view.models))
	}

	// (b) The rendered output carries the Selected background on the highlighted
	// model row's line, and ONLY there.
	assertHighlightOnCursorLine(test, view, "after load")

	// (c) ↓ moves the highlight to the next model row, still highlighted.
	before := view.window.Cursor()
	_ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
	if view.window.Cursor() == before {
		test.Fatalf("↓ should move the cursor, stayed at %d", before)
	}
	if view.window.Cursor() < 0 || view.window.Cursor() >= len(view.models) {
		test.Fatalf("after ↓ the cursor must sit on a model row, got %d", view.window.Cursor())
	}
	assertHighlightOnCursorLine(test, view, "after ↓")
}

// The two section headers must be bold + secondary-coloured with a blank line above
// and below each, so they clearly separate the Installed and Installable sections.
func TestLocalModelsSectionHeadersStyled(test *testing.T) {
	forceTrueColor(test)
	view := buildLocal(test,
		[]ollama.Model{{Name: "qwen2.5:7b", Size: 4700000000, ParameterSize: "7.6B"}},
		[]ollama.LibraryModel{
			{Name: "qwen2.5", Description: "Qwen 2.5", Tags: libTags("7b", "72b")},
			{Name: "llama3.2", Description: "Llama 3.2", Tags: libTags("1b", "3b")},
		},
		noShow,
	)
	rendered := view.View()
	lines := strings.Split(rendered, "\n")

	for _, label := range []string{"Installed", "Installable"} {
		headerIndex := -1
		for index, line := range lines {
			// The header line carries the label, the secondary FOREGROUND, and the bold
			// SGR (1) — and is NOT the highlighted cursor row (no accent background).
			if strings.Contains(line, label) &&
				strings.Contains(line, secondaryForegroundANSI) &&
				!strings.Contains(line, selectedBackgroundANSI) {
				headerIndex = index
				break
			}
		}
		if headerIndex < 0 {
			test.Fatalf("section header %q must be present, bold + secondary-coloured:\n%s", label, rendered)
		}
		// Blank line above and below the header (top + bottom padding).
		if headerIndex == 0 || strings.TrimSpace(stripANSI(lines[headerIndex-1])) != "" {
			test.Errorf("section header %q must have a blank line above it:\n%s", label, rendered)
		}
		if headerIndex+1 >= len(lines) || strings.TrimSpace(stripANSI(lines[headerIndex+1])) != "" {
			test.Errorf("section header %q must have a blank line below it:\n%s", label, rendered)
		}
	}
}

// stripANSI removes SGR escape sequences so a "blank" padding line (which may carry a
// reset) is recognised as visually empty.
func stripANSI(line string) string {
	var out strings.Builder
	for {
		start := strings.IndexByte(line, '\x1b')
		if start < 0 {
			out.WriteString(line)
			break
		}
		out.WriteString(line[:start])
		end := strings.IndexByte(line[start:], 'm')
		if end < 0 {
			break
		}
		line = line[start+end+1:]
	}
	return out.String()
}

// assertHighlightOnCursorLine renders the view and asserts the Selected background is
// present on the list line at the cursor and on no other line.
func assertHighlightOnCursorLine(test *testing.T, view *LocalModels, when string) {
	test.Helper()
	rendered := view.View()
	if !strings.Contains(rendered, selectedBackgroundANSI) {
		test.Fatalf("%s: the Selected background %q is absent from the rendered view:\n%s",
			when, selectedBackgroundANSI, rendered)
	}
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
	// The highlighted line must contain the cursor row's model name AND the highlight
	// must span the full row — the closing reset is the LAST escape on the line (no
	// unstyled trailing pad spaces after it), so the background bar reaches the right
	// edge rather than stopping short and looking ragged.
	wantName := view.models[view.window.Cursor()].name
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
