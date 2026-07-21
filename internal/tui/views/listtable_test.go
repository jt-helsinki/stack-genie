package views

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func tableRows(count int) [][]string {
	rows := make([][]string, 0, count)
	for index := 0; index < count; index++ {
		rows = append(rows, []string{"row" + string(rune('a'+index%26)), "data"})
	}
	return rows
}

// View renders the pinned header above exactly the sized number of rows, padding when
// there are fewer rows than the window — a CONSTANT total height (header + rows).
func TestListTableConstantHeightAndPinnedHeader(test *testing.T) {
	table := newListTable([]listColumn{{title: "NAME", width: 10}, {title: "KIND", width: 6}})
	table.SetSize(40, 8) // total height 8 -> 1 header + 7 rows
	table.SetRows(tableRows(3))

	lines := strings.Split(table.View(), "\n")
	if len(lines) != 8 {
		test.Fatalf("rendered height = %d, want 8 (header + 7 rows):\n%s", len(lines), table.View())
	}
	if !strings.Contains(lines[0], "NAME") || !strings.Contains(lines[0], "KIND") {
		test.Fatalf("first line must be the pinned column header, got %q", lines[0])
	}
	// With more rows than the window, the height stays constant (it scrolls, not grows).
	table.SetRows(tableRows(50))
	if got := len(strings.Split(table.View(), "\n")); got != 8 {
		test.Fatalf("rendered height with many rows = %d, want constant 8", got)
	}
}

// SelectedRow / Cursor / SetCursor track the highlighted row, and the header pins
// while the rows scroll past it.
func TestListTableSelectionAndScroll(test *testing.T) {
	table := newListTable([]listColumn{{title: "NAME", width: 10}})
	table.SetSize(20, 4) // 1 header + 3 rows
	table.SetRows(tableRows(20))

	if got := table.SelectedRow(); got == nil || got[0] != "rowa" {
		test.Fatalf("initial SelectedRow = %v, want rowa", got)
	}
	// Step down past the visible window; the header stays on line 0 and the cursor row
	// is still within the rendered window.
	for step := 0; step < 10; step++ {
		table.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if table.Cursor() != 10 {
		test.Fatalf("cursor after 10 downs = %d, want 10", table.Cursor())
	}
	lines := strings.Split(table.View(), "\n")
	if !strings.Contains(lines[0], "NAME") {
		test.Fatalf("header must remain pinned on line 0 after scrolling, got %q", lines[0])
	}
	selected := table.SelectedRow()
	if selected == nil || !strings.Contains(table.View(), selected[0]) {
		test.Fatalf("the selected row %v must be within the rendered window:\n%s", selected, table.View())
	}
	// PgUp/Home jump back to the top.
	table.Update(tea.KeyMsg{Type: tea.KeyHome})
	if table.Cursor() != 0 {
		test.Fatalf("home should jump to the first row, got %d", table.Cursor())
	}
}

// The last positive-width column is stretched so a data row spans the full pane width
// (so the selected-row highlight reaches the right edge on every table).
func TestListTableStretchesLastColumnToWidth(test *testing.T) {
	table := newListTable([]listColumn{{title: "A", width: 4}, {title: "B", width: 4}})
	table.SetSize(40, 4)
	table.SetRows([][]string{{"x", "y"}})

	// The single data row (line 1) must be exactly the pane width.
	row := strings.Split(table.View(), "\n")[1]
	if got := len([]rune(stripANSI(row))); got != 40 {
		test.Fatalf("data row width = %d, want 40 (last column stretched):\n%q", got, row)
	}
}
