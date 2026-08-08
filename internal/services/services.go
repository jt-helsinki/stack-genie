// Package services is the single source of truth for the AI Development
// Platform's host-service topology — the facts about each platform service that
// were previously hand-written (and silently drifting) across internal/logs,
// internal/console, internal/versions, and internal/setup.
//
// It is a LEAF package: it imports only the standard library (no setup /
// versions / console / logs), so those packages can derive their data from it
// without an import cycle. The model is rich enough that every consumer can be a
// thin projection over it:
//
//   - the ordered `ai logs --service` scope list (LogScopes)
//   - the per-service host endpoint specs (Endpoints / Endpoint)
//   - the versions.yaml image/native pins (VersionPins)
//   - (for the later internal/setup migration) the logical core/optional service
//     names, the service→image-keys mapping, the companion→owner mapping, and the
//     service→container-names mapping (CoreServiceNames, OptionalServiceNames,
//     ImageKeys, OwningService, ContainerNames).
//
// The data is declared ONCE in registry below; the accessors are pure
// projections, so adding a service is a single edit here.
package services

// Mode is a pinned component's runtime tier (the versions.yaml `mode`).
const (
	ModeContainer = "container"
	ModeNative    = "native"
)

// Endpoint is a service's host-reachable endpoint, the same shape internal/console
// renders. Port is the published host port (0 → nothing published / internal-only).
// ConsolePath is the path appended to the base URL for the admin UI; HasConsole
// distinguishes "console at the root URL" from "no console". LoopbackAddress, when
// set, is a verbatim host-independent address (loopback-only services, e.g. the DNS
// resolver) used regardless of the display host. UISubdomain, when set, means the
// service's UI is served as a Host-based nginx vhost on the single gateway port
// (<UISubdomain>.<domain>:GatewayPort) — its direct Port is internal-only now, so
// its host-reachable address + console are the nginx subdomain forms, NOT
// http://host:Port. GatewayPath, when set, is the prefix the host CLI reaches a
// non-UI service through the gateway at (e.g. litellm → "/llm").
type Endpoint struct {
	Port            int
	ConsolePath     string
	HasConsole      bool
	LoopbackAddress string
	UISubdomain     string
	GatewayPath     string
}

// GatewayPort is the single host port the nginx gateway (aip-proxy) publishes —
// the SOLE host entry to the service tier. Every UI subdomain and every host-CLI
// gateway path (/llm, /v1) is reached on this port. It mirrors the
// internal/setup proxyHostPort const (kept here so the leaf services package — and
// its projections — owns the value without importing setup).
const GatewayPort = 18787

// Pin is the version pin for one component: a container image+tag, or a native
// version+sha256. Exactly one shape is populated, per Mode.
type Pin struct {
	Mode    string
	Image   string
	Tag     string
	Version string
	SHA256  string
}

// Component is one image/container that a logical service is built from. Most
// services have a single component; a few have several (Presidio's analyzer +
// anonymizer; LiteLLM + its Postgres).
// ImageKey is the versions.yaml key (and the containerImage() lookup key);
// Container is the runtime container name (aip-*); LogScope is the
// `ai logs --service` scope this component contributes ("" → none, e.g.
// litellm-db, which has no log scope of its own); Endpoint is this component's own
// endpoint (usually the zero internal-only endpoint).
type Component struct {
	ImageKey  string
	Container string
	LogScope  string
	Endpoint  Endpoint
	Pin       Pin
}

// Service is one logical host service (the `ai services` vocabulary). Optional
// services are reconciled only when the user enables them at `ai setup`. Endpoint
// is the service's host endpoint (what internal/console keys by the service name);
// LogScope is the service's own log scope; Components are the image(s)/container(s)
// it owns.
type Service struct {
	Name     string
	Optional bool
	Endpoint Endpoint
	LogScope string
	// UISubdomain is the subdomain LABEL this service's web UI is served at, as a
	// Host-based nginx vhost on the single gateway port: <label>.<domain> (e.g.
	// "litellm" → litellm.<domain>). Empty means the
	// service has no UI vhost (it is reached on a path of the gateway, or is
	// internal-only). It is the SINGLE source of truth for the UI→subdomain
	// mapping consumed by internal/setup (nginx vhosts + /etc/hosts) and the
	// `ai doctor` / uninstall flows.
	UISubdomain string
	Components  []Component
}

// Native is the microVM runtime (microsandbox): a native version pin, a log scope,
// and a console entry — but NOT a reconciled service-tier container.
type Native struct {
	Name     string
	LogScope string
	Endpoint Endpoint
	Pin      Pin
}

// nativeRuntime is the microVM runtime. It is tailable and has an (empty) console
// entry, but is not reconciled as a container service.
//
// microsandbox is a user-installed PREREQUISITE that `ai setup`/`ai doctor` DETECT
// (msb on PATH), not an image the platform pulls or pins — so the native
// version/SHA below are informational placeholders, never an actionable pin. The
// entry exists only to give the runtime a log scope + registry slot; the platform
// detects msb rather than versioning it.
var nativeRuntime = Native{
	Name:     "microsandbox",
	LogScope: "microsandbox",
	Endpoint: Endpoint{},                                            // no host address, no console
	Pin:      Pin{Mode: ModeNative, Version: "v0.x", SHA256: "TBD"}, // placeholders — see comment above
}

// registry is the single declaration of the logical service topology, in the
// canonical order used to derive the log-scope list (LogScopes prepends the native
// runtime). Each service lists its components in the order they were captured from
// the production code.
var registry = []Service{
	{
		// vLLM is HOST-NATIVE and the SOLE local-inference backend (Ollama was removed):
		// the platform runs NO aip-* container and pulls no image for it — it is per-model
		// `vllm serve` host processes probed over HTTP. It stays a logical service so the
		// status line + `ai logs --service vllm` keep it visible; its served models are
		// reached through LiteLLM's `/v1` model path (there is no dedicated nginx /vllm
		// route), so Endpoint carries no gateway path. It has NO container Component.
		Name:     "vllm",
		Endpoint: Endpoint{},
		LogScope: "vllm",
	},
	{
		Name:     "presidio",
		Endpoint: Endpoint{}, // analyzer/anonymizer internal-only on :3000
		LogScope: "presidio", // ONE log scope for the analyzer+anonymizer pair
		Components: []Component{
			{
				ImageKey:  "presidio-analyzer",
				Container: "aip-presidio-analyzer",
				Pin:       Pin{Mode: ModeContainer, Image: "mcr.microsoft.com/presidio-analyzer", Tag: "latest"},
			},
			{
				ImageKey:  "presidio-anonymizer",
				Container: "aip-presidio-anonymizer",
				Pin:       Pin{Mode: ModeContainer, Image: "mcr.microsoft.com/presidio-anonymizer", Tag: "latest"},
			},
		},
	},
	{
		// Valkey (Redis-compatible) — LiteLLM's response cache. Single instance,
		// INTERNAL-ONLY on :6379 (reached by name aip-valkey by LiteLLM); no console.
		Name:     "valkey",
		Endpoint: Endpoint{},
		LogScope: "valkey",
		Components: []Component{
			{
				ImageKey:  "valkey",
				Container: "aip-valkey",
				Pin:       Pin{Mode: ModeContainer, Image: "valkey/valkey", Tag: "9.1.0-alpine"},
			},
		},
	},
	{
		// RedisInsight — Redis's official GUI for the cache, reached through the nginx
		// gateway at valkey.<domain>:GatewayPort (served at ROOT; its :5540 container
		// port is internal-only). Preconfigured to the standalone aip-valkey.
		Name:        "redisinsight",
		Endpoint:    Endpoint{HasConsole: true, UISubdomain: "valkey"},
		LogScope:    "redisinsight",
		UISubdomain: "valkey",
		Components: []Component{
			{
				ImageKey:  "redisinsight",
				Container: "aip-redisinsight",
				Pin:       Pin{Mode: ModeContainer, Image: "redis/redisinsight", Tag: "latest"},
			},
		},
	},
	{
		// Headroom precedes LiteLLM: it is a LiteLLM pre_call compression GUARDRAIL
		// (LiteLLM POSTs to aip-headroom:8787/v1/compress in-process), so it must be
		// up before LiteLLM. It is a standalone service, NOT an nginx proxy in front
		// of LiteLLM. Internal-only on :8787.
		Name:     "headroom",
		Endpoint: Endpoint{}, // internal-only on :8787, called by LiteLLM by name
		LogScope: "headroom",
		Components: []Component{
			{
				ImageKey:  "headroom",
				Container: "aip-headroom",
				Pin:       Pin{Mode: ModeContainer, Image: "ghcr.io/chopratejas/headroom", Tag: "latest"},
			},
		},
	},
	{
		Name: "litellm",
		// The :14000 host port is GONE — LiteLLM is internal-only on aip-net now. Its
		// admin UI is reached through the nginx gateway at litellm.<domain>:GatewayPort/ui/login.
		Endpoint:    Endpoint{ConsolePath: "/ui/login", HasConsole: true, UISubdomain: "litellm"},
		LogScope:    "litellm",
		UISubdomain: "litellm", // litellm.<domain> → the LiteLLM admin UI (/ui/login)
		Components: []Component{
			{
				ImageKey:  "litellm",
				Container: "aip-litellm",
				LogScope:  "litellm",
				// Tracks `latest`. LiteLLM v1.92.x+ carries the `headroom`
				// compression guardrail (see litellm.buildGuardrails), which
				// `latest` now satisfies.
				Pin: Pin{Mode: ModeContainer, Image: "ghcr.io/berriai/litellm", Tag: "latest"},
			},
			{
				// The Postgres backing LiteLLM's admin UI / virtual keys. A separate
				// versions key with NO logical service and NO log scope of its own.
				ImageKey:  "litellm-db",
				Container: "aip-litellm-db",
				Pin:       Pin{Mode: ModeContainer, Image: "postgres", Tag: "18.4-alpine3.23"},
			},
		},
	},
	{
		Name:     "proxy",
		Endpoint: Endpoint{Port: 18787}, // aip-proxy nginx gateway entry, no UI
		LogScope: "proxy",
		Components: []Component{
			{
				ImageKey:  "proxy",
				Container: "aip-proxy",
				Pin:       Pin{Mode: ModeContainer, Image: "nginx", Tag: "stable-alpine3.23-slim"},
			},
		},
	},
	{
		Name:     "dns",
		Endpoint: Endpoint{LoopbackAddress: "127.0.0.1:15353/udp"}, // aip-dns CoreDNS resolver, host loopback
		LogScope: "dns",
		Components: []Component{
			{
				ImageKey:  "dns",
				Container: "aip-dns",
				Pin:       Pin{Mode: ModeContainer, Image: "coredns/coredns", Tag: "latest"},
			},
		},
	},
}

// logScopeOrder is the canonical order log scopes are emitted in: the native
// runtime first, then — walking the registry in declaration order — each logical
// service's own scope, immediately followed by its companion components' scopes.
// Presidio contributes one scope (its components carry none); litellm contributes
// "litellm" (litellm-db carries none) — reproducing logs.services exactly.
func logScopeOrder() []string {
	scopes := []string{nativeRuntime.LogScope}
	for _, service := range registry {
		if service.LogScope != "" {
			scopes = append(scopes, service.LogScope)
		}
		for _, component := range service.Components {
			// A component scope equal to the service scope is the service's own
			// container (already emitted above) — skip it to avoid duplication.
			if component.LogScope == "" || component.LogScope == service.LogScope {
				continue
			}
			scopes = append(scopes, component.LogScope)
		}
	}
	return scopes
}

// LogScopes returns the ordered `ai logs --service` scope list — the native
// runtime followed by every service / companion log scope, in canonical order. A
// fresh slice each call (callers cannot mutate the backing data).
func LogScopes() []string {
	scopes := logScopeOrder()
	out := make([]string, len(scopes))
	copy(out, scopes)
	return out
}

// All returns the logical services in declaration order (fresh slice).
func All() []Service {
	out := make([]Service, len(registry))
	copy(out, registry)
	return out
}

// NativeRuntime returns the microVM runtime pin/topology.
func NativeRuntime() Native { return nativeRuntime }

// CoreServiceNames returns the names of the non-optional services, in order.
func CoreServiceNames() []string {
	var names []string
	for _, service := range registry {
		if !service.Optional {
			names = append(names, service.Name)
		}
	}
	return names
}

// OptionalServiceNames returns the names of the optional (opt-in) services, in order.
func OptionalServiceNames() []string {
	var names []string
	for _, service := range registry {
		if service.Optional {
			names = append(names, service.Name)
		}
	}
	return names
}

// ImageKeys returns the versions.yaml image keys a logical service is built from
// (in component order) — e.g. presidio → [presidio-analyzer, presidio-anonymizer].
// The "litellm-db" standalone key is attached to its owning litellm service, so
// litellm → [litellm, litellm-db].
// Returns nil for an unknown service.
func ImageKeys(service string) []string {
	for _, entry := range registry {
		if entry.Name != service {
			continue
		}
		keys := make([]string, 0, len(entry.Components))
		for _, component := range entry.Components {
			keys = append(keys, component.ImageKey)
		}
		return keys
	}
	return nil
}

// ContainerNames returns the runtime container names a logical service owns (in
// component order). Returns nil for an unknown service.
func ContainerNames(service string) []string {
	for _, entry := range registry {
		if entry.Name != service {
			continue
		}
		names := make([]string, 0, len(entry.Components))
		for _, component := range entry.Components {
			names = append(names, component.Container)
		}
		return names
	}
	return nil
}

// OwningService maps a companion image-key/component to the logical service that
// owns it, for components that are NOT themselves a logical service (e.g.
// litellm-db). Returns "" when the name IS a logical service or is unknown.
func OwningService(component string) string {
	for _, entry := range registry {
		for _, candidate := range entry.Components {
			if candidate.ImageKey != component {
				continue
			}
			if candidate.ImageKey == entry.Name {
				return "" // the component IS the logical service (e.g. litellm)
			}
			return entry.Name
		}
	}
	return ""
}

// Endpoints returns the per-name host endpoint specs, keyed by the name each
// consumer uses: every logical service by its own name, PLUS any companion
// components that need an addressable name, PLUS the native runtime. This is
// exactly the key set internal/console registers. A component shares its service's
// name only when it is the sole/primary one, so companions get their own
// (internal-only) entry.
func Endpoints() map[string]Endpoint {
	endpoints := make(map[string]Endpoint)
	for _, service := range registry {
		endpoints[service.Name] = service.Endpoint
		for _, component := range service.Components {
			if component.ImageKey == service.Name {
				continue // the service's own entry already covers it
			}
			// Companion components are keyed by their image-key when they carry a
			// distinct log scope (they are individually addressable);
			// litellm-db carries no scope and no endpoint, so it is not surfaced.
			if component.LogScope == "" {
				continue
			}
			endpoints[component.ImageKey] = component.Endpoint
		}
	}
	endpoints[nativeRuntime.Name] = nativeRuntime.Endpoint
	return endpoints
}

// UIVhost is one Host-based nginx vhost the platform serves a web UI on: the
// logical service Name, its Subdomain LABEL (Subdomain.<domain>), whether it is
// Optional (rendered/wired only when enabled), and the in-network Upstream the
// vhost proxies to (e.g. "http://aip-litellm:4000" — the host:port nginx forwards
// to, BYPASSING Headroom: these are UI-serving routes, not the model path). It is
// the single projection consumers (internal/setup, doctor, uninstall) use so the
// UI→subdomain mapping is never duplicated.
type UIVhost struct {
	Name      string
	Subdomain string
	Optional  bool
	Upstream  string
	// ConsolePath is the service's UI path — nginx redirects the vhost root there when
	// non-empty + not "/" (e.g. litellm serves its UI at /ui). Empty ("" or "/") means
	// the UI is at the root, so no redirect (e.g. redisinsight).
	ConsolePath string
}

// uiUpstreams maps each UI service to the in-network upstream nginx proxies its
// vhost to. Declared alongside the registry so adding a UI service is a single
// edit. The port is the container's internal listen port (reached by name on the
// shared aip-net). These bypass Headroom — they serve the app UI, not the model
// path (the apps' MODEL calls ride the gateway's /v1 → Headroom route).
var uiUpstreams = map[string]string{
	"litellm":      "http://aip-litellm:4000",
	"redisinsight": "http://aip-redisinsight:5540",
}

// UIVhosts returns every service that is served as a Host-based UI vhost (those
// with a non-empty UISubdomain), in registry order, with its subdomain label and
// upstream. Fresh slice each call.
func UIVhosts() []UIVhost {
	var vhosts []UIVhost
	for _, service := range registry {
		if service.UISubdomain == "" {
			continue
		}
		vhosts = append(vhosts, UIVhost{
			Name:        service.Name,
			Subdomain:   service.UISubdomain,
			Optional:    service.Optional,
			Upstream:    uiUpstreams[service.Name],
			ConsolePath: service.Endpoint.ConsolePath,
		})
	}
	return vhosts
}

// VersionPins returns the versions.yaml Services map content: every CONTAINER
// component's pin keyed by its image-key (including the split presidio keys and
// litellm-db). This is exactly the key set versions.Default() produces.
//
// The native microsandbox runtime is deliberately EXCLUDED: it is a user-installed
// prerequisite that `ai setup`/`ai doctor` DETECT on PATH, not an image the
// platform pulls or pins — so emitting its placeholder version/SHA into
// versions.yaml would be a fake, never-actionable "pin". It still lives in the
// registry (NativeRuntime) for its log scope + console slot; it just does not
// belong in the image pin set.
func VersionPins() map[string]Pin {
	pins := make(map[string]Pin)
	for _, service := range registry {
		for _, component := range service.Components {
			pins[component.ImageKey] = component.Pin
		}
	}
	return pins
}
