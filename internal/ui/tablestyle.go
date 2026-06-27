package ui

import (
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
)

// Accent returns the active theme's accent colour, for components that must style
// themselves (bubbles tables, the filepicker) to match the rest of the UI.
func Accent() lipgloss.Color { return active.accent }

// Secondary returns the active theme's secondary colour (the BluePink default's
// bright blue), the complement of Accent() — used by chrome and views that style
// a secondary element to track the chosen theme.
func Secondary() lipgloss.Color { return active.secondary }

// TableStyles returns bubbles/table styles themed with the active accent — a bold
// accent header and a clearly-highlighted selected row (secondary text on the
// primary/accent background, a distinct colour pair from the default rows) — so the
// K9s-style tables in `ai ui` track the chosen theme (see `ai theme`). The Cell
// style is left at its default (padding only, no Foreground), so the highlight is
// applied AFTER the table's (ANSI-unaware) truncation rather than via inline cell
// colours, which would corrupt truncation.
func TableStyles() table.Styles {
	styles := table.DefaultStyles()
	styles.Header = styles.Header.Bold(true).Foreground(active.accent)
	styles.Selected = styles.Selected.Bold(true).
		Foreground(active.secondary).
		Background(active.accent)
	return styles
}

// cellPadding is the per-column horizontal padding of the bubbles-table Cell/Header
// style (DefaultStyles uses Padding(0, 1) → one space each side), counted when
// distributing width across columns so the rendered columns add up to the table
// width exactly.
const cellPadding = 2

// StretchColumns widens the given columns so they fill totalWidth exactly (each
// column gets +cellPadding for its cell padding). Without this the bubbles table
// pads each rendered row out to its width with UNSTYLED trailing spaces AFTER the
// Selected style wraps only the joined cells — so the selected-row highlight stops
// short of the right edge and looks ragged. Stretching the columns makes the joined
// row span the full width, so the Selected background fills the whole row. The slack
// is added to the LAST positive-width column (the description/value column), leaving
// fixed columns at their declared widths; a totalWidth too small to hold the columns
// is left unchanged. Returns a fresh slice (the input is not mutated).
func StretchColumns(columns []table.Column, totalWidth int) []table.Column {
	out := make([]table.Column, len(columns))
	copy(out, columns)
	used := 0
	lastPositive := -1
	for index, column := range out {
		if column.Width <= 0 {
			continue
		}
		used += column.Width + cellPadding
		lastPositive = index
	}
	if lastPositive < 0 {
		return out
	}
	if slack := totalWidth - used; slack > 0 {
		out[lastPositive].Width += slack
	}
	return out
}
