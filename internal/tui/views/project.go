package views

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/project"
	"github.com/jt-helsinki/stack-genie/internal/ui"
)

// spinnerFrames are the braille spinner glyphs for the in-flight lifecycle status.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// ProjectInfoFetcher returns the current state of one project (name, OS, agents,
// workspace status). Injected; the parent wires it over project.List.
type ProjectInfoFetcher func(name string) (project.Entry, bool, error)

// ConfigField is one diagnostic key/value of the live sandbox configuration, shown
// under the "Sandbox Configuration" heading. ConfigFetcher returns them for the named
// workspace (a nil fetcher, an error, or no fields → the block shows a hint instead).
type ConfigField struct{ Label, Value string }

// ConfigFetcher returns the live sandbox configuration fields for a workspace.
type ConfigFetcher func(name string) ([]ConfigField, error)

// EndpointsFetcher returns the host gateway endpoints (the nginx entry to LiteLLM)
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

// WorkspaceActionRequestedMsg is emitted for a workspace lifecycle action
// (start/stop/restart/delete). For start/stop/restart the parent app runs
// `ai <action> <name>` DETACHED (it keeps running even if `ai ui` is closed) and
// shows a spinner + status here while polling — the TUI stays navigable and no log
// is shown. delete runs in the confirm overlay.
type WorkspaceActionRequestedMsg struct {
	Action  string
	Project string
}

// WorkspaceResizeRequestedMsg is emitted when the user sets a new disk size via the
// inline "z" prompt on the Workspace tab. The parent persists workspace.disk_limit and
// restarts the workspace so the microVM's writable rootfs is resized (the SDK applies
// WithOCIUpperSize on the --replace rebuild at start).
type WorkspaceResizeRequestedMsg struct {
	Project string
	Disk    string // GiB, plain number
}

type projectRefreshedMsg struct {
	entry     project.Entry
	found     bool
	err       error
	config    []ConfigField
	configErr error
}

// Project is the detail view for the current project: its summary + workspace
// lifecycle (start/stop/restart/delete). The workspace LOG and METRICS are their own
// sub-tabs (Sandbox Logs / Metrics) — not embedded here.
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
	// configFields is the live sandbox configuration (diagnostics), refreshed with the
	// summary; configErr explains why it is unavailable (not running / CLI backend).
	configFields []ConfigField
	configErr    error
	// pending is the in-flight lifecycle action ("start"/"stop"/"restart"), shown as
	// an animated spinner on the workspace status line; empty when idle. The parent
	// sets/clears it (StartPending/ClearPending) and advances the frame (TickSpinner)
	// while it polls the detached action — so the spinner animates regardless of which
	// tab is focused.
	pending      string
	pendingFrame int
	// resizing + diskInput drive the inline "resize disk" prompt (z): while resizing,
	// keystrokes edit the GiB value and enter persists it + restarts the workspace.
	resizing  bool
	diskInput string
	// viewport makes the summary + Sandbox Configuration block scrollable when it
	// overflows the pane (e.g. a config with many fields). rendered caches the last
	// body so SetContent (and thus a scroll-position reset risk) only fires on change.
	viewport viewport.Model
	rendered string
	// endpoints resolves the host gateway endpoints (nginx entry to LiteLLM) shown in the
	// "Gateway Endpoints" block so external apps know how to reach the served models. It
	// is workspace-independent; endpointFields caches the one-time resolution.
	endpoints       EndpointsFetcher
	endpointFields  []ConfigField
	endpointsLoaded bool
}

// SetEndpoints injects the host gateway-endpoints fetcher (nginx → LiteLLM). Called
// once at wiring; the result is cached and rendered in the "Gateway Endpoints" block.
// A nil fetcher omits that block.
func (view *Project) SetEndpoints(fetcher EndpointsFetcher) { view.endpoints = fetcher }

// NewProject builds the project-detail (summary) view over the injected info fetcher
// and a live sandbox-configuration fetcher (for the "Sandbox Configuration" block;
// may be nil).
func NewProject(info ProjectInfoFetcher, configOf ConfigFetcher) *Project {
	return &Project{info: info, configOf: configOf, viewport: viewport.New(0, 0)}
}

// StartPending shows the "<action>ing…" spinner on the workspace status line; the
// parent calls this when it kicks off a detached lifecycle action.
func (view *Project) StartPending(action string) {
	view.pending = action
	view.pendingFrame = 0
	view.flash = ""
}

// TickSpinner advances the pending spinner one frame (driven by the parent's poll).
func (view *Project) TickSpinner() { view.pendingFrame++ }

// ClearPending stops the spinner (the action finished or timed out).
func (view *Project) ClearPending() { view.pending = "" }

// SetFlash shows a one-line message under the summary (e.g. a failure to launch a
// detached lifecycle action).
func (view *Project) SetFlash(message string) { view.flash = message }

func (view *Project) Title() string { return "Workspace" }
func (view *Project) Hints() string {
	if view.name == "" {
		return "open a workspace from the Workspaces view"
	}
	if view.resizing {
		return "type a disk size in GB · enter apply (restarts) · esc cancel"
	}
	return "s start · x stop · r restart · d delete · e shell · z disk · ↑/↓ scroll"
}

// CapturingInput reports whether the inline disk-resize prompt is open, so the parent
// routes keystrokes here (editing the value) instead of treating them as navigation.
func (view *Project) CapturingInput() bool { return view.resizing }

// handleResizeKey edits the inline disk-size value: digits append, backspace deletes,
// enter emits a resize request (persist + restart) when the value is a positive number,
// and esc cancels. Non-digits are ignored (disk size is a plain GB integer).
func (view *Project) handleResizeKey(key tea.KeyMsg) tea.Cmd {
	switch key.Type {
	case tea.KeyEsc:
		view.resizing = false
		view.diskInput = ""
		return nil
	case tea.KeyBackspace:
		if len(view.diskInput) > 0 {
			view.diskInput = view.diskInput[:len(view.diskInput)-1]
		}
		return nil
	case tea.KeyEnter:
		value := strings.TrimSpace(view.diskInput)
		name := view.name
		view.resizing = false
		view.diskInput = ""
		if value == "" || name == "" {
			return nil
		}
		view.flash = ui.Muted.Render("resizing disk to " + value + " GB — restarting…")
		return func() tea.Msg { return WorkspaceResizeRequestedMsg{Project: name, Disk: value} }
	case tea.KeyRunes:
		for _, glyph := range key.Runes {
			if glyph >= '0' && glyph <= '9' {
				view.diskInput += string(glyph)
			}
		}
		return nil
	}
	return nil
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
		// The inline "resize disk" prompt owns the keyboard while open.
		if view.resizing {
			return view.handleResizeKey(message)
		}
		// s/x/r FORCE start/stop/restart even with another lifecycle op already in
		// flight (mid-"starting…" spinner, or steady-state running) — the app
		// overrides the in-flight op and starts tracking the new one. z/e/d still
		// require a settled (non-pending) workspace — resizing/exec/delete mid-op is
		// not a "just redirect the outcome" action like start/stop/restart is.
		if name != "" {
			if action, ok := map[string]string{"s": "start", "x": "stop", "r": "restart"}[message.String()]; ok {
				return func() tea.Msg { return WorkspaceActionRequestedMsg{Action: action, Project: name} }
			}
		}
		if name != "" && view.pending == "" {
			if message.String() == "z" {
				// Begin the inline disk-resize prompt; next keystrokes edit the GiB value.
				view.resizing = true
				view.diskInput = ""
				view.flash = ""
				return nil
			}
			if message.String() == "e" {
				if view.entry.Status != "started" {
					view.flash = ui.Muted.Render("workspace not running — press s to start it first")
					return nil
				}
				return func() tea.Msg { return ExecRequestedMsg{Project: name} }
			}
			if message.String() == "d" {
				// The app handles this: delete confirms in the terminal overlay.
				return func() tea.Msg { return WorkspaceActionRequestedMsg{Action: "delete", Project: name} }
			}
		}
	}
	// Non-lifecycle keys (scroll) and other messages drive the scrollable viewport.
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

// renderBody builds the summary + Sandbox Configuration text shown in the viewport.
func (view *Project) renderBody() string {
	var body strings.Builder
	body.WriteString(ui.Heading.Render(view.entry.Name) + "\n")
	body.WriteString(field("OS", view.entry.OS))
	body.WriteString(field("agents", strings.Join(view.entry.Agents, ", ")))
	if view.pending != "" {
		// A lifecycle action is in flight: show the progress verb in orange with a spinner
		// (starting / restarting / stopping) — the workspace is NOT usable yet.
		glyph := ui.Warn.Render(spinnerFrames[view.pendingFrame%len(spinnerFrames)])
		body.WriteString(field("workspace", glyph+" "+ui.Warn.Render(pendingVerb(view.pending)+"…")))
	} else {
		body.WriteString(field("workspace", workspaceStatusLabel(view.entry.Status)))
	}
	body.WriteString(field("path", view.entry.Path))
	if view.resizing {
		body.WriteString(ui.Heading.Render("New disk size (GB): ") + ui.Value.Render(view.diskInput+"▏") + "\n")
	}
	if view.flash != "" {
		body.WriteString(view.flash + "\n")
	}
	// Sandbox Configuration (diagnostics): the live msb/SDK sandbox config.
	body.WriteString("\n")
	body.WriteString(ui.Heading.Render("Sandbox Configuration") + "\n")
	switch {
	case len(view.configFields) > 0:
		for _, configField := range view.configFields {
			body.WriteString(field(configField.Label, configField.Value))
		}
	case view.configErr != nil:
		body.WriteString("  " + ui.Muted.Render("unavailable — "+view.configErr.Error()) + "\n")
	default:
		body.WriteString("  " + ui.Muted.Render("not running — start the workspace to see its live configuration") + "\n")
	}
	// Gateway Endpoints: the host nginx entry to LiteLLM external apps use to reach the
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

// pendingVerb turns a lifecycle action into its progress verb (start → starting).
func pendingVerb(action string) string {
	switch action {
	case "start":
		return "starting"
	case "restart":
		return "restarting"
	case "stop":
		return "stopping"
	default:
		return action + "ing"
	}
}

// workspaceStatusLabel renders the workspace lifecycle status with a semantic colour:
// a running workspace is green ("running"); stopped / not-yet-created are muted. It
// maps both the state-handle vocabulary ("started") and the live SDK vocabulary
// ("running") to the same "running" label.
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
