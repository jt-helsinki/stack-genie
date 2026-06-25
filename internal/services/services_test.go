package services

import (
	"reflect"
	"sort"
	"testing"
)

// TestLogScopesMatchesCurrent asserts the derived log-scope list is byte-for-byte
// the legacy internal/logs `services` slice (same order, same 12 entries).
func TestLogScopesMatchesCurrent(test *testing.T) {
	want := []string{
		"microsandbox",
		"ollama",
		"presidio",
		"litellm",
		"headroom",
		"proxy",
		"open-webui",
		"odysseus",
		"chromadb",
		"searxng",
		"ntfy",
		"dns",
	}
	got := LogScopes()
	if !reflect.DeepEqual(got, want) {
		test.Errorf("LogScopes() = %v, want %v", got, want)
	}
}

func TestLogScopesIsFreshCopy(test *testing.T) {
	first := LogScopes()
	first[0] = "mutated"
	second := LogScopes()
	if second[0] != "microsandbox" {
		test.Errorf("LogScopes() shares mutable state: second[0] = %q", second[0])
	}
}

// TestEndpointsMatchCurrentConsoleRegistry asserts the derived endpoint specs
// match the legacy internal/console registry (same keys, same fields).
func TestEndpointsMatchCurrentConsoleRegistry(test *testing.T) {
	want := map[string]Endpoint{
		"litellm":      {Port: 14000, ConsolePath: "/ui", HasConsole: true},
		"ollama":       {Port: 11434},
		"proxy":        {Port: 18787},
		"open-webui":   {Port: 18090, HasConsole: true},
		"odysseus":     {Port: 7000, HasConsole: true},
		"chromadb":     {},
		"searxng":      {},
		"ntfy":         {},
		"dns":          {LoopbackAddress: "127.0.0.1:15353/udp"},
		"headroom":     {},
		"presidio":     {},
		"microsandbox": {},
	}
	got := Endpoints()
	if !reflect.DeepEqual(got, want) {
		test.Errorf("Endpoints() = %#v\nwant %#v", got, want)
	}
}

// TestVersionPinsMatchCurrentDefault asserts the derived version pins match the
// legacy versions.Default() Services map (same keys, same fields).
func TestVersionPinsMatchCurrentDefault(test *testing.T) {
	want := map[string]Pin{
		"microsandbox":        {Mode: ModeNative, Version: "v0.x", SHA256: "TBD"},
		"litellm":             {Mode: ModeContainer, Image: "ghcr.io/berriai/litellm", Tag: "latest"},
		"litellm-db":          {Mode: ModeContainer, Image: "postgres", Tag: "18.4-alpine3.23"},
		"headroom":            {Mode: ModeContainer, Image: "ghcr.io/chopratejas/headroom", Tag: "latest"},
		"open-webui":          {Mode: ModeContainer, Image: "ghcr.io/open-webui/open-webui", Tag: "latest"},
		"odysseus":            {Mode: ModeContainer, Image: "ghcr.io/pewdiepie-archdaemon/odysseus", Tag: "latest"},
		"chromadb":            {Mode: ModeContainer, Image: "chromadb/chroma", Tag: "latest"},
		"searxng":             {Mode: ModeContainer, Image: "searxng/searxng", Tag: "latest"},
		"ntfy":                {Mode: ModeContainer, Image: "binwiederhier/ntfy", Tag: "latest"},
		"presidio-analyzer":   {Mode: ModeContainer, Image: "mcr.microsoft.com/presidio-analyzer", Tag: "latest"},
		"presidio-anonymizer": {Mode: ModeContainer, Image: "mcr.microsoft.com/presidio-anonymizer", Tag: "latest"},
		"proxy":               {Mode: ModeContainer, Image: "nginx", Tag: "stable-alpine3.23-slim"},
		"ollama":              {Mode: ModeContainer, Image: "ollama/ollama", Tag: "latest"},
		"dns":                 {Mode: ModeContainer, Image: "coredns/coredns", Tag: "latest"},
	}
	got := VersionPins()
	if !reflect.DeepEqual(got, want) {
		test.Errorf("VersionPins() = %#v\nwant %#v", got, want)
	}
}

// TestCoreAndOptionalServiceNames pins the logical core/optional partition (the
// frozen API the internal/setup migration consumes).
func TestCoreAndOptionalServiceNames(test *testing.T) {
	wantCore := []string{"ollama", "presidio", "litellm", "headroom", "proxy", "dns"}
	wantOptional := []string{"open-webui", "odysseus"}
	if got := CoreServiceNames(); !reflect.DeepEqual(got, wantCore) {
		test.Errorf("CoreServiceNames() = %v, want %v", got, wantCore)
	}
	if got := OptionalServiceNames(); !reflect.DeepEqual(got, wantOptional) {
		test.Errorf("OptionalServiceNames() = %v, want %v", got, wantOptional)
	}
}

// TestImageKeysMatchServiceImageKeys pins the logical-service → image-keys map the
// setup migration must reproduce (serviceImageKeys). litellm-db rides on litellm.
func TestImageKeysMatchServiceImageKeys(test *testing.T) {
	cases := map[string][]string{
		"ollama":     {"ollama"},
		"presidio":   {"presidio-analyzer", "presidio-anonymizer"},
		"litellm":    {"litellm", "litellm-db"},
		"headroom":   {"headroom"},
		"proxy":      {"proxy"},
		"dns":        {"dns"},
		"open-webui": {"open-webui"},
		"odysseus":   {"odysseus", "chromadb", "searxng", "ntfy"},
	}
	for service, want := range cases {
		if got := ImageKeys(service); !reflect.DeepEqual(got, want) {
			test.Errorf("ImageKeys(%q) = %v, want %v", service, got, want)
		}
	}
	if got := ImageKeys("nope"); got != nil {
		test.Errorf("ImageKeys(unknown) = %v, want nil", got)
	}
}

// TestContainerNames pins the service → container-names map (aip-*).
func TestContainerNames(test *testing.T) {
	cases := map[string][]string{
		"ollama":     {"aip-ollama"},
		"presidio":   {"aip-presidio-analyzer", "aip-presidio-anonymizer"},
		"litellm":    {"aip-litellm", "aip-litellm-db"},
		"headroom":   {"aip-headroom"},
		"proxy":      {"aip-proxy"},
		"dns":        {"aip-dns"},
		"open-webui": {"aip-open-webui"},
		"odysseus":   {"aip-odysseus", "aip-chromadb", "aip-searxng", "aip-ntfy"},
	}
	for service, want := range cases {
		if got := ContainerNames(service); !reflect.DeepEqual(got, want) {
			test.Errorf("ContainerNames(%q) = %v, want %v", service, got, want)
		}
	}
	if got := ContainerNames("nope"); got != nil {
		test.Errorf("ContainerNames(unknown) = %v, want nil", got)
	}
}

// TestOwningService pins the companion → owner map (matching setup.owningService,
// extended with litellm-db → litellm).
func TestOwningService(test *testing.T) {
	cases := map[string]string{
		"chromadb":   "odysseus",
		"searxng":    "odysseus",
		"ntfy":       "odysseus",
		"litellm-db": "litellm",
		// A logical service is not "owned".
		"odysseus": "",
		"litellm":  "",
		"ollama":   "",
		// Unknown.
		"nope": "",
	}
	for component, want := range cases {
		if got := OwningService(component); got != want {
			test.Errorf("OwningService(%q) = %q, want %q", component, got, want)
		}
	}
}

// TestVersionPinsModeShapes guards the per-mode field discipline (container pins
// image+tag and no native fields; native pins version+sha256 and no image fields).
func TestVersionPinsModeShapes(test *testing.T) {
	for key, pin := range VersionPins() {
		switch pin.Mode {
		case ModeContainer:
			if pin.Image == "" || pin.Tag == "" {
				test.Errorf("container pin %q missing image/tag: %+v", key, pin)
			}
			if pin.Version != "" || pin.SHA256 != "" {
				test.Errorf("container pin %q has native fields: %+v", key, pin)
			}
		case ModeNative:
			if pin.Version == "" || pin.SHA256 == "" {
				test.Errorf("native pin %q missing version/sha256: %+v", key, pin)
			}
			if pin.Image != "" || pin.Tag != "" {
				test.Errorf("native pin %q has container fields: %+v", key, pin)
			}
		default:
			test.Errorf("pin %q has unexpected mode %q", key, pin.Mode)
		}
	}
}

// TestLogScopesAreUniqueAndKnown is a sanity check: every scope is distinct and
// every endpoint key is a known scope/service (no orphans introduced here).
func TestLogScopesAreUniqueAndKnown(test *testing.T) {
	seen := map[string]bool{}
	for _, scope := range LogScopes() {
		if seen[scope] {
			test.Errorf("duplicate log scope %q", scope)
		}
		seen[scope] = true
	}
	// Every endpoint key (except the address-less standalone ones) should be a log
	// scope too — the consistency surfaces rely on this.
	scopes := append([]string(nil), LogScopes()...)
	sort.Strings(scopes)
	for name := range Endpoints() {
		if !seen[name] {
			test.Errorf("endpoint key %q is not a known log scope", name)
		}
	}
}

// TestUIVhostsMapping pins the UI→subdomain mapping the host-side nginx vhosts +
// /etc/hosts logic depends on: litellm (core) → litellm, open-webui → chat,
// odysseus → odysseus, each with a non-empty in-network upstream.
func TestUIVhostsMapping(test *testing.T) {
	want := map[string]struct {
		subdomain string
		optional  bool
	}{
		"litellm":    {"litellm", false},
		"open-webui": {"chat", true},
		"odysseus":   {"odysseus", true},
	}
	vhosts := UIVhosts()
	if len(vhosts) != len(want) {
		test.Fatalf("expected %d UI vhosts, got %d: %+v", len(want), len(vhosts), vhosts)
	}
	for _, vhost := range vhosts {
		expected, ok := want[vhost.Name]
		if !ok {
			test.Errorf("unexpected UI vhost %q", vhost.Name)
			continue
		}
		if vhost.Subdomain != expected.subdomain {
			test.Errorf("%s subdomain = %q, want %q", vhost.Name, vhost.Subdomain, expected.subdomain)
		}
		if vhost.Optional != expected.optional {
			test.Errorf("%s optional = %v, want %v", vhost.Name, vhost.Optional, expected.optional)
		}
		if vhost.Upstream == "" {
			test.Errorf("%s vhost has no upstream", vhost.Name)
		}
	}
}
