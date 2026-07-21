package views

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/mattn/go-runewidth"
)

// listColumn is one column of a listTable: a title and a fixed display width. A width
// ≤ 0 means the column is skipped (mirroring bubbles/table). The LAST positive-width
// column is stretched to fill the pane so the selected-row highlight spans the full
// width on every table (previously only Cloud Models stretched).
type listColumn struct {
	title string
	width int
}

// listCellPad is the per-column horizontal padding (one space each side), matching the
// bubbles/table Cell/Header padding so a width-W column renders as W+2 cells.
const listCellPad = 2

// defaultListTableHeight is the data-row count before SetSize is called, matching the
// bubbles/table default viewport height (20) so a table that is never explicitly sized
// (e.g. in a unit test) still renders its rows rather than a single line.
const defaultListTableHeight = 20

// listTable is the reusable COLUMN-TABLE panel, built on listWindow so every
// selectable-list panel in `ai ui` shares one scrolling/highlight implementation. It
// renders a PINNED column-header row above a fixed-height, cursored window of data
// rows (the header never scrolls; the rows scroll through listWindow). It mirrors the
// slice of the bubbles/table API the views use (SetSize/SetRows/SetColumns/SetCursor/
// Cursor/SelectedRow/Rows/Update/View) so the views read the same, and reproduces
// bubbles' cell rendering (truncate-to-width + 1-col padding) so column layout is
// unchanged — only the scroll engine is now the shared listWindow.
type listTable struct {
	columns      []listColumn
	rows         [][]string
	window       listWindow
	width        int
	windowHeight int // data-row count = total height − the 1-line header
}

// newListTable builds a column table over the given columns.
func newListTable(columns []listColumn) listTable {
	return listTable{columns: columns, windowHeight: defaultListTableHeight}
}

// SetSize sets the pane width and the TOTAL height (header + rows): the data window
// gets height−1 rows, so View() renders exactly height lines — identical to a bubbles
// table's SetHeight(height). Re-syncs the rendered rows at the new geometry.
func (table *listTable) SetSize(width, height int) {
	table.width = width
	table.windowHeight = height - 1
	if table.windowHeight < 1 {
		table.windowHeight = 1
	}
	table.syncWindow()
}

// SetColumns replaces the columns (re-syncing the rendered rows).
func (table *listTable) SetColumns(columns []listColumn) {
	table.columns = columns
	table.syncWindow()
}

// SetRows replaces the data rows (re-syncing + re-clamping the cursor/scroll).
func (table *listTable) SetRows(rows [][]string) {
	table.rows = rows
	table.syncWindow()
}

// Rows returns the current data rows.
func (table *listTable) Rows() [][]string { return table.rows }

// Cursor is the selected row index.
func (table *listTable) Cursor() int { return table.window.Cursor() }

// SetCursor moves the selection to a specific row index (clamped).
func (table *listTable) SetCursor(index int) { table.window.SetCursor(index) }

// SelectedRow returns the highlighted row's cells (nil when out of range).
func (table *listTable) SelectedRow() []string {
	cursor := table.window.Cursor()
	if cursor < 0 || cursor >= len(table.rows) {
		return nil
	}
	return table.rows[cursor]
}

// Update routes the navigation keys to the data window (↑/↓/k/j step, PgUp/PgDn page,
// ctrl+u/ctrl+d half-page, home/g top, end/G bottom). Action keys are intercepted by
// the owning view BEFORE this, so they never reach here. Returns no command (the
// window mutates in place). Non-key messages are ignored.
func (table *listTable) Update(msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	page := table.windowHeight
	if page < 1 {
		page = 1
	}
	switch key.String() {
	case "up", "k":
		table.window.Move(-1)
	case "down", "j":
		table.window.Move(1)
	case "pgup":
		table.window.Move(-page)
	case "pgdown":
		table.window.Move(page)
	case "ctrl+u":
		table.window.Move(-page / 2)
	case "ctrl+d":
		table.window.Move(page / 2)
	case "home", "g":
		table.window.SetCursor(0)
	case "end", "G":
		table.window.SetCursor(table.window.Count() - 1)
	}
	return nil
}

// View renders the pinned header row above the windowed, fixed-height data rows.
func (table *listTable) View() string {
	return table.headerRow() + "\n" + table.window.View(selectedStyle())
}

// syncWindow renders the current rows into the listWindow at the current geometry.
func (table *listTable) syncWindow() {
	widths := table.columnWidths()
	lines := make([]ListLine, 0, len(table.rows))
	for _, row := range table.rows {
		lines = append(lines, ListLine{Text: renderTableRow(row, widths), Selectable: true})
	}
	table.window.SetContent(lines, table.width, table.windowHeight)
}

// columnWidths returns each column's display width, stretching the LAST positive-width
// column to fill the pane (so a highlighted row spans the full width). A pane too
// narrow to hold the declared widths is left unstretched.
func (table *listTable) columnWidths() []int {
	widths := make([]int, len(table.columns))
	used, last := 0, -1
	for index, column := range table.columns {
		widths[index] = column.width
		if column.width > 0 {
			used += column.width + listCellPad
			last = index
		}
	}
	if last >= 0 {
		if slack := table.width - used; slack > 0 {
			widths[last] += slack
		}
	}
	return widths
}

// headerRow renders the bold, accent-coloured column titles (the pinned header).
func (table *listTable) headerRow() string {
	header := lipgloss.NewStyle().Bold(true).Foreground(ui.Accent())
	widths := table.columnWidths()
	cells := make([]string, 0, len(table.columns))
	for index, column := range table.columns {
		width := widths[index]
		if width <= 0 {
			continue
		}
		cells = append(cells, header.Render(tableCell(column.title, width)))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cells...)
}

// renderTableRow renders one data row's cells (truncate-to-width + padding), joined to
// the full pane width so the selected-row highlight (applied by listWindow) spans it.
func renderTableRow(values []string, widths []int) string {
	cells := make([]string, 0, len(widths))
	for index, width := range widths {
		if width <= 0 {
			continue
		}
		value := ""
		if index < len(values) {
			value = values[index]
		}
		cells = append(cells, tableCell(value, width))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cells...)
}

// tableCell renders one cell: the value truncated to width DISPLAY cells, padded to
// the column width, with one space of padding each side — identical to bubbles/table's
// cell so column layout is unchanged. The cell carries no colour (the row highlight is
// applied AFTER, over the whole row), matching ui.TableStyles' Cell contract.
func tableCell(value string, width int) string {
	inner := lipgloss.NewStyle().Width(width).MaxWidth(width).Inline(true).
		Render(runewidth.Truncate(value, width, "…"))
	return lipgloss.NewStyle().Padding(0, 1).Render(inner)
}
