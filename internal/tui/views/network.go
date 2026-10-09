package views

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/ui"
)

// CurrentRoot returns the current project's root directory and whether a project
// is selected. Injected and shared by the per-project views (Network, Context);
// the parent wires it over the app's current-project state. Defined once here.
type CurrentRoot func() (root string, ok bool)

// NetworkGetter returns the per-project egress policy. Injected; the parent
// wires egress.Get(root).
type NetworkGetter func(root string) (config.NetworkConfig, error)

// EgressModeSetter sets the project's egress mode. Injected; the parent wires
// egress.SetMode(root, mode).
type EgressModeSetter func(root, mode string) error

// NetworkMutator adds or removes one domain from the egress allow/deny list.
// Injected; the parent wires egress.Allow/Deny/Clear, so the view holds no
// validation logic.
type NetworkMutator func(root, domain string) error

// ProjectNameGetter returns the name of the currently-viewed workspace (empty if
// none). Injected; the parent wires it over its current-project state — distinct
// from CurrentRoot, which resolves the project's root path, not its name.
type ProjectNameGetter func() string

type networkRefreshedMsg struct {
	network config.NetworkConfig
	err     error
}
type networkModeDoneMsg struct {
	mode string
	err  error
}
type networkMutateDoneMsg struct {
	action string
	err    error
}

// Network is the per-project view of the workspace egress policy: the resolved
// mode and the allow/deny domain lists, applied the next time a nono profile is
// generated for the workspace (there is no persistent sandbox to restart).
type Network struct {
	current     CurrentRoot
	projectName ProjectNameGetter
	get         NetworkGetter
	setMode     EgressModeSetter
	allow       NetworkMutator
	disallow    NetworkMutator
	network     config.NetworkConfig
	flash       string
	err         error
	loaded      bool
	// inputMode is the active inline prompt ("allow"/"disallow"), empty when not
	// prompting; input is the value typed so far.
	inputMode string
	input     string
}

// NewNetwork builds the network view over the injected current-project resolver,
// policy getter, mode setter, and the two domain mutators (allow/deny).
// projectName resolves the current workspace's name (kept for parity with other
// per-project views; unused here since there is nothing to restart).
func NewNetwork(current CurrentRoot, projectName ProjectNameGetter, get NetworkGetter, setMode EgressModeSetter,
	allow, disallow NetworkMutator) *Network {
	return &Network{
		current: current, projectName: projectName, get: get, setMode: setMode,
		allow: allow, disallow: disallow,
	}
}

func (view *Network) Title() string { return "Network" }

// CapturingInput reports whether an inline allow/disallow prompt is open, so the
// app routes all keys here.
func (view *Network) CapturingInput() bool { return view.inputMode != "" }

func (view *Network) Hints() string {
	return "i internet on/off · a allow domain · d disallow domain"
}
func (view *Network) SetSize(int, int) {}

// Init refreshes the current project's egress policy (no-op with no project).
func (view *Network) Init() tea.Cmd {
	root, ok := view.current()
	if !ok {
		return nil
	}
	return view.refreshCmd(root)
}

func (view *Network) refreshCmd(root string) tea.Cmd {
	get := view.get
	return func() tea.Msg {
		network, err := get(root)
		return networkRefreshedMsg{network: network, err: err}
	}
}

// Update advances the view: a refresh fills the policy; "m"/"i" set the egress
// mode (async); the mode result flashes and re-refreshes.
func (view *Network) Update(msg tea.Msg) tea.Cmd {
	switch message := msg.(type) {
	case networkRefreshedMsg:
		view.loaded = true
		view.err = message.err
		if message.err == nil {
			view.network = message.network
		}
		return nil
	case networkModeDoneMsg:
		view.flash = networkModeFlash(message)
		root, ok := view.current()
		if !ok {
			return nil
		}
		return view.refreshCmd(root)
	case networkMutateDoneMsg:
		if message.err != nil {
			view.flash = ui.Failure.Render(ui.IconFail + " " + message.action + ": " + message.err.Error())
		} else {
			view.flash = ui.Success.Render(ui.IconOK + " " + message.action + " applied")
		}
		root, ok := view.current()
		if !ok {
			return nil
		}
		return view.refreshCmd(root) // reflect the change immediately
	case tea.KeyMsg:
		if view.inputMode != "" {
			return view.handleInputKey(message)
		}
		return view.handleActionKey(message)
	}
	return nil
}

// handleActionKey toggles internet on/off or starts an inline allow/disallow prompt.
func (view *Network) handleActionKey(key tea.KeyMsg) tea.Cmd {
	root, ok := view.current()
	if !ok {
		return nil
	}
	switch key.String() {
	case "i":
		next := "public"
		if view.network.ResolvedEgress() != "deny" {
			next = "deny"
		}
		view.flash = ui.Muted.Render("turning internet " + internetLabel(next) + "…")
		return view.modeCmd(root, next)
	case "a", "d":
		view.inputMode = map[string]string{"a": "allow", "d": "disallow"}[key.String()]
		view.input = ""
		view.flash = ""
	}
	return nil
}

// handleInputKey edits the inline prompt: runes append, backspace deletes, enter
// applies the value via the matching mutator, esc cancels. Spaces are dropped.
func (view *Network) handleInputKey(key tea.KeyMsg) tea.Cmd {
	switch key.Type {
	case tea.KeyEsc:
		view.inputMode, view.input = "", ""
		return nil
	case tea.KeyEnter:
		action, raw := view.inputMode, strings.TrimSpace(view.input)
		view.inputMode, view.input = "", ""
		root, ok := view.current()
		if raw == "" || !ok {
			return nil
		}
		view.flash = ui.Muted.Render(action + " " + raw + "…")
		return view.mutateCmd(action, root, raw)
	case tea.KeyBackspace:
		runes := []rune(view.input)
		if len(runes) > 0 {
			view.input = string(runes[:len(runes)-1])
		}
		return nil
	case tea.KeyRunes:
		for _, char := range key.Runes {
			if char != ' ' {
				view.input += string(char)
			}
		}
		return nil
	}
	return nil
}

// mutateCmd runs the chosen egress mutation off the UI thread.
func (view *Network) mutateCmd(action, root, domain string) tea.Cmd {
	mutator := view.mutatorFor(action)
	return func() tea.Msg {
		if mutator == nil {
			return networkMutateDoneMsg{action: action}
		}
		return networkMutateDoneMsg{action: action, err: mutator(root, domain)}
	}
}

func (view *Network) mutatorFor(action string) NetworkMutator {
	switch action {
	case "allow":
		return view.allow
	case "disallow":
		return view.disallow
	}
	return nil
}

func (view *Network) modeCmd(root, mode string) tea.Cmd {
	setMode := view.setMode
	return func() tea.Msg {
		return networkModeDoneMsg{mode: mode, err: setMode(root, mode)}
	}
}

// internetLabel renders an egress mode as a plain on/off switch label: "deny" blocks
// all outbound traffic (off); any other mode (public) allows it (on).
func internetLabel(mode string) string {
	if mode == "deny" {
		return "off"
	}
	return "on"
}

// View renders the egress mode and the allow/deny domain lists (or the
// no-project / load / error line) with the latest flash.
func (view *Network) View() string {
	if _, ok := view.current(); !ok {
		return ui.Muted.Render("no workspace selected — open one from the Workspaces view")
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading network policy…")
	}
	var body strings.Builder
	body.WriteString(ui.Heading.Render("Workspace egress") + "\n")
	body.WriteString(field("internet", internetLabel(view.network.ResolvedEgress())))
	body.WriteString(field("mode", view.network.ResolvedEgress()))
	body.WriteString(field("allow", strings.Join(view.network.AllowDomain, ", ")))
	body.WriteString(field("deny", strings.Join(view.network.DenyDomain, ", ")))
	switch {
	case view.inputMode != "":
		body.WriteString("\n" + ui.Heading.Render(view.inputMode+" ") + view.input + "▏" +
			ui.Muted.Render("  (domain, e.g. api.github.com · enter apply · esc cancel)"))
	case view.flash != "":
		body.WriteString("\n" + view.flash)
	}
	return body.String()
}

// networkModeFlash renders the outcome of an egress-mode change.
func networkModeFlash(msg networkModeDoneMsg) string {
	if msg.err != nil {
		return ui.Failure.Render(ui.IconFail + " set egress " + msg.mode + ": " + msg.err.Error())
	}
	return ui.Success.Render(ui.IconOK + " egress set to " + msg.mode)
}
