package views

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
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
