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

type networkRefreshedMsg struct {
	network config.NetworkConfig
	err     error
}
type networkModeDoneMsg struct {
	mode string
	err  error
}

// Network is the per-project view of the workspace egress policy: the resolved
// mode, the host-service allow-list, and published ports, with a key to cycle
// the egress mode through config.EgressModes.
type Network struct {
	current CurrentRoot
	get     NetworkGetter
	setMode EgressModeSetter
	network config.NetworkConfig
	flash   string
	err     error
	loaded  bool
}

// NewNetwork builds the network view over the injected current-project resolver,
// policy getter, and mode setter.
func NewNetwork(current CurrentRoot, get NetworkGetter, setMode EgressModeSetter) *Network {
	return &Network{current: current, get: get, setMode: setMode}
}

func (view *Network) Title() string    { return "Network" }
func (view *Network) Hints() string    { return "m cycle mode" }
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
	case tea.KeyMsg:
		if message.String() == "m" {
			root, ok := view.current()
			if !ok {
				return nil
			}
			next := nextEgressMode(view.network.ResolvedEgress())
			view.flash = ui.Muted.Render("setting egress " + next + "…")
			return view.modeCmd(root, next)
		}
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
	if view.flash != "" {
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
