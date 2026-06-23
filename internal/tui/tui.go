// Package tui is the K9s-style full-screen management UI behind `ai ui`. It is a
// thin interactive layer over the existing platform package APIs: each screen
// (see internal/tui/views) reads/acts through injected funcs that wire to
// setup/workspace/egress/contextopt/litellm/secrets, so no management logic is
// duplicated here. The UI is interactive-only (it requires a TTY) and renders to
// stderr via the alternate screen, keeping stdout free of any output (consistent
// with the platform's JSON-envelope contract — `ai ui` simply has no envelope).
package tui

import (
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/contextopt"
	"github.com/jt-helsinki/ideal-robot/internal/egress"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/secrets"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
	"github.com/jt-helsinki/ideal-robot/internal/state"
	"github.com/jt-helsinki/ideal-robot/internal/tui/scope"
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

// Run resolves the startup scope from cwd, wires the views to the real platform
// APIs, and runs the full-screen program until the user exits.
func Run(cwd string) error {
	resolution, err := scope.Resolve(cwd)
	if err != nil {
		return err
	}

	application := &app{
		cwd:            cwd,
		currentProject: resolution.DefaultProject,
		role:           roleLabel(),
		gateway:        gatewayLabel(),
	}

	deps := setup.RealDeps(goruntime.GOOS, goruntime.GOARCH, nowRFC3339)
	litellmClient := litellm.RealClient()
	secretsBroker := secrets.RealBroker()
	// The per-project views (Network/Context) resolve the LIVE current project at
	// fetch time, so switching projects reflects immediately without re-wiring.
	currentRoot := func() (string, bool) { return resolveProjectRoot(application.currentProject) }

	servicesView := views.NewServices(
		func() ([]setup.ServiceStatus, error) { return setup.ServicesStatus(deps) },
		func(action, service string) error {
			_, err := setup.ControlService(deps, action, service)
			return err
		},
		openURL,
	)
	projectsView := views.NewProjects(project.List)
	projectDetail := views.NewProject(projectInfo, workspaceControl)
	networkView := views.NewNetwork(currentRoot, egress.Get, egress.SetMode)
	contextView := views.NewContext(currentRoot, contextopt.GetStatus, contextopt.SetStrategy, contextopt.SetCavemanLevel)
	modelsView := views.NewModels(litellmClient.Status, litellmClient.Test)
	secretsView := views.NewSecrets(secretsBroker.List, secretsBroker.Remove)

	// View order = menu order. Projects (the switcher) is index 1, Project detail
	// index 2 (the app points the detail at a project on selection).
	application.views = []View{servicesView, projectsView, projectDetail, networkView, contextView, modelsView, secretsView}
	application.projectsIndex = 1
	application.projectDetail = projectDetail
	application.projectDetailIndex = 2

	// A project at/above the cwd opens straight to its detail; otherwise the UI
	// opens on the server (Services) view. The switcher reaches any project.
	if resolution.DefaultProject != "" {
		projectDetail.SetProject(resolution.DefaultProject)
		application.current = application.projectDetailIndex
	}
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

// createFinishedMsg / execFinishedMsg report that a suspended subprocess (the
// project-create wizard / an in-workspace shell) has returned.
type createFinishedMsg struct{ err error }
type execFinishedMsg struct{ err error }

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

// workspaceControl applies a workspace lifecycle action to a project via the real
// Manager (the same one the `ai workspace` commands use).
func workspaceControl(action, projectName string) error {
	manager := workspace.RealManager(goruntime.GOOS, nowRFC3339)
	switch action {
	case "start":
		_, err := manager.Start(projectName)
		return err
	case "stop":
		return manager.Stop(projectName)
	case "restart":
		_, err := manager.Restart(projectName)
		return err
	case "destroy":
		return manager.Destroy(projectName)
	default:
		return fmt.Errorf("unknown workspace action %q", action)
	}
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

	// projectDetail + its index let the app point the detail view at a project
	// (a method off the View interface) when one is selected in the switcher;
	// projectsIndex is the switcher (refreshed after a create).
	projectDetail      *views.Project
	projectDetailIndex int
	projectsIndex      int
	currentProject     string

	// createView is the modal directory-picker overlay for creating a new
	// project; non-nil only while it is open (it is not a menu/slice view).
	createView *views.Create

	paletteOpen   bool
	palette       []paletteItem
	paletteCursor int

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

// reservedRows is the chrome height (header + its rule + footer) the body sits
// inside.
const reservedRows = 3

func (application *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch message := msg.(type) {
	case tea.WindowSizeMsg:
		application.width, application.height = message.Width, message.Height
		for _, view := range application.views {
			view.SetSize(message.Width, application.bodyHeight())
		}
		if application.createView != nil {
			application.createView.SetSize(message.Width, application.bodyHeight())
		}
		return application, nil

	case views.ProjectSelectedMsg:
		// The switcher chose a project — make it current and jump to its detail.
		application.currentProject = message.Name
		application.projectDetail.SetProject(message.Name)
		application.current = application.projectDetailIndex
		return application, application.projectDetail.Init()

	case views.NewProjectRequestedMsg:
		// Open the create overlay (a directory picker) starting at the cwd.
		create := views.NewCreate(application.cwd)
		create.SetSize(application.width, application.bodyHeight())
		application.createView = create
		return application, create.Init()

	case views.CreateCancelledMsg:
		application.createView = nil
		return application, nil

	case views.CreateConfirmedMsg:
		// Run the existing project-create wizard in the chosen directory (it
		// targets cwd), suspending the TUI for the interactive subprocess.
		application.createView = nil
		command := exec.Command(executablePath(), "project", "create")
		command.Dir = message.Dir
		return application, tea.ExecProcess(command, func(execErr error) tea.Msg {
			return createFinishedMsg{err: execErr}
		})

	case createFinishedMsg:
		// Back from the wizard — show the switcher and refresh it.
		application.current = application.projectsIndex
		return application, application.views[application.projectsIndex].Init()

	case views.ExecRequestedMsg:
		// Open an interactive shell inside the project's workspace microVM.
		command := exec.Command(executablePath(), "workspace", "exec", message.Project, "--", "/bin/sh")
		return application, tea.ExecProcess(command, func(execErr error) tea.Msg {
			return execFinishedMsg{err: execErr}
		})

	case execFinishedMsg:
		return application, application.projectDetail.Init()

	case tea.KeyMsg:
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
		switch message.String() {
		case "ctrl+c", "q":
			application.quitting = true
			return application, tea.Quit
		case ":", "/":
			application.paletteOpen = true
			application.paletteCursor = application.current
			return application, nil
		case "?":
			application.helpOpen = true
			return application, nil
		}
	}

	// Delegate everything else (ticks, refresh results, navigation, the
	// filepicker's own messages) to the create overlay if open, else the
	// active view.
	if application.createView != nil {
		return application, application.createView.Update(msg)
	}
	return application, application.views[application.current].Update(msg)
}

// bodyHeight is the content area height (window minus the header/footer chrome),
// clamped to at least one row.
func (application *app) bodyHeight() int {
	body := application.height - reservedRows
	if body < 1 {
		return 1
	}
	return body
}

// updatePalette handles input while the menu overlay is open.
func (application *app) updatePalette(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc", "q":
		application.paletteOpen = false
	case "up", "k":
		if application.paletteCursor > 0 {
			application.paletteCursor--
		}
	case "down", "j":
		if application.paletteCursor < len(application.palette)-1 {
			application.paletteCursor++
		}
	case "enter":
		item := application.palette[application.paletteCursor]
		application.paletteOpen = false
		if item.kind == paletteExit {
			application.quitting = true
			return application, tea.Quit
		}
		application.current = item.view
	}
	return application, nil
}

func (application *app) View() string {
	if application.quitting {
		return ""
	}
	var screen strings.Builder
	screen.WriteString(application.header() + "\n")
	switch {
	case application.createView != nil:
		screen.WriteString(application.createView.View())
	case application.helpOpen:
		screen.WriteString(application.helpView())
	case application.paletteOpen:
		screen.WriteString(application.paletteView())
	default:
		screen.WriteString(application.views[application.current].View())
	}
	screen.WriteString("\n" + application.footer())
	return screen.String()
}

// helpView lists the global key bindings plus the active view's own bindings.
func (application *app) helpView() string {
	var help strings.Builder
	help.WriteString(ui.Heading.Render("Keys") + "\n\n")
	help.WriteString(ui.Muted.Render("Global") + "\n")
	help.WriteString("  :, /    open the menu\n")
	help.WriteString("  ?       toggle this help\n")
	help.WriteString("  ↑/↓     navigate\n")
	help.WriteString("  q       quit\n\n")
	help.WriteString(ui.Muted.Render(application.views[application.current].Title()+" view") + "\n")
	help.WriteString("  " + application.views[application.current].Hints() + "\n")
	return help.String()
}

func (application *app) header() string {
	scope := "server"
	if application.currentProject != "" {
		scope = "project:" + application.currentProject
	}
	viewName := application.views[application.current].Title()
	if application.createView != nil {
		viewName = application.createView.Title()
	}
	title := ui.Heading.Render("ai ui")
	context := ui.Muted.Render(fmt.Sprintf("role:%s  gateway:%s  scope:%s  view:%s",
		application.role, application.gateway, scope, viewName))
	return title + "  " + context
}

func (application *app) footer() string {
	if application.createView != nil {
		return ui.Muted.Render(application.createView.Hints())
	}
	if application.helpOpen {
		return ui.Muted.Render("any key to close")
	}
	if application.paletteOpen {
		return ui.Muted.Render("↑/↓ select · enter choose · esc close")
	}
	global := ": menu · ? help · q quit"
	if hints := application.views[application.current].Hints(); hints != "" {
		return ui.Muted.Render(hints + " · " + global)
	}
	return ui.Muted.Render(global)
}

func (application *app) paletteView() string {
	var menu strings.Builder
	menu.WriteString(ui.Heading.Render("Menu") + "\n")
	for index, item := range application.palette {
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
		return fmt.Errorf("no console URL")
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
