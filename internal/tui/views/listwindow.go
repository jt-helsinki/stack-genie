package views

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ListLine is one rendered line of a listWindow: either a SELECTABLE item row or a
// non-selectable header/padding line (a section heading, a blank separator, …). The
// Text is the fully-rendered line (already styled + padded to the pane width by the
// owner); the listWindow only windows, scrolls, and highlights it.
type ListLine struct {
	Text       string
	Selectable bool
}

// listWindow is the reusable layout+scroll behaviour for a fixed-height, cursored
// list whose content interleaves SELECTABLE rows with non-selectable header/padding
// lines (e.g. the Local Models "Installed"/"Installable" sections). It is defined
// ONCE here and embedded by any view that needs that behaviour, so the windowing
// rules live in one place:
//
//   - A single cursor moves over SELECTABLE lines only (it never lands on a header or
//     padding line).
//   - The visible window is a CONSTANT height (it pads with blank lines), so the pane's
//     bottom never shifts as you scroll.
//   - Scrolling ANCHORS to the cursor's content block, not its bare row: the window is
//     scrolled so the contiguous header/padding lines directly ABOVE the cursor's row
//     are revealed whenever the cursor sits on that block's first item — so returning to
//     a section's first row shows that section's heading, even if only the heading (not
//     the row) had scrolled off. A very short window still keeps the cursor visible.
//   - The last page is kept full (the top is clamped to totalLines-height) so a short
//     tail never leaves a growing gap above the bottom margin.
//
// The owner builds the []ListLine each render (it knows its own sections + styling),
// pushes them in via SetContent, and reads Cursor() to map back to its domain item.
type listWindow struct {
	lines  []ListLine
	cursor int // ordinal over SELECTABLE lines (0-based)
	top    int // first visible DISPLAY-line index (the scroll window top)
	width  int
	height int // visible row count (the fixed list-block height)
}

// SetContent replaces the rendered lines and the pane geometry, then re-clamps the
// cursor + scroll window. It does NOT move the cursor (a rebuild that should reseat
// the cursor calls Reset first). width is kept for owners that want it; height is the
// fixed number of visible rows.
func (window *listWindow) SetContent(lines []ListLine, width, height int) {
	window.lines = lines
	window.width = width
	window.height = height
	window.clamp()
}

// Reset seats the cursor on the first item and scrolls to the top — used when the
// owner rebuilds the list from scratch.
func (window *listWindow) Reset() {
	window.cursor = 0
	window.top = 0
}

// Cursor is the current selectable ordinal (its position among the selectable lines).
func (window *listWindow) Cursor() int { return window.cursor }

// SetCursor seats the cursor on a specific selectable ordinal, re-clamping the scroll
// window so the new cursor (and its heading block) is positioned per the rules.
func (window *listWindow) SetCursor(ordinal int) {
	window.cursor = ordinal
	window.clamp()
}

// Count is the number of selectable lines.
func (window *listWindow) Count() int {
	count := 0
	for _, line := range window.lines {
		if line.Selectable {
			count++
		}
	}
	return count
}

// Move steps the cursor by step (±1) over selectable lines only, clamped to the ends,
// then re-clamps the scroll window so the cursor (and its heading block) stays visible.
func (window *listWindow) Move(step int) {
	count := window.Count()
	if count == 0 {
		return
	}
	next := window.cursor + step
	if next < 0 {
		next = 0
	}
	if next > count-1 {
		next = count - 1
	}
	window.cursor = next
	window.clamp()
}

// cursorLine is the DISPLAY-line index of the cursor's selectable line (-1 if there
// is none).
func (window *listWindow) cursorLine() int {
	ordinal := 0
	for index, line := range window.lines {
		if !line.Selectable {
			continue
		}
		if ordinal == window.cursor {
			return index
		}
		ordinal++
	}
	return -1
}

// effectiveHeight is the visible row count, guarded to ≥1 for a degenerate pane.
func (window *listWindow) effectiveHeight() int {
	if window.height < 1 {
		return 1
	}
	return window.height
}

// clamp keeps the cursor in range and the scroll window positioned per the rules in
// the type doc: anchor to the cursor's content block, keep the cursor visible, keep
// the last page full.
func (window *listWindow) clamp() {
	count := window.Count()
	if count == 0 {
		window.cursor = 0
		window.top = 0
		return
	}
	if window.cursor < 0 {
		window.cursor = 0
	}
	if window.cursor > count-1 {
		window.cursor = count - 1
	}
	height := window.effectiveHeight()
	cursorLine := window.cursorLine()
	if cursorLine < 0 {
		window.top = 0
		return
	}
	// Anchor to the cursor's CONTENT block: the first of the contiguous
	// non-selectable header/padding lines directly above the cursor's row.
	anchor := cursorLine
	for anchor > 0 && !window.lines[anchor-1].Selectable {
		anchor--
	}
	if anchor < window.top {
		window.top = anchor
	}
	// Keep the cursor visible at the bottom edge — also the short-window guard: if the
	// heading block cannot fit, the cursor wins over showing the heading.
	if cursorLine >= window.top+height {
		window.top = cursorLine - height + 1
	}
	// Keep the LAST window full (no short tail).
	if maxTop := len(window.lines) - height; window.top > maxTop {
		window.top = maxTop
	}
	if window.top < 0 {
		window.top = 0
	}
}

// View renders the visible window at the FIXED height: exactly height lines from the
// top, the cursor's selectable line highlighted with selected (its Text is already
// padded to the pane width by the owner, so the highlight bar spans the whole row),
// padded with blank lines when the window is short so the block height is constant.
func (window *listWindow) View(selected lipgloss.Style) string {
	height := window.effectiveHeight()
	cursorLine := window.cursorLine()
	first := window.top
	if first < 0 {
		first = 0
	}
	last := first + height
	if last > len(window.lines) {
		last = len(window.lines)
	}
	rendered := make([]string, 0, height)
	for index := first; index < last; index++ {
		text := window.lines[index].Text
		if index == cursorLine {
			text = selected.Render(text)
		}
		rendered = append(rendered, text)
	}
	for len(rendered) < height {
		rendered = append(rendered, "")
	}
	return strings.Join(rendered, "\n")
}
