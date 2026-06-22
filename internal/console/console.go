// Package console is the single source of truth for the host services'
// reachable endpoints: the address a user (or workspace) can hit, and — where
// the service has one — its admin-console URL. `ai setup`, `ai doctor`, and
// `ai services status` all read from this registry so the user can find each
// service's UI, and `ai services console` opens the admin URL in the browser.
// Services without a host address and/or a web console are listed explicitly so
// the CLI can tell "no console" apart from "unknown service".
package console

import (
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

// registry maps a host service to its endpoint. Addresses are the verified host
// ports the service tier publishes (see internal/setup host-port consts):
//   - litellm  :14000  + admin UI at /ui
//   - ollama   :11434 (HTTP API, no UI)
//   - headroom :18787  (aip-headroom compression proxy; has /stats, no UI)
//   - open-webui :18090 (aip-open-webui chat UI → LiteLLM; the address IS its UI)
//   - dns      127.0.0.1:15353/udp (aip-dns CoreDNS egress-audit resolver, loopback)
//   - presidio analyzer/anonymizer are internal-only on :3000 (not host-published)
//   - microsandbox is the microVM runtime (no host address, no console)
var registry = map[string]Endpoint{
	"litellm":      {Address: "http://localhost:14000", Console: "http://localhost:14000/ui"},
	"ollama":       {Address: "http://localhost:11434"},                                    // HTTP API on :11434, no console UI
	"headroom":     {Address: "http://localhost:18787"},                                    // aip-headroom compression proxy, no UI
	"open-webui":   {Address: "http://localhost:18090", Console: "http://localhost:18090"}, // chat UI; root IS the console
	"dns":          {Address: "127.0.0.1:15353/udp"},                                       // aip-dns CoreDNS resolver, host loopback
	"presidio":     {},                                                                     // analyzer/anonymizer internal-only on :3000
	"microsandbox": {},                                                                     // microVM runtime, no address/console
}

// Known reports whether name is a recognized host service.
func Known(name string) bool {
	_, ok := registry[name]
	return ok
}

// EndpointFor returns the full endpoint for a service and whether it is known.
func EndpointFor(name string) (Endpoint, bool) {
	endpoint, ok := registry[name]
	return endpoint, ok
}

// Address returns the host-reachable address for a service and whether it has
// one (a known service with a non-empty Address).
func Address(name string) (string, bool) {
	endpoint, ok := registry[name]
	return endpoint.Address, ok && endpoint.Address != ""
}

// URL returns the admin-console URL for a service and whether it has one (a
// known service with a non-empty Console).
func URL(name string) (string, bool) {
	endpoint, ok := registry[name]
	return endpoint.Console, ok && endpoint.Console != ""
}

// WithConsoles returns the services that have an admin console, sorted by name.
func WithConsoles() []NamedURL {
	var named []NamedURL
	for name, endpoint := range registry {
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
