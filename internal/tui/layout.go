// K9s-style chrome for the `ai ui` shell: a styled header block, a horizontal tab
// bar with the active tab highlighted in the theme accent, a rounded border around
// the active view's body, and a context-sensitive footer. All chrome rendering and
// sizing math lives here; the root model in tui.go owns state and routing. The body
// content is sized so the active view fits INSIDE the border (see bodyContentSize).
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// Chrome geometry. The body is wrapped in a rounded border with one column of
// horizontal padding on each side; the header, tab bar, and footer are one line each.
// headerRows/tabRows/footerRows are the rows the chrome consumes above/below the body;
// borderRows/borderCols are the rounded border's two lines on each axis; bodyPadX is
// the inner padding column on each side.
const (
	headerRows = 1
	tabRows    = 1
	footerRows = 1
	borderRows = 2
	borderCols = 2
	bodyPadX   = 1
)

// bodyContentSize is the inner area passed to a view's SetSize: the window minus the
// header, tab bar, footer, the border's two lines, and the horizontal border+padding.
// Both dimensions are clamped to at least one so a tiny window never panics or goes
// negative.
func (application *app) bodyContentSize() (width, height int) {
	width = application.width - borderCols - 2*bodyPadX
	height = application.height - headerRows - tabRows - footerRows - borderRows
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	return width, height
}

// header is the top block: the bold-accent app name plus muted context
// (role / gateway / scope / active view). Kept to a single line.
func (application *app) header() string {
	scope := "server"
	if application.currentProject != "" {
		scope = "project:" + application.currentProject
	}
	name := ui.Heading.Render("ai ui")
	context := ui.Muted.Render(fmt.Sprintf("role:%s  gateway:%s  scope:%s  view:%s",
		application.role, application.gateway, scope, application.activeTitle()))
	return name + "  " + context
}

// tabBar renders a horizontal row of the views' Titles separated by a muted dot, with
// the active tab highlighted in the theme accent (bold + bracketed). While a modal
// overlay (create) is open there is no active tab, so nothing is highlighted.
func (application *app) tabBar() string {
	accent := lipgloss.NewStyle().Bold(true).Foreground(ui.Accent())
	separator := ui.Muted.Render(" · ")
	labels := make([]string, 0, len(application.views))
	for index, view := range application.views {
		title := view.Title()
		if application.createView == nil && index == application.current {
			labels = append(labels, accent.Render("["+title+"]"))
			continue
		}
		labels = append(labels, ui.Muted.Render(title))
	}
	return strings.Join(labels, separator)
}

// body wraps the inner content in a rounded border coloured with the accent, sized to
// the window. The content (the active view, or an overlay) renders INSIDE the border.
func (application *app) body(content string) string {
	width, height := application.bodyContentSize()
	border := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ui.Accent()).
		Padding(0, bodyPadX).
		Width(width).
		Height(height)
	return border.Render(content)
}

// activeTitle is the title shown in the header/help: the active view's, or the create
// overlay's title while it is open.
func (application *app) activeTitle() string {
	if application.createView != nil {
		return application.createView.Title()
	}
	return application.views[application.current].Title()
}
