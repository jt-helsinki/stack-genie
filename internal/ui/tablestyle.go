package ui

import (
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
)

// Accent returns the active theme's accent colour, for components that must style
// themselves (bubbles tables, the filepicker) to match the rest of the UI.
func Accent() lipgloss.Color { return active.accent }

// TableStyles returns bubbles/table styles themed with the active accent — a bold
// accent header and a bold accent-highlighted selected row — so the K9s-style
// tables in `ai ui` track the chosen theme (see `ai theme`).
func TableStyles() table.Styles {
	styles := table.DefaultStyles()
	styles.Header = styles.Header.Bold(true).Foreground(active.accent)
	styles.Selected = styles.Selected.Bold(true).Foreground(active.accent)
	return styles
}
