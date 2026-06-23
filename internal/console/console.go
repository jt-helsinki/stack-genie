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
// (EndpointFor/Address/URL/WithConsoles). The host-published services are bound
// to loopback in every role except server, so localhost is the right default;
// server-role hosts render their endpoints against the machine hostname via the
// *Host variants (see internal/setup statusDisplayHost) so LAN clients get a
// reachable address. This is DISPLAY ONLY — it never changes the container bind.
const DefaultHost = "localhost"

// endpointSpec is the host-agnostic data for a service's endpoint: the host port
// it publishes (rendered as http://<host>:<port>) and, where it has an admin UI,
// the path appended to that base. A spec with port 0 publishes nothing to the
// host (internal-only); loopbackAddress, when set, is used verbatim regardless of
// the display host (e.g. the DNS resolver, which always stays on loopback).
type endpointSpec struct {
	port            int
	consolePath     string // "" → no console; "/" or "" semantics: see endpointForHost
	hasConsole      bool   // distinguishes "console at the root URL" from "no console"
	loopbackAddress string // verbatim address, host-independent (loopback-only services)
}

// registry maps a host service to its endpoint spec. Ports are the verified host
// ports the service tier publishes (see internal/setup host-port consts):
//   - litellm  :14000  + admin UI at /ui
//   - ollama   :11434 (HTTP API, no UI)
//   - proxy    :18787 (aip-proxy nginx gateway entry → Headroom; no separate UI)
//   - open-webui :18090 (aip-open-webui chat UI → LiteLLM; the address IS its UI)
//   - dns      127.0.0.1:15353/udp (aip-dns CoreDNS egress-audit resolver, loopback)
//   - headroom is internal-only on :8787 behind nginx (no longer host-published)
//   - presidio analyzer/anonymizer are internal-only on :3000 (not host-published)
//   - microsandbox is the microVM runtime (no host address, no console)
var registry = map[string]endpointSpec{
	"litellm":      {port: 14000, consolePath: "/ui", hasConsole: true},
	"ollama":       {port: 11434},                            // HTTP API on :11434, no console UI
	"proxy":        {port: 18787},                            // aip-proxy nginx gateway entry, no UI
	"open-webui":   {port: 18090, hasConsole: true},          // chat UI; root IS the console
	"odysseus":     {port: 7000, hasConsole: true},           // optional AI workspace UI; root IS the console
	"chromadb":     {},                                       // internal-only on aip-net (no host publish)
	"searxng":      {},                                       // internal-only on aip-net (no host publish)
	"ntfy":         {},                                       // internal-only on aip-net (no host publish)
	"dns":          {loopbackAddress: "127.0.0.1:15353/udp"}, // aip-dns CoreDNS resolver, host loopback
	"headroom":     {},                                       // internal-only on :8787 behind nginx
	"presidio":     {},                                       // analyzer/anonymizer internal-only on :3000
	"microsandbox": {},                                       // microVM runtime, no address/console
}

// endpointForHost renders a spec into a concrete Endpoint for the given display
// host. Loopback-only specs (e.g. dns) ignore host and use their verbatim
// address; internal-only specs (port 0, no loopback address) render empty.
func (spec endpointSpec) endpointForHost(host string) Endpoint {
	if spec.loopbackAddress != "" {
		return Endpoint{Address: spec.loopbackAddress}
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
