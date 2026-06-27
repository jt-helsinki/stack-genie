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
	"io"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jt-helsinki/ideal-robot/internal/apps"
	"github.com/jt-helsinki/ideal-robot/internal/catalog"
	"github.com/jt-helsinki/ideal-robot/internal/contextopt"
	"github.com/jt-helsinki/ideal-robot/internal/egress"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/logs"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
	"github.com/jt-helsinki/ideal-robot/internal/state"
	"github.com/jt-helsinki/ideal-robot/internal/tui/views"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
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
	litellmClient := litellm.RealClient()
	// The per-project views (Network/Context) resolve the LIVE current project at
	// fetch time, so switching projects reflects immediately without re-wiring.
	currentRoot := func() (string, bool) { return resolveProjectRoot(application.currentProject) }

	servicesView := views.NewServices(
		func() ([]setup.ServiceStatus, error) { return setup.ServicesStatus(deps) },
		func(action, service string) error {
			_, err := setup.ControlService(deps, action, service)
			return err
		},
		func(service string) error {
			// Re-pull latest images + recreate. The TUI owns the screen, so the
			// native pull progress is discarded here (the flash reports completion).
			_, err := setup.UpdateService(deps, service, io.Discard, nil)
			return err
		},
		openURL,
		tailService, // logs are viewed from the Services view (the `l` key)
	)
	projectsView := views.NewProjects(project.List)
	projectDetail := views.NewProject(projectInfo)
	// The Sessions view resolves the LIVE current project at fetch time (over the
	// real Manager), so switching projects reflects immediately. With no current
	// project the lister is not invoked (the view shows "no project selected").
	sessionsView := views.NewSessions(
		func() ([]workspace.Session, error) {
			if application.currentProject == "" {
				return nil, nil
			}
			return workspace.RealManager(goruntime.GOOS, nowRFC3339).ListSessions(application.currentProject)
		},
		func(session string) error {
			return workspace.RealManager(goruntime.GOOS, nowRFC3339).KillSession(application.currentProject, session)
		},
		func() string { return application.currentProject },
	)
	// The Apps view resolves the LIVE current project at fetch time (over the real
	// AppManager), so switching workspaces reflects immediately. With no current
	// project the lister returns nothing (the view shows "no workspace selected").
	appsView := views.NewApps(
		func() ([]apps.Status, error) {
			if application.currentProject == "" {
				return nil, nil
			}
			manager, err := workspace.RealManager(goruntime.GOOS, nowRFC3339).AppManagerFor(application.currentProject)
			if err != nil {
				return nil, err
			}
			return manager.List()
		},
		func() string { return application.currentProject },
	)
	networkView := views.NewNetwork(currentRoot, egress.Get, egress.SetMode)
	contextView := views.NewContext(currentRoot, contextopt.GetStatus, contextopt.SetStrategy, contextopt.SetCavemanLevel)
	// Local Models: the installed Ollama store + the installable ollama.com library
	// (live, cache-backed), with a per-model tag drill-down and the gateway tester.
	localModelsView := views.NewLocalModels(
		ollama.RealClient().List,
		ollama.Library,
		ollama.RealClient().Show,
		litellmClient.Test,
	)
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
	// The Settings tab is a live theme picker plus read-only platform info.
	// Applying a theme persists it and recolors the whole UI (ThemeChangedMsg).
	settingsView := views.NewSettings(
		ui.ThemeNames(), ui.CurrentTheme,
		func(name string) error {
			if err := ui.Apply(name); err != nil {
				return err
			}
			return ui.SaveThemeName(name)
		},
		application.role, application.gateway,
	)

	// The Workspaces tab is a two-level hub: it opens on the switcher (the
	// workspace list) and, once a workspace is selected, reveals per-workspace
	// sub-tabs — Workspace · Network · Context · Sessions · Apps — for it.
	projectsHub := views.NewProjectsHub(
		projectsView,
		[]views.Screen{projectDetail, networkView, contextView, sessionsView, appsView},
		[]string{"Workspace", "Network", "Context", "Sessions", "Apps"},
	)

	// Top-level tab order = menu order: Services · Workspaces · Local Models · Cloud
	// Models · API Keys · Settings. Workspace / Network / Context / Sessions are
	// nested under Workspaces (the hub); logs are consolidated into the Services view
	// (the `l` key).
	application.views = []View{servicesView, projectsHub, localModelsView, cloudModelsView, apiKeysView, settingsView}
	application.projectsIndex = 1
	application.projectsHub = projectsHub
	application.projectDetail = projectDetail
	application.sessionsView = sessionsView
	application.appsView = appsView
	application.localModelsView = localModelsView
	application.cloudModelsView = cloudModelsView
	application.apiKeysView = apiKeysView

	// Always land on the home screen (Services, index 0 — current's zero value); a
	// project is opened only when the user selects it from the Projects switcher.
	application.buildPalette()

	program := tea.NewProgram(application, tea.WithAltScreen(), tea.WithOutput(os.Stderr))
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

// createFinishedMsg reports that the suspended workspace-create wizard subprocess
// has returned. (Workspace lifecycle / shell / attach run in the live embedded
// terminal overlay instead — see openTerminal — so they have no finished message.)
type createFinishedMsg struct{ err error }

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

// tailService resolves the host service's log source under ~/.ai-platform/logs
// (the first matching *.log file) and returns its last logs.TailLines lines. A
// service with nothing on disk yet yields an empty result (not an error) — live
// capture is wired during hardware bring-up.
func tailService(service string) ([]string, error) {
	sources, err := logs.Sources("", service)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return []string{}, nil
	}
	return logs.Tail(sources[0], logs.TailLines)
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
	creds, err := litellm.NewKeyManager(runtime.RealProber()).ListCredentials()
	if err != nil {
		return nil, err
	}
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
	return rows, nil
}

// loadCloudCatalog loads the models.dev catalog for the Cloud Models view, reporting
// the data SOURCE (fresh vs cached) + the live-fetch error so the view can message
// availability. It prefers a fresh fetch (persisting it) and falls back to the
// on-disk cache, mirroring catalog.LoadOrFetchStatus.
func loadCloudCatalog() (*catalog.Catalog, catalog.Source, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return catalog.LoadOrFetchStatus(ctx, nil, "")
}

// refreshModelCatalog is the Models view's `r`-refresh network work: it re-fetches
// the models.dev catalog (and persists it) then resyncs the gateway's model set to
// the keyed providers' catalog models + the installed Ollama models. The keyed set
// is read back from the live credential store so the desired set always reflects
// what is actually keyed. Best-effort — the caller surfaces any error as a flash and
// reloads the displayed data regardless.
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
	_, err = manager.SyncModels(cat, keyed, installedOllamaModels())
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

// installedOllamaModels lists the installed Ollama model names so a resync
// re-registers the local models alongside the keyed cloud providers. A down/empty
// Ollama is tolerated (returns nil) — it must never fail the resync.
func installedOllamaModels() []string {
	installed, err := ollama.RealClient().List()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(installed))
	for _, model := range installed {
		names = append(names, model.Name)
	}
	return names
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

	// sessionsView lets the app refresh the Sessions view when an attach
	// subprocess returns (the user may have created/killed a session).
	sessionsView *views.Sessions

	// appsView lets the app refresh the Apps view when an apps lifecycle
	// subprocess returns (the user may have installed/removed/started an app).
	appsView *views.Apps

	// localModelsView / cloudModelsView let the app refresh the model lists after a
	// pull/rm/keys subprocess returns from the terminal overlay.
	localModelsView *views.LocalModels
	cloudModelsView *views.CloudModels

	// apiKeysView lets the app refresh the provider/keyed list after an
	// `ai keys add|remove` subprocess returns from the terminal overlay.
	apiKeysView *views.APIKeys

	// createView is the modal directory-picker overlay for creating a new
	// project; non-nil only while it is open (it is not a menu/slice view).
	createView *views.Create

	// terminal is the live embedded-terminal overlay (workspace start/stop/…,
	// shell, agent, attach); non-nil only while it is open. While set it owns all
	// input — keystrokes are forwarded to the PTY — except ctrl+q (force-detach)
	// and, once the process has exited, any key (close).
	terminal *views.Terminal
	// terminalEscCloses makes <esc> cancel/close the terminal overlay (killing the
	// inner process) for one-shot prompt commands like `ai keys add/remove`, so the
	// user can back out of the add/edit-key screen with a single esc. It is false for
	// interactive sessions (shell/agent) where esc belongs to the program. Set when
	// the overlay is opened; reset by openTerminal.
	terminalEscCloses bool

	paletteOpen   bool
	palette       []paletteItem
	paletteCursor int
	paletteFilter string

	helpOpen bool
	quitting bool
}

type paletteKind int

const (
	paletteSwitch paletteKind = iota
	paletteExit
)

type paletteItem struct {
	label string
	kind  paletteKind
	view  int
}

func (application *app) buildPalette() {
	items := make([]paletteItem, 0, len(application.views)+1)
	for index, view := range application.views {
		items = append(items, paletteItem{label: view.Title(), kind: paletteSwitch, view: index})
	}
	items = append(items, paletteItem{label: "Exit", kind: paletteExit})
	application.palette = items
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

	case views.ThemeChangedMsg:
		// A theme was applied in Settings — re-push sizes so every view's table
		// re-picks the new styles (the chrome already reads the accent live).
		application.resizeViews()
		return application, nil

	case views.ProjectSelectedMsg:
		// The switcher chose a project — make it current (so the closure-driven
		// sub-views resolve it) and drop into it inside the Projects hub.
		application.currentProject = message.Name
		application.switchTab(application.projectsIndex)
		return application, application.projectsHub.OpenProject(message.Name)

	case views.NewProjectRequestedMsg:
		// Open the create overlay (a directory picker) starting at the cwd.
		create := views.NewCreate(application.cwd)
		bodyWidth, bodyHeight := application.bodyContentSize()
		create.SetSize(bodyWidth, bodyHeight)
		application.createView = create
		return application, create.Init()

	case views.CreateCancelledMsg:
		application.createView = nil
		return application, nil

	case views.CreateConfirmedMsg:
		// Run the existing project-create wizard in the chosen directory (it
		// targets cwd), suspending the TUI for the interactive subprocess.
		application.createView = nil
		command := exec.Command(executablePath(), "create")
		command.Dir = message.Dir
		return application, tea.ExecProcess(command, func(execErr error) tea.Msg {
			return createFinishedMsg{err: execErr}
		})

	case createFinishedMsg:
		// Back from the wizard — show the switcher (backed out of any open project)
		// and refresh it so a newly created project appears.
		application.switchTab(application.projectsIndex)
		return application, application.projectsHub.Reset()

	case views.WorkspaceActionRequestedMsg:
		// Run a workspace lifecycle action (start/stop/restart/destroy) in the live
		// embedded terminal: msb's verbose build/boot progress streams INTO the pane
		// (no suspend, no flicker) and a destructive `destroy` can prompt inline.
		return application, application.openTerminal(
			message.Action+" "+message.Project,
			[]string{message.Action, message.Project})

	case views.ExecRequestedMsg:
		// Open an interactive shell inside the workspace microVM, live in
		// the pane (a real PTY via `ai shell` → msb exec -t).
		return application, application.openTerminal(
			"shell "+message.Project,
			[]string{"shell", message.Project})

	case views.AttachRequestedMsg:
		// Attach to (or create) a workspace session, live in the pane (`ai attach
		// <session> <name>` → tmux new-session -A).
		return application, application.openTerminal(
			"attach "+message.Session+" "+message.Project,
			[]string{"attach", message.Session, message.Project})

	case views.AppActionRequestedMsg:
		// Run an apps lifecycle action live in the terminal overlay (`ai apps
		// <action> <app> <name>` → nerdctl in the VM); the Apps view refreshes when
		// the overlay closes.
		return application, application.openTerminal(
			"apps "+message.Action+" "+message.App+" "+message.Project,
			[]string{"apps", message.Action, message.App, message.Project})

	case views.ModelsPullRequestedMsg:
		// Pull one or more selected tag references (streaming progress): run the real
		// `ai models pull <refs...>` live in the terminal overlay, then refresh the
		// Local Models list when the overlay closes.
		return application, application.openTerminal(
			"models pull "+strings.Join(message.Refs, " "),
			append([]string{"models", "pull"}, message.Refs...))

	case views.ModelRemoveRequestedMsg:
		// Remove confirms before deleting: run `ai models rm <name>` live in the
		// overlay (its TTY confirm prompt shows in the pane), then refresh the list.
		return application, application.openTerminal(
			"models rm "+message.Name, []string{"models", "rm", message.Name})

	case views.APIKeyAddRequestedMsg:
		// Add runs `ai keys add <provider>` live in the overlay (its hidden key
		// prompt shows in the pane), then the API Keys view refreshes on close.
		// esc cancels the add/edit-key screen (one-shot prompt).
		cmd := application.openTerminal(
			"keys add "+message.Provider, []string{"keys", "add", message.Provider})
		application.terminalEscCloses = true
		return application, cmd

	case views.APIKeyRemoveRequestedMsg:
		// Remove runs `ai keys remove <provider>` live in the overlay, then refresh.
		// esc cancels the confirm screen.
		cmd := application.openTerminal(
			"keys remove "+message.Provider, []string{"keys", "remove", message.Provider})
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
		// The help overlay is dismissed by any key.
		if application.helpOpen {
			application.helpOpen = false
			return application, nil
		}
		if application.paletteOpen {
			return application.updatePalette(message)
		}
		// When the active view captures navigation (the Projects hub with a project
		// open), Tab/←→ cycle ITS sub-tabs and esc backs up a level inside it — so
		// those keys are delegated to the view rather than switching top-level tabs.
		captures := capturesNav(application.views[application.current])
		switch message.String() {
		case "ctrl+c", "q":
			application.quitting = true
			return application, tea.Quit
		case ":", "/":
			application.paletteOpen = true
			application.paletteCursor = application.current
			application.paletteFilter = ""
			return application, nil
		case "?":
			application.helpOpen = true
			return application, nil
		case "tab", "right":
			if captures {
				return application, application.views[application.current].Update(msg)
			}
			application.switchTab(application.current + 1)
			return application, application.views[application.current].Init()
		case "shift+tab", "left":
			if captures {
				return application, application.views[application.current].Update(msg)
			}
			application.switchTab(application.current - 1)
			return application, application.views[application.current].Init()
		}
		// Number keys 1-9 jump straight to that tab (1-based).
		if message.Type == tea.KeyRunes && len(message.Runes) == 1 {
			if digit := message.Runes[0]; digit >= '1' && digit <= '9' {
				target := int(digit - '1')
				if target < len(application.views) {
					application.switchTab(target)
					return application, application.views[application.current].Init()
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

// openTerminal opens the live embedded-terminal overlay running `ai <args…>` on a
// PTY, sized to the body. Workspace output streams into the pane (no suspend) and
// interactive programs run inside it.
func (application *app) openTerminal(label string, args []string) tea.Cmd {
	argv := append([]string{executablePath()}, args...)
	term := views.NewTerminal(label, argv)
	bodyWidth, bodyHeight := application.bodyContentSize()
	term.SetSize(bodyWidth, bodyHeight)
	application.terminal = term
	application.terminalEscCloses = false // default: esc belongs to the inner program
	return term.Init()
}

// updateTerminal routes a key while the terminal overlay is open: ctrl+q
// force-detaches; once the process has exited any key closes; otherwise the key is
// forwarded to the PTY. Closing refreshes the project detail + sessions (a start /
// stop / kill may have changed them).
func (application *app) updateTerminal(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	term := application.terminal
	// <esc> cancels/closes a one-shot prompt overlay (keys add/edit/remove); for
	// interactive sessions esc is forwarded to the program instead.
	escCancels := application.terminalEscCloses && key.String() == "esc"
	if key.String() == "ctrl+q" || escCancels || term.Exited() {
		term.Close()
		application.terminal = nil
		commands := []tea.Cmd{application.projectDetail.Init()}
		if application.sessionsView != nil {
			commands = append(commands, application.sessionsView.Init())
		}
		if application.appsView != nil {
			commands = append(commands, application.appsView.Init())
		}
		if application.localModelsView != nil {
			commands = append(commands, application.localModelsView.Init())
		}
		if application.cloudModelsView != nil {
			commands = append(commands, application.cloudModelsView.Init())
		}
		if application.apiKeysView != nil {
			commands = append(commands, application.apiKeysView.Init())
		}
		return application, tea.Batch(commands...)
	}
	return application, term.Update(key)
}

// switchTab moves the active tab to index, wrapping around the ends so Tab/←/→ cycle
// (a direct number jump passes an in-range index). A no-op when there are no views.
func (application *app) switchTab(index int) {
	count := len(application.views)
	if count == 0 {
		return
	}
	application.current = ((index % count) + count) % count
}

// resizeViews pushes the current inner body size (inside the border) to every view
// and the create overlay, so tables/viewports fit within the chrome.
func (application *app) resizeViews() {
	bodyWidth, bodyHeight := application.bodyContentSize()
	for _, view := range application.views {
		view.SetSize(bodyWidth, bodyHeight)
	}
	if application.createView != nil {
		application.createView.SetSize(bodyWidth, bodyHeight)
	}
	if application.terminal != nil {
		application.terminal.SetSize(bodyWidth, bodyHeight)
	}
}

// updatePalette handles input while the menu overlay is open. Typing letters
// filters the menu (case-insensitive substring); ↑/↓ + enter operate on the
// filtered list; backspace edits the filter; esc closes.
func (application *app) updatePalette(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	filtered := application.filteredPalette()
	switch key.String() {
	case "esc":
		application.paletteOpen = false
		application.paletteFilter = ""
	case "up":
		if application.paletteCursor > 0 {
			application.paletteCursor--
		}
	case "down":
		if application.paletteCursor < len(filtered)-1 {
			application.paletteCursor++
		}
	case "enter":
		if application.paletteCursor < len(filtered) {
			item := filtered[application.paletteCursor]
			application.paletteOpen = false
			application.paletteFilter = ""
			if item.kind == paletteExit {
				application.quitting = true
				return application, tea.Quit
			}
			application.current = item.view
		}
	case "backspace":
		if application.paletteFilter != "" {
			runes := []rune(application.paletteFilter)
			application.paletteFilter = string(runes[:len(runes)-1])
			application.paletteCursor = 0
		}
	default:
		// A single printable rune extends the filter.
		if key.Type == tea.KeyRunes && len(key.Runes) == 1 {
			application.paletteFilter += string(key.Runes)
			application.paletteCursor = 0
		}
	}
	return application, nil
}

// filteredPalette returns the menu items matching the current filter (all items
// when the filter is empty), in palette order.
func (application *app) filteredPalette() []paletteItem {
	if application.paletteFilter == "" {
		return application.palette
	}
	needle := strings.ToLower(application.paletteFilter)
	filtered := make([]paletteItem, 0, len(application.palette))
	for _, item := range application.palette {
		if strings.Contains(strings.ToLower(item.label), needle) {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func (application *app) View() string {
	if application.quitting {
		return ""
	}
	var content string
	switch {
	case application.terminal != nil:
		content = application.terminal.View()
	case application.createView != nil:
		content = application.createView.View()
	case application.helpOpen:
		content = application.helpView()
	case application.paletteOpen:
		content = application.paletteView()
	default:
		content = application.views[application.current].View()
	}
	// header / tab bar / bordered body / footer, stacked top-to-bottom. The
	// overlays (palette/help/create/describe) render INSIDE the body border, just
	// as the active view does.
	return lipgloss.JoinVertical(
		lipgloss.Left,
		application.header(),
		"", // headerGapRows: breathing room between the header and the tab bar
		application.tabBar(),
		application.body(content),
		application.footer(),
	)
}

// helpView lists the global key bindings plus the active view's own bindings.
func (application *app) helpView() string {
	var help strings.Builder
	help.WriteString(ui.Heading.Render("Keys") + "\n\n")
	help.WriteString(ui.Muted.Render("Global") + "\n")
	help.WriteString("  tab/⇧tab  cycle tabs\n")
	help.WriteString("  ←/→       previous/next tab\n")
	help.WriteString("  1-9       jump to tab\n")
	help.WriteString("  :, /      open the menu\n")
	help.WriteString("  ?         toggle this help\n")
	help.WriteString("  ↑/↓       navigate\n")
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

func (application *app) paletteView() string {
	var menu strings.Builder
	title := "Menu"
	if application.paletteFilter != "" {
		title += "  /" + application.paletteFilter
	}
	menu.WriteString(ui.Heading.Render(title) + "\n")
	filtered := application.filteredPalette()
	if len(filtered) == 0 {
		menu.WriteString(ui.Muted.Render("  (no match)") + "\n")
	}
	for index, item := range filtered {
		marker := "  "
		label := item.label
		if index == application.paletteCursor {
			marker = ui.Success.Render(ui.IconArrow) + " "
			label = ui.Heading.Render(label)
		}
		menu.WriteString(marker + label + "\n")
	}
	return menu.String()
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
