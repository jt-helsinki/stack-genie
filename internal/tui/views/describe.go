package views

import (
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// describePane is the scrollable, read-only detail pane shared by the row-list
// views (the K9s-style "d" describe). It is inactive until show() is called; the
// host view routes input to it while active() and renders view() in its place.
type describePane struct {
	viewport viewport.Model
	on       bool
}

func newDescribePane() describePane {
	return describePane{viewport: viewport.New(0, 0)}
}

func (pane *describePane) active() bool { return pane.on }

func (pane *describePane) show(content string) {
	pane.on = true
	pane.viewport.SetContent(content)
	pane.viewport.GotoTop()
}

// showLive opens the pane pinned to the BOTTOM (newest first), for live logs.
func (pane *describePane) showLive(content string) {
	pane.on = true
	pane.viewport.SetContent(content)
	pane.viewport.GotoBottom()
}

// refresh replaces the content in place, keeping the scroll position — except it
// stays pinned to the bottom when already there, so live logs follow the tail
// until the user scrolls up to read history (k9s behaviour).
func (pane *describePane) refresh(content string) {
	atBottom := pane.viewport.AtBottom()
	pane.viewport.SetContent(content)
	if atBottom {
		pane.viewport.GotoBottom()
	}
}

func (pane *describePane) setSize(width, height int) {
	pane.viewport.Width = width
	if height > 0 {
		pane.viewport.Height = height
	}
}

// update routes input to the pane: esc backs out (closes it, per the app-wide
// esc-goes-up-one-level convention); other keys scroll. (The app intercepts q for
// quit before the view sees it, so it is not handled here.)
func (pane *describePane) update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		if key.String() == "esc" {
			pane.on = false
			return nil
		}
	}
	var cmd tea.Cmd
	pane.viewport, cmd = pane.viewport.Update(msg)
	return cmd
}

func (pane *describePane) view() string { return pane.viewport.View() }
