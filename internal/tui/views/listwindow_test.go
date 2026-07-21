package views

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// header builds a ListLine block of [blank, "<label>", blank] + n selectable item rows
// named "item<offset+i>", mirroring how a sectioned list (e.g. Local Models) lays out.
func sectionLines(label string, offset, count int) []ListLine {
	lines := []ListLine{{Text: ""}, {Text: label}, {Text: ""}}
	for index := 0; index < count; index++ {
		lines = append(lines, ListLine{Text: "item" + string(rune('a'+offset+index)), Selectable: true})
	}
	return lines
}

func noStyle() lipgloss.Style { return lipgloss.NewStyle() }

// The cursor moves over SELECTABLE lines only and never lands on a header/blank line.
func TestListWindowCursorSkipsNonSelectable(test *testing.T) {
	window := &listWindow{}
	window.SetContent(sectionLines("Header", 0, 5), 40, 10)

	if window.Count() != 5 {
		test.Fatalf("Count = %d, want 5 selectable rows", window.Count())
	}
	if window.Cursor() != 0 {
		test.Fatalf("initial cursor = %d, want 0", window.Cursor())
	}
	window.Move(1)
	if window.Cursor() != 1 {
		test.Fatalf("after one step cursor = %d, want 1", window.Cursor())
	}
	// Moving past the end clamps to the last selectable row.
	window.Move(100)
	if window.Cursor() != 4 {
		test.Fatalf("cursor past end = %d, want 4 (clamped)", window.Cursor())
	}
}

// The rendered window is a CONSTANT height (padded with blanks) regardless of scroll.
func TestListWindowConstantRenderHeight(test *testing.T) {
	window := &listWindow{}
	window.SetContent(sectionLines("Header", 0, 20), 40, 8)

	want := 8
	for step := 0; step < 25; step++ {
		got := strings.Count(window.View(noStyle()), "\n") + 1
		if got != want {
			test.Fatalf("step %d: rendered height = %d, want constant %d", step, got, want)
		}
		window.Move(1)
	}
}

// Anchor scroll: after scrolling DOWN so the header leaves the window (the row itself
// still visible), scrolling back to the first item must re-reveal the header block —
// the behaviour defined once in listWindow.
func TestListWindowAnchorsToHeaderOnScrollBackUp(test *testing.T) {
	window := &listWindow{}
	// 3 header lines + 20 items, a short 6-row window.
	window.SetContent(sectionLines("Header", 0, 20), 40, 6)

	headerVisible := func() bool {
		return strings.Contains(window.View(noStyle()), "Header")
	}
	if !headerVisible() {
		test.Fatal("the header should be visible at the top")
	}
	// Step down until the header first scrolls off (a PARTIAL scroll, not the bottom).
	steps := 0
	for headerVisible() && steps < window.Count() {
		window.Move(1)
		steps++
	}
	if headerVisible() {
		test.Fatal("the header should have scrolled off after stepping down")
	}
	if steps >= window.Count()-1 {
		test.Fatalf("the header left only at the very bottom (steps=%d); expected a partial scroll", steps)
	}
	// Step back up to the first item: the header block must be revealed again.
	for index := 0; index < steps; index++ {
		window.Move(-1)
	}
	if window.Cursor() != 0 {
		test.Fatalf("cursor should be back on the first item, got %d", window.Cursor())
	}
	if !headerVisible() {
		test.Fatalf("scrolling back to the first item must reveal the header:\n%s", window.View(noStyle()))
	}
}

// A second section's header is revealed when the cursor reaches that section's first
// item (the anchor walks the contiguous non-selectable block above the cursor).
func TestListWindowRevealsSecondSectionHeader(test *testing.T) {
	window := &listWindow{}
	lines := append(sectionLines("First", 0, 6), sectionLines("Second", 6, 6)...)
	window.SetContent(lines, 40, 5)

	// Move to the first item of the "Second" section (selectable ordinal 6).
	window.SetCursor(6)
	if !strings.Contains(window.View(noStyle()), "Second") {
		test.Fatalf("the Second section header should be revealed on its first item:\n%s", window.View(noStyle()))
	}
}
