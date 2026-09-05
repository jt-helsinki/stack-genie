package console

import (
	"strings"
	"testing"
)

func TestURLAndKnown(test *testing.T) {
	// The LiteLLM admin UI is the nginx subdomain vhost on the gateway port, NOT
	// the old internal-only :14000.
	if url, ok := URL("litellm"); !ok || url != "http://litellm.localhost:18787/ui/login" {
		test.Errorf("litellm console = (%q,%v)", url, ok)
	}
	// omlx has a REAL console now: its own admin panel, the only place models are
	// managed (this platform has no `ai models pull/rm` equivalent).
	if url, ok := URL("omlx"); !ok || url != "http://localhost:8100/admin" {
		test.Errorf("omlx console = (%q,%v), want the admin panel", url, ok)
	}
	if !Known("omlx") {
		test.Error("omlx should be a known service")
	}
	if Known("nope") {
		test.Error("unknown service must not be Known")
	}
}

func TestWithConsolesSortedAndFiltered(test *testing.T) {
	named := WithConsoles()
	// litellm + omlx + redisinsight expose consoles — litellm/redisinsight as nginx
	// subdomain vhosts on the gateway port, omlx as its own direct loopback port
	// (its admin panel). Sorted by name.
	if len(named) != 3 || named[0].Name != "litellm" || named[1].Name != "omlx" || named[2].Name != "redisinsight" {
		test.Fatalf("WithConsoles = %+v, want [litellm omlx redisinsight]", named)
	}
	want := map[string]string{
		"litellm":      "http://litellm.localhost:18787/ui/login",
		"omlx":         "http://localhost:8100/admin",
		"redisinsight": "http://valkey.localhost:18787",
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
	if !ok || endpoint.Address != "http://litellm.localhost:18787" || endpoint.Console != "http://litellm.localhost:18787/ui/login" {
		test.Errorf("litellm endpoint = (%+v,%v)", endpoint, ok)
	}
	if address, ok := Address("litellm"); !ok || address != "http://litellm.localhost:18787" {
		test.Errorf("litellm address = (%q,%v)", address, ok)
	}

	// omlx: host-native, but DOES have its own address+console (the admin panel) —
	// unlike the old per-model vLLM servers, there's exactly one omlx endpoint.
	endpoint, ok = EndpointFor("omlx")
	if !ok || endpoint.Address != "http://localhost:8100" || endpoint.Console != "http://localhost:8100/admin" {
		test.Errorf("omlx endpoint = (%+v,%v), want the admin panel", endpoint, ok)
	}
	if url, ok := URL("omlx"); !ok || url != "http://localhost:8100/admin" {
		test.Errorf("omlx console = (%q,%v), want the admin panel", url, ok)
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
	if !ok || endpoint.Address != "http://litellm.localhost:18787" || endpoint.Console != "http://litellm.localhost:18787/ui/login" {
		test.Errorf("litellm@localhost endpoint = (%+v,%v)", endpoint, ok)
	}

	// A custom domain is woven into the subdomain URL.
	endpoint, ok = EndpointForHost("litellm", "build-host.lan")
	if !ok || endpoint.Address != "http://litellm.build-host.lan:18787" || endpoint.Console != "http://litellm.build-host.lan:18787/ui/login" {
		test.Errorf("litellm@build-host.lan endpoint = (%+v,%v)", endpoint, ok)
	}

	// omlx is host-native but has its own address+console (rendered against
	// whatever display host is given, like the direct-port "proxy" entry above).
	endpoint, ok = EndpointForHost("omlx", "build-host.lan")
	if !ok || endpoint.Address != "http://build-host.lan:8100" || endpoint.Console != "http://build-host.lan:8100/admin" {
		test.Errorf("omlx@build-host.lan endpoint = (%+v,%v)", endpoint, ok)
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
