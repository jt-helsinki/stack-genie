package views

import (
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/jt-helsinki/stack-genie/internal/setup"
	"github.com/jt-helsinki/stack-genie/internal/ui"
)

// TestSubTabHitIndex pins the click hit-testing to renderSubTabBar's cell layout:
// prefix cell (subject + "  ▸ "), then one Padding(0,1) cell per title.
func TestSubTabHitIndex(test *testing.T) {
	subject := "my-app"
	titles := []string{"Workspace", "Logs", "Metrics"}
	prefix := lipgloss.Width(ui.Heading.Render(subject) + "  ▸ ")

	cases := []struct {
		name string
		x    int
		want int
	}{
		{name: "prefix is not a tab", x: 0, want: -1},
		{name: "last prefix cell", x: prefix - 1, want: -1},
		{name: "first cell of tab 0 (left pad)", x: prefix, want: 0},
		{name: "last cell of tab 0 (right pad)", x: prefix + lipgloss.Width("Workspace") + 1, want: 0},
		{name: "first cell of tab 1", x: prefix + lipgloss.Width("Workspace") + 2, want: 1},
		{name: "inside tab 2", x: prefix + lipgloss.Width("Workspace") + 2 + lipgloss.Width("Logs") + 2 + 1, want: 2},
		{name: "past the last tab", x: prefix + lipgloss.Width("Workspace") + lipgloss.Width("Logs") + lipgloss.Width("Metrics") + 6, want: -1},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			if got := subTabHitIndex(subject, titles, tc.x); got != tc.want {
				test.Errorf("subTabHitIndex(x=%d) = %d, want %d", tc.x, got, tc.want)
			}
		})
	}
}

// TestServiceDetailClickSubTab verifies the detail's bar toggles on click and a
// closed detail never handles one.
func TestServiceDetailClickSubTab(test *testing.T) {
	detail := newDetailForTest(setup.ServiceStatus{Name: "litellm", State: "running"}, "line", false)
	detail.name = "" // no service open yet
	if _, handled := detail.ClickSubTab(20); handled {
		test.Error("a detail with no open service must not handle clicks")
	}
	detail.name = "litellm"
	prefix := lipgloss.Width(ui.Heading.Render("litellm") + "  ▸ ")
	// Click the second tab ("Logs") — the detail starts on Service (index 0).
	_, handled := detail.ClickSubTab(prefix + lipgloss.Width("Service") + 2)
	if !handled {
		test.Fatal("a click on the Logs tab must be handled")
	}
	if detail.subIndex != serviceTabLogs {
		test.Errorf("subIndex = %d, want the Logs tab", detail.subIndex)
	}
	// Clicking the tab that is already active is handled but does not toggle back.
	_, handled = detail.ClickSubTab(prefix + lipgloss.Width("Service") + 2)
	if !handled || detail.subIndex != serviceTabLogs {
		test.Errorf("re-click must be a handled no-op, subIndex = %d", detail.subIndex)
	}
}
