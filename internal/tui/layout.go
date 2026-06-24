// K9s-style chrome for the `ai ui` shell. The HEADER (top) is an ASCII wordmark on
// the left and the active context's key bindings laid out as a neat grid on the
// right. Below it is a tab bar with solid-background tabs (the active one filled
// with the theme accent). The active view's body sits in a rounded border, and the
// FOOTER carries the status line (role / gateway / scope / view). All chrome
// rendering and sizing math lives here; the root model in tui.go owns state and
// routing. The body is sized to fit INSIDE the border (see bodyContentSize).
package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// Chrome geometry. The header height is dynamic (logo + wrapped command grid), so
// it is measured at render time; the tab bar and footer are one line each, and the
// body is wrapped in a rounded border (two lines + two columns) with one column of
// inner padding per side.
const (
	headerGapRows = 1 // blank line between the header and the tab bar
	tabRows       = 1
	footerRows    = 1
	borderRows    = 2
	borderCols    = 2
	bodyPadX      = 1
	// commandColumns is how many key bindings sit per row in the header; logoGap is
	// the spacer between the logo and the command list.
	commandColumns = 4
	logoGap        = "      "
)

// aiLogo is the compact ASCII wordmark shown in the header's top-left corner.
var aiLogo = []string{
	"▄▀█ █",
	"█▀█ █",
}

// keyHint is one key binding (key + what it does) for the header command grid.
type keyHint struct{ key, desc string }

// bodyContentSize is the inner area passed to a view's SetSize: the window minus
// the (dynamic) header, the tab bar, the footer, and the border's two lines +
// padding. Both dimensions clamp to ≥1 so a tiny window never panics.
func (application *app) bodyContentSize() (width, height int) {
	width = application.width - borderCols - 2*bodyPadX
	chrome := lipgloss.Height(application.header()) + headerGapRows + tabRows + footerRows + borderRows
	height = application.height - chrome
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	return width, height
}

// header is the top block: the ASCII logo on the left and, to its right, the
// active context's key bindings as a grid (see commandsPanel).
func (application *app) header() string {
	logo := application.logo()
	panelWidth := application.width - lipgloss.Width(logo) - lipgloss.Width(logoGap)
	if panelWidth < 1 {
		panelWidth = 1
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, logo, logoGap, application.commandsPanel(panelWidth))
}

// logo renders the ASCII wordmark in the theme accent.
func (application *app) logo() string {
	return lipgloss.NewStyle().Bold(true).Foreground(ui.Accent()).Render(strings.Join(aiLogo, "\n"))
}

// commandsPanel lays the active context's key bindings out as aligned
// "<key>  action" columns — commandColumns per row, the bracketed key labels all
// padded to the same width so the actions line up, with a gutter between items and
// a left margin. Context-aware: an open overlay shows the overlay's keys;
// otherwise the active view's Hints() plus the global keys.
func (application *app) commandsPanel(_ int) string {
	hints := application.currentHints()
	if len(hints) == 0 {
		return ""
	}
	keyStyle := lipgloss.NewStyle().Bold(true).Foreground(ui.Accent())

	const keyGutter = 2 // spaces between the <key> label and its action
	const itemGap = 3   // spaces between items across a row
	keyWidth, descWidth := 0, 0
	for _, hint := range hints {
		if labelWidth := lipgloss.Width("<" + hint.key + ">"); labelWidth > keyWidth {
			keyWidth = labelWidth
		}
		if actionWidth := lipgloss.Width(hint.desc); actionWidth > descWidth {
			descWidth = actionWidth
		}
	}
	keyCell := lipgloss.NewStyle().Width(keyWidth + keyGutter)
	item := lipgloss.NewStyle().Width(keyWidth + keyGutter + descWidth + itemGap)

	var rows []string
	for start := 0; start < len(hints); start += commandColumns {
		end := start + commandColumns
		if end > len(hints) {
			end = len(hints)
		}
		cells := make([]string, 0, end-start)
		for _, hint := range hints[start:end] {
			label := keyCell.Render(keyStyle.Render("<" + hint.key + ">"))
			cells = append(cells, item.Render(label+ui.Muted.Render(hint.desc)))
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, cells...))
	}
	return lipgloss.NewStyle().MarginLeft(1).Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

// currentHints is the key bindings for the header grid: the active overlay's keys
// when one is open, else the active view's Hints() followed by the global keys.
func (application *app) currentHints() []keyHint {
	switch {
	case application.createView != nil:
		return parseHints(application.createView.Hints())
	case application.helpOpen:
		return []keyHint{{"any key", "close help"}}
	case application.paletteOpen:
		return []keyHint{{"type", "filter"}, {"↑/↓", "select"}, {"enter", "choose"}, {"esc", "close"}}
	}
	hints := parseHints(application.views[application.current].Hints())
	return append(hints,
		keyHint{"tab/←→", "switch"}, keyHint{":", "menu"}, keyHint{"?", "help"}, keyHint{"q", "quit"})
}

// parseHints splits a view's " · "-joined Hints() string into key/action pairs
// (the first token of each part is the key, the rest the action).
func parseHints(line string) []keyHint {
	var hints []keyHint
	for _, part := range strings.Split(line, " · ") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, desc, _ := strings.Cut(part, " ")
		hints = append(hints, keyHint{key: key, desc: desc})
	}
	return hints
}

// tabBar renders the views as solid-background tabs: the active tab is filled with
// the theme accent (and bracketed); inactive tabs sit on a muted fill. While the
// create overlay is open there is no active tab.
func (application *app) tabBar() string {
	activeStyle := lipgloss.NewStyle().Bold(true).
		Foreground(lipgloss.Color("0")).Background(ui.Accent()).Padding(0, 1)
	inactiveStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("252")).Background(lipgloss.Color("238")).Padding(0, 1)

	cells := make([]string, 0, len(application.views))
	for index, view := range application.views {
		title := view.Title()
		if application.createView == nil && index == application.current {
			cells = append(cells, activeStyle.Render("["+title+"]"))
			continue
		}
		cells = append(cells, inactiveStyle.Render(title))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cells...)
}

// body wraps the inner content in a rounded border coloured with the accent, sized
// to the window. The content (the active view, or an overlay) renders INSIDE it.
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

// activeTitle is the active view's title, or the create overlay's while it is open.
func (application *app) activeTitle() string {
	if application.createView != nil {
		return application.createView.Title()
	}
	return application.views[application.current].Title()
}
