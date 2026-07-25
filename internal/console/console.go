// Package console is the single source of truth for the host services'
// reachable endpoints: the address a user (or workspace) can hit, and — where
// the service has one — its admin-console URL. `ai setup`, `ai doctor`, and
// `ai services status` all read from this registry so the user can find each
// service's UI, and `ai services console` opens the admin URL in the browser.
// Services without a host address and/or a web console are listed explicitly so
// the CLI can tell "no console" apart from "unknown service".
package console

import (
	"fmt"
	"os/exec"
	"sort"

	"github.com/jt-helsinki/stack-genie/internal/services"
)

// Endpoint is what a host service exposes. Address is the host-reachable URL or
// host:port the user can hit (empty when the service publishes nothing to the
// host, e.g. the microVM runtime). Console is the admin-UI URL (empty when the
// service has no web console — e.g. an HTTP API or a proxy with no UI).
type Endpoint struct {
	Address string `json:"address,omitempty"`
	Console string `json:"console,omitempty"`
}

// DefaultHost is the display host used by the host-agnostic accessors
// (EndpointFor/Address/URL/WithConsoles). It is the platform base DOMAIN the
// nginx UI subdomains hang off and the host-CLI gateway paths resolve under. The
// gateway (aip-proxy) binds loopback in every role except server, and the UI
// subdomains map to 127.0.0.1 via /etc/hosts in standalone — so the default
// domain renders reachable browser URLs. Server/other domains are threaded in via
// the *Host variants (see internal/setup statusDisplayHost / runtime.ResolveDomain).
// This is DISPLAY ONLY — it never changes the container bind.
const DefaultHost = "localhost"

// gatewayPort is the single nginx gateway (aip-proxy) host port — the SOLE host
// entry to the service tier. Every UI subdomain and host-CLI gateway path is
// reached on this port. Mirrors services.GatewayPort.
const gatewayPort = services.GatewayPort

// endpointSpec is the host-agnostic data for a service's endpoint. Three shapes,
// all reached through the single nginx gateway port now:
//   - a UI vhost (uiSubdomain set): served at <uiSubdomain>.<domain>:gatewayPort,
//     console at that base + consolePath.
//   - a host-CLI gateway path (gatewayPath set, e.g. ollama "/ollama"): reached at
//     http://<host>:gatewayPort<gatewayPath> (HTTP API, no console).
//   - a directly-published host port (port != 0, e.g. the proxy itself on
//     gatewayPort): http://<host>:port.
//
// loopbackAddress, when set, is used verbatim regardless of the display host (e.g.
// the DNS resolver, which always stays on loopback). A spec with none of these set
// is internal-only and renders empty.
type endpointSpec struct {
	port            int
	consolePath     string // "" → no console; "/" or "" semantics: see endpointForHost
	hasConsole      bool   // distinguishes "console at the root URL" from "no console"
	loopbackAddress string // verbatim address, host-independent (loopback-only services)
	uiSubdomain     string // nginx UI vhost label (<uiSubdomain>.<domain>:gatewayPort)
	gatewayPath     string // host-CLI gateway prefix (http://<host>:gatewayPort<path>)
}

// registry maps a host service to its endpoint spec. The data is DERIVED from the
// internal/services registry (the single source of truth for the platform's
// service topology), so it cannot drift from the log scopes / version pins / setup
// reconcile. nginx (aip-proxy) is the SOLE host entry on the gateway port now; the
// per-service direct ports are internal-only. The endpoints render through nginx:
//   - litellm  admin UI at litellm.<domain>:18787/ui/login (nginx vhost; :4000 internal)
//   - ollama   http://<domain>:18787/ollama (host-CLI gateway path; :11434 internal)
//   - proxy    http://<domain>:18787 (aip-proxy nginx gateway entry; no separate UI)
//   - dns      127.0.0.1:15353/udp (aip-dns CoreDNS egress-audit resolver, loopback)
//   - headroom is internal-only on :8787 behind nginx (no longer host-published)
//   - presidio analyzer/anonymizer are internal-only on :3000 (not host-published)
//   - microsandbox is the microVM runtime (no host address, no console)
var registry = buildRegistry()

// buildRegistry projects the internal/services endpoint topology into this
// package's host-display endpointSpec (the host-rendering logic stays here).
func buildRegistry() map[string]endpointSpec {
	specs := make(map[string]endpointSpec)
	for name, endpoint := range services.Endpoints() {
		specs[name] = endpointSpec{
			port:            endpoint.Port,
			consolePath:     endpoint.ConsolePath,
			hasConsole:      endpoint.HasConsole,
			loopbackAddress: endpoint.LoopbackAddress,
			uiSubdomain:     endpoint.UISubdomain,
			gatewayPath:     endpoint.GatewayPath,
		}
	}
	return specs
}

// endpointForHost renders a spec into a concrete Endpoint for the given display
// host (the platform base DOMAIN). All host-reachable endpoints now go through the
// single nginx gateway port:
//   - loopback-only specs (e.g. dns) ignore host and use their verbatim address;
//   - a UI vhost renders http://<uiSubdomain>.<host>:gatewayPort (+ consolePath);
//   - a gateway-path spec (e.g. ollama) renders http://<host>:gatewayPort<path>
//     (HTTP API, no console);
//   - a directly-published port (e.g. the proxy) renders http://<host>:port;
//   - everything else is internal-only and renders empty.
func (spec endpointSpec) endpointForHost(host string) Endpoint {
	if spec.loopbackAddress != "" {
		return Endpoint{Address: spec.loopbackAddress}
	}
	if spec.uiSubdomain != "" {
		base := fmt.Sprintf("http://%s.%s:%d", spec.uiSubdomain, host, gatewayPort)
		endpoint := Endpoint{Address: base}
		if spec.hasConsole {
			endpoint.Console = base + spec.consolePath
		}
		return endpoint
	}
	if spec.gatewayPath != "" {
		return Endpoint{Address: fmt.Sprintf("http://%s:%d%s", host, gatewayPort, spec.gatewayPath)}
	}
	if spec.port == 0 {
		return Endpoint{}
	}
	base := fmt.Sprintf("http://%s:%d", host, spec.port)
	endpoint := Endpoint{Address: base}
	if spec.hasConsole {
		endpoint.Console = base + spec.consolePath
	}
	return endpoint
}

// Known reports whether name is a recognized host service.
func Known(name string) bool {
	_, ok := registry[name]
	return ok
}

// EndpointForHost returns the full endpoint for a service rendered against the
// given display host (e.g. "localhost" or a machine hostname), and whether the
// service is known. Loopback-only services (dns) keep their loopback address.
func EndpointForHost(name, host string) (Endpoint, bool) {
	spec, ok := registry[name]
	if !ok {
		return Endpoint{}, false
	}
	return spec.endpointForHost(host), true
}

// EndpointFor returns the full endpoint for a service against DefaultHost
// ("localhost") and whether it is known.
func EndpointFor(name string) (Endpoint, bool) {
	return EndpointForHost(name, DefaultHost)
}

// Address returns the host-reachable address for a service (against DefaultHost)
// and whether it has one (a known service with a non-empty Address).
func Address(name string) (string, bool) {
	endpoint, ok := EndpointFor(name)
	return endpoint.Address, ok && endpoint.Address != ""
}

// URL returns the admin-console URL for a service (against DefaultHost) and
// whether it has one (a known service with a non-empty Console).
func URL(name string) (string, bool) {
	endpoint, ok := EndpointFor(name)
	return endpoint.Console, ok && endpoint.Console != ""
}

// WithConsoles returns the services that have an admin console (rendered against
// DefaultHost), sorted by name.
func WithConsoles() []NamedURL {
	var named []NamedURL
	for name, spec := range registry {
		endpoint := spec.endpointForHost(DefaultHost)
		if endpoint.Console != "" {
			named = append(named, NamedURL{Name: name, URL: endpoint.Console})
		}
	}
	sort.Slice(named, func(left, right int) bool { return named[left].Name < named[right].Name })
	return named
}

// NamedURL pairs a service with its console URL (for listings / JSON output).
type NamedURL struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Opener opens a URL in the user's browser. Injectable so the CLI is testable.
type Opener interface {
	Open(url string) error
}

// RealOpener returns an Opener that shells out to the platform's URL handler.
func RealOpener(goos string) Opener { return osOpener{goos: goos} }

type osOpener struct{ goos string }

func (opener osOpener) Open(url string) error {
	name, args := opener.command(url)
	return exec.Command(name, args...).Start()
}

func (opener osOpener) command(url string) (string, []string) {
	if opener.goos == "darwin" {
		return "open", []string{url}
	}
	return "xdg-open", []string{url} // Linux
}
