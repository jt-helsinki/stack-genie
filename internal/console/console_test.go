package console

import (
	"strings"
	"testing"
)

func TestURLAndKnown(test *testing.T) {
	// The LiteLLM admin UI is the nginx subdomain vhost on the gateway port, NOT
	// the old internal-only :14000.
	if url, ok := URL("litellm"); !ok || url != "http://litellm.localhost:18787/ui" {
		test.Errorf("litellm console = (%q,%v)", url, ok)
	}
	// Known service with no web console.
	if _, ok := URL("ollama"); ok {
		test.Error("ollama should report no console")
	}
	if !Known("ollama") {
		test.Error("ollama should be a known service")
	}
	if Known("nope") {
		test.Error("unknown service must not be Known")
	}
}

func TestWithConsolesSortedAndFiltered(test *testing.T) {
	named := WithConsoles()
	// litellm is now the ONLY host service exposing a console. Its console URL must
	// be an nginx subdomain vhost on the gateway port — never a direct (now
	// internal-only) per-service port.
	if len(named) != 1 || named[0].Name != "litellm" {
		test.Fatalf("WithConsoles = %+v, want [litellm]", named)
	}
	want := map[string]string{
		"litellm": "http://litellm.localhost:18787/ui",
	}
	for _, namedURL := range named {
		if namedURL.URL != want[namedURL.Name] {
			test.Errorf("%s console = %q, want %q", namedURL.Name, namedURL.URL, want[namedURL.Name])
		}
		for _, deadPort := range []string{":14000", ":11434", ":18090", ":7000"} {
			if strings.Contains(namedURL.URL, deadPort) {
				test.Errorf("%s console %q must not use internal-only port %s", namedURL.Name, namedURL.URL, deadPort)
			}
		}
	}
}

func TestEndpointAndAddress(test *testing.T) {
	// litellm: admin UI subdomain vhost on the gateway port; the address is the
	// same base, the console adds /ui.
	endpoint, ok := EndpointFor("litellm")
	if !ok || endpoint.Address != "http://litellm.localhost:18787" || endpoint.Console != "http://litellm.localhost:18787/ui" {
		test.Errorf("litellm endpoint = (%+v,%v)", endpoint, ok)
	}
	if address, ok := Address("litellm"); !ok || address != "http://litellm.localhost:18787" {
		test.Errorf("litellm address = (%q,%v)", address, ok)
	}

	// ollama: host-CLI gateway PATH (no console). The /api/* calls land on
	// /ollama/api/* through nginx.
	endpoint, ok = EndpointFor("ollama")
	if !ok || endpoint.Address != "http://localhost:18787/ollama" || endpoint.Console != "" {
		test.Errorf("ollama endpoint = (%+v,%v)", endpoint, ok)
	}
	if address, ok := Address("ollama"); !ok || address != "http://localhost:18787/ollama" {
		test.Errorf("ollama address = (%q,%v)", address, ok)
	}
	if _, ok := URL("ollama"); ok {
		test.Error("ollama should report no console")
	}

	// proxy is the nginx gateway entry on :18787 (no separate admin console).
	endpoint, ok = EndpointFor("proxy")
	if !ok || endpoint.Address != "http://localhost:18787" || endpoint.Console != "" {
		test.Errorf("proxy endpoint = (%+v,%v)", endpoint, ok)
	}

	// headroom (now behind nginx), presidio and microsandbox have neither an
	// address nor a console — all internal-only.
	for _, name := range []string{"headroom", "presidio", "microsandbox"} {
		endpoint, ok := EndpointFor(name)
		if !ok {
			test.Errorf("%s should be a known service", name)
		}
		if endpoint.Address != "" || endpoint.Console != "" {
			test.Errorf("%s endpoint = %+v, want empty address+console", name, endpoint)
		}
		if _, ok := Address(name); ok {
			test.Errorf("%s should report no address", name)
		}
		if _, ok := URL(name); ok {
			test.Errorf("%s should report no console", name)
		}
	}

	// Unknown service is not known and has no endpoint.
	if _, ok := EndpointFor("nope"); ok {
		test.Error("unknown service must not have an endpoint")
	}
}

func TestEndpointForHostRendersGivenDomain(test *testing.T) {
	// The host argument is the platform base DOMAIN: UI subdomains hang off it and
	// the gateway-path addresses resolve under it — always on the single gateway port.
	endpoint, ok := EndpointForHost("litellm", DefaultHost)
	if !ok || endpoint.Address != "http://litellm.localhost:18787" || endpoint.Console != "http://litellm.localhost:18787/ui" {
		test.Errorf("litellm@localhost endpoint = (%+v,%v)", endpoint, ok)
	}

	// A custom domain is woven into the subdomain URL.
	endpoint, ok = EndpointForHost("litellm", "build-host.lan")
	if !ok || endpoint.Address != "http://litellm.build-host.lan:18787" || endpoint.Console != "http://litellm.build-host.lan:18787/ui" {
		test.Errorf("litellm@build-host.lan endpoint = (%+v,%v)", endpoint, ok)
	}

	// ollama is a gateway-path service: <domain>:18787/ollama, no subdomain.
	endpoint, ok = EndpointForHost("ollama", "build-host.lan")
	if !ok || endpoint.Address != "http://build-host.lan:18787/ollama" || endpoint.Console != "" {
		test.Errorf("ollama@build-host.lan endpoint = (%+v,%v)", endpoint, ok)
	}

	// dns is loopback-only and must NOT be rewritten to the custom domain.
	endpoint, ok = EndpointForHost("dns", "build-host.lan")
	if !ok || endpoint.Address != "127.0.0.1:15353/udp" || endpoint.Console != "" {
		test.Errorf("dns@build-host.lan endpoint = (%+v,%v), want loopback unchanged", endpoint, ok)
	}

	// Internal-only services stay empty regardless of domain.
	for _, name := range []string{"headroom", "presidio", "microsandbox"} {
		endpoint, ok := EndpointForHost(name, "build-host.lan")
		if !ok {
			test.Errorf("%s should be a known service", name)
		}
		if endpoint.Address != "" || endpoint.Console != "" {
			test.Errorf("%s@build-host.lan endpoint = %+v, want empty", name, endpoint)
		}
	}

	// Unknown service is not known.
	if _, ok := EndpointForHost("nope", "build-host.lan"); ok {
		test.Error("unknown service must not have an endpoint")
	}
}

func TestOpenerCommandPerOS(test *testing.T) {
	cases := map[string]struct {
		name string
		args []string
	}{
		"darwin": {"open", []string{"https://x"}},
		"linux":  {"xdg-open", []string{"https://x"}},
	}
	for goos, want := range cases {
		name, args := osOpener{goos: goos}.command("https://x")
		if name != want.name || len(args) != len(want.args) {
			test.Errorf("%s command = (%q,%v), want (%q,%v)", goos, name, args, want.name, want.args)
			continue
		}
		for index := range args {
			if args[index] != want.args[index] {
				test.Errorf("%s args[%d] = %q, want %q", goos, index, args[index], want.args[index])
			}
		}
	}
}
