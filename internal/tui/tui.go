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
	goruntime "runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
	"github.com/jt-helsinki/ideal-robot/internal/tui/scope"
	"github.com/jt-helsinki/ideal-robot/internal/tui/views"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
)

// View is one screen of the UI. Views are pointer models that mutate in place;
// Update returns only a command (the parent owns the model), keeping the parent's
// routing simple. Satisfied structurally by the types in internal/tui/views.
type View interface {
	Init() tea.Cmd
	Update(tea.Msg) tea.Cmd
	View() string
	Title() string
	SetSize(width, height int)
}

// Run resolves the startup scope from cwd, wires the views to the real platform
// APIs, and runs the full-screen program until the user exits.
func Run(cwd string) error {
	resolution, err := scope.Resolve(cwd)
	if err != nil {
		return err
	}

	deps := setup.RealDeps(goruntime.GOOS, goruntime.GOARCH, nowRFC3339)
	servicesView := views.NewServices(func() ([]setup.ServiceStatus, error) {
		return setup.ServicesStatus(deps)
	})

	application := &app{
		views:      []View{servicesView},
		scopeLabel: scopeLabel(resolution),
		role:       roleLabel(),
		gateway:    gatewayLabel(),
	}
	application.buildPalette()

	program := tea.NewProgram(application, tea.WithAltScreen(), tea.WithOutput(os.Stderr))
	_, runErr := program.Run()
	return runErr
}

// app is the root tea.Model: it owns the views, the header/footer chrome, and the
// command palette (the menu, which includes Exit).
type app struct {
	views      []View
	current    int
	width      int
	height     int
	scopeLabel string
	role       string
	gateway    string

	paletteOpen   bool
	palette       []paletteItem
	paletteCursor int

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
		body := message.Height - reservedRows
		if body < 1 {
			body = 1
		}
		for _, view := range application.views {
			view.SetSize(message.Width, body)
		}
		return application, nil

	case tea.KeyMsg:
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
		}
	}

	// Delegate everything else (ticks, refresh results, navigation keys) to the
	// active view.
	return application, application.views[application.current].Update(msg)
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
	if application.paletteOpen {
		screen.WriteString(application.paletteView())
	} else {
		screen.WriteString(application.views[application.current].View())
	}
	screen.WriteString("\n" + application.footer())
	return screen.String()
}

func (application *app) header() string {
	title := ui.Heading.Render("ai ui")
	context := ui.Muted.Render(fmt.Sprintf("role:%s  gateway:%s  scope:%s  view:%s",
		application.role, application.gateway, application.scopeLabel,
		application.views[application.current].Title()))
	return title + "  " + context
}

func (application *app) footer() string {
	if application.paletteOpen {
		return ui.Muted.Render("↑/↓ select · enter choose · esc close")
	}
	return ui.Muted.Render(": menu · ↑/↓ navigate · q quit")
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

// scopeLabel describes the resolved startup scope for the header.
func scopeLabel(resolution scope.Resolution) string {
	if resolution.DefaultProject != "" {
		return "project:" + resolution.DefaultProject
	}
	return "server"
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
