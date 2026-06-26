// Package uihosts is the host-side logic for the platform's UI subdomains: the
// Host-based nginx vhosts the web UIs are served on (only litellm.<domain> now,
// reached PORTLESS on the standard :80 nginx also publishes — no :18787 suffix)
// and the /etc/hosts entries that point those names at 127.0.0.1 in standalone
// mode. (Open WebUI is now a per-workspace in-VM app and is no longer a host UI
// vhost; Odysseus has been removed from the platform.)
//
// It ties together the service registry (internal/services — the single source
// of the UI→subdomain mapping) and internal/hostsfile (the managed-block writer),
// keeping the decision logic pure and unit-testable: callers in internal/setup
// and internal/uninstall use it to COMPUTE the entries, URLs, and operator
// guidance, then perform the privileged /etc/hosts write themselves (the package
// never elevates).
//
// It is a small composition package: it imports only internal/services and
// internal/hostsfile (both leaf packages), so it introduces no import cycle.
package uihosts

import (
	"fmt"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/hostsfile"
	"github.com/jt-helsinki/ideal-robot/internal/services"
)

// loopbackIP is where the UI subdomains resolve in standalone mode (the service
// tier runs on this host, fronted by nginx on loopback).
const loopbackIP = "127.0.0.1"

// gatewayPort is the single host port the nginx gateway publishes; every UI vhost
// is reached at <subdomain>.<domain>:gatewayPort. Kept here (not imported from
// internal/setup, which would be a cycle) and asserted against runtime's
// DefaultGatewayPort by the caller's tests.
const gatewayPort = 18787

// URL is one UI vhost's host-reachable address for display (doctor / setup
// guidance): the logical Service name, its Subdomain.<domain> Host, and the full
// portless HTTP URL (the UI subdomains answer on the standard :80 nginx also
// publishes — no :18787 suffix).
type URL struct {
	Service string
	Host    string
	URL     string
}

// Names returns the fully-qualified UI subdomain names for the resolved domain
// (e.g. [litellm.aip.local]), in registry order. domain is the already-resolved
// base domain (the caller passes runtime Info.ResolveDomain()).
//
// It returns the names for ALL UI services. (Currently only litellm; the host UI
// set has no optional members anymore — Open WebUI moved in-VM and Odysseus was
// removed.)
func Names(domain string) []string {
	var names []string
	for _, vhost := range services.UIVhosts() {
		names = append(names, vhost.Subdomain+"."+domain)
	}
	return names
}

// Entries returns the hostsfile entries that point the UI subdomains at 127.0.0.1
// for the resolved domain — ONE entry carrying ALL the UI names (so the managed
// block is a single line and stable across enable/disable), or nil when there are
// no UI services.
func Entries(domain string) []hostsfile.Entry {
	names := Names(domain)
	if len(names) == 0 {
		return nil
	}
	return []hostsfile.Entry{{IP: loopbackIP, Names: names}}
}

// URLs returns the display URLs for ALL UI vhosts, in registry order. The UI
// subdomains are reached PORTLESS on the standard HTTP port: nginx publishes host
// :80 → container :80 (alongside :18787), so litellm.<domain> answers with no
// :18787 suffix. (The gateway/API surfaces — /ollama, /v1, the proxy entry — keep
// :18787; only these browser-facing UI subdomains drop the port.)
func URLs(domain string) []URL {
	var urls []URL
	for _, vhost := range services.UIVhosts() {
		host := vhost.Subdomain + "." + domain
		urls = append(urls, URL{
			Service: vhost.Name,
			Host:    host,
			URL:     "http://" + host,
		})
	}
	return urls
}

// RenderBlock returns the exact /etc/hosts managed block (markers + entries) for
// the resolved domain — what a user would paste manually when the platform cannot
// write /etc/hosts itself.
func RenderBlock(domain string) string {
	return hostsfile.Render(Entries(domain))
}

// ManualMessage is the message shown when the platform does NOT write /etc/hosts
// (no TTY / --json, declined, or a sudo failure): a clear instruction plus the
// exact block to paste. It always ends with a trailing newline.
func ManualMessage(domain string) string {
	var builder strings.Builder
	builder.WriteString("To reach the platform UIs by name, add this block to /etc/hosts ")
	builder.WriteString("(needs sudo):\n\n")
	builder.WriteString(RenderBlock(domain))
	builder.WriteString("\nThen open: ")
	urls := URLs(domain)
	parts := make([]string, 0, len(urls))
	for _, url := range urls {
		parts = append(parts, url.URL)
	}
	builder.WriteString(strings.Join(parts, "  "))
	builder.WriteString("\n")
	return builder.String()
}

// ServerGuidance is the operator contract printed in SERVER mode (and surfaced by
// `ai doctor` on a server): create real DNS records for the UI subdomains pointed
// at this server, and provide a TLS cert terminated at nginx. The platform does
// NOT edit /etc/hosts on a server. It always ends with a trailing newline.
func ServerGuidance(domain string) string {
	var builder strings.Builder
	builder.WriteString("Server mode — DNS + TLS are operator-managed (no /etc/hosts editing):\n")
	builder.WriteString(fmt.Sprintf(
		"  1. Create DNS records pointing the UI subdomains at THIS server's IP:\n"+
			"       *.%s            (a wildcard), or per host:\n", domain))
	for _, name := range Names(domain) {
		builder.WriteString("       " + name + "\n")
	}
	builder.WriteString(fmt.Sprintf(
		"  2. Provide a TLS certificate terminated at nginx — a wildcard for *.%s\n"+
			"     (e.g. Let's Encrypt DNS-01) or an internal CA. Clients then reach the\n"+
			"     UIs over HTTPS at the names above (portless http on :80 until TLS lands).\n",
		domain))
	return builder.String()
}

// ServerCredentialsGuide is the post-install admin-access guidance printed after a
// SERVER-mode setup (and surfaced by `ai doctor`): for each exposed UI it lists the
// host-reachable URL and how to set or rotate its admin password. A server binds
// the service tier to 0.0.0.0, so these UIs are network-exposed and MUST be
// secured. It always ends with a trailing newline.
//
//   - LiteLLM admin UI — set/rotate with `ai litellm password`.
//
// (Open WebUI is now a per-workspace in-VM app, not a host UI; Odysseus has been
// removed — so neither appears here anymore.)
func ServerCredentialsGuide(domain string) string {
	var builder strings.Builder
	builder.WriteString("Set up admin access for the exposed UIs (server mode is network-exposed):\n")
	for _, url := range URLs(domain) {
		switch url.Service {
		case "litellm":
			builder.WriteString("  - LiteLLM admin UI — " + url.URL + "/ui\n")
			builder.WriteString("      set/rotate the admin password with: ai litellm password\n")
		default:
			builder.WriteString("  - " + url.Service + " — " + url.URL + "\n")
		}
	}
	return builder.String()
}

// GatewayPort exposes the fixed gateway port for callers that need to assert it
// matches runtime.DefaultGatewayPort (keeping the two in sync without an import).
func GatewayPort() int { return gatewayPort }
