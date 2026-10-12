package views

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/project"
	"github.com/jt-helsinki/stack-genie/internal/ui"
)

// ProjectInfoFetcher returns the current state of one project (name, OS, agents,
// workspace status). Injected; the parent wires it over project.List.
type ProjectInfoFetcher func(name string) (project.Entry, bool, error)

// ConfigField is one diagnostic key/value of the workspace's declared configuration,
// shown under the "Configuration" heading. ConfigFetcher returns them for the named
// workspace (a nil fetcher, an error, or no fields → the block shows a hint instead).
type ConfigField struct{ Label, Value string }

// ConfigFetcher returns the declared configuration fields for a workspace.
type ConfigFetcher func(name string) ([]ConfigField, error)

// EndpointsFetcher returns the host gateway endpoints (the proxy entry to LiteLLM)
// that external apps use to reach the served models, as label/URL fields. It is
// workspace-independent (one gateway per host) and resolved from runtime.yaml.
// Injected; a nil fetcher (or empty result) omits the "Gateway Endpoints" block.
type EndpointsFetcher func() []ConfigField

// ExecRequestedMsg is emitted when the user asks to open an interactive shell in
// the current project's workspace (the "e" key). The parent app suspends the TUI
// and tea.ExecProcess an interactive shell via `ai shell`.
type ExecRequestedMsg struct {
	Project string
}

// WorkspaceActionRequestedMsg is emitted for a workspace action: start/restart
// launch (or reattach to) the default agent CLI's sandboxed session, suspending
// the TUI into the user's real terminal (tea.ExecProcess, same as
// ExecRequestedMsg/AttachRequestedMsg); stop runs in-process (StopAll, no
// sandbox to suspend into); delete runs in the confirm overlay.
type WorkspaceActionRequestedMsg struct {
	Action  string
	Project string
}

type projectRefreshedMsg struct {
	entry     project.Entry
	found     bool
	err       error
	config    []ConfigField
	configErr error
}

// Project is the detail view for the current project: its summary + the
// start/stop/restart/delete/shell actions. Live sessions are their own sub-tab
// (Shell) — not embedded here.
type Project struct {
	info     ProjectInfoFetcher
	configOf ConfigFetcher
	name     string
	entry    project.Entry
	hasEntry bool
	flash    string
	err      error
	width    int
	height   int
	// configFields is the workspace's declared configuration (diagnostics),
	// refreshed with the summary; configErr explains why it is unavailable.
	configFields []ConfigField
	configErr    error
	// viewport makes the summary + Configuration block scrollable when it
	// overflows the pane (e.g. a config with many fields). rendered caches the last
	// body so SetContent (and thus a scroll-position reset risk) only fires on change.
	viewport viewport.Model
	rendered string
	// endpoints resolves the host gateway endpoints (proxy entry to LiteLLM) shown in the
	// "Gateway Endpoints" block so external apps know how to reach the served models. It
	// is workspace-independent; endpointFields caches the one-time resolution.
	endpoints       EndpointsFetcher
	endpointFields  []ConfigField
	endpointsLoaded bool
}

// SetEndpoints injects the host gateway-endpoints fetcher (proxy → LiteLLM). Called
// once at wiring; the result is cached and rendered in the "Gateway Endpoints" block.
// A nil fetcher omits that block.
func (view *Project) SetEndpoints(fetcher EndpointsFetcher) { view.endpoints = fetcher }

// NewProject builds the project-detail (summary) view over the injected info fetcher
// and a declared-configuration fetcher (for the "Configuration" block; may be nil).
func NewProject(info ProjectInfoFetcher, configOf ConfigFetcher) *Project {
	return &Project{info: info, configOf: configOf, viewport: viewport.New(0, 0)}
}

// SetFlash shows a one-line message under the summary (e.g. a failure to launch an
// action).
func (view *Project) SetFlash(message string) { view.flash = message }

func (view *Project) Title() string { return "Workspace" }
func (view *Project) Hints() string {
	if view.name == "" {
		return "open a workspace from the Workspaces view"
	}
	return "s start · x stop · r restart · d delete · e shell · ↑/↓ scroll"
}

func (view *Project) SetSize(width, height int) {
	view.width, view.height = width, height
	view.viewport.Width = width
	if height > 0 {
		view.viewport.Height = height
	}
}

// SetProject points the view at a project (the parent calls this, then Init, when
// the user selects one in the switcher).
func (view *Project) SetProject(name string) { view.name = name }

// Init refreshes the current project summary (no-op until a workspace is selected).
func (view *Project) Init() tea.Cmd {
	if view.name == "" {
		return nil
	}
	return view.refreshCmd()
}

func (view *Project) refreshCmd() tea.Cmd {
	info := view.info
	configOf := view.configOf
	name := view.name
	return func() tea.Msg {
		entry, found, err := info(name)
		message := projectRefreshedMsg{entry: entry, found: found, err: err}
		if configOf != nil {
			message.config, message.configErr = configOf(name)
		}
		return message
	}
}

// Update advances the detail view: a refresh fills the summary; s/x/r/d/e act on the
// workspace.
func (view *Project) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case projectRefreshedMsg:
		view.err = message.err
		view.hasEntry = message.found
		if message.err == nil && message.found {
			view.entry = message.entry
			view.flash = "" // a fresh status supersedes any stale hint
		}
		view.configFields = message.config
		view.configErr = message.configErr
		return nil
	case tea.KeyMsg:
		name := view.name
		if name == "" {
			break
		}
		if action, ok := map[string]string{"s": "start", "x": "stop", "r": "restart", "d": "delete"}[message.String()]; ok {
			return func() tea.Msg { return WorkspaceActionRequestedMsg{Action: action, Project: name} }
		}
		if message.String() == "e" {
			return func() tea.Msg { return ExecRequestedMsg{Project: name} }
		}
	}
	// Non-action keys (scroll) and other messages drive the scrollable viewport.
	var cmd tea.Cmd
	view.viewport, cmd = view.viewport.Update(msg)
	return cmd
}

func (view *Project) View() string {
	if view.name == "" {
		return ui.Muted.Render("no workspace selected — open one from the Workspaces view (menu: Workspaces)")
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.hasEntry {
		return ui.Muted.Render("loading " + view.name + "…")
	}
	body := view.renderBody()
	// Before the layout sizes the pane, render inline (no scroll); once sized, render
	// into the viewport so the summary + config scroll when they overflow.
	if view.viewport.Height <= 0 {
		return body
	}
	if body != view.rendered {
		view.viewport.SetContent(body)
		view.rendered = body
	}
	return view.viewport.View()
}

// renderBody builds the summary + Configuration text shown in the viewport.
func (view *Project) renderBody() string {
	var body strings.Builder
	body.WriteString(ui.Heading.Render(view.entry.Name) + "\n")
	body.WriteString(field("agents", strings.Join(view.entry.Agents, ", ")))
	body.WriteString(field("workspace", workspaceStatusLabel(view.entry.Status)))
	body.WriteString(field("path", view.entry.Path))
	if view.flash != "" {
		body.WriteString(view.flash + "\n")
	}
	// Configuration (diagnostics): the declared config.yaml — shell + egress policy.
	body.WriteString("\n")
	body.WriteString(ui.Heading.Render("Configuration") + "\n")
	switch {
	case len(view.configFields) > 0:
		for _, configField := range view.configFields {
			body.WriteString(field(configField.Label, configField.Value))
		}
	case view.configErr != nil:
		body.WriteString("  " + ui.Muted.Render("unavailable — "+view.configErr.Error()) + "\n")
	default:
		body.WriteString("  " + ui.Muted.Render("no configuration found") + "\n")
	}
	// Gateway Endpoints: the host proxy entry to LiteLLM external apps use to reach the
	// served models. Workspace-independent (one gateway per host), so it shows regardless
	// of workspace state. Resolved once and cached.
	if view.endpoints != nil {
		if !view.endpointsLoaded {
			view.endpointFields = view.endpoints()
			view.endpointsLoaded = true
		}
		if len(view.endpointFields) > 0 {
			body.WriteString("\n")
			body.WriteString(ui.Heading.Render("Gateway Endpoints") + "\n")
			for _, endpointField := range view.endpointFields {
				body.WriteString(field(endpointField.Label, endpointField.Value))
			}
		}
	}
	return body.String()
}

// workspaceStatusLabel renders the workspace lifecycle status with a semantic colour:
// a workspace with a live session is green ("running"); stopped / not-yet-created
// are muted. It maps both the state-handle vocabulary ("started") and the nono
// session vocabulary ("running") to the same "running" label.
func workspaceStatusLabel(status string) string {
	switch status {
	case "started", "running":
		return ui.Success.Render("running")
	case "stopped":
		return ui.Muted.Render("stopped")
	case "none", "":
		return ui.Muted.Render("not created")
	default:
		return status
	}
}

func field(label, value string) string {
	if value == "" {
		value = ui.Muted.Render("—")
	}
	return "  " + ui.Muted.Render(label+":") + " " + value + "\n"
}
