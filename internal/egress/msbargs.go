package egress

import (
	"fmt"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// hostGatewayTarget is the DNS name that, from inside a sandbox, resolves to the
// host machine (host.microsandbox.internal). MsbNetworkArgs uses it to RECOGNIZE
// when the model gateway is the local host (standalone) vs a remote server (client
// mode). It is the single source of truth runtime.DefaultGatewayHost so the
// host-gateway DNS name can't drift between the egress rules and the gateway wiring.
//
// NB: this NAME must NOT be used as a --net-rule TARGET. Verified live: a net-rule
// targeting host.microsandbox.internal (a host-NAME) lets the guest CONNECT to the
// msb gateway but msb does NOT host-forward it — the request never reaches the host
// service and the guest gets an empty reply. Only msb's `host` GROUP token
// (hostGroupTarget) engages host-forwarding. So we emit hostGroupTarget in rules
// and reserve hostGatewayTarget for the local-vs-remote comparison.
const hostGatewayTarget = runtime.DefaultGatewayHost

// hostGroupTarget is msb's `host` GROUP token — the rule target that means "the
// host machine" and engages msb's host-forwarding (so the guest actually reaches
// services ON the host, e.g. the model gateway and allow-listed local services).
// It is the only target that works for host access under a default-deny policy.
const hostGroupTarget = "host"

// gatewayToken is the placeholder in a HostService.Host that means "the host
// machine" (mirrors config's unexported gatewayToken; see egress.Allow/Deny).
const gatewayToken = "gateway"

// MsbNetworkArgs translates a project's egress policy (config.NetworkConfig)
// into the `msb create`/`msb run` argv fragment that enforces it. gatewayHost +
// gatewayPort are the resolved model-gateway endpoint every workspace must reach
// (arch §29.2): host.microsandbox.internal:18787 in standalone/local mode, or the
// remote server `ai gateway set` configured in client mode. The gateway is always
// allowed on this host:port so the agent can reach the model gateway in every mode.
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
//  2. the always-on DNS allow rules (udp/53 + tcp/53 to the `host` group),
//  3. --net-default-egress (deny for deny/public, allow for unrestricted),
//  4. for public mode, the broad allow:egress@public rule,
//  5. one allow rule per AllowHostServices entry (in slice order),
//  6. one -p publish-port flag per PublishPorts entry (in slice order).
func MsbNetworkArgs(network config.NetworkConfig, gatewayHost string, gatewayPort int) []string {
	mode := network.ResolvedEgress()

	args := make([]string, 0, 6+2*len(network.AllowHostServices)+2*len(network.PublishPorts))

	// (1) Always allow the model gateway on gatewayHost:gatewayPort so the agent can
	// reach it in every mode. When the gateway is the LOCAL host (standalone), the
	// rule MUST target msb's `host` GROUP token, not the host.microsandbox.internal
	// NAME: the NAME lets the guest connect to the msb gateway but is NOT
	// host-forwarded (verified live — empty reply, the request never reaches the
	// host). In client mode gatewayHost is a remote server, reached as an ordinary
	// external host (the public allow / its own allow rule), not via the host group.
	gatewayTarget := gatewayHost
	if gatewayHost == hostGatewayTarget {
		gatewayTarget = hostGroupTarget
	}
	args = append(args, "--net-rule", fmt.Sprintf("allow:egress@%s:tcp:%d", gatewayTarget, gatewayPort))

	// (1b) Always allow DNS to the LOCAL msb gateway forwarder. Under a
	// default-deny egress policy msb filters DNS like any other egress, and a
	// policy whose rules are all IP/host-name based matches nothing at
	// DNS-decision time (the query name hasn't resolved to an IP yet) — so every
	// lookup is denied and the guest can resolve nothing. msb's `host` GROUP
	// token is the documented matcher for "the gateway forwarder the query is
	// delivered to" (the convenience Rule::allow_dns(); see networking/dns docs).
	// Verified live: this is the only target that re-opens DNS under default-deny
	// (public/private/the resolved gateway name all fail). It always targets the
	// local `host` group — the DNS forwarder is local even in client mode — and
	// is emitted in every mode so name resolution works regardless of the policy.
	args = append(args, "--net-rule", "allow:egress@host:udp:53")
	args = append(args, "--net-rule", "allow:egress@host:tcp:53")

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
	// default-deny would otherwise block them). A "gateway" or empty Host targets
	// the host machine via msb's `host` GROUP token (which engages host-forwarding);
	// any other Host (a domain/IP) is an ordinary external target left verbatim.
	for _, service := range network.AllowHostServices {
		target := service.Host
		if target == "" || target == gatewayToken {
			target = hostGroupTarget
		}
		args = append(args, "--net-rule", fmt.Sprintf("allow:egress@%s:tcp:%d", target, service.Port))
	}

	// (5) Published ports (host -> guest), in all modes.
	for _, mapping := range network.PublishPorts {
		args = append(args, "-p", fmt.Sprintf("%d:%d", mapping.Host, mapping.Guest))
	}

	return args
}
