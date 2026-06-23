package console

import "testing"

func TestURLAndKnown(test *testing.T) {
	if url, ok := URL("litellm"); !ok || url != "http://localhost:14000/ui" {
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
	// litellm, odysseus, and open-webui all expose a console; sorted by name.
	if len(named) != 3 || named[0].Name != "litellm" || named[1].Name != "odysseus" || named[2].Name != "open-webui" {
		test.Fatalf("WithConsoles = %+v, want [litellm odysseus open-webui]", named)
	}
}

func TestOdysseusEndpointHasAddressAndConsole(test *testing.T) {
	endpoint, ok := EndpointFor("odysseus")
	if !ok || endpoint.Address != "http://localhost:7000" || endpoint.Console != "http://localhost:7000" {
		test.Errorf("odysseus endpoint = (%+v,%v)", endpoint, ok)
	}
}

func TestOpenWebUIEndpointHasAddressAndConsole(test *testing.T) {
	endpoint, ok := EndpointFor("open-webui")
	if !ok || endpoint.Address != "http://localhost:18090" || endpoint.Console != "http://localhost:18090" {
		test.Errorf("open-webui endpoint = (%+v,%v)", endpoint, ok)
	}
}

func TestEndpointAndAddress(test *testing.T) {
	// litellm has both an address and a console.
	endpoint, ok := EndpointFor("litellm")
	if !ok || endpoint.Address != "http://localhost:14000" || endpoint.Console != "http://localhost:14000/ui" {
		test.Errorf("litellm endpoint = (%+v,%v)", endpoint, ok)
	}
	if address, ok := Address("litellm"); !ok || address != "http://localhost:14000" {
		test.Errorf("litellm address = (%q,%v)", address, ok)
	}

	// ollama has an address only (HTTP API, no console).
	endpoint, ok = EndpointFor("ollama")
	if !ok || endpoint.Address != "http://localhost:11434" || endpoint.Console != "" {
		test.Errorf("ollama endpoint = (%+v,%v)", endpoint, ok)
	}
	if address, ok := Address("ollama"); !ok || address != "http://localhost:11434" {
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

func TestEndpointForHostRendersGivenHost(test *testing.T) {
	// Default host ("localhost") matches the legacy hardcoded URLs.
	endpoint, ok := EndpointForHost("litellm", DefaultHost)
	if !ok || endpoint.Address != "http://localhost:14000" || endpoint.Console != "http://localhost:14000/ui" {
		test.Errorf("litellm@localhost endpoint = (%+v,%v)", endpoint, ok)
	}

	// A custom display host (e.g. a server-role machine hostname) is woven into
	// both the address and the console URL.
	endpoint, ok = EndpointForHost("litellm", "build-host.lan")
	if !ok || endpoint.Address != "http://build-host.lan:14000" || endpoint.Console != "http://build-host.lan:14000/ui" {
		test.Errorf("litellm@build-host.lan endpoint = (%+v,%v)", endpoint, ok)
	}

	// A console whose root IS the UI gets the host in both fields, no path.
	endpoint, ok = EndpointForHost("open-webui", "build-host.lan")
	if !ok || endpoint.Address != "http://build-host.lan:18090" || endpoint.Console != "http://build-host.lan:18090" {
		test.Errorf("open-webui@build-host.lan endpoint = (%+v,%v)", endpoint, ok)
	}

	// dns is loopback-only and must NOT be rewritten to the custom host.
	endpoint, ok = EndpointForHost("dns", "build-host.lan")
	if !ok || endpoint.Address != "127.0.0.1:15353/udp" || endpoint.Console != "" {
		test.Errorf("dns@build-host.lan endpoint = (%+v,%v), want loopback unchanged", endpoint, ok)
	}

	// Internal-only services stay empty regardless of host.
	for _, name := range []string{"headroom", "presidio", "microsandbox", "chromadb"} {
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
