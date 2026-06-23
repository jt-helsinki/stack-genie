package ui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
)

// Table renders a clean bordered table: the header row in the Heading style,
// muted borders, and one row per rows entry. lipgloss strips colour when stdout
// is not a TTY (or NO_COLOR is set), so the output is plain in automation. The
// returned string has no trailing newline, matching the Human() contract. It is
// the single shared table renderer for every list command's human output.
func Table(headers []string, rows [][]string) string {
	rendered := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(Muted).
		Headers(headers...).
		Rows(rows...).
		StyleFunc(func(row, _ int) lipgloss.Style {
			if row == table.HeaderRow {
				return Heading.Padding(0, 1)
			}
			return lipgloss.NewStyle().Padding(0, 1)
		})
	return rendered.String()
}
