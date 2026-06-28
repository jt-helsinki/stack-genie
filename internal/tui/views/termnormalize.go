package views

import (
	"strconv"
	"strings"
)

// normalizeTerminalOutput reduces captured terminal output to its final on-screen
// text, applying the control codes a real terminal would so that progress redraws
// (docker/nerdctl image pulls etc.) collapse IN PLACE — while preserving the FULL
// scrollback (rows are unbounded, never a fixed screen height). It interprets:
//   - \n (next row), \r (column 0), \t (tab stops every 8 cols)
//   - CSI cursor up/down/forward/back (A/B/C/D)
//   - CSI erase-line (K: to end / to start / whole line)
//
// Colour (SGR) and every other escape sequence are stripped, so the result is plain,
// SELECTABLE text. It is a pure function (no live emulator), letting a plain
// scrollable viewer render terminal-style progress. Plain line-based logs (no
// carriage returns / cursor moves) pass through essentially unchanged.
func normalizeTerminalOutput(raw string) string {
	grid := &lineGrid{}
	data := []rune(raw)
	for index := 0; index < len(data); index++ {
		char := data[index]
		switch char {
		case '\n':
			grid.newline()
		case '\r':
			grid.col = 0
		case '\t':
			next := ((grid.col / 8) + 1) * 8
			for grid.col < next {
				grid.put(' ')
			}
		case 0x1b:
			grid.applyEscape(data[index:], &index)
		default:
			if char >= 0x20 {
				grid.put(char)
			}
		}
	}
	var builder strings.Builder
	for index, line := range grid.lines {
		if index > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(string(line))
	}
	return builder.String()
}

// lineGrid is an unbounded virtual screen: rows of runes plus a cursor. Unlike a
// real terminal it never scrolls content off the top, so all history is kept. Rows
// are kept as []rune (not string) so writes are O(1) amortised — a string-backed
// grid re-converted the whole row on EVERY character (O(L²) per line), which made
// normalizing a 1000-line TUI-redraw buffer slow enough to stall the event loop.
type lineGrid struct {
	lines    [][]rune
	row, col int
}

func (grid *lineGrid) ensureRow() {
	for len(grid.lines) <= grid.row {
		grid.lines = append(grid.lines, nil)
	}
}

func (grid *lineGrid) newline() {
	grid.row++
	grid.col = 0
	grid.ensureRow()
}

// put writes a rune at the cursor (padding with spaces if the cursor is past the end
// of the row) and advances the column. Operates on the row's []rune in place.
func (grid *lineGrid) put(char rune) {
	grid.ensureRow()
	runes := grid.lines[grid.row]
	for len(runes) < grid.col {
		runes = append(runes, ' ')
	}
	if grid.col < len(runes) {
		runes[grid.col] = char
	} else {
		runes = append(runes, char)
	}
	grid.lines[grid.row] = runes
	grid.col++
}

// eraseLine clears part of the current row: mode 0 = cursor→end, 1 = start→cursor,
// 2 = whole line.
func (grid *lineGrid) eraseLine(mode int) {
	grid.ensureRow()
	runes := grid.lines[grid.row]
	switch mode {
	case 1:
		for index := 0; index < grid.col && index < len(runes); index++ {
			runes[index] = ' '
		}
	case 2:
		runes = runes[:0]
	default: // 0
		if grid.col < len(runes) {
			runes = runes[:grid.col]
		}
	}
	grid.lines[grid.row] = runes
}

// applyEscape interprets the escape sequence starting at data[0] (which is ESC),
// advancing *index past it. Recognised CSI cursor/erase codes mutate the grid;
// everything else (SGR colour, OSC, private modes, 2-byte escapes) is consumed and
// discarded.
func (grid *lineGrid) applyEscape(data []rune, index *int) {
	if len(data) < 2 {
		return // a trailing lone ESC — drop it
	}
	switch data[1] {
	case '[': // CSI: ESC [ <params> <final>
		params, final, length := parseCSI(data)
		*index += length - 1
		grid.applyCSI(final, params)
	case ']': // OSC: ESC ] ... (BEL | ESC \)
		*index += oscLength(data) - 1
	default: // 2-byte escape: ESC <byte>
		*index++ // skip the byte after ESC
	}
}

// applyCSI mutates the grid for the cursor/erase final bytes; other finals are no-ops.
func (grid *lineGrid) applyCSI(final rune, params []int) {
	count := 1
	if len(params) > 0 && params[0] > 0 {
		count = params[0]
	}
	switch final {
	case 'A': // cursor up
		grid.row -= count
		if grid.row < 0 {
			grid.row = 0
		}
	case 'B': // cursor down
		grid.row += count
		grid.ensureRow()
	case 'C': // cursor forward
		grid.col += count
	case 'D': // cursor back
		grid.col -= count
		if grid.col < 0 {
			grid.col = 0
		}
	case 'K': // erase line
		mode := 0
		if len(params) > 0 {
			mode = params[0]
		}
		grid.eraseLine(mode)
	}
}

// parseCSI parses a CSI sequence beginning at data[0]=ESC, data[1]='['. It returns
// the numeric params, the final byte, and the total rune length consumed (including
// ESC and '['). A malformed/unterminated sequence consumes just "ESC [".
func parseCSI(data []rune) (params []int, final rune, length int) {
	index := 2 // past ESC '['
	start := index
	for index < len(data) {
		char := data[index]
		// Parameter/intermediate bytes: digits, ';', '?', and the 0x20–0x2f range.
		if (char >= '0' && char <= '9') || char == ';' || char == '?' || (char >= 0x20 && char <= 0x2f) {
			index++
			continue
		}
		// Final byte (0x40–0x7e).
		final = char
		for _, field := range strings.Split(string(data[start:index]), ";") {
			field = strings.TrimPrefix(field, "?")
			if field == "" {
				params = append(params, 0)
				continue
			}
			value, err := strconv.Atoi(field)
			if err != nil {
				value = 0
			}
			params = append(params, value)
		}
		return params, final, index + 1
	}
	return nil, 0, 2 // unterminated — consume just "ESC ["
}

// oscLength returns the rune length of an OSC sequence beginning at data[0]=ESC,
// data[1]=']', terminated by BEL (0x07) or ST (ESC '\'). An unterminated sequence
// consumes the whole remainder.
func oscLength(data []rune) int {
	for index := 2; index < len(data); index++ {
		if data[index] == 0x07 {
			return index + 1
		}
		if data[index] == 0x1b && index+1 < len(data) && data[index+1] == '\\' {
			return index + 2
		}
	}
	return len(data)
}
