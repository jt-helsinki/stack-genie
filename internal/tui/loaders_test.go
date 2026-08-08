package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/project"
	"github.com/jt-helsinki/stack-genie/internal/setup"
	"github.com/jt-helsinki/stack-genie/internal/state"
	"github.com/jt-helsinki/stack-genie/internal/tui/views"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
)

// seedProjectIndex writes a global projects index with one entry so the
// state-backed loaders (projectInfo/resolveProjectRoot/lifecycleLogPath) resolve.
func seedProjectIndex(test *testing.T, name, root string) {
	test.Helper()
	index := state.NewProjectsIndex()
	index.Projects[name] = state.ProjectIndexEntry{Path: root}
	if err := state.SaveIndex(index); err != nil {
		test.Fatalf("save index: %v", err)
	}
}

// TestCatalogIDsForCredentials pins the credential-prefix → catalog-id mapping the
// resync uses: only routable providers with a matching keyed prefix are returned.
func TestCatalogIDsForCredentials(test *testing.T) {
	cat := apiKeyTestCatalog(test)

	if ids := catalogIDsForCredentials(cat, nil); len(ids) != 0 {
		test.Errorf("no credentials must yield no catalog ids, got %v", ids)
	}

	prefix, ok := litellm.LiteLLMPrefix("openai")
	if !ok {
		test.Fatal("openai must be a LiteLLM-routable provider")
	}
	ids := catalogIDsForCredentials(cat, []litellm.Credential{{Provider: prefix}})
	if len(ids) != 1 || ids[0] != "openai" {
		test.Fatalf("openai credential must map back to catalog id 'openai', got %v", ids)
	}

	// A credential whose prefix matches no routable provider contributes nothing.
	if ids := catalogIDsForCredentials(cat, []litellm.Credential{{Provider: "not-a-provider"}}); len(ids) != 0 {
		test.Errorf("an unknown credential prefix must yield no ids, got %v", ids)
	}
}

// TestServiceStatusByName covers the injected-fetch projection: found, not-found,
// and a fetch error (which reads as not-found so the detail shows its hint).
func TestServiceStatusByName(test *testing.T) {
	statuses := []setup.ServiceStatus{
		{Name: "litellm", State: "running"},
		{Name: "vllm", State: "stopped"},
	}
	fetch := func() ([]setup.ServiceStatus, error) { return statuses, nil }

	status, found := serviceStatusByName(fetch, "litellm")
	if !found || status.State != "running" {
		test.Errorf("litellm should be found running, got found=%v state=%q", found, status.State)
	}
	if _, found := serviceStatusByName(fetch, "missing"); found {
		test.Error("an unknown service must not be found")
	}

	failing := func() ([]setup.ServiceStatus, error) { return nil, errors.New("gateway down") }
	if _, found := serviceStatusByName(failing, "litellm"); found {
		test.Error("a fetch error must read as not-found")
	}
}

// TestProjectInfoResolvesFromIndex covers projectInfo's found + not-found paths
// against a seeded global projects index.
func TestProjectInfoResolvesFromIndex(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	// Empty index: not found, no error.
	if _, found, err := projectInfo("ghost"); err != nil || found {
		test.Fatalf("empty index: found=%v err=%v, want false/nil", found, err)
	}

	root := filepath.Join(home, "projects", "app")
	seedProjectIndex(test, "app", root)

	entry, found, err := projectInfo("app")
	if err != nil || !found {
		test.Fatalf("seeded project must be found: found=%v err=%v", found, err)
	}
	if entry.Name != "app" {
		test.Errorf("entry.Name = %q, want app", entry.Name)
	}
	if _, found, _ := projectInfo("other"); found {
		test.Error("a name absent from the index must not be found")
	}
}

// TestResolveProjectRoot covers the empty-name, not-indexed, and indexed paths.
func TestResolveProjectRoot(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	if _, ok := resolveProjectRoot(""); ok {
		test.Error("an empty name must not resolve")
	}
	if _, ok := resolveProjectRoot("nope"); ok {
		test.Error("an unindexed name must not resolve")
	}

	root := filepath.Join(home, "projects", "app")
	seedProjectIndex(test, "app", root)
	got, ok := resolveProjectRoot("app")
	if !ok || got != root {
		test.Fatalf("resolveProjectRoot(app) = %q,%v, want %q,true", got, ok, root)
	}
}

// TestLifecycleLogPathAndOpen covers lifecycleLogPath (unknown vs known project),
// openLifecycleLog (creates the file with a header), and readCavemanInstallLog.
func TestLifecycleLogPathAndOpen(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	if _, ok := lifecycleLogPath("ghost", "start"); ok {
		test.Error("an unresolvable project must yield ok=false")
	}

	root := filepath.Join(home, "projects", "app")
	seedProjectIndex(test, "app", root)

	logPath, ok := lifecycleLogPath("app", "start")
	if !ok {
		test.Fatal("a known project must resolve a lifecycle log path")
	}
	if !strings.HasSuffix(logPath, filepath.Join(".ai-platform", "run", "start.log")) {
		test.Errorf("unexpected lifecycle log path %q", logPath)
	}

	// No caveman log yet.
	if _, ok := readCavemanInstallLog("app"); ok {
		test.Error("no caveman install log should be present initially")
	}

	file := openLifecycleLog("app", "start")
	if file == nil {
		test.Fatal("openLifecycleLog must create the log file")
	}
	_ = file.Close()

	content, err := os.ReadFile(logPath)
	if err != nil {
		test.Fatalf("read lifecycle log: %v", err)
	}
	if !strings.Contains(string(content), "=== ai start app @") {
		test.Errorf("lifecycle log missing its self-describing header: %q", content)
	}
}

// TestRoleAndGatewayLabelDefaults: with no runtime.yaml the labels fall back to the
// standalone role + the default local gateway URL.
func TestRoleAndGatewayLabelDefaults(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	if role := roleLabel(); role != "standalone" {
		test.Errorf("roleLabel default = %q, want standalone", role)
	}
	gateway := gatewayLabel()
	if gateway == "" || !strings.Contains(gateway, "18787") {
		test.Errorf("gatewayLabel default = %q, want a URL on port 18787", gateway)
	}
}

// TestOpenURLEmpty: an empty console URL is a hard error (no browser launch).
func TestOpenURLEmpty(test *testing.T) {
	if err := openURL(""); err == nil {
		test.Error("openURL(\"\") must error rather than launch a browser")
	}
}

// TestInitBatchesEveryViewInit verifies app.Init fires each view's Init (so every
// tab begins its own async load at startup).
func TestInitBatchesEveryViewInit(test *testing.T) {
	first := &initTrackingView{fakeView: fakeView{title: "A"}}
	second := &initTrackingView{fakeView: fakeView{title: "B"}}
	application := &app{views: []View{first, second}}

	application.Init()
	if first.inits != 1 || second.inits != 1 {
		test.Errorf("Init must init every view once, got %d/%d", first.inits, second.inits)
	}
}

// TestLifecyclePollCmdReturnsCommand: the poll tick command is always non-nil.
func TestLifecyclePollCmdReturnsCommand(test *testing.T) {
	application := &app{}
	if application.lifecyclePollCmd() == nil {
		test.Fatal("lifecyclePollCmd must return a tick command")
	}
}

// activatableView records SetActive toggles so switchTab's pause/resume of a
// background poll (the tabActivatable contract) can be asserted.
type activatableView struct {
	fakeView
	active bool
	calls  int
}

func (view *activatableView) SetActive(active bool) {
	view.active = active
	view.calls++
}

// TestSwitchTabPausesAndResumesBackgroundPoll: activating a new tab must pause the
// outgoing view's poll (SetActive false) and resume the incoming one (SetActive true).
func TestSwitchTabPausesAndResumesBackgroundPoll(test *testing.T) {
	outgoing := &activatableView{fakeView: fakeView{title: "Services"}}
	incoming := &activatableView{fakeView: fakeView{title: "Models"}}
	application := &app{views: []View{outgoing, incoming}}

	application.activateTab(1)
	if application.current != 1 {
		test.Fatalf("activateTab(1): current = %d, want 1", application.current)
	}
	if outgoing.active {
		test.Error("the outgoing view's background poll must be paused (SetActive false)")
	}
	if !incoming.active {
		test.Error("the incoming view's background poll must be resumed (SetActive true)")
	}
	if outgoing.calls == 0 || incoming.calls == 0 {
		test.Errorf("SetActive must be called on both views, got %d/%d", outgoing.calls, incoming.calls)
	}
}

// TestMouseToggledSwitchesCapture: the MouseToggledMsg returns a live capture
// enable/disable command in both directions.
func TestMouseToggledSwitchesCapture(test *testing.T) {
	application := newTestApp("Services")
	if _, cmd := application.Update(views.MouseToggledMsg{Enabled: true}); cmd == nil {
		test.Error("MouseToggledMsg{Enabled:true} must return the enable-capture command")
	}
	if _, cmd := application.Update(views.MouseToggledMsg{Enabled: false}); cmd == nil {
		test.Error("MouseToggledMsg{Enabled:false} must return the disable-capture command")
	}
}

// TestThemeChangedResizesWithoutPanic: applying a theme re-pushes view sizes and
// does not panic (so tables re-pick the new styles).
func TestThemeChangedResizesWithoutPanic(test *testing.T) {
	application := newTestApp("Services", "Models")
	application.width, application.height = 100, 40
	if _, cmd := application.Update(views.ThemeChangedMsg{}); cmd != nil {
		test.Errorf("ThemeChangedMsg should not return a command, got %v", cmd)
	}
}

// TestSessionFinishedRefreshes: returning from a suspended shell/attach refreshes
// the Sessions list and the workspace detail (a non-nil refresh batch is returned).
func TestSessionFinishedRefreshes(test *testing.T) {
	sessionsView := views.NewSessions(
		func() ([]workspace.Session, error) { return nil, nil },
		func(string) error { return nil },
		func() string { return "" },
	)
	projectDetail := views.NewProject(
		func(string) (project.Entry, bool, error) { return project.Entry{}, false, nil },
		nil,
	)
	application := &app{
		views:         []View{sessionsView},
		sessionsView:  sessionsView,
		projectDetail: projectDetail,
	}
	if _, cmd := application.Update(sessionFinishedMsg{}); cmd == nil {
		test.Fatal("sessionFinishedMsg must return a refresh command batch")
	}
}

// terminalOpener drives one message through Update and returns the resulting app so a
// test can assert an embedded-terminal overlay was opened.
func terminalOpener(test *testing.T, msg tea.Msg) *app {
	test.Helper()
	application := &app{views: []View{&fakeView{title: "X"}}}
	application.Update(msg)
	return application
}

// TestOverlayOpeningMessagesOpenTerminal: the lifecycle-adjacent action messages
// each open the embedded-terminal overlay, with esc-cancel set for the one-shot
// prompt commands and left off for the streaming ones.
func TestOverlayOpeningMessagesOpenTerminal(test *testing.T) {
	cases := []struct {
		name      string
		msg       tea.Msg
		escCloses bool
	}{
		{"services update", views.ServiceUpdateRequestedMsg{Service: "litellm"}, true},
		{"apps action", views.AppActionRequestedMsg{Action: "start", App: "open-webui", Project: "app"}, true},
		{"models pull", views.ModelsPullRequestedMsg{Refs: []string{"llama3:8b"}}, false},
		{"models rm", views.ModelRemoveRequestedMsg{Name: "llama3:8b"}, false},
		{"keys add", views.APIKeyAddRequestedMsg{Provider: "openai"}, true},
		{"keys remove", views.APIKeyRemoveRequestedMsg{Provider: "openai"}, true},
	}
	for _, testCase := range cases {
		application := terminalOpener(test, testCase.msg)
		if application.terminal == nil {
			test.Errorf("%s: must open the embedded terminal overlay", testCase.name)
			continue
		}
		if application.terminalEscCloses != testCase.escCloses {
			test.Errorf("%s: terminalEscCloses = %v, want %v", testCase.name, application.terminalEscCloses, testCase.escCloses)
		}
	}
}

// TestFollowMessagesRunInRealTerminal: the log-follow messages suspend to the real
// terminal (return an ExecProcess command) rather than opening the embedded overlay.
func TestFollowMessagesRunInRealTerminal(test *testing.T) {
	for _, testCase := range []struct {
		name string
		msg  tea.Msg
	}{
		{"workspace log follow", views.WorkspaceLogFollowRequestedMsg{Project: "app"}},
		{"service log follow", views.ServiceLogFollowRequestedMsg{Service: "litellm"}},
	} {
		application := &app{views: []View{&fakeView{title: "X"}}}
		_, cmd := application.Update(testCase.msg)
		if cmd == nil {
			test.Errorf("%s: must return the ExecProcess command", testCase.name)
		}
		if application.terminal != nil {
			test.Errorf("%s: must NOT open the embedded overlay", testCase.name)
		}
	}
}

// TestTerminalOverlayClosesOnCtrlQ: with an overlay open, ctrl+q closes it (Close is
// safe on an unspawned terminal) and the app refreshes the active sub-view.
func TestTerminalOverlayClosesOnCtrlQ(test *testing.T) {
	application, _ := newTestHubApp(test, "Workspace", "Network")
	// Open a streaming overlay (models pull) — routed to the current top-level view is
	// irrelevant; openTerminal only sets application.terminal.
	application.Update(views.ModelsPullRequestedMsg{Refs: []string{"llama3:8b"}})
	if application.terminal == nil {
		test.Fatal("precondition: the overlay must be open")
	}
	// ctrl+q force-detaches: Update routes the key to updateTerminal, which closes it.
	application.Update(tea.KeyMsg{Type: tea.KeyCtrlQ})
	if application.terminal != nil {
		test.Fatal("ctrl+q must close the embedded terminal overlay")
	}
}

// TestTerminalOverlayEscClosesOneShot: for a one-shot prompt overlay (keys add),
// terminalEscCloses is set so a single esc cancels/closes it.
func TestTerminalOverlayEscClosesOneShot(test *testing.T) {
	application, _ := newTestHubApp(test, "Workspace", "Network")
	application.Update(views.APIKeyAddRequestedMsg{Provider: "openai"})
	if application.terminal == nil || !application.terminalEscCloses {
		test.Fatal("precondition: an esc-cancel overlay must be open")
	}
	application.Update(esc())
	if application.terminal != nil {
		test.Fatal("esc must close a one-shot prompt overlay")
	}
}
