package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/create"
	"github.com/jt-helsinki/stack-genie/internal/project"
	"github.com/jt-helsinki/stack-genie/internal/state"
	"github.com/jt-helsinki/stack-genie/internal/tui/views"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
	"github.com/muesli/termenv"
)

// fakeView is a minimal View for exercising the app's routing/menu logic.
type fakeView struct{ title string }

func (view *fakeView) Init() tea.Cmd          { return nil }
func (view *fakeView) Update(tea.Msg) tea.Cmd { return nil }
func (view *fakeView) View() string           { return "fake-" + view.title }
func (view *fakeView) Title() string          { return view.title }
func (view *fakeView) Hints() string          { return "" }
func (view *fakeView) SetSize(int, int)       {}

func newTestApp(titles ...string) *app {
	views := make([]View, 0, len(titles))
	for _, title := range titles {
		views = append(views, &fakeView{title: title})
	}
	application := &app{views: views}
	return application
}

func qKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")} }
func esc() tea.KeyMsg  { return tea.KeyMsg{Type: tea.KeyEsc} }

func TestQuitKey(test *testing.T) {
	application := newTestApp("Services")
	application.Update(qKey())
	if !application.quitting {
		test.Fatal("q must set quitting")
	}
}

// leftClick builds a left-button press at (x, y).
func leftClick(x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}
}

func TestMouseClickSwitchesTopTab(test *testing.T) {
	application := newTestApp("Services", "Workspaces", "Settings")
	tabRow := lipgloss.Height(application.header()) + headerGapRows

	// "Services" renders 10 cells wide (title + 1 pad each side) — clicking just
	// past it lands on "Workspaces".
	application.Update(leftClick(lipgloss.Width("Services")+2+1, tabRow))
	if application.current != 1 {
		test.Fatalf("click on the second tab: current = %d, want 1", application.current)
	}

	// A click on the same row past every tab changes nothing.
	application.Update(leftClick(500, tabRow))
	if application.current != 1 {
		test.Errorf("click past the tabs must not switch, current = %d", application.current)
	}

	// A click on a different row changes nothing.
	application.Update(leftClick(2, tabRow+10))
	if application.current != 1 {
		test.Errorf("click off the tab bar must not switch, current = %d", application.current)
	}

	// Non-press / non-left events are ignored even on the bar.
	application.Update(tea.MouseMsg{X: 2, Y: tabRow, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	application.Update(tea.MouseMsg{X: 2, Y: tabRow, Action: tea.MouseActionPress, Button: tea.MouseButtonRight})
	if application.current != 1 {
		test.Errorf("release/right-click must not switch, current = %d", application.current)
	}

	// While a modal overlay is open, tab clicks are inert.
	application.helpOpen = true
	application.Update(leftClick(2, tabRow))
	if application.current != 1 {
		test.Errorf("click during the help overlay must not switch, current = %d", application.current)
	}
}

// initTrackingView records Init() calls so a test can assert a tab's data load fires when
// the tab is activated.
type initTrackingView struct {
	fakeView
	inits int
}

func (view *initTrackingView) Init() tea.Cmd { view.inits++; return nil }

// REGRESSION GUARD: switching to a top-level tab — by MOUSE CLICK or KEYBOARD — must fire
// the now-active view's Init() so its async data loads. Previously the mouse tab-click
// switched without loading, so Workspaces / Local Models / Cloud Models / API Keys sat on
// "loading…" forever (their startup Init result was routed to the startup-active tab and
// dropped, and clicking never re-loaded them).
func TestTabActivationFiresViewInit(test *testing.T) {
	build := func() (*app, *initTrackingView) {
		tracked := &initTrackingView{fakeView: fakeView{title: "Workspaces"}}
		return &app{views: []View{&fakeView{title: "Services"}, tracked, &fakeView{title: "Settings"}}}, tracked
	}

	// Mouse click on the second tab's cell.
	application, tracked := build()
	tabRow := lipgloss.Height(application.header()) + headerGapRows
	application.Update(leftClick(lipgloss.Width("Services")+2+1, tabRow))
	if application.current != 1 {
		test.Fatalf("mouse click: current = %d, want 1", application.current)
	}
	if tracked.inits == 0 {
		test.Error("mouse click on a tab must fire the activated view's Init() (data load)")
	}

	// Keyboard number key.
	application, tracked = build()
	application.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if application.current != 1 || tracked.inits == 0 {
		test.Errorf("number-key nav must activate + Init: current=%d inits=%d", application.current, tracked.inits)
	}

	// Keyboard tab key.
	application, tracked = build()
	application.Update(tea.KeyMsg{Type: tea.KeyTab})
	if application.current != 1 || tracked.inits == 0 {
		test.Errorf("tab-key nav must activate + Init: current=%d inits=%d", application.current, tracked.inits)
	}
}

// newTestHubApp builds an app whose Projects tab (index 1) is a real ProjectsHub
// over the given sub-tab titles, with Services/Models as the flanking top tabs.
func newTestHubApp(test *testing.T, subTitles ...string) (*app, *views.ProjectsHub) {
	test.Helper()
	detail := views.NewProject(
		func(string) (project.Entry, bool, error) { return project.Entry{Name: "app"}, true, nil },
		nil,
	)
	subViews := make([]views.Screen, 0, len(subTitles))
	for index, title := range subTitles {
		if index == 0 {
			subViews = append(subViews, detail) // first sub-tab is the project detail
			continue
		}
		subViews = append(subViews, &fakeView{title: title})
	}
	hub := views.NewProjectsHub(&fakeView{title: "Projects"}, subViews, subTitles)
	application := &app{
		views:         []View{&fakeView{title: "Services"}, hub, &fakeView{title: "Models"}},
		projectsHub:   hub,
		projectDetail: detail,
		projectsIndex: 1,
	}
	return application, hub
}

func TestProjectSelectedOpensHub(test *testing.T) {
	application, hub := newTestHubApp(test, "Project", "Network")

	application.Update(views.ProjectSelectedMsg{Name: "app", Path: "/p/app"})
	if application.currentProject != "app" {
		test.Fatalf("currentProject = %q, want app", application.currentProject)
	}
	if application.current != application.projectsIndex {
		test.Fatalf("current view = %d, want %d (Projects hub)", application.current, application.projectsIndex)
	}
	if !hub.CapturesNav() {
		test.Fatal("selecting a project should open it in the hub (CapturesNav must be true)")
	}
}

// fakeEscView is a sub-view with a toggleable open overlay, to test that the hub
// lets a sub-view absorb esc before backing out of the project.
type fakeEscView struct {
	fakeView
	overlayOpen bool
}

func (view *fakeEscView) WantsEsc() bool { return view.overlayOpen }
func (view *fakeEscView) Update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyEsc {
		view.overlayOpen = false // esc closes the overlay
	}
	return nil
}

// TestProjectsHubEscClosesSubViewOverlayBeforeBackingOut: when a sub-tab has an
// open overlay, esc closes the overlay first; a second esc backs out to the
// switcher (esc goes up one level).
func TestProjectsHubEscClosesSubViewOverlayBeforeBackingOut(test *testing.T) {
	escView := &fakeEscView{fakeView: fakeView{title: "Secrets"}, overlayOpen: true}
	hub := views.NewProjectsHub(&fakeView{title: "Projects"}, []views.Screen{escView}, []string{"Secrets"})
	application := &app{
		views:         []View{&fakeView{title: "Services"}, hub, &fakeView{title: "Models"}},
		projectsHub:   hub,
		projectsIndex: 1,
	}
	application.Update(views.ProjectSelectedMsg{Name: "app"})

	// First esc: the sub-view's overlay absorbs it; still inside the project.
	application.Update(esc())
	if escView.overlayOpen {
		test.Fatal("esc should close the sub-view's overlay first")
	}
	if !hub.CapturesNav() {
		test.Fatal("the first esc must NOT back out of the project (overlay closes first)")
	}
	// Second esc: nothing left to close, so back out to the switcher.
	application.Update(esc())
	if hub.CapturesNav() {
		test.Fatal("the second esc should back out to the switcher")
	}
}

// TestProjectsHubSubTabNavAndEscBack: once a project is open the hub captures
// Tab/esc — Tab cycles sub-tabs (the top-level tab stays put) and esc backs up to
// the switcher, after which Tab cycles top-level tabs again.
func TestProjectsHubSubTabNavAndEscBack(test *testing.T) {
	application, hub := newTestHubApp(test, "Project", "Network", "Context")
	application.Update(views.ProjectSelectedMsg{Name: "app"})

	// Tab inside an open project cycles sub-tabs — the top-level tab is unchanged.
	application.Update(tabKey())
	if application.current != application.projectsIndex {
		test.Fatalf("tab inside an open project must not switch top-level tab; current=%d", application.current)
	}
	// esc backs out to the switcher (hub stops capturing), staying on Projects.
	application.Update(esc())
	if hub.CapturesNav() {
		test.Fatal("esc should back out of the open project (switcher mode)")
	}
	if application.current != application.projectsIndex {
		test.Fatalf("esc should stay on the Projects tab; current=%d", application.current)
	}
	// With the project closed, Tab cycles TOP-level tabs again (Projects -> Models).
	application.Update(tabKey())
	if application.current != 2 {
		test.Fatalf("tab in switcher mode should switch top-level tab; current=%d", application.current)
	}
}

func TestNewProjectRequestedOpensCreateOverlay(test *testing.T) {
	application := &app{cwd: test.TempDir(), views: []View{&fakeView{title: "Projects"}}}

	application.Update(views.NewProjectRequestedMsg{})
	if application.createView == nil {
		test.Fatal("NewProjectRequestedMsg must open the create overlay")
	}
}

func TestCreateCancelledClosesOverlay(test *testing.T) {
	application := &app{cwd: test.TempDir(), views: []View{&fakeView{title: "Projects"}}}
	application.createView = views.NewCreate(application.cwd, nil, 0, 0)

	application.Update(views.CreateCancelledMsg{})
	if application.createView != nil {
		test.Fatal("CreateCancelledMsg must close the overlay")
	}
}

func TestCreateConfirmedClosesOverlayAndRunsCreate(test *testing.T) {
	application := &app{cwd: test.TempDir(), projectsIndex: 0, views: []View{&fakeView{title: "Projects"}}}
	application.createView = views.NewCreate(application.cwd, nil, 0, 0)

	// CreateConfirmedMsg closes the overlay, marks the workspace as creating, and
	// returns the in-process create.Execute command (run off the event loop).
	_, cmd := application.Update(views.CreateConfirmedMsg{Spec: project.Spec{Name: "demo", Root: test.TempDir()}})
	if application.createView != nil {
		test.Fatal("CreateConfirmedMsg must close the overlay")
	}
	if application.creating != "demo" {
		test.Fatalf("CreateConfirmedMsg must mark the workspace creating; got %q", application.creating)
	}
	if cmd == nil {
		test.Fatal("CreateConfirmedMsg must return the in-process create command")
	}
}

// ExecRequestedMsg runs the interactive shell in the user's REAL terminal via
// tea.ExecProcess (suspends the TUI) — it does NOT open the embedded overlay. It
// returns the ExecProcess command and leaves application.terminal nil.
func TestExecRequestedRunsInRealTerminal(test *testing.T) {
	application := &app{
		views:         []View{&fakeView{title: "Project"}},
		projectDetail: views.NewProject(func(string) (project.Entry, bool, error) { return project.Entry{}, false, nil }, nil),
	}
	_, cmd := application.Update(views.ExecRequestedMsg{Project: "app"})
	if application.terminal != nil {
		test.Fatal("ExecRequestedMsg must NOT open the embedded overlay (it suspends to the real terminal)")
	}
	if cmd == nil {
		test.Fatal("ExecRequestedMsg must return the ExecProcess command")
	}
}

// AttachRequestedMsg runs `ai attach` in the user's REAL terminal via tea.ExecProcess
// (suspends the TUI) — not the embedded overlay.
func TestAttachRequestedRunsInRealTerminal(test *testing.T) {
	sessionsView := views.NewSessions(
		func() ([]workspace.Session, error) { return nil, nil },
		func(string) error { return nil },
		func() string { return "app" },
	)
	application := &app{
		views:        []View{sessionsView},
		sessionsView: sessionsView,
	}
	_, cmd := application.Update(views.AttachRequestedMsg{Project: "app", Session: "shell"})
	if cmd == nil {
		test.Fatal("AttachRequestedMsg must return the ExecProcess command")
	}
	if application.terminal != nil {
		test.Fatal("AttachRequestedMsg must NOT open the embedded overlay (it suspends to the real terminal)")
	}
}

func TestHelpToggle(test *testing.T) {
	application := newTestApp("Services")

	application.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	if !application.helpOpen {
		test.Fatal("? must open the help overlay")
	}
	if !strings.Contains(application.helpView(), "Global") {
		test.Error("help should list the global key bindings")
	}
	// Any key dismisses it.
	application.Update(qKey())
	if application.helpOpen {
		test.Fatal("any key must close the help overlay")
	}
}

func TestWindowSizeSetsViewportWithoutPanic(test *testing.T) {
	application := newTestApp("Services")
	// A tiny window must clamp the body height to >=1, not go negative.
	application.Update(tea.WindowSizeMsg{Width: 80, Height: 1})
	if application.width != 80 {
		test.Fatalf("width = %d, want 80", application.width)
	}
}

// fillView is a View whose content fills exactly the content size it is given, so the
// body's inner margins can be measured against fully-occupied content.
type fillView struct {
	width, height int
}

func (view *fillView) Init() tea.Cmd          { return nil }
func (view *fillView) Update(tea.Msg) tea.Cmd { return nil }
func (view *fillView) Title() string          { return "Fill" }
func (view *fillView) Hints() string          { return "" }
func (view *fillView) SetSize(width, height int) {
	view.width, view.height = width, height
}
func (view *fillView) View() string {
	row := strings.Repeat("X", view.width)
	rows := make([]string, view.height)
	for index := range rows {
		rows[index] = row
	}
	return strings.Join(rows, "\n")
}

// The body must total the window dimensions (no overflow / no right-or-bottom gap) and
// carry a SYMMETRIC inner margin: the blank rows above/below the content and the blank
// columns left/right of it are all equal to bodyPadX (== bodyPadY). This pins part A
// (the vertical margin) against the existing horizontal margin.
func TestBodySymmetricMargin(test *testing.T) {
	application := &app{views: []View{&fillView{}}, width: 100, height: 40}
	application.resizeViews()
	contentWidth, contentHeight := application.bodyContentSize()

	rendered := application.body(application.views[0].View())
	lines := strings.Split(rendered, "\n")

	// Total body height = content + both vertical pads + the two border rows, and it
	// must equal what JoinVertical will stack into the window (chrome + this).
	wantBodyHeight := contentHeight + 2*bodyPadY + borderRows
	if len(lines) != wantBodyHeight {
		test.Fatalf("body height = %d lines, want %d (content %d + 2*pad %d + border %d)",
			len(lines), wantBodyHeight, contentHeight, 2*bodyPadY, borderRows)
	}
	if lipgloss.Width(rendered) != contentWidth+2*bodyPadX+borderCols {
		test.Fatalf("body width = %d, want %d", lipgloss.Width(rendered), contentWidth+2*bodyPadX+borderCols)
	}

	// The content rows are the 'X' rows; the inner blank margin above the first and
	// below the last must each equal bodyPadY, and equal the horizontal margin bodyPadX.
	firstContent, lastContent := -1, -1
	for index, line := range lines {
		if strings.Contains(line, "X") {
			if firstContent < 0 {
				firstContent = index
			}
			lastContent = index
		}
	}
	if firstContent < 0 {
		test.Fatalf("no content rows rendered:\n%s", rendered)
	}
	topGap := firstContent - 1                // minus the top border row
	bottomGap := len(lines) - 2 - lastContent // minus the bottom border row
	if topGap != bodyPadY || bottomGap != bodyPadY {
		test.Fatalf("vertical margins: top=%d bottom=%d, want both %d", topGap, bottomGap, bodyPadY)
	}
	if topGap != bodyPadX || bottomGap != bodyPadX {
		test.Fatalf("vertical margin (%d/%d) must equal the horizontal margin %d", topGap, bottomGap, bodyPadX)
	}
}

func tabKey() tea.KeyMsg      { return tea.KeyMsg{Type: tea.KeyTab} }
func shiftTabKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyShiftTab} }
func rightKey() tea.KeyMsg    { return tea.KeyMsg{Type: tea.KeyRight} }
func leftKey() tea.KeyMsg     { return tea.KeyMsg{Type: tea.KeyLeft} }
func digit(value string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)}
}

// TestHeaderShowsLogoAndCommands: the header has the ASCII logo and the active
// context's command grid (incl. the global keys).
func TestHeaderShowsLogoAndCommands(test *testing.T) {
	application := newTestApp("Services")
	application.width = 120
	header := application.header()
	if !strings.Contains(header, "█") {
		test.Error("header should contain the ASCII logo")
	}
	if !strings.Contains(header, "help") || !strings.Contains(header, "quit") {
		test.Errorf("header command grid missing global keys: %q", header)
	}
}

// TestFooterShowsContext: the status line (moved down from the header) carries
// role / gateway / scope.
func TestFooterShowsContext(test *testing.T) {
	application := &app{views: []View{&fakeView{title: "Services"}}, role: "standalone", gateway: "http://x:18787"}
	footer := application.footer()
	if !strings.Contains(footer, "role:standalone") || !strings.Contains(footer, "scope:server") {
		test.Errorf("footer missing context: %q", footer)
	}
}

// TestTabBarListsAllTitles confirms every view's Title appears in the tab bar.
func TestTabBarListsAllTitles(test *testing.T) {
	titles := []string{"Services", "Projects", "Project", "Network", "Context"}
	application := newTestApp(titles...)
	bar := application.tabBar()
	for _, title := range titles {
		if !strings.Contains(bar, title) {
			test.Errorf("tab bar %q missing title %q", bar, title)
		}
	}
}

// TestTabBarHighlightsActive checks the active tab is styled differently from the
// inactive ones. The highlight is colour-only (accent background), so the test
// forces a colour profile — otherwise lipgloss strips styling on the non-TTY test
// output and the active/inactive cells would be indistinguishable.
func TestTabBarHighlightsActive(test *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	test.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	onProjects := newTestApp("Services", "Projects", "Models")
	onProjects.current = 1
	onServices := newTestApp("Services", "Projects", "Models")
	onServices.current = 0

	// All titles still render.
	for _, title := range []string{"Services", "Projects", "Models"} {
		if !strings.Contains(onProjects.tabBar(), title) {
			test.Errorf("tab bar missing title %q", title)
		}
	}
	// Moving the active tab changes the rendered bar even though the titles are
	// identical — i.e. the active tab is highlighted distinctly.
	if onProjects.tabBar() == onServices.tabBar() {
		test.Error("the active tab should render differently from the inactive tabs")
	}
}

// TestTabKeysCycle checks Tab/Shift+Tab and →/← move application.current, wrapping.
func TestTabKeysCycle(test *testing.T) {
	application := newTestApp("Services", "Projects", "Models")

	application.Update(tabKey())
	if application.current != 1 {
		test.Fatalf("after tab current = %d, want 1", application.current)
	}
	application.Update(rightKey())
	if application.current != 2 {
		test.Fatalf("after right current = %d, want 2", application.current)
	}
	// Tab past the end wraps to the first tab.
	application.Update(tabKey())
	if application.current != 0 {
		test.Fatalf("tab past end should wrap to 0, got %d", application.current)
	}
	// Shift+Tab / ← from the first tab wrap to the last.
	application.Update(shiftTabKey())
	if application.current != 2 {
		test.Fatalf("shift+tab from 0 should wrap to 2, got %d", application.current)
	}
	application.Update(leftKey())
	if application.current != 1 {
		test.Fatalf("after left current = %d, want 1", application.current)
	}
}

// TestNumberKeysJumpToTab checks 1-9 jump to that tab (1-based) and out-of-range
// numbers are ignored.
func TestNumberKeysJumpToTab(test *testing.T) {
	application := newTestApp("Services", "Projects", "Models")

	application.Update(digit("3"))
	if application.current != 2 {
		test.Fatalf("'3' should select index 2, got %d", application.current)
	}
	application.Update(digit("1"))
	if application.current != 0 {
		test.Fatalf("'1' should select index 0, got %d", application.current)
	}
	// '9' is out of range (only 3 views) — must be a no-op.
	application.Update(digit("9"))
	if application.current != 0 {
		test.Fatalf("out-of-range '9' should not move; got %d", application.current)
	}
}

// TestTinyWindowClampsBodySize checks a tiny window yields a body size of >=1 on both
// axes without panicking, and that the chrome still renders.
func TestTinyWindowClampsBodySize(test *testing.T) {
	application := newTestApp("Services", "Projects")
	application.Update(tea.WindowSizeMsg{Width: 2, Height: 2})
	width, height := application.bodyContentSize()
	if width < 1 || height < 1 {
		test.Fatalf("body size = %dx%d, both must be >=1", width, height)
	}
	// View() must not panic on a degenerate window.
	if application.View() == "" {
		test.Error("View() should render chrome even on a tiny window")
	}
}

// capturingTestView is a view with an always-open inline prompt: it records the keys
// it receives, to prove the app routes global keys (q/:) to it, not the shortcuts.
type capturingTestView struct {
	fakeView
	got []string
}

func (view *capturingTestView) CapturingInput() bool { return true }
func (view *capturingTestView) Update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		view.got = append(view.got, key.String())
	}
	return nil
}

// While a view captures text input, global keys (q, etc.) reach the view instead of
// triggering the global shortcut (so typed characters aren't stolen).
func TestCapturingViewReceivesGlobalKeys(test *testing.T) {
	view := &capturingTestView{fakeView: fakeView{title: "Network"}}
	application := &app{views: []View{view}}

	application.Update(qKey())
	if application.quitting {
		test.Fatal("q must NOT quit while a view is capturing input")
	}
	if len(view.got) != 1 || view.got[0] != "q" {
		test.Fatalf("the capturing view should receive 'q', got %v", view.got)
	}
}

// A delete lifecycle action opens the confirm terminal overlay (it prompts inline);
// start/stop/restart do not (they run detached with a spinner) — tested via the
// Project view spinner, not here, to avoid spawning a real detached process.
func TestWorkspaceDeleteOpensOverlay(test *testing.T) {
	application := &app{
		views: []View{&fakeView{title: "Workspace"}},
		projectDetail: views.NewProject(
			func(string) (project.Entry, bool, error) { return project.Entry{}, false, nil },
			nil,
		),
	}
	application.Update(views.WorkspaceActionRequestedMsg{Action: "delete", Project: "app"})
	if application.terminal == nil {
		test.Fatal("a delete action must open the confirm terminal overlay")
	}
}

func TestWorkspaceLogReadableDuringPendingStart(test *testing.T) {
	application := &app{
		currentProject: "app",
		lifecycle:      &lifecycleOp{project: "app", action: "start"},
	}
	if !application.workspaceLogReadable() {
		test.Fatal("workspace log should poll while start is pending so startup diagnostics stream")
	}
	application.lifecycle.action = "restart"
	if !application.workspaceLogReadable() {
		test.Fatal("workspace log should poll while restart is pending so boot diagnostics stream")
	}
}

func pgDownKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyPgDown} }

// tallView is a no-sub-tab view whose content is always taller than any pane, so it
// overflows the body and exercises the body-level scroll. It records the keys it
// receives so a test can prove the body scroll keys are NOT delegated to it.
type tallView struct {
	fakeView
	got []string
}

func (view *tallView) Update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		view.got = append(view.got, key.String())
	}
	return nil
}
func (view *tallView) View() string {
	rows := make([]string, 200)
	for index := range rows {
		rows[index] = fmt.Sprintf("line-%03d", index)
	}
	return strings.Join(rows, "\n")
}

// navTallView is the same overflowing content but reports CapturesNav() — i.e. a tab
// WITH sub-tabs — so the outer body must NOT scroll; the keys are delegated to it.
type navTallView struct{ tallView }

func (view *navTallView) CapturesNav() bool { return true }

// shortRecordingView fits the pane (never overflows) and records the keys it gets, so a
// test can prove a wheel event is delegated to the view as up/down.
type shortRecordingView struct {
	fakeView
	got []string
}

func (view *shortRecordingView) Update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		view.got = append(view.got, key.String())
	}
	return nil
}
func (view *shortRecordingView) View() string { return "one line" }

func wheel(up bool) tea.MouseMsg {
	button := tea.MouseButtonWheelDown
	if up {
		button = tea.MouseButtonWheelUp
	}
	return tea.MouseMsg{Action: tea.MouseActionPress, Button: button}
}

// TestMouseWheelScrollsFocusedList: on a non-overflowing pane the wheel is delegated to
// the active view as up/down (moves a list/table selection); an open overlay swallows it.
func TestMouseWheelScrollsFocusedList(test *testing.T) {
	view := &shortRecordingView{}
	application := &app{views: []View{view}, bodyViewport: viewport.New(0, 0)}
	application.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	application.Update(wheel(false)) // wheel down
	application.Update(wheel(true))  // wheel up
	if len(view.got) != 2 || view.got[0] != "down" || view.got[1] != "up" {
		test.Fatalf("wheel must delegate down/up to the view, got %v", view.got)
	}

	application.helpOpen = true
	application.Update(wheel(false))
	if len(view.got) != 2 {
		test.Errorf("wheel must be inert while an overlay is open, got %v", view.got)
	}
}

// TestMouseWheelScrollsOverflowingBody: on a no-sub-tab pane that overflows, the wheel
// scrolls the outer body viewport (rendered output changes) and is NOT delegated.
func TestMouseWheelScrollsOverflowingBody(test *testing.T) {
	view := &tallView{}
	application := &app{views: []View{view}, bodyViewport: viewport.New(0, 0)}
	application.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	first := application.View()
	if !application.bodyOverflowing() {
		test.Fatal("a 200-line view must overflow a 24-row window")
	}
	application.Update(wheel(false))
	if application.View() == first {
		test.Fatal("wheel-down should scroll the overflowing body pane")
	}
	for _, key := range view.got {
		if key == "down" || key == "up" {
			test.Fatal("body scroll must not be delegated to the view on wheel")
		}
	}
}

// TestScrollsBodyOnlyForTabsWithoutSubTabs: a plain tab gets the body-level scroll; a
// nav-capturing tab (one with sub-tabs) does not, and an open overlay disables it.
func TestScrollsBodyOnlyForTabsWithoutSubTabs(test *testing.T) {
	plain := &app{views: []View{&fakeView{title: "Settings"}}}
	if !plain.scrollsBody() {
		test.Fatal("a tab without sub-tabs should get the body-level scroll")
	}
	navApp := &app{views: []View{&navTallView{}}}
	if navApp.scrollsBody() {
		test.Fatal("a tab WITH sub-tabs must NOT scroll the outer body")
	}
	// An open overlay (help) suspends the body scroll regardless of the tab.
	plain.helpOpen = true
	if plain.scrollsBody() {
		test.Fatal("an open overlay must disable the body scroll")
	}
}

// TestBodyPaneScrollsWhenOverflowing: on a no-sub-tab tab whose content overflows the
// pane, PgDn scrolls the body viewport (the rendered body changes), the keystroke is
// NOT delegated to the view, and the chrome stays intact (no overflow break).
func TestBodyPaneScrollsWhenOverflowing(test *testing.T) {
	view := &tallView{}
	application := &app{views: []View{view}, bodyViewport: viewport.New(0, 0)}
	application.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	first := application.View() // sets the viewport content
	if !application.bodyOverflowing() {
		test.Fatal("a 200-line view must overflow a 24-row window")
	}
	application.Update(pgDownKey())
	second := application.View()
	if first == second {
		test.Fatal("PgDn should scroll the body pane (rendered output unchanged)")
	}
	for _, key := range view.got {
		if key == "pgdown" {
			test.Fatal("PgDn must drive the body scroll, not be delegated to the view")
		}
	}
	if !strings.Contains(second, "ai ui") {
		test.Error("the footer chrome must survive an overflowing pane (no overflow break)")
	}
}

// TestTabWithSubTabsDelegatesScrollKeys: a nav-capturing tab (sub-tabs) does not use
// the outer body scroll — PgDn is delegated to the view (whose sub-panes scroll).
func TestTabWithSubTabsDelegatesScrollKeys(test *testing.T) {
	view := &navTallView{}
	application := &app{views: []View{view}, bodyViewport: viewport.New(0, 0)}
	application.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	application.View()

	application.Update(pgDownKey())
	found := false
	for _, key := range view.got {
		if key == "pgdown" {
			found = true
		}
	}
	if !found {
		test.Fatal("a tab with sub-tabs should delegate PgDn to the view, not scroll the outer pane")
	}
}

// TestWorkspaceConfigFieldsReflectDeclaredConfig verifies the Sandbox Configuration
// block is built from config.yaml (so published-port / resource edits reflect).
func TestWorkspaceConfigFieldsReflectDeclaredConfig(test *testing.T) {
	projectConfig := &config.Config{
		OS:        "ubuntu",
		Workspace: config.WorkspaceConfig{CPULimit: 2, MemoryLimit: "4G"},
		Network: config.NetworkConfig{PublishPorts: []config.PortMapping{
			{Host: 8080, Guest: 8080}, {Host: 9000, Guest: 3000},
		}},
	}
	got := map[string]string{}
	for _, field := range workspaceConfigFields(projectConfig) {
		got[field.Label] = field.Value
	}
	if got["published ports"] != "8080, 9000→3000" {
		test.Errorf("published ports = %q, want '8080, 9000→3000'", got["published ports"])
	}
	if got["vcpus"] != "2" || got["memory"] != "4G" {
		test.Errorf("resources = %q/%q, want 2/4G", got["vcpus"], got["memory"])
	}
	if publishPortsValue(nil) != "none" {
		test.Errorf("empty ports = %q, want 'none'", publishPortsValue(nil))
	}
}

// TestCreatingSwallowsNavKeys verifies the "creating…" pane is MODAL: nav keys are
// swallowed while a create runs, so the tab bar can't desync from the fixed pane.
func TestCreatingSwallowsNavKeys(test *testing.T) {
	application := newTestApp("Services", "Workspaces")
	application.current = 0
	application.creating = "app"
	application.Update(tabKey())
	if application.current != 0 {
		test.Errorf("tab must be swallowed while creating; current = %d, want 0", application.current)
	}
	if application.creating != "app" {
		test.Error("a swallowed key must not clear the creating state")
	}
}

// TestCreateProgressAccumulates verifies streamed create.Progress builds the step log +
// a live download bar in the creating pane.
func TestCreateProgressAccumulates(test *testing.T) {
	application := newTestApp("Services")
	application.creating = "app"
	ch := make(chan create.Progress)
	application.Update(createProgressMsg{progress: create.Progress{Step: "scaffolding"}, ch: ch})
	application.Update(createProgressMsg{progress: create.Progress{Step: "pulling model"}, ch: ch})
	application.Update(createProgressMsg{progress: create.Progress{Step: "pulling model", Completed: 1, Total: 2}, ch: ch})

	if len(application.createSteps) != 2 || application.createSteps[0] != "scaffolding" || application.createSteps[1] != "pulling model" {
		test.Errorf("step log = %v, want [scaffolding, pulling model]", application.createSteps)
	}
	if application.createPull.Total != 2 || application.createPull.Completed != 1 {
		test.Errorf("createPull = %+v, want the live pull frame", application.createPull)
	}
	view := application.creatingView()
	for _, want := range []string{"scaffolding", "pulling model", "50%"} {
		if !strings.Contains(view, want) {
			test.Errorf("creating view missing %q:\n%s", want, view)
		}
	}
}

// TestReadLatestLifecycleLogAppendsCavemanLog verifies the Logs view's lifecycle
// reader concatenates the detached Caveman install log (which is written past the
// end of `ai start`) after the start transcript, so both are visible in the pane.
func TestReadLatestLifecycleLogAppendsCavemanLog(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	root := filepath.Join(home, "projects", "app")
	runDir := filepath.Join(root, ".ai-platform", "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "start.log"), []byte("▸ booting microVM\n"), 0o644); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "caveman-install.log"), []byte("cloning caveman...\n"), 0o644); err != nil {
		test.Fatal(err)
	}
	index := state.NewProjectsIndex()
	index.Projects["app"] = state.ProjectIndexEntry{Path: root}
	if err := state.SaveIndex(index); err != nil {
		test.Fatal(err)
	}

	content, ok := readLatestLifecycleLog("app")
	if !ok {
		test.Fatal("expected a lifecycle log")
	}
	if !strings.Contains(content, "booting microVM") {
		test.Errorf("missing the start transcript:\n%s", content)
	}
	if !strings.Contains(content, "caveman install (background)") || !strings.Contains(content, "cloning caveman") {
		test.Errorf("missing the appended caveman install log:\n%s", content)
	}
}

// TestWheelTargetRouting pins which surface the wheel drives per app state — in
// particular that the create wizard overlay receives wheel scroll (the reported
// gap: wizard lists weren't scrollable), while the terminal/creating overlays don't.
func TestWheelTargetRouting(test *testing.T) {
	base := func() *app {
		return &app{views: []View{&shortRecordingView{}}, bodyViewport: viewport.New(80, 24)}
	}

	if got := base().wheelTarget(); got != wheelActiveView {
		test.Errorf("plain view: wheelTarget = %d, want wheelActiveView", got)
	}

	withCreate := base()
	withCreate.createView = views.NewCreate(test.TempDir(), nil, 16, 12)
	if got := withCreate.wheelTarget(); got != wheelCreate {
		test.Errorf("create wizard open: wheelTarget = %d, want wheelCreate (its lists must scroll)", got)
	}

	withHelp := base()
	withHelp.helpOpen = true
	if got := withHelp.wheelTarget(); got != wheelBody {
		test.Errorf("help open: wheelTarget = %d, want wheelBody", got)
	}

	withCreating := base()
	withCreating.creating = "app"
	if got := withCreating.wheelTarget(); got != wheelNone {
		test.Errorf("creating pane: wheelTarget = %d, want wheelNone (modal)", got)
	}
}
