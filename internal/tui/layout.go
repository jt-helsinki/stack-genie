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
	// bodyPadY is the inner vertical padding inside the body border — kept equal to
	// bodyPadX so the content sits the SAME margin off the top/bottom border as it
	// does off the left/right (a symmetric 1-line / 1-col margin).
	bodyPadY = bodyPadX
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

// navCapturer is satisfied by a view that wants Tab/←→/esc for its own internal
// navigation (the Projects hub while a project is open). When it is capturing, the
// app delegates those keys to it instead of switching top-level tabs, and the
// header labels them as sub-tab navigation.
type navCapturer interface{ CapturesNav() bool }

// capturesNav reports whether view is currently capturing navigation keys.
func capturesNav(view View) bool {
	capturer, ok := view.(navCapturer)
	return ok && capturer.CapturesNav()
}

// bodyContentSize is the inner area passed to a view's SetSize: the window minus
// the (dynamic) header, the tab bar, the footer, and the border's two lines +
// padding. Both dimensions clamp to ≥1 so a tiny window never panics.
func (application *app) bodyContentSize() (width, height int) {
	width = application.width - borderCols - 2*bodyPadX
	chrome := lipgloss.Height(application.header()) + headerGapRows + tabRows + footerRows + borderRows
	height = application.height - chrome - 2*bodyPadY
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
	return lipgloss.NewStyle().Bold(true).Foreground(ui.Secondary()).Render(strings.Join(aiLogo, "\n"))
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
	case application.terminal != nil:
		if application.terminal.Exited() {
			return []keyHint{{"any key", "close"}}
		}
		return []keyHint{{"keys", "→ terminal"}, {"ctrl+q", "detach"}}
	case application.createView != nil:
		return parseHints(application.createView.Hints())
	case application.helpOpen:
		return []keyHint{{"any key", "close help"}}
	case application.paletteOpen:
		return []keyHint{{"type", "filter"}, {"↑/↓", "select"}, {"enter", "choose"}, {"esc", "close"}}
	}
	active := application.views[application.current]
	hints := parseHints(active.Hints())
	if capturesNav(active) {
		// Inside an open project: Tab cycles the sub-tabs and esc backs up.
		return append(hints,
			keyHint{"tab/←→", "sub-tab"}, keyHint{"esc", "back"},
			keyHint{":", "menu"}, keyHint{"?", "help"}, keyHint{"q", "quit"})
	}
	return append(hints,
		keyHint{"tab/←→", "switch tab"}, keyHint{":", "menu"}, keyHint{"?", "help"}, keyHint{"q", "quit"})
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
		Foreground(ui.Secondary()).Background(ui.Accent()).Padding(0, 1)
	inactiveStyle := lipgloss.NewStyle().
		Foreground(ui.Accent()).Background(ui.Secondary()).Padding(0, 1)

	cells := make([]string, 0, len(application.views))
	for index, view := range application.views {
		title := view.Title()
		if application.createView == nil && index == application.current {
			cells = append(cells, activeStyle.Render(title))
		} else {
			cells = append(cells, inactiveStyle.Render(title))
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cells...)
}

// body wraps the inner content in a rounded border coloured with the accent, sized
// to the window. The content (the active view, or an overlay) renders INSIDE it.
func (application *app) body(content string) string {
	width, height := application.bodyContentSize()
	// lipgloss Width(w)/Height(h) are the box dimensions INCLUDING padding (the content
	// area is w-2*padX × h-2*padY), so to give the content the full bodyContentSize we
	// add the padding back onto both dimensions. Without this the real content area is
	// 2*bodyPadX narrower / 2*bodyPadY shorter than every view was sized to, so
	// full-width rows (e.g. the selected-row highlight) overflow and wrap, and a
	// full-height table overflows the box. The symmetric Padding gives a 1-line margin
	// top/bottom and a 1-col margin left/right inside the border.
	border := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ui.Accent()).
		Padding(bodyPadY, bodyPadX).
		Width(width + 2*bodyPadX).
		Height(height + 2*bodyPadY)
	return border.Render(content)
}

// activeTitle is the active view's title, or the open overlay's (terminal label /
// create) while one is open.
func (application *app) activeTitle() string {
	if application.terminal != nil {
		return "term:" + application.terminal.Label()
	}
	if application.createView != nil {
		return application.createView.Title()
	}
	return application.views[application.current].Title()
}
