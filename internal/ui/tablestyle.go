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
