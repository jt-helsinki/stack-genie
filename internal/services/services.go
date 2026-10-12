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
// service's UI is served as a Host-based proxy vhost on the single gateway port
// (<UISubdomain>.<domain>:GatewayPort) — its direct Port is internal-only now, so
// its host-reachable address + console are the proxy subdomain forms, NOT
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

// GatewayPort is the single host port the proxy gateway (aip-proxy) publishes —
// the SOLE host entry to the service tier. Every UI subdomain and every host-CLI
// gateway path (/llm, /v1) is reached on this port. It mirrors the
// internal/setup proxyHostPort const (kept here so the leaf services package — and
// its projections — owns the value without importing setup).
const GatewayPort = 18787

// OmlxPort is the fixed host loopback port the omlx local-inference server binds
// (`omlx serve --model-dir <dir> --port <port>`) — one shared server for every
// locally-served model, so unlike the old per-model vLLM ports there is exactly
// ONE port to fix. It is reached directly (host-native, not through the proxy
// gateway) by both LiteLLM (via host.docker.internal) and this CLI's own
// `ai services console omlx` (the omlx admin panel, the ONLY place models are
// managed now — see ConsolePath below).
const OmlxPort = 8100

// ColibriPort is the fixed host loopback port the colibri local-inference server
// binds (`coli start`/`coli setup`, both configured with --port). Deliberately NOT
// colibri's own default (8000 — a very commonly used dev port, high collision
// risk) and distinct from OmlxPort, so both backends can run side by side.
// Host-native, reached directly by LiteLLM (via host.docker.internal) and this
// CLI's own `ai services console colibri` (colibri's own web dashboard).
const ColibriPort = 8200

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
	// Host-based proxy vhost on the single gateway port: <label>.<domain> (e.g.
	// "litellm" → litellm.<domain>). Empty means the
	// service has no UI vhost (it is reached on a path of the gateway, or is
	// internal-only). It is the SINGLE source of truth for the UI→subdomain
	// mapping consumed by internal/setup (proxy vhosts + /etc/hosts) and the
	// `ai doctor` / uninstall flows.
	UISubdomain string
	Components  []Component
}

// registry is the single declaration of the logical service topology, in the
// canonical order used to derive the log-scope list. Each service lists its
// components in the order they were captured from the production code.
var registry = []Service{
	{
		// omlx is HOST-NATIVE and the platform's REQUIRED local-inference backend (the
		// second, OPTIONAL one is colibri, just below): the platform runs NO aip-*
		// container and pulls no image for it — it is ONE shared `omlx serve` host
		// process (multi-model, managed by omlx itself: LRU eviction, TTL, model
		// downloads) reached directly on its fixed loopback port (OmlxPort), probed
		// over HTTP. Its served models are reached through LiteLLM's `/v1` model path
		// (there is no dedicated /omlx route), so Endpoint carries no gateway
		// path — but it DOES have a console: omlx's own admin panel at /admin is the
		// ONLY place models are managed (this platform has no `ai models pull/rm`
		// equivalent), so `ai services console omlx` opens it directly. It has NO
		// container Component.
		Name:     "omlx",
		Endpoint: Endpoint{Port: OmlxPort, HasConsole: true, ConsolePath: "/admin"},
		LogScope: "omlx",
	},
	{
		// colibri is the SECOND local-inference backend (internal/colibri,
		// https://github.com/JustVugg/colibri) — HOST-NATIVE like omlx (no aip-*
		// container, no image) but OPTIONAL end to end: a missing install or a failed
		// start are never fatal to `ai setup` (see internal/setup/colibri_host.go).
		// Keeps exactly ONE model loaded per process (unlike omlx's one-shared-
		// server-many-models design), on its own fixed loopback port (ColibriPort,
		// deliberately distinct from omlx's so both can run side by side). Its
		// `coli start --no-browser` serves the OpenAI/Anthropic-compatible API AND its
		// own web dashboard on that SAME port, so it gets a console entry too —
		// `ai services console colibri` opens the dashboard directly. Model
		// selection/download is the user's own one-time `coli setup`, never this
		// platform's — see the package doc comment for why.
		Name:     "colibri",
		Endpoint: Endpoint{Port: ColibriPort, HasConsole: true},
		LogScope: "colibri",
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
		// RedisInsight — Redis's official GUI for the cache, reached through the proxy
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
				Pin:       Pin{Mode: ModeContainer, Image: "redis/redisinsight", Tag: "3.8"},
			},
		},
	},
	{
		// Headroom precedes LiteLLM: it is a LiteLLM pre_call compression GUARDRAIL
		// (LiteLLM POSTs to aip-headroom:8787/v1/compress in-process), so it must be
		// up before LiteLLM. It is a standalone service, not a reverse-proxy hop in front
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
		// admin UI is reached through the proxy gateway at litellm.<domain>:GatewayPort/ui/login.
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
		// Open WebUI is a CORE service (not opt-in — there is no setup picker for
		// it): always installed, always reconciled, started/stopped with the rest
		// of the tier. INTERNAL-ONLY, fronted by the proxy at chat.<domain>:GatewayPort
		// (root IS the console) like every other UI vhost.
		Name:        "open-webui",
		Endpoint:    Endpoint{HasConsole: true, UISubdomain: "chat"},
		LogScope:    "open-webui",
		UISubdomain: "chat", // chat.<domain> → the Open WebUI chat UI
		Components: []Component{
			{
				ImageKey:  "open-webui",
				Container: "aip-open-webui",
				Pin:       Pin{Mode: ModeContainer, Image: "ghcr.io/open-webui/open-webui", Tag: "latest"},
			},
		},
	},
	{
		Name:     "proxy",
		Endpoint: Endpoint{Port: 18787}, // aip-proxy gateway entry, no UI
		LogScope: "proxy",
		Components: []Component{
			{
				ImageKey:  "proxy",
				Container: "aip-proxy",
				// Caddy, not nginx: a Caddyfile needs no manual Host/X-Forwarded-*/
				// Upgrade header wiring (reverse_proxy preserves the Host header
				// VERBATIM, including the port, and forwards WebSocket upgrades and
				// X-Forwarded-* automatically) and no buffering directive (it never
				// buffers responses) — see proxyCaddyfile's doc comment.
				Pin: Pin{Mode: ModeContainer, Image: "caddy", Tag: "2.11.7-alpine"},
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

// logScopeOrder is the canonical order log scopes are emitted in: walking the
// registry in declaration order, each logical service's own scope, immediately
// followed by its companion components' scopes. Presidio contributes one scope
// (its components carry none); litellm contributes "litellm" (litellm-db
// carries none) — reproducing logs.services exactly. There is no scope for the
// nono workspace runtime: unlike the old Microsandbox microVM (one long-running
// instance with its own `msb logs` stream), nono has no persistent process of
// its own to tail — every agent/shell launch is its own ephemeral session.
func logScopeOrder() []string {
	var scopes []string
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
	return endpoints
}

// UIVhost is one Host-based proxy vhost the platform serves a web UI on: the
// logical service Name, its Subdomain LABEL (Subdomain.<domain>), whether it is
// Optional (rendered/wired only when enabled), and the in-network Upstream the
// vhost proxies to (e.g. "http://aip-litellm:4000" — the host:port the proxy forwards
// to, BYPASSING Headroom: these are UI-serving routes, not the model path). It is
// the single projection consumers (internal/setup, doctor, uninstall) use so the
// UI→subdomain mapping is never duplicated.
type UIVhost struct {
	Name      string
	Subdomain string
	Optional  bool
	Upstream  string
	// ConsolePath is the service's UI path — the proxy redirects the vhost root there when
	// non-empty + not "/" (e.g. litellm serves its UI at /ui). Empty ("" or "/") means
	// the UI is at the root, so no redirect (e.g. redisinsight).
	ConsolePath string
}

// uiUpstreams maps each UI service to the in-network upstream the proxy forwards its
// vhost to. Declared alongside the registry so adding a UI service is a single
// edit. The port is the container's internal listen port (reached by name on the
// shared aip-net). These bypass Headroom — they serve the app UI, not the model
// path (the apps' MODEL calls ride the gateway's /v1 → Headroom route).
var uiUpstreams = map[string]string{
	"litellm":      "http://aip-litellm:4000",
	"redisinsight": "http://aip-redisinsight:5540",
	"open-webui":   "http://aip-open-webui:8080",
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
// The nono workspace runtime is NOT in this registry at all: it is a
// user-installed prerequisite that `ai setup`/`ai doctor` DETECT on PATH, not
// an image the platform pulls or pins, and unlike the old Microsandbox `msb`
// it has no persistent process of its own to give a log scope or console slot
// to (see logScopeOrder's doc comment) — so there is nothing here to exclude.
func VersionPins() map[string]Pin {
	pins := make(map[string]Pin)
	for _, service := range registry {
		for _, component := range service.Components {
			pins[component.ImageKey] = component.Pin
		}
	}
	return pins
}
