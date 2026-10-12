// Package tui is the K9s-style full-screen management UI behind `ai ui`. It is a
// thin interactive layer over the existing platform package APIs: each screen
// (see internal/tui/views) reads/acts through injected funcs that wire to
// setup/workspace/egress/contextopt/litellm/catalog, so no management logic is
// duplicated here. The UI is interactive-only (it requires a TTY) and renders to
// stderr via the alternate screen, keeping stdout free of any output (consistent
// with the platform's JSON-envelope contract — `ai ui` simply has no envelope).
package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/stack-genie/internal/catalog"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/contextopt"
	"github.com/jt-helsinki/stack-genie/internal/create"
	"github.com/jt-helsinki/stack-genie/internal/egress"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/omlx"
	"github.com/jt-helsinki/stack-genie/internal/project"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/setup"
	"github.com/jt-helsinki/stack-genie/internal/state"
	"github.com/jt-helsinki/stack-genie/internal/tui/views"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
)

// View is one screen of the UI. Views are pointer models that mutate in place;
// Update returns only a command (the parent owns the model), keeping the parent's
// routing simple. Satisfied structurally by the types in internal/tui/views.
type View interface {
	Init() tea.Cmd
	Update(tea.Msg) tea.Cmd
	View() string
	Title() string
	Hints() string
	SetSize(width, height int)
}

// Run wires the views to the real platform APIs and runs the full-screen program
// until the user exits. It always opens on the home screen (Services); the active
// view is not persisted across restarts.
func Run(cwd string) error {
	// The UI always opens on the home screen (Services) with no project selected —
	// the active view is never persisted across restarts, and the cwd is not
	// auto-drilled into a project (the user picks one from the Projects switcher).
	// cwd is still kept for the create overlay's default directory.
	application := &app{
		cwd:     cwd,
		role:    roleLabel(),
		gateway: gatewayLabel(),
	}

	deps := setup.RealDeps(goruntime.GOOS, goruntime.GOARCH, nowRFC3339)
	// Host-native service progress (the omlx server launch line) defaults to stderr for
	// plain CLI use. Inside this bubbletea alt-screen a direct stderr write corrupts
	// the rendered frame (stray lines bleed through the bordered panes) until a
	// resize forces a full repaint — silence it; the list's flash line + Services
	// detail already report the outcome.
	setup.SetHostNativeProgressWriter(nil)
	litellmClient := litellm.RealClient()
	workspaceManager := workspace.RealManager(nowRFC3339)
	application.workspaceManager = workspaceManager
	// The per-project views (Network/Context) resolve the LIVE current project at
	// fetch time, so switching projects reflects immediately without re-wiring.
	currentRoot := func() (string, bool) { return resolveProjectRoot(application.currentProject) }

	// The Services tab mirrors the Workspaces hub: a LIST that drills into a
	// per-service DETAIL (summary + the live CONTAINER LOG embedded beneath it +
	// in-place lifecycle). The container log is the SAME generic LogView the workspace
	// log uses (views.NewLogView), configured for a service container and tailed via
	// the docker-logs tailer (setup.ServiceLogTail) — the service-tier analogue of
	// `nono ps`. The container log shows while the service is RUNNING; a stopped
	// service shows the "not running" hint.
	servicesControl := func(action, service string) error {
		_, err := setup.ControlService(deps, action, service)
		return err
	}
	servicesStatus := func() ([]setup.ServiceStatus, error) { return setup.ServicesStatus(deps) }
	// serviceDetail is declared up front so the log closures can read the OPEN service
	// (set on it by the list's drill-in) — the same late-binding the per-project
	// sub-views use via application.currentProject.
	var serviceDetail *views.ServiceDetail
	serviceLogView := views.NewLogView(
		func() (string, error) {
			if serviceDetail.Service() == "" {
				return "", nil
			}
			return setup.ServiceLogTail(deps, serviceDetail.Service(), setup.ServiceLogTailLines)
		},
		func() bool {
			// The container log is shown only while the service is RUNNING, so a stopped
			// service shows the "not running" hint instead of stale output.
			if serviceDetail.Service() == "" {
				return false
			}
			status, found := serviceStatusByName(servicesStatus, serviceDetail.Service())
			return found && status.State == "running"
		},
		func() string { return serviceDetail.Service() },
		views.LogViewLabels{
			NotRunning: "service not running — its container log appears here while it is running (press s to start)",
			Loading:    "loading container log…",
			Empty:      "no container output captured yet — it streams in as the service runs",
		},
		func(subject string) tea.Msg { return views.ServiceLogFollowRequestedMsg{Service: subject} },
	)
	serviceDetail = views.NewServiceDetail(
		func(service string) (setup.ServiceStatus, bool) { return serviceStatusByName(servicesStatus, service) },
		func(service string) ([]setup.ContainerStats, error) { return setup.ServiceStats(deps, service) },
		servicesControl,
		openURL,
		serviceLogView,
		setup.SyncOmlxModels,
	)
	servicesView := views.NewServices(servicesStatus, servicesControl, serviceDetail)
	projectsView := views.NewProjects(project.List)
	projectDetail := views.NewProject(projectInfo, workspaceConfigFetcher)
	// Gateway Endpoints block: the host proxy entry to LiteLLM external apps use to reach
	// the served models. Workspace-independent; resolved from runtime.yaml (domain +
	// DefaultGatewayPort), mirroring the addresses `ai services` shows.
	projectDetail.SetEndpoints(func() []views.ConfigField {
		info, err := runtime.Load()
		if err != nil || info == nil {
			return nil
		}
		domain := info.ResolveDomain()
		base := fmt.Sprintf("http://%s:%d", domain, runtime.DefaultGatewayPort)
		return []views.ConfigField{
			{Label: "models (OpenAI /v1)", Value: base + "/v1"},
			{Label: "LiteLLM admin API", Value: base + "/llm"},
			{Label: "LiteLLM console", Value: fmt.Sprintf("http://litellm.%s:%d", domain, runtime.DefaultGatewayPort)},
			{Label: "cache console", Value: fmt.Sprintf("http://valkey.%s:%d", domain, runtime.DefaultGatewayPort)},
			{Label: "auth", Value: "external apps send a LiteLLM key — create one with: ai keys"},
		}
	})

	// The Sessions view (Shell tab) resolves the LIVE current project at fetch time
	// (over the real Manager), so switching projects reflects immediately. With no
	// current project the lister is not invoked (the view shows "no project
	// selected").
	sessionsView := views.NewSessions(
		func() ([]workspace.Session, error) {
			if application.currentProject == "" {
				return nil, nil
			}
			return workspaceManager.ListSessions(application.currentProject)
		},
		func(sessionID string) error {
			return workspaceManager.Stop(sessionID, false)
		},
		func() string { return application.currentProject },
	)
	// Network management: the egress mode plus the allow/deny domain lists. The view
	// holds no egress parsing — these closures are its only wiring.
	networkView := views.NewNetwork(currentRoot,
		func() string { return application.currentProject },
		egress.Get, egress.SetMode, egress.Allow, egress.Deny,
	)
	contextView := views.NewContext(currentRoot, contextopt.GetStatus, contextopt.SetStrategy, contextopt.SetCavemanLevel)
	// Cloud Models: the models.dev catalog (with its data source for availability
	// messaging), the gateway's live (registered) set, the `r`-refresh (re-fetch the
	// catalog + resync the gateway), and the gateway tester. All over the existing
	// package APIs so the view stays fakeable in tests.
	cloudModelsView := views.NewCloudModels(
		loadCloudCatalog,
		litellm.NewKeyManager(runtime.RealProber()).ListModels,
		refreshModelCatalog,
		litellmClient.Test,
	)
	// The API Keys view lists the routable catalog providers + their keyed status
	// (the same catalog + ListCredentials join `ai keys list` uses). Add/remove run
	// `ai keys add|remove <provider>` live in the terminal overlay (the hidden key
	// prompt shows there) — the view never sees a key value.
	apiKeysView := views.NewAPIKeys(listAPIKeyProviders)
	// The Settings tab is a live theme picker + the mouse tab-clicking toggle plus
	// read-only platform info. Applying a theme persists it and recolors the whole
	// UI (ThemeChangedMsg); toggling the mouse persists the setting and switches
	// mouse capture live (MouseToggledMsg).
	settingsView := views.NewSettings(
		ui.ThemeNames(), ui.CurrentTheme,
		func(name string) error {
			if err := ui.Apply(name); err != nil {
				return err
			}
			return ui.SaveThemeName(name)
		},
		ui.LoadMouseEnabled(), ui.SaveMouseEnabled,
		application.role, application.gateway,
	)

	// The Workspaces tab is a two-level hub: it opens on the switcher (the
	// workspace list) and, once a workspace is selected, reveals per-workspace
	// sub-tabs — Workspace · Network · Context · Shell — for it. The "Shell" tab is
	// the session manager (sessionsView): list/attach/new/stop, with the
	// interactive shell run in the real terminal via ExecProcess.
	projectsHub := views.NewProjectsHub(
		projectsView,
		[]views.Screen{projectDetail, networkView, contextView, sessionsView},
		[]string{"Workspace", "Network", "Context", "Shell"},
	)

	// Top-level tab order = menu order: Services · Workspaces · Cloud Models · API
	// Keys · Settings. Local model management is gone — omlx (the sole
	// local-inference runtime) manages its own models entirely through its own
	// admin panel (`ai services console omlx`).
	application.views = []View{servicesView, projectsHub, cloudModelsView, apiKeysView, settingsView}
	application.projectsIndex = 1
	application.servicesView = servicesView
	application.projectsHub = projectsHub
	application.projectDetail = projectDetail
	application.sessionsView = sessionsView
	application.cloudModelsView = cloudModelsView
	application.apiKeysView = apiKeysView

	// The body-level scroll viewport clips + scrolls a no-sub-tab tab's pane when its
	// content overflows the body (e.g. a tall table on a small window). Tabs WITH
	// sub-tabs do not use it — their content is rendered directly (the sub-tab bar
	// stays pinned) and their sub-panes scroll themselves. See scrollsBody / body().
	application.bodyViewport = viewport.New(0, 0)

	// Always land on the home screen (Services, index 0 — current's zero value); a
	// project is opened only when the user selects it from the Projects switcher.

	// Mouse reporting (a persisted user setting, default ON — ui.LoadMouseEnabled,
	// toggled live in the Settings tab with `m`) makes the top-level tab bar and the
	// per-workspace / service-detail sub-tab bars CLICKABLE (handleMouse). The
	// trade-off: with the mouse captured, the host terminal's native text selection
	// needs the terminal's selection modifier (Shift on most, Option on macOS
	// terminals) — turning the setting OFF restores modifier-free selection.
	// Scrolling remains keyboard-driven (PgUp/PgDn/arrows) either way.
	programOptions := []tea.ProgramOption{tea.WithAltScreen(), tea.WithOutput(os.Stderr)}
	if ui.LoadMouseEnabled() {
		programOptions = append(programOptions, tea.WithMouseCellMotion())
	}
	program := tea.NewProgram(application, programOptions...)
	_, runErr := program.Run()
	return runErr
}

// resolveProjectRoot maps a project name to its root via the global index, or
// ("", false) when no project is current / it is not indexed.
func resolveProjectRoot(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	index, err := state.LoadIndex()
	if err != nil {
		return "", false
	}
	if entry, ok := index.Projects[name]; ok {
		return entry.Path, true
	}
	return "", false
}

// executablePath is this `ai` binary, used to spawn sub-commands (the project
// wizard, an in-workspace shell) via tea.ExecProcess.
func executablePath() string {
	path, err := os.Executable()
	if err != nil {
		return "ai"
	}
	return path
}

// createDoneMsg reports that the in-process create.Execute for a new workspace has
// finished (running off the event loop). warnings are best-effort issues; err is a
// hard failure.
type createDoneMsg struct {
	name     string
	warnings []string
	err      error
}

// createProgressMsg carries one create.Progress event streamed from the in-process
// create.Execute (via a channel), so the "creating…" pane shows a live step log.
type createProgressMsg struct {
	progress create.Progress
	ch       chan create.Progress // the source channel, re-read to stream the next event
}

// waitCreateProgress reads the next create.Progress from ch into a createProgressMsg
// (carrying ch so the handler re-arms). A closed channel (Execute finished) yields nil,
// stopping the pump.
func waitCreateProgress(ch chan create.Progress) tea.Cmd {
	return func() tea.Msg {
		progress, ok := <-ch
		if !ok {
			return nil
		}
		return createProgressMsg{progress: progress, ch: ch}
	}
}

// sessionFinishedMsg reports that a suspended interactive shell/agent/attach
// subprocess (run via tea.ExecProcess in the user's real terminal) has exited, so
// the TUI can refresh the session list + project detail.
type sessionFinishedMsg struct{ err error }

// projectInfo returns the current state of one project by name (over project.List).
func projectInfo(name string) (project.Entry, bool, error) {
	entries, err := project.List()
	if err != nil {
		return project.Entry{}, false, err
	}
	for _, entry := range entries {
		if entry.Name == name {
			return entry, true, nil
		}
	}
	return project.Entry{}, false, nil
}

// serviceStatusByName fetches the current service-tier statuses and returns the one
// matching name (and whether it was found). It is the per-service projection the
// Services detail view + its container-log running-check resolve through, mirroring
// projectInfo for the workspace detail. A fetch error reads as "not found" (the
// detail then shows its loading/not-running hint rather than a hard error).
func serviceStatusByName(fetch func() ([]setup.ServiceStatus, error), name string) (setup.ServiceStatus, bool) {
	statuses, err := fetch()
	if err != nil {
		return setup.ServiceStatus{}, false
	}
	for _, status := range statuses {
		if status.Name == name {
			return status, true
		}
	}
	return setup.ServiceStatus{}, false
}

// omlxLiveModelNames feeds the create wizard's Graphify-model picker: it queries
// omlx's live GET /v1/models directly (the wizard runs on the SAME host as omlx,
// so no gateway round trip is needed) and returns the sorted model ids it
// currently reports. Degrades gracefully to an empty list — never an error —
// when omlx isn't installed/running or is currently serving nothing; the picker
// still shows its "leave Graphify unconfigured" option in that case.
func omlxLiveModelNames() []string {
	models, err := omlx.ListModels()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(models))
	for _, model := range models {
		names = append(names, model.ID)
	}
	sort.Strings(names)
	return names
}

// listAPIKeyProviders joins the model catalog's LiteLLM-routable providers with the
// gateway's stored credentials so the API Keys view can render PROVIDER · NAME ·
// KEY? · MODELS. It never returns a key value (ListCredentials reports names +
// provider prefixes only). A provider is keyed when a credential's LiteLLM prefix
// matches the provider's prefix (e.g. the catalog "google" keyed by a "gemini" cred).
func listAPIKeyProviders() ([]views.APIKeyProvider, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cat, err := catalog.LoadOrFetch(ctx, nil, "")
	if err != nil {
		return nil, err
	}
	// The gateway may be unreachable (services down / mid-restart). That must NOT blank
	// the API Keys tab — the routable providers are known from the catalog. Degrade: on a
	// ListCredentials error, treat every provider as unkeyed rather than failing the whole
	// view (mirrors Cloud Models, which renders catalog rows regardless of the gateway).
	creds, _ := litellm.NewKeyManager(runtime.RealProber()).ListCredentials()
	return apiKeyProviderRows(cat, creds), nil
}

// apiKeyProviderRows joins the catalog's LiteLLM-routable providers with the keyed
// credentials into the API Keys rows. creds may be nil/empty (gateway unreachable) → every
// provider renders unkeyed. Pure (no I/O) so the join + degrade behavior is unit-tested.
func apiKeyProviderRows(cat *catalog.Catalog, creds []litellm.Credential) []views.APIKeyProvider {
	keyedPrefix := make(map[string]bool, len(creds))
	for _, cred := range creds {
		keyedPrefix[cred.Provider] = true
	}
	providers := litellm.LiteLLMProviders(cat)
	rows := make([]views.APIKeyProvider, 0, len(providers))
	for _, provider := range providers {
		prefix, _ := litellm.LiteLLMPrefix(provider.ID)
		rows = append(rows, views.APIKeyProvider{
			Provider: provider.ID,
			Name:     provider.Name,
			HasKey:   keyedPrefix[prefix],
			Models:   len(provider.Models),
		})
	}
	return rows
}

// loadCloudCatalog loads the models.dev catalog for the Cloud Models view. It is
// CACHE-FIRST: a cached catalog is used as-is (no network) because it changes
// rarely — the live fetch happens only on a first run (no cache) or the `r`
// refresh (refreshModelCatalog). It reports the data SOURCE + any fetch error so
// the view can message availability.
func loadCloudCatalog() (*catalog.Catalog, catalog.Source, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return catalog.LoadCachedOrFetchStatus(ctx, nil, "")
}

// refreshModelCatalog is the Models view's `r`-refresh network work: it re-fetches
// the models.dev catalog (and persists it) then resyncs the gateway's model set to
// the keyed providers' catalog models. The keyed set is read back from the live
// credential store so the desired set always reflects what is actually keyed. Local
// omlx models are synced separately (SyncOmlxModels, driven by omlx's own live
// model list, not the catalog) and shielded from this resync's delete pass.
// Best-effort — the caller surfaces any error as a flash and reloads the displayed
// data regardless.
//
// hardware bring-up: the live models.dev fetch + the /model/* resync round-trips run
// only against the network / a running aip-litellm.
func refreshModelCatalog() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cat, err := catalog.Fetch(ctx, nil, "")
	if err != nil {
		// Offline: fall back to the saved copy so the resync still runs against it.
		cat, err = catalog.Load()
		if err != nil {
			return err
		}
	} else {
		_ = catalog.Save(cat) // best-effort persist; a save failure must not abort the resync
	}
	manager := litellm.NewKeyManager(runtime.RealProber())
	creds, err := manager.ListCredentials()
	if err != nil {
		return err
	}
	keyed := catalogIDsForCredentials(cat, creds)
	_, err = manager.SyncModels(cat, keyed)
	return err
}

// catalogIDsForCredentials maps the stored credentials' LiteLLM provider prefixes
// back to the CATALOG provider ids that route under them (e.g. a "gemini" credential
// keys the catalog's "google" provider), so SyncModels (which matches on catalog
// ids) registers the right providers' models. Only routable catalog providers are
// considered.
func catalogIDsForCredentials(cat *catalog.Catalog, creds []litellm.Credential) []string {
	keyedPrefix := make(map[string]bool, len(creds))
	for _, cred := range creds {
		keyedPrefix[cred.Provider] = true
	}
	var ids []string
	for _, provider := range litellm.LiteLLMProviders(cat) {
		prefix, _ := litellm.LiteLLMPrefix(provider.ID)
		if keyedPrefix[prefix] {
			ids = append(ids, provider.ID)
		}
	}
	return ids
}

// app is the root tea.Model: it owns the views, the header/footer chrome, and the
// command palette (the menu, which includes Exit).
type app struct {
	views   []View
	current int
	width   int
	height  int
	cwd     string
	role    string
	gateway string

	// projectsHub is the two-level "Projects" tab (switcher + per-project
	// sub-tabs); projectsIndex is its slot in views. The app forwards project
	// selection / create-return to it and reads the live current project for the
	// footer + the Sessions lister. projectDetail is kept so a returning shell can
	// refresh the detail pane directly.
	projectsHub    *views.ProjectsHub
	projectDetail  *views.Project
	projectsIndex  int
	currentProject string

	// workspaceManager is the ONE workspace Manager for the whole session — a thin
	// wrapper over the nono CLI; every call shells out independently (there is no
	// persistent connection to share or release, unlike the old Microsandbox relay).
	workspaceManager workspace.Manager

	// servicesView lets the app refresh the Services view (list or open detail) when
	// a `services update` terminal overlay returns.
	servicesView *views.Services

	// sessionsView lets the app refresh the Sessions view when an attach/shell
	// subprocess returns (the user may have created/stopped a session).
	sessionsView *views.Sessions

	// cloudModelsView lets the app refresh the model list after a keys subprocess
	// returns from the terminal overlay.
	cloudModelsView *views.CloudModels

	// apiKeysView lets the app refresh the provider/keyed list after an
	// `ai keys add|remove` subprocess returns from the terminal overlay.
	apiKeysView *views.APIKeys

	// createView is the modal multi-step new-workspace wizard overlay; non-nil only
	// while it is open (it is not a menu/slice view).
	createView *views.Create

	// creating holds the name of a workspace whose in-process create.Execute is
	// running (async); the View shows a "creating…" pane while it is non-empty.
	creating string
	// createSteps is the accumulated log of create.Progress step labels, shown in
	// the "creating…" pane.
	createSteps []string
	// createFlash is a transient banner shown after a create completes (an error, or
	// best-effort warnings); cleared on the next tab switch.
	createFlash string

	// terminal is the live embedded-terminal overlay (delete confirm, services
	// update, apps/keys prompts); non-nil only while it is open. While set it owns
	// all input — keystrokes are forwarded to the PTY — except ctrl+q (force-detach)
	// and, once the process has exited, any key (close).
	terminal *views.Terminal
	// terminalEscCloses makes <esc> cancel/close the terminal overlay (killing the
	// inner process) for one-shot prompt commands like `ai keys add/remove`, so the
	// user can back out of the add/edit-key screen with a single esc. It is false for
	// interactive sessions (shell/agent) where esc belongs to the program. Set when
	// the overlay is opened; reset by openTerminal.
	terminalEscCloses bool

	helpOpen bool
	quitting bool

	// bodyViewport scrolls the active top-level tab's pane when it has NO sub-tabs and
	// its content overflows the body height (the small-window case). A tab WITH
	// sub-tabs (CapturesNav) is rendered directly and clipped — its sub-tab bar stays
	// pinned and its sub-panes do the scrolling. PgUp/PgDn (and ctrl+u/ctrl+d) drive
	// this scroll, intercepted only while the pane overflows so the view keeps ↑/↓ and
	// its own paging otherwise.
	bodyViewport viewport.Model
}

// textInputCapturer is implemented by a view (or the hub on behalf of its active
// sub-view) that currently has an inline text prompt open. While it captures input,
// the app routes EVERY key to it so typed characters (':', 'q', '?', …) are not
// stolen by the global shortcuts.
type textInputCapturer interface{ CapturingInput() bool }

// capturingInput reports whether view currently owns text input.
func capturingInput(view View) bool {
	capturer, ok := view.(textInputCapturer)
	return ok && capturer.CapturingInput()
}

// Init initializes every view (each begins its own refresh) so switching between
// them is instant.
func (application *app) Init() tea.Cmd {
	commands := make([]tea.Cmd, 0, len(application.views))
	for _, view := range application.views {
		commands = append(commands, view.Init())
	}
	return tea.Batch(commands...)
}

func (application *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch message := msg.(type) {
	case tea.WindowSizeMsg:
		application.width, application.height = message.Width, message.Height
		application.resizeViews()
		return application, nil

	case tea.MouseMsg:
		return application, application.handleMouse(message)

	case views.ThemeChangedMsg:
		// A theme was applied in Settings — re-push sizes so every view's table
		// re-picks the new styles (the chrome already reads the accent live).
		application.resizeViews()
		return application, nil

	case views.MouseToggledMsg:
		// The mouse tab-clicking setting was toggled (and persisted) in Settings —
		// switch mouse capture live. While disabled, the terminal delivers no mouse
		// events, so handleMouse simply never fires and native text selection works
		// without a modifier.
		if message.Enabled {
			return application, tea.EnableMouseCellMotion
		}
		return application, tea.DisableMouse

	case views.ProjectSelectedMsg:
		// The switcher chose a project — make it current (so the closure-driven
		// sub-views resolve it) and drop into it inside the Projects hub.
		application.currentProject = message.Name
		application.switchTab(application.projectsIndex)
		return application, application.projectsHub.OpenProject(message.Name)

	case views.NewProjectRequestedMsg:
		// Open the multi-step create WIZARD in-TUI (no subprocess), seeded with the
		// current directory.
		wizard := views.NewCreate(application.cwd, omlxLiveModelNames)
		bodyWidth, bodyHeight := application.bodyContentSize()
		wizard.SetSize(bodyWidth, bodyHeight)
		application.createView = wizard
		application.createFlash = ""
		return application, wizard.Init()

	case views.CreateCancelledMsg:
		application.createView = nil
		return application, nil

	case views.CreateConfirmedMsg:
		// Run create IN-PROCESS (shared create.Execute) off the event loop; show a
		// "creating…" pane until createDoneMsg arrives. There is no image to build
		// and nothing to download — this finishes almost immediately, but still runs
		// off the event loop so the UI never blocks.
		application.createView = nil
		spec := message.Spec
		application.creating = spec.Name
		application.createSteps = nil
		progressCh := make(chan create.Progress, 64)
		doneCh := make(chan createDoneMsg, 1)
		go func() {
			_, warnings, err := create.Execute(spec, nowRFC3339(), func(progress create.Progress) {
				select {
				case progressCh <- progress:
				default:
				}
			})
			doneCh <- createDoneMsg{name: spec.Name, warnings: warnings, err: err}
			close(progressCh)
		}()
		return application, tea.Batch(
			waitCreateProgress(progressCh),
			func() tea.Msg { return <-doneCh },
		)

	case createProgressMsg:
		if step := message.progress.Step; step != "" {
			application.createSteps = append(application.createSteps, step)
		}
		return application, waitCreateProgress(message.ch)

	case createDoneMsg:
		application.creating = ""
		application.createSteps = nil
		application.switchTab(application.projectsIndex)
		if message.err != nil {
			application.createFlash = ui.Failure.Render(ui.IconFail + " create failed: " + message.err.Error())
			return application, application.projectsHub.Reset()
		}
		if len(message.warnings) > 0 {
			application.createFlash = ui.Warn.Render(ui.IconDot + " created " + message.name + " with warnings: " + strings.Join(message.warnings, "; "))
		}
		// Land on the freshly-created workspace. Set currentProject FIRST (as
		// ProjectSelectedMsg does) so the sub-views' closures (Network/Shell)
		// resolve it immediately.
		application.currentProject = message.name
		cmd := tea.Sequence(application.projectsHub.Reset(), application.projectsHub.OpenProject(message.name))
		return application, cmd

	case views.WorkspaceActionRequestedMsg:
		switch message.Action {
		case "delete":
			// Delete confirms inline, so run it in the embedded terminal overlay (its
			// y/n prompt shows in the pane); esc cancels the pane.
			cmd := application.openTerminal(
				"delete "+message.Project, []string{"delete", message.Project}, false)
			application.terminalEscCloses = true
			return application, cmd
		case "stop":
			// Stop runs IN-PROCESS (no sandbox to suspend into — it just stops every
			// live session for the workspace).
			return application, application.stopWorkspaceCmd(message.Project)
		default: // start, restart
			// start/restart launch (or reattach to) the default agent CLI's sandboxed
			// session: an INTERACTIVE program, so — exactly like ExecRequestedMsg/
			// AttachRequestedMsg — the app SUSPENDS and runs it in the user's real
			// terminal via tea.ExecProcess, restoring the TUI on exit.
			command := exec.Command(executablePath(), message.Action, message.Project)
			return application, tea.ExecProcess(command, func(execErr error) tea.Msg {
				return sessionFinishedMsg{err: execErr}
			})
		}

	case views.ExecRequestedMsg:
		// An interactive shell runs in the user's REAL terminal: suspend the TUI and
		// run `ai shell <name>` via tea.ExecProcess (full keys, native selection,
		// in-place output via the host terminal), restoring the TUI on exit.
		command := exec.Command(executablePath(), "shell", message.Project)
		return application, tea.ExecProcess(command, func(execErr error) tea.Msg {
			return sessionFinishedMsg{err: execErr}
		})

	case views.AttachRequestedMsg:
		// Attach to an existing workspace session in the user's REAL terminal:
		// suspend the TUI and run `ai attach <session> <name>` via tea.ExecProcess,
		// restoring the TUI on exit.
		command := exec.Command(executablePath(), "attach", message.Session, message.Project)
		return application, tea.ExecProcess(command, func(execErr error) tea.Msg {
			return sessionFinishedMsg{err: execErr}
		})

	case views.ServiceLogFollowRequestedMsg:
		// Follow a service's container log live in the user's REAL terminal. It runs
		// `ai logs --service <svc> --follow` (setup.FollowServiceLogs → `<runtime>
		// logs -f`), restoring the TUI on Ctrl-C.
		command := exec.Command(executablePath(), "logs", "--service", message.Service, "--follow")
		return application, tea.ExecProcess(command, func(execErr error) tea.Msg {
			return sessionFinishedMsg{err: execErr}
		})

	case views.ServiceUpdateRequestedMsg:
		// `p` (update) in the service detail: run `ai services update <svc>` live in the
		// embedded terminal overlay (a real PTY → the CLI's pull spinner renders),
		// EXACTLY like the keys subprocess. The Services view refreshes when the
		// overlay closes; esc cancels it.
		cmd := application.openTerminal(
			"services update "+message.Service, []string{"services", "update", message.Service}, false)
		application.terminalEscCloses = true
		return application, cmd

	case sessionFinishedMsg:
		// Back from an interactive shell/agent/attach, or a start/stop/restart —
		// refresh the Shell (sessions) list and the project detail so the change is
		// reflected.
		commands := []tea.Cmd{}
		if application.sessionsView != nil {
			commands = append(commands, application.sessionsView.Init())
		}
		if application.projectDetail != nil {
			commands = append(commands, application.projectDetail.Init())
		}
		return application, tea.Batch(commands...)

	case workspaceStoppedMsg:
		if application.projectDetail != nil {
			if message.err != nil {
				application.projectDetail.SetFlash(ui.Failure.Render(ui.IconFail + " stop: " + message.err.Error()))
			} else {
				application.projectDetail.SetFlash(ui.Success.Render(ui.IconOK + " stopped"))
			}
		}
		commands := []tea.Cmd{}
		if application.sessionsView != nil {
			commands = append(commands, application.sessionsView.Init())
		}
		if application.projectDetail != nil {
			commands = append(commands, application.projectDetail.Init())
		}
		return application, tea.Batch(commands...)

	case views.APIKeyAddRequestedMsg:
		// Add runs `ai keys add <provider>` live in the overlay (its hidden key
		// prompt shows in the pane), then the API Keys view refreshes on close.
		// esc cancels the add/edit-key screen (one-shot prompt).
		cmd := application.openTerminal(
			"keys add "+message.Provider, []string{"keys", "add", message.Provider}, false)
		application.terminalEscCloses = true
		return application, cmd

	case views.APIKeyRemoveRequestedMsg:
		// Remove runs `ai keys remove <provider>` live in the overlay, then refresh.
		// esc cancels the confirm screen.
		cmd := application.openTerminal(
			"keys remove "+message.Provider, []string{"keys", "remove", message.Provider}, false)
		application.terminalEscCloses = true
		return application, cmd

	case tea.KeyMsg:
		// While the live terminal overlay is open it owns input (keystrokes go to
		// the PTY); ctrl+q force-detaches and, once the process exits, any key closes.
		if application.terminal != nil {
			return application.updateTerminal(message)
		}
		// While the create overlay is open it owns all input.
		if application.createView != nil {
			return application, application.createView.Update(msg)
		}
		// A workspace create is running: the "creating…" pane is MODAL — swallow keys
		// (except quit) so tab/←→/number navigation can't move the tab bar while the
		// pane stays fixed on the create progress (the desync bug). It clears itself on
		// createDoneMsg; the async progress messages still flow (they are not KeyMsgs).
		if application.creating != "" {
			if key := message.String(); key == "ctrl+c" || key == "q" {
				application.quitting = true
				return application, tea.Quit
			}
			return application, nil
		}
		// The help overlay is dismissed by any key.
		if application.helpOpen {
			application.helpOpen = false
			return application, nil
		}
		// A view with an inline text prompt open (e.g. the Shell "new session" or the
		// Network allow/disallow prompts) owns EVERY key, so typed characters like ':',
		// 'q' or '?' are not stolen by the global shortcuts.
		if capturingInput(application.views[application.current]) {
			return application, application.views[application.current].Update(msg)
		}
		// When the active view captures navigation (the Projects hub with a project
		// open), Tab/←→ cycle ITS sub-tabs and esc backs up a level inside it — so
		// those keys are delegated to the view rather than switching top-level tabs.
		captures := capturesNav(application.views[application.current])
		// On a no-sub-tab tab whose pane OVERFLOWS the body, the dedicated body-scroll
		// keys (PgUp/PgDn, ctrl+u/ctrl+d) drive the outer viewport — but ONLY while it
		// overflows, so in the common fits-the-pane case those keys still reach the view
		// (e.g. a table's own paging). ↑/↓ always stay with the view.
		if !captures && application.bodyOverflowing() {
			switch message.String() {
			case "pgup", "pgdown", "ctrl+u", "ctrl+d":
				var cmd tea.Cmd
				application.bodyViewport, cmd = application.bodyViewport.Update(message)
				return application, cmd
			}
		}
		switch message.String() {
		case "ctrl+c", "q":
			application.quitting = true
			return application, tea.Quit
		case "?":
			application.helpOpen = true
			return application, nil
		case "tab", "right":
			if captures {
				return application, application.views[application.current].Update(msg)
			}
			return application, application.activateTab(application.current + 1)
		case "shift+tab", "left":
			if captures {
				return application, application.views[application.current].Update(msg)
			}
			return application, application.activateTab(application.current - 1)
		}
		// Number keys 1-9 jump straight to that tab (1-based).
		if message.Type == tea.KeyRunes && len(message.Runes) == 1 {
			if digit := message.Runes[0]; digit >= '1' && digit <= '9' {
				target := int(digit - '1')
				if target < len(application.views) {
					return application, application.activateTab(target)
				}
			}
		}
	}

	// Delegate everything else (ticks, refresh results, navigation, the
	// filepicker's own messages) to the live terminal / create overlay if open,
	// else the active view. The terminal's own dirty/exit messages drive its
	// render loop here.
	if application.terminal != nil {
		return application, application.terminal.Update(msg)
	}
	if application.createView != nil {
		return application, application.createView.Update(msg)
	}
	return application, application.views[application.current].Update(msg)
}

// workspaceStoppedMsg reports the outcome of an in-process StopAll triggered by
// the Workspace tab's "x" key.
type workspaceStoppedMsg struct{ err error }

// stopWorkspaceCmd stops every live session for project IN-PROCESS (there is no
// sandbox process to suspend into, unlike start/restart/shell/agent/attach).
func (application *app) stopWorkspaceCmd(projectName string) tea.Cmd {
	manager := application.workspaceManager
	return func() tea.Msg {
		return workspaceStoppedMsg{err: manager.StopAll(projectName)}
	}
}

// workspaceConfigFetcher renders the DECLARED project config (config.yaml) as the
// Workspace tab's Configuration diagnostics — the values `ai create`/`ai network`
// set, so edits reflect immediately (no live-sandbox read).
func workspaceConfigFetcher(name string) ([]views.ConfigField, error) {
	root, ok := resolveProjectRoot(name)
	if !ok {
		return nil, fmt.Errorf("workspace %q not found", name)
	}
	projectConfig, err := config.LoadProjectConfig(root)
	if err != nil {
		return nil, err
	}
	return workspaceConfigFields(projectConfig), nil
}

// workspaceConfigFields renders the declared project config as the Workspace tab's
// Configuration diagnostics.
func workspaceConfigFields(projectConfig *config.Config) []views.ConfigField {
	fields := []views.ConfigField{
		{Label: "os", Value: dashIfEmpty(projectConfig.OS)},
		{Label: "shell", Value: dashIfEmpty(projectConfig.Workspace.Shell)},
		{Label: "egress", Value: projectConfig.Network.ResolvedEgress()},
	}
	if count := len(projectConfig.Network.AllowDomain); count > 0 {
		fields = append(fields, views.ConfigField{Label: "allowed domains", Value: strings.Join(projectConfig.Network.AllowDomain, ", ")})
	}
	if count := len(projectConfig.Network.DenyDomain); count > 0 {
		fields = append(fields, views.ConfigField{Label: "denied domains", Value: strings.Join(projectConfig.Network.DenyDomain, ", ")})
	}
	return fields
}

func dashIfEmpty(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

// openTerminal opens the live embedded-terminal overlay running `ai <args…>` on a
// PTY, sized to the body. Workspace output streams into the pane (no suspend) and
// interactive programs run inside it. interactive=true means the child is a full
// interactive program that needs the arrow keys (a shell / attached session), so
// arrows are forwarded to it; interactive=false means a command run whose output
// streams (delete confirm, services update, keys), so the arrow/page keys SCROLL
// the pane's scrollback instead of being forwarded (otherwise they echo as ^[[A).
func (application *app) openTerminal(label string, args []string, interactive bool) tea.Cmd {
	argv := append([]string{executablePath()}, args...)
	term := views.NewTerminal(label, argv, interactive)
	bodyWidth, bodyHeight := application.bodyContentSize()
	term.SetSize(bodyWidth, application.terminalBodyHeight(bodyHeight))
	application.terminal = term
	application.terminalEscCloses = false // default: esc belongs to the inner program
	return term.Init()
}

// subTabBarRows is the height the per-workspace sub-tab bar (bar + blank line)
// occupies above the terminal when it is shown inside a workspace sub-tab.
const subTabBarRows = 2

// inWorkspaceSubTab reports whether the Workspaces hub is the active top-level view
// AND a workspace is open in it — i.e. an open terminal belongs to a per-workspace
// sub-tab and should keep the sub-tab bar visible above it.
func (application *app) inWorkspaceSubTab() bool {
	return application.current == application.projectsIndex &&
		capturesNav(application.views[application.current])
}

// terminalBodyHeight is the height the terminal pane gets: the full body, minus the
// sub-tab bar rows when it is shown inside a workspace sub-tab (so the bar fits above
// it without the terminal overflowing the body).
func (application *app) terminalBodyHeight(bodyHeight int) int {
	if !application.inWorkspaceSubTab() {
		return bodyHeight
	}
	height := bodyHeight - subTabBarRows
	if height < 1 {
		return 1
	}
	return height
}

// updateTerminal routes a key while the terminal overlay is open: ctrl+q
// force-detaches; once the process has exited any key closes; otherwise the key is
// forwarded to the PTY. Closing refreshes the project detail + sessions (a start /
// stop / kill may have changed them).
func (application *app) updateTerminal(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	term := application.terminal
	// In scrollback mode the pane owns every key (nav scrolls, esc/q resume live);
	// only ctrl+q still force-detaches.
	if term.InScrollMode() && key.String() != "ctrl+q" {
		return application, term.Update(key)
	}
	// <esc> cancels/closes a one-shot prompt overlay (keys add/edit/remove); for
	// interactive sessions esc is forwarded to the program instead.
	escCancels := application.terminalEscCloses && key.String() == "esc"
	if key.String() == "ctrl+q" || escCancels || term.Exited() {
		term.Close()
		application.terminal = nil
		// If the open workspace was just deleted it no longer exists — back out to the
		// switcher rather than leaving the hub on a gone project.
		if application.currentProject != "" {
			if _, ok := resolveProjectRoot(application.currentProject); !ok {
				application.currentProject = ""
				application.switchTab(application.projectsIndex)
				return application, application.projectsHub.Reset()
			}
		}
		// Refresh ONLY the sub-view under the active sub-tab (not every sub-view at
		// once). The top-level model/key views are cheap host-side fetches, so they
		// always refresh.
		commands := []tea.Cmd{application.projectsHub.RefreshActive()}
		if application.servicesView != nil {
			// A `services update` overlay closing must re-fetch the Services list / open
			// detail so the recreated container's state + log are reflected.
			commands = append(commands, application.servicesView.RefreshActive())
		}
		if application.cloudModelsView != nil {
			// Cloud Models lists ONLY models whose provider has a key, so a
			// `keys add`/`keys remove` overlay closing must re-fetch the registered
			// set so the list reflects the change immediately.
			commands = append(commands, application.cloudModelsView.Init())
		}
		if application.apiKeysView != nil {
			commands = append(commands, application.apiKeysView.Init())
		}
		return application, tea.Batch(commands...)
	}
	return application, term.Update(key)
}

// tabActivatable is a top-level view that runs a BACKGROUND poll (the Services view's
// embedded container log) and must pause it while another top-level tab is visible —
// mirroring the per-project sub-views' activatable contract in the hub.
type tabActivatable interface{ SetActive(active bool) }

// subTabClicker is implemented by views whose first content row is a sub-tab bar
// (the Workspaces hub while drilled, the Services list while a detail is open), so
// a click on that row can switch the sub-tab. x is the bar's own column (the
// chrome has already subtracted the body border + padding).
type subTabClicker interface {
	ClickSubTab(x int) (tea.Cmd, bool)
}

// handleMouse makes the tab bars clickable: a left-click on the top-level tab bar
// switches to the tab under the pointer, and a left-click on the first body row
// is offered to the active view's sub-tab bar (subTabClicker). Everything else is
// ignored; clicks are inert while any overlay (terminal/create/creating/help) is
// open so a modal can never be escaped by mouse.
func (application *app) handleMouse(msg tea.MouseMsg) tea.Cmd {
	// Wheel: scroll the focused list/table/viewport. Translated to the SAME up/down
	// the keyboard uses so every scrollable surface (bubbles tables, the viewport-
	// windowed lists, logview, the Project/Workspace pane) responds identically —
	// one row per wheel notch, k9s-style. Overlays own their own scrollback keys, so
	// wheel is inert while one is open.
	if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
		keyMsg := tea.KeyMsg{Type: tea.KeyDown}
		if msg.Button == tea.MouseButtonWheelUp {
			keyMsg = tea.KeyMsg{Type: tea.KeyUp}
		}
		switch application.wheelTarget() {
		case wheelNone:
			return nil
		case wheelCreate:
			return application.createView.Update(keyMsg)
		case wheelBody:
			var cmd tea.Cmd
			application.bodyViewport, cmd = application.bodyViewport.Update(keyMsg)
			return cmd
		default: // wheelActiveView
			return application.views[application.current].Update(keyMsg)
		}
	}

	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return nil
	}
	if application.terminal != nil || application.createView != nil ||
		application.creating != "" || application.helpOpen {
		return nil
	}
	tabRow := lipgloss.Height(application.header()) + headerGapRows
	// The tab bar may WRAP to several rows when it is wider than the window (see tabBar);
	// every row of it is clickable, and the body (hence the sub-tab bar) sits below the
	// whole wrapped bar.
	tabBarRows := application.tabBarRows()
	switch {
	case msg.Y >= tabRow && msg.Y < tabRow+tabBarRows:
		// Top-level tabs, wrap-aware: topTabAt mirrors tabBar()'s layout across rows.
		if index := application.topTabAt(msg.X, msg.Y-tabRow); index >= 0 && index != application.current {
			// Load the clicked tab (mirrors keyboard nav) — without this the tab switches
			// but its data never loads, leaving it on "loading…".
			return application.activateTab(index)
		}
		return nil
	case msg.Y == tabRow+tabBarRows+1+bodyPadY:
		// First body-content row (below the wrapped tab bar → border line → padding row):
		// the active view's sub-tab bar when it has one. Translate to the bar's own column.
		clickX := msg.X - 1 - bodyPadX
		if clickX < 0 {
			return nil
		}
		if clicker, ok := application.views[application.current].(subTabClicker); ok {
			cmd, _ := clicker.ClickSubTab(clickX)
			return cmd
		}
	}
	return nil
}

// wheelSurface is which scrollable surface a mouse-wheel event drives.
type wheelSurface int

const (
	wheelNone       wheelSurface = iota // nothing (terminal owns its keys; creating is modal)
	wheelCreate                         // the create wizard overlay (its current step's list)
	wheelBody                           // the outer body viewport (help text, or an overflowing no-sub-tab pane)
	wheelActiveView                     // the active view (its table/list selection or internal viewport)
)

// wheelTarget decides which surface a wheel/trackpad scroll drives, honouring the
// same precedence as key routing: the embedded terminal + the modal "creating…"
// pane are left alone; the create wizard and help overlays scroll even though they
// are overlays (they contain lists / scrollable text); otherwise the active view
// scrolls (its own content, or the body viewport when a no-sub-tab pane overflows).
func (application *app) wheelTarget() wheelSurface {
	switch {
	case application.terminal != nil || application.creating != "":
		return wheelNone
	case application.createView != nil:
		return wheelCreate
	case application.helpOpen:
		return wheelBody
	case !capturesNav(application.views[application.current]) && application.bodyOverflowing():
		return wheelBody
	default:
		return wheelActiveView
	}
}

// switchTab moves the active tab to index, wrapping around the ends so Tab/←/→ cycle
// (a direct number jump passes an in-range index). A no-op when there are no views.
// It pauses the OUTGOING view's background poll and resumes the INCOMING one (for
// views that run one), so the Services container-log poll only ticks while the
// Services tab is visible.
func (application *app) switchTab(index int) {
	count := len(application.views)
	if count == 0 {
		return
	}
	if outgoing, ok := application.views[application.current].(tabActivatable); ok {
		outgoing.SetActive(false)
	}
	application.current = ((index % count) + count) % count
	application.createFlash = "" // a transient create banner is dismissed on any tab switch
	// A fresh tab starts at the top of its (possibly overflowing) pane.
	application.bodyViewport.GotoTop()
	if incoming, ok := application.views[application.current].(tabActivatable); ok {
		incoming.SetActive(true)
	}
}

// activateTab switches to a top-level tab AND returns the now-active view's Init() load
// command. Every user tab-navigation path (keyboard tab/arrows/number keys AND mouse
// clicks) MUST go through this: a view's startup Init() result is routed to whatever tab
// was active at startup, so an inactive tab never receives it and would sit on "loading…"
// forever unless its load is re-fired when it becomes active. (This is the fix for the
// mouse-click path, which previously switched without re-loading.)
func (application *app) activateTab(index int) tea.Cmd {
	application.switchTab(index)
	return application.views[application.current].Init()
}

// scrollsBody reports whether the active top-level tab gets the body-level scroll:
// true when no overlay is open AND the active view has NO sub-tabs (it does not
// capture nav). A tab WITH sub-tabs renders directly (clipped) so its sub-tab bar
// stays pinned and its sub-panes scroll themselves.
func (application *app) scrollsBody() bool {
	if application.terminal != nil || application.createView != nil || application.helpOpen {
		return false
	}
	if len(application.views) == 0 {
		return false
	}
	return !capturesNav(application.views[application.current])
}

// bodyOverflowing reports whether the body viewport's current content is taller than
// the pane — the condition under which the dedicated body-scroll keys are engaged.
func (application *app) bodyOverflowing() bool {
	return application.bodyViewport.Height > 0 &&
		application.bodyViewport.TotalLineCount() > application.bodyViewport.Height
}

// resizeViews pushes the current inner body size (inside the border) to every view
// and the create overlay, so tables/viewports fit within the chrome.
func (application *app) resizeViews() {
	bodyWidth, bodyHeight := application.bodyContentSize()
	for _, view := range application.views {
		view.SetSize(bodyWidth, bodyHeight)
	}
	application.bodyViewport.Width = bodyWidth
	application.bodyViewport.Height = bodyHeight
	if application.createView != nil {
		application.createView.SetSize(bodyWidth, bodyHeight)
	}
	if application.terminal != nil {
		application.terminal.SetSize(bodyWidth, application.terminalBodyHeight(bodyHeight))
	}
}

func (application *app) View() string {
	if application.quitting {
		return ""
	}
	var content string
	switch {
	case application.terminal != nil:
		content = application.terminal.View()
		// When the terminal was launched from a workspace sub-tab (delete/apps/keys
		// prompts), keep the per-workspace sub-tab bar visible ABOVE it so the
		// tabs don't disappear — it reads like the other sub-tab panes, just with a
		// live terminal as the body.
		if application.inWorkspaceSubTab() {
			content = lipgloss.JoinVertical(lipgloss.Left,
				application.projectsHub.SubTabBar(), "", content)
		}
	case application.creating != "":
		content = application.creatingView()
	case application.createView != nil:
		content = application.createView.View()
	case application.helpOpen:
		content = application.helpView()
	default:
		view := application.views[application.current]
		if capturesNav(view) {
			// A tab WITH sub-tabs renders directly; body() clips it so the sub-tab bar
			// stays pinned and the outer pane never scrolls (sub-panes scroll themselves).
			content = view.View()
		} else {
			// A no-sub-tab tab flows through the body viewport so its pane scrolls when
			// the content overflows the body height (e.g. a tall table on a small window).
			bodyWidth, bodyHeight := application.bodyContentSize()
			application.bodyViewport.Width = bodyWidth
			application.bodyViewport.Height = bodyHeight
			application.bodyViewport.SetContent(view.View())
			content = application.bodyViewport.View()
		}
	}
	// A create result (error / warnings) shows as a transient banner above the body
	// until the next tab switch.
	if application.createFlash != "" {
		content = application.createFlash + "\n\n" + content
	}
	// header / tab bar / bordered body / footer, stacked top-to-bottom. The
	// overlays (help/create/describe) render INSIDE the body border, just as the
	// active view does.
	return lipgloss.JoinVertical(
		lipgloss.Left,
		application.header(),
		"", // headerGapRows: breathing room between the header and the tab bar
		application.tabBar(),
		application.body(content),
		application.footer(),
	)
}

// creatingView renders the live "creating…" pane: the accumulated step log (✓ per
// done step, → on the one in progress). Streamed from create.Execute via
// createProgressMsg.
func (application *app) creatingView() string {
	var body strings.Builder
	body.WriteString(ui.Heading.Render("Creating workspace "+application.creating+"…") + "\n\n")
	if len(application.createSteps) == 0 {
		body.WriteString(ui.Muted.Render("  starting…") + "\n")
	}
	for index, step := range application.createSteps {
		last := index == len(application.createSteps)-1
		marker := ui.Success.Render(ui.IconOK)
		if last {
			marker = ui.Primary.Render("→")
		}
		body.WriteString("  " + marker + " " + step + "\n")
	}
	body.WriteString("\n" + ui.Muted.Render("The UI stays responsive; run `ai agent <cli>` or `ai shell` afterwards to start a sandboxed session."))
	return body.String()
}

// helpView lists the global key bindings plus the active view's own bindings.
func (application *app) helpView() string {
	var help strings.Builder
	help.WriteString(ui.Heading.Render("Keys") + "\n\n")
	help.WriteString(ui.Muted.Render("Global") + "\n")
	help.WriteString("  tab/⇧tab  cycle tabs\n")
	help.WriteString("  ←/→       previous/next tab\n")
	help.WriteString("  1-9       jump to tab\n")
	help.WriteString("  ?         toggle this help\n")
	help.WriteString("  ↑/↓       navigate\n")
	help.WriteString("  PgUp/PgDn or the mouse wheel scroll the pane / move a list\n")
	help.WriteString("  q         quit\n\n")
	help.WriteString(ui.Muted.Render(application.views[application.current].Title()+" view") + "\n")
	help.WriteString("  " + application.views[application.current].Hints() + "\n")
	return help.String()
}

// footer is the status line (moved down from the old header): the deployment
// role, the model gateway, the current scope, and the active view. The key
// bindings now live at the top (see commandsPanel).
func (application *app) footer() string {
	scope := "server"
	if application.currentProject != "" {
		scope = "workspace:" + application.currentProject
	}
	return ui.Muted.Render(fmt.Sprintf("ai ui · role:%s · gateway:%s · scope:%s · view:%s",
		application.role, application.gateway, scope, application.activeTitle()))
}

func roleLabel() string {
	info, err := runtime.Load()
	if err != nil || info == nil || info.Role == "" {
		return "standalone"
	}
	return info.Role
}

func gatewayLabel() string {
	info, err := runtime.Load()
	host := ""
	if err == nil && info != nil {
		host = info.HostAddress()
	}
	_, _, url := runtime.ResolveGateway(host)
	return url
}

// nowRFC3339 stamps state writes (matches the CLI's clock helper).
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// openURL opens a console URL in the host's default browser. It returns quickly
// (Start, not Run) so the TUI never blocks on the browser launch.
func openURL(url string) error {
	if url == "" {
		return fmt.Errorf("no admin console for this service")
	}
	var opener string
	switch goruntime.GOOS {
	case "darwin":
		opener = "open"
	case "linux":
		opener = "xdg-open"
	default:
		return fmt.Errorf("cannot open URLs on %s", goruntime.GOOS)
	}
	return exec.Command(opener, url).Start()
}
