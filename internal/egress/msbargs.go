package egress

import (
	"fmt"

	"github.com/jt-helsinki/ideal-robot/internal/config"
)

// hostGatewayTarget is the special msb target that resolves, from inside a
// sandbox, to the host machine (verified with msb v0.5.7). The in-VM agent
// reaches the host Headroom proxy (and any allow-listed host services) through
// this address.
const hostGatewayTarget = "host.microsandbox.internal"

// gatewayToken is the placeholder in a HostService.Host that means "the host
// machine" (mirrors config's unexported gatewayToken; see egress.Allow/Deny).
const gatewayToken = "gateway"

// MsbNetworkArgs translates a project's egress policy (config.NetworkConfig)
// into the `msb create`/`msb run` argv fragment that enforces it. gatewayPort
// is the host port the in-workspace Headroom proxy listens on (default 18787);
// the host gateway is always allowed on this port so the agent can reach the
// model gateway in every mode.
//
// msb's network engine (verified against `msb create --help`, v0.5.7) is
// first-match-wins over the ordered list of --net-rule entries, with
// --net-default-egress supplying the fallthrough action for traffic that
// matches no rule. Because --net-default-egress is the *default* and not a
// rule, it never participates in match ordering — only --net-rule entries do.
// We therefore emit the specific allow rules (gateway, then the host-service
// allow-list) ahead of relying on the default-deny fallthrough; in "public"
// mode the broad `allow:egress@public` rule is emitted after the specific
// host-gateway/host-service allows so that a future tightening of those
// targets (e.g. a deny rule) would still take precedence if placed before it.
//
// Emission order is stable and deterministic:
//  1. the always-on host-gateway allow rule,
//  2. --net-default-egress (deny for deny/public, allow for unrestricted),
//  3. for public mode, the broad allow:egress@public rule,
//  4. one allow rule per AllowHostServices entry (in slice order),
//  5. one -p publish-port flag per PublishPorts entry (in slice order).
func MsbNetworkArgs(network config.NetworkConfig, gatewayPort int) []string {
	mode := network.ResolvedEgress()

	args := make([]string, 0, 4+2*len(network.AllowHostServices)+2*len(network.PublishPorts))

	// (1) Always allow the host gateway on gatewayPort. The host gateway is a
	// private/link-local address which msb blocks by default, so this explicit
	// allow is required even in "public"/"unrestricted" modes (in "unrestricted"
	// it is harmless but kept for clarity).
	args = append(args, "--net-rule", fmt.Sprintf("allow:egress@%s:tcp:%d", hostGatewayTarget, gatewayPort))

	// (2) Default fallthrough action.
	switch mode {
	case "unrestricted":
		args = append(args, "--net-default-egress", "allow")
	default: // "deny" and "public" both default-deny; public widens via a rule.
		args = append(args, "--net-default-egress", "deny")
	}

	// (3) Public mode widens the default-deny posture to the open internet
	// while private ranges stay blocked by the default-deny fallthrough.
	if mode == "public" {
		args = append(args, "--net-rule", "allow:egress@public")
	}

	// (4) Per-host-service allow rules. These keep allow-listed private host
	// services reachable in every mode (including "public", where the
	// default-deny would otherwise block them). A "gateway" or empty Host
	// targets the host machine.
	for _, service := range network.AllowHostServices {
		target := service.Host
		if target == "" || target == gatewayToken {
			target = hostGatewayTarget
		}
		args = append(args, "--net-rule", fmt.Sprintf("allow:egress@%s:tcp:%d", target, service.Port))
	}

	// (5) Published ports (host -> guest), in all modes.
	for _, mapping := range network.PublishPorts {
		args = append(args, "-p", fmt.Sprintf("%d:%d", mapping.Host, mapping.Guest))
	}

	return args
}
