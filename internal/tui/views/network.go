package views

import (
	"slices"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
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

// NetworkMutator adds or removes one egress entry from a raw "host[:port]" or
// "guest:host" string. Injected; the parent wires egress.Allow/Deny/Publish/
// Unpublish (parsing the raw value via egress.SplitHostPort/SplitPortPair), so the
// view holds no parsing logic.
type NetworkMutator func(root, raw string) error

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
// mode, the host-service allow-list, and published ports, with a key to cycle
// the egress mode through config.EgressModes.
type Network struct {
	current   CurrentRoot
	get       NetworkGetter
	setMode   EgressModeSetter
	allow     NetworkMutator
	disallow  NetworkMutator
	publish   NetworkMutator
	unpublish NetworkMutator
	network   config.NetworkConfig
	flash     string
	err       error
	loaded    bool
	// inputMode is the active inline prompt ("allow"/"disallow"/"publish"/
	// "unpublish"), empty when not prompting; input is the value typed so far.
	inputMode string
	input     string
}

// NewNetwork builds the network view over the injected current-project resolver,
// policy getter, mode setter, and the four egress mutators (add/remove allow + port).
func NewNetwork(current CurrentRoot, get NetworkGetter, setMode EgressModeSetter,
	allow, disallow, publish, unpublish NetworkMutator) *Network {
	return &Network{
		current: current, get: get, setMode: setMode,
		allow: allow, disallow: disallow, publish: publish, unpublish: unpublish,
	}
}

func (view *Network) Title() string { return "Network" }
func (view *Network) Hints() string {
	return "m mode · a allow · d disallow · p publish · u unpublish"
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

// Update advances the view: a refresh fills the policy; "m" cycles the egress
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

// handleActionKey starts the mode cycle or an inline add/remove prompt.
func (view *Network) handleActionKey(key tea.KeyMsg) tea.Cmd {
	root, ok := view.current()
	if !ok {
		return nil
	}
	switch key.String() {
	case "m":
		next := nextEgressMode(view.network.ResolvedEgress())
		view.flash = ui.Muted.Render("setting egress " + next + "…")
		return view.modeCmd(root, next)
	case "a", "d", "p", "u":
		view.inputMode = map[string]string{"a": "allow", "d": "disallow", "p": "publish", "u": "unpublish"}[key.String()]
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
func (view *Network) mutateCmd(action, root, raw string) tea.Cmd {
	mutator := view.mutatorFor(action)
	return func() tea.Msg {
		if mutator == nil {
			return networkMutateDoneMsg{action: action}
		}
		return networkMutateDoneMsg{action: action, err: mutator(root, raw)}
	}
}

func (view *Network) mutatorFor(action string) NetworkMutator {
	switch action {
	case "allow":
		return view.allow
	case "disallow":
		return view.disallow
	case "publish":
		return view.publish
	case "unpublish":
		return view.unpublish
	}
	return nil
}

func (view *Network) modeCmd(root, mode string) tea.Cmd {
	setMode := view.setMode
	return func() tea.Msg {
		return networkModeDoneMsg{mode: mode, err: setMode(root, mode)}
	}
}

// nextEgressMode returns the mode after the current one in config.EgressModes,
// wrapping around (an unknown current mode starts the cycle at the first mode).
func nextEgressMode(current string) string {
	index := slices.Index(config.EgressModes, current)
	if index < 0 {
		return config.EgressModes[0]
	}
	return config.EgressModes[(index+1)%len(config.EgressModes)]
}

// View renders the egress mode, allow-list, and published ports (or the
// no-project / load / error line) with the latest flash.
func (view *Network) View() string {
	if _, ok := view.current(); !ok {
		return ui.Muted.Render("no project selected — open one from the Projects view")
	}
	if view.err != nil {
		return ui.Failure.Render(ui.IconFail + " " + view.err.Error())
	}
	if !view.loaded {
		return ui.Muted.Render("loading network policy…")
	}
	var body strings.Builder
	body.WriteString(ui.Heading.Render("Workspace egress") + "\n")
	body.WriteString(field("mode", view.network.ResolvedEgress()))
	body.WriteString(field("allow-list", egressAllowList(view.network.AllowHostServices)))
	body.WriteString(field("published ports", egressPublishPorts(view.network.PublishPorts)))
	switch {
	case view.inputMode != "":
		hint := "host[:port]"
		if view.inputMode == "publish" || view.inputMode == "unpublish" {
			hint = "guest:host"
		}
		body.WriteString("\n" + ui.Heading.Render(view.inputMode+" ") + view.input + "▏" +
			ui.Muted.Render("  ("+hint+" · enter apply · esc cancel)"))
	case view.flash != "":
		body.WriteString("\n" + view.flash)
	}
	return body.String()
}

func egressAllowList(services []config.HostService) string {
	if len(services) == 0 {
		return ""
	}
	parts := make([]string, 0, len(services))
	for _, service := range services {
		entry := service.Host
		if service.Port != 0 {
			entry += ":" + strconv.Itoa(service.Port)
		}
		parts = append(parts, entry)
	}
	return strings.Join(parts, ", ")
}

func egressPublishPorts(ports []config.PortMapping) string {
	if len(ports) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		parts = append(parts, strconv.Itoa(port.Host)+"→"+strconv.Itoa(port.Guest))
	}
	return strings.Join(parts, ", ")
}

// networkModeFlash renders the outcome of an egress-mode change.
func networkModeFlash(msg networkModeDoneMsg) string {
	if msg.err != nil {
		return ui.Failure.Render(ui.IconFail + " set egress " + msg.mode + ": " + msg.err.Error())
	}
	return ui.Success.Render(ui.IconOK + " egress set to " + msg.mode)
}
