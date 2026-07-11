package views

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/stack-genie/internal/ui"
)

// renderSubTabBar draws "<subject> ▸ [Tab0] Tab1 …" with the active tab filled in the
// theme accent — the shared two-level sub-tab bar used by the Workspaces hub
// (ProjectsHub) and the Services detail (ServiceDetail).
func renderSubTabBar(subject string, titles []string, active int) string {
	activeStyle := lipgloss.NewStyle().Bold(true).
		Foreground(lipgloss.Color("0")).Background(ui.Accent()).Padding(0, 1)
	inactiveStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("252")).Background(lipgloss.Color("238")).Padding(0, 1)

	cells := make([]string, 0, len(titles)+1)
	cells = append(cells, ui.Heading.Render(subject)+ui.Muted.Render("  ▸ "))
	for index, title := range titles {
		if index == active {
			cells = append(cells, activeStyle.Render(title))
			continue
		}
		cells = append(cells, inactiveStyle.Render(title))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cells...)
}

// subTabHitIndex maps a click column (x, in the bar's own coordinates) to the
// sub-tab index under it, mirroring renderSubTabBar's cell layout exactly: the
// subject+"  ▸ " prefix cell, then one Padding(0,1) cell per title. Returns -1
// when x is not on a tab (the prefix or past the end).
func subTabHitIndex(subject string, titles []string, x int) int {
	offset := lipgloss.Width(ui.Heading.Render(subject) + "  ▸ ")
	for index, title := range titles {
		width := lipgloss.Width(title) + 2 // Padding(0,1) adds one cell each side
		if x >= offset && x < offset+width {
			return index
		}
		offset += width
	}
	return -1
}
