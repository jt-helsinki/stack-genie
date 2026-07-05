package setup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/catalog"
	"github.com/jt-helsinki/ideal-robot/internal/console"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/services"
	"github.com/jt-helsinki/ideal-robot/internal/versions"
)

// serviceStartError builds the user-facing error for a failed service-tier
// container launch. It hides the container runtime's raw stderr (already noisy
// and tool-specific) and points the user at the one fix that resolves almost all
// launch failures: make sure Docker is up, then re-run setup.
func serviceStartError(service string) error {
	return output.Errorf(output.ExitRuntimeFailure,
		"could not start the %s service — make sure Docker is running, then re-run `ai setup`", service)
}

// containerImage returns the image reference (repo:tag) for a service, from
// config/versions.yaml when present and complete, else the built-in default
// pin (versions.Default). Pinned by tag — digests are platform-specific and
// intentionally not used.
func containerImage(service string) string {
	if loaded, err := versions.Load(); err == nil && loaded != nil {
		if ref := versionsImageRef(loaded, service); ref != "" {
			return ref
		}
	}
	return versionsImageRef(versions.Default(), service)
}

func versionsImageRef(file *versions.File, service string) string {
	svc, ok := file.Services[service]
	if !ok || svc.Image == "" || svc.Tag == "" {
		return ""
	}
	return svc.Image + ":" + svc.Tag
}

type serviceSpec struct{ Name, Mode string }

// coreServices is the always-on host-service set (arch §5), all containers:
// Ollama (required local model backend), Presidio (PII guardrail backend),
// LiteLLM (gateway/router), Headroom (input compression proxy in front of
// LiteLLM), the nginx gateway proxy, and the aip-dns egress-audit resolver.
// Ollama is required — LiteLLM routes local model traffic to it (arch §14, §16).
// Headroom runs as a shared host-side proxy (agents point at :18787, it forwards
// to LiteLLM); the per-project Caveman skill handles output compression inside
// the workspace (arch §8–10). These are reconciled on every `ai setup`. The set
// (and its order) is derived from the internal/services registry, the single
// source of truth for the service topology — these are all container-mode.
func coreServices() []serviceSpec {
	return serviceSpecsFor(services.CoreServiceNames())
}

// optionalServices is the opt-in host-service set: services that run on the
// host, OUTSIDE the workspace microVM sandbox, and so are reconciled only when
// the user has explicitly enabled them at `ai setup`. The set is derived from the
// internal/services registry (its optional services), so adding a new optional
// service is a single registry edit (plus its ensure*/stop wiring). The optional
// MECHANISM is retained for future host optional services, but there are currently
// NONE: Open WebUI moved to a per-workspace in-VM app and Odysseus was removed.
func optionalServices() []serviceSpec {
	return serviceSpecsFor(services.OptionalServiceNames())
}

// serviceSpecsFor turns a registry-derived list of logical service names into the
// container-mode serviceSpecs the reconcile path consumes, preserving order. All
// host services run as containers (the native microsandbox runtime is not part of
// this reconciled set).
func serviceSpecsFor(names []string) []serviceSpec {
	specs := make([]serviceSpec, 0, len(names))
	for _, name := range names {
		specs = append(specs, serviceSpec{name, "container"})
	}
	return specs
}

// optionalServiceNames returns the names of the optional services (the universe
// of opt-in tools), in declaration order.
func optionalServiceNames() []string {
	specs := optionalServices()
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

// isOptionalService reports whether name is an opt-in service (vs a core one).
func isOptionalService(name string) bool {
	return slices.Contains(optionalServiceNames(), name)
}

// desiredServices is the full known host-service set: core + ALL optional. It is
// the universe of addressable service names (for ServiceNames / Status /
// Control), regardless of which optional services are currently enabled —
// `ai services status` lists every one (a not-enabled optional shows "disabled")
// and `ai services start <name>` works ad hoc on any of them.
func desiredServices() []serviceSpec {
	return append(coreServices(), optionalServices()...)
}

// serviceImageKeys maps a service spec to the versions keys whose images it pulls.
// Most services map 1:1 to their name, but a few are one logical service backed by
// SEVERAL images: "presidio" → analyzer + anonymizer. Derived from the
// internal/services registry
// (the single source of truth for the service→image-keys mapping). Pure helper,
// shared by requiredImages.
func serviceImageKeys(service string) []string {
	return services.ImageKeys(service)
}

// requiredImages returns the image references (repo:tag) to pre-pull, gated by the
// enabled optional-service set: CORE images are always included, but an OPTIONAL
// service's images are included ONLY when that service is in enabled — so a
// disabled optional service never triggers a pull of its (large) images. References are
// resolved via containerImage; desiredServices lists the services but NOT
// litellm-db (no health line / Status entry), so it is added explicitly. A few
// services expand to several images (serviceImageKeys). Pure and testable;
// duplicate refs are de-duplicated so a shared image is only pulled once.
func requiredImages(enabled []string) []string {
	serviceKeys := []string{"litellm-db"}
	for _, service := range desiredServices() {
		if isOptionalService(service.Name) && !slices.Contains(enabled, service.Name) {
			continue // a not-enabled optional service: don't pull its images
		}
		serviceKeys = append(serviceKeys, serviceImageKeys(service.Name)...)
	}
	seen := make(map[string]bool, len(serviceKeys))
	images := make([]string, 0, len(serviceKeys))
	for _, key := range serviceKeys {
		ref := containerImage(key)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		images = append(images, ref)
	}
	return images
}

// litellm container naming (the image reference is resolved from versions.yaml
// by containerImage("litellm"), falling back to the built-in default pin).
const (
	litellmContainer = "aip-litellm"
	// litellmUIUsername is the (non-secret) admin-UI login name. The password and
	// master key are secrets, so they are never inlined — see litellmRunArgs.
	litellmUIUsername = "admin"

	// LiteLLM's admin UI / virtual keys / spend tracking are DB-backed and
	// PostgreSQL-only (docs.litellm.ai). We run a small Postgres beside LiteLLM on
	// a private docker network (not published to the host), with trust auth — so
	// DATABASE_URL carries no secret and there is no DB password to store. This is
	// the one stateful piece of the otherwise-stateless service tier.
	platformNetwork    = "aip-net"
	litellmDBContainer = "aip-litellm-db"
	litellmDBUser      = "litellm"
	litellmDBName      = "litellm"
	litellmDatabaseURL = "postgresql://" + litellmDBUser + "@" + litellmDBContainer + ":5432/" + litellmDBName
	// Published on the host at 5442 (not the default 5432) and bound to loopback,
	// to avoid clashing with any other Postgres on the machine. LiteLLM itself
	// reaches the DB over the private network (5432), not this host port.
	litellmDBHostPort = "5442"

	// Ollama runs as a container on aip-net (so LiteLLM reaches it by name at
	// aip-ollama:11434). It is INTERNAL-ONLY — no host publish; the host CLI reaches
	// the Ollama HTTP API through the nginx gateway's /ollama route. Models persist
	// on the host under ~/.ai-platform/volumes/models (the standardized system-volume
	// home; bind-mounted to ollamaModelsGuest, with OLLAMA_MODELS pointing there) so
	// they are visible on disk and removed with the rest of platform state on
	// `ai uninstall --purge` (which RemoveAll's ~/.ai-platform). Migration caveat:
	// models previously stored under ~/.ai-platform/models do NOT auto-migrate — the
	// next `ai setup` starts with a fresh dir and re-pulls; acceptable for this dev
	// platform.
	ollamaContainer    = "aip-ollama"
	ollamaModelsVolume = "models"  // subdir under VolumesDir: ~/.ai-platform/volumes/models
	ollamaModelsGuest  = "/models" // where the host models volume is mounted in the container

	// litellmDBVolume is the per-name subdir under VolumesDir for the LiteLLM
	// Postgres data dir: ~/.ai-platform/volumes/litellm-db, HOST-BIND-MOUNTED into
	// the Postgres container at /var/lib/postgresql (NOT a Docker named volume), so
	// all system data lives in one discoverable place under ~/.ai-platform.
	litellmDBVolume = "litellm-db"

	// Headroom is the input-compression proxy in front of LiteLLM. Official image
	// (no build). It is now INTERNAL-ONLY on aip-net at :8787 (no host publish) —
	// the nginx reverse proxy (aip-proxy) is the gateway entry on host :18787 and
	// forwards to Headroom by name; Headroom forwards to LiteLLM via
	// OPENAI_TARGET_API_URL.
	headroomContainer = "aip-headroom"
	headroomTargetURL = "http://" + litellmContainer + ":4000"

	// Valkey (Redis-compatible) is LiteLLM's response cache — single instance,
	// INTERNAL-ONLY on aip-net (aip-valkey:6379; LiteLLM's cache_params point here). No
	// persistence volume: it is a cache. aip-valkey-admin is its web UI (:8080 internal,
	// reached through nginx at valkey.<domain>), pointed at aip-valkey.
	valkeyContainer      = "aip-valkey"
	valkeyAdminContainer = "aip-valkey-admin"

	// aip-proxy is the nginx reverse proxy that is the SOLE host ENTRY to the service
	// tier: every other service container is internal-only on aip-net and only nginx
	// publishes to the host. It fronts the model path (host :18787 /v1 → Headroom →
	// LiteLLM, what resolveGateway returns — transparent to workspaces), the LiteLLM
	// admin (/llm) and Ollama (/ollama) surfaces on the same :18787, and the LiteLLM
	// admin UI as a Host-based vhost on that SAME :18787 (litellm.<domain>
	// — no separate host ports). nginx terminates TLS later (the future HTTPS endpoint,
	// per-vhost :443 + http→https redirect). Pinned minor tag.
	proxyContainer = "aip-proxy"
	proxyHostPort  = "18787"
	proxyTargetURL = "http://" + headroomContainer + ":8787"

	// Presidio backs LiteLLM's always-on PII guardrail (arch §17). The analyzer
	// detects PII and the anonymizer masks it; LiteLLM reaches both by name on the
	// shared network via PRESIDIO_*_API_BASE (port 3000, the image default). They
	// are internal only — not published to the host.
	presidioAnalyzerContainer   = "aip-presidio-analyzer"
	presidioAnonymizerContainer = "aip-presidio-anonymizer"
	presidioAnalyzerURL         = "http://" + presidioAnalyzerContainer + ":3000"
	presidioAnonymizerURL       = "http://" + presidioAnonymizerContainer + ":3000"

	// aip-dns is the egress-audit DNS resolver (arch §29). microVMs are booted
	// with --dns-nameserver pointing at it (DNSNameserver), so every name a
	// workspace tries to resolve is forwarded here and logged by CoreDNS's `log`
	// plugin — a host-wide attempted-egress-by-name audit, surfaced by
	// `ai network log`. It is a pure resolver: enforcement stays on the msb
	// net-rules (L3/L4); a resolver answer cannot bypass them. CoreDNS forwards
	// to the HOST's resolver (Docker's embedded DNS) and caches briefly. Published to the host LOOPBACK at
	// dnsHostPort so msb's netstack can forward guest DNS to it; not exposed off
	// the machine. The image reference is resolved from versions.yaml (CoreDNS).
	dnsContainer = "aip-dns"
	dnsHostPort  = "15353"
	// DNSNameserver is the guest-facing target passed to `msb create
	// --dns-nameserver` (a fixed platform setting). msb's netstack forwards guest
	// DNS to this host loopback address.
	DNSNameserver = "127.0.0.1:" + dnsHostPort
	// dnsUpstreams is what aip-dns (CoreDNS) forwards to. It is the CONTAINER'S OWN
	// /etc/resolv.conf — i.e. Docker's embedded DNS (127.0.0.11), which resolves via
	// the HOST's configured resolvers. Forwarding to the host's DNS (rather than
	// hardcoded public resolvers like 1.1.1.1/8.8.8.8) is essential on restricted or
	// corporate networks that block direct external DNS: there, forwarding to public
	// resolvers times out and workspaces "Could not resolve host", while the host's own
	// resolver works. This way the workspace resolves names exactly like the host does
	// (corporate DNS, VPN split-DNS, etc.). Audit-only; not security-sensitive.
	dnsUpstreams = "/etc/resolv.conf"
)

// proxyNginxConf renders the nginx reverse-proxy config written to
// ~/.ai-platform/config/proxy/nginx.conf and bind-mounted at /etc/nginx/nginx.conf.
// nginx (aip-proxy) is the SOLE host entry point to the service tier: every other
// service container is internal-only on aip-net, reached BY NAME, and only nginx
// publishes ports to the host. EVERYTHING listens on the SAME port :80 (host
// :18787); the web UIs are now Host-based VHOSTS (subdomains), matched by
// server_name, NOT separate host ports:
//
//   - the DEFAULT server (server_name <domain> localhost _; default_server) — the
//     model path + LiteLLM/Ollama management surfaces:
//   - location /     → Headroom (aip-headroom:8787) → LiteLLM: the DEFAULT route.
//     The host CLI (localhost:18787) and the microVM gateway
//     (host.microsandbox.internal:18787) hit this; agents + the UIs' MODEL calls
//     ride it. Goes through Headroom (compression + always-on guardrails).
//   - location /v1/  → Headroom too, with the SSE-friendly settings (the agent
//     CHAT path; what every workspace agent's base_url=…/v1 hits) — PRESERVED.
//   - location /llm/ → aip-litellm:4000 (prefix stripped): the LiteLLM ADMIN/
//     management surface. BYPASSES Headroom (direct to LiteLLM).
//   - location /ollama/ → aip-ollama:11434 (prefix stripped): the Ollama HTTP API.
//     BYPASSES Headroom.
//   - server_name litellm.<domain>; → aip-litellm:4000 at ROOT (the LiteLLM admin
//     UI is served at /ui; / redirects there). BYPASSES Headroom.
//
// litellm.<domain> is now the ONLY host UI vhost: Open WebUI moved to a per-workspace
// in-VM app and Odysseus was removed from the platform.
//
// All blocks are tuned for LLM/WebSocket traffic: HTTP/1.1, disabled response
// buffering so streamed (SSE) responses flush promptly, long read/send timeouts,
// and Upgrade/Connection headers so the LiteLLM admin UI's websockets work. The
// blocks are structured so a `listen 443 ssl;` + per-vhost ssl directives can be
// added later (TLS termination — out of scope now).
//
// domain is the resolved platform base domain (runtime Info.ResolveDomain(),
// default aip.local) the UI subdomains hang off.
func proxyNginxConf(domain string) string {
	var builder strings.Builder
	builder.WriteString("events {}\n")
	builder.WriteString("http {\n")
	// TLS (deferred — hardware bring-up): when HTTPS is enabled, each server block
	// below gains a `listen 443 ssl;` (+ ssl_certificate/ssl_certificate_key — a
	// wildcard for *.<domain> or per-vhost), TLS terminating per-vhost AT NGINX,
	// and THIS http listener becomes a redirect-only block:
	//     listen 80 default_server;
	//     return 301 https://$host$request_uri;
	// i.e. http MUST 301-redirect to https on the single :18787 entry. We do NOT
	// emit that redirect now — there is no https listener yet, so a 301 would break
	// every plain-http caller (the host CLI, the microVM gateway, the UIs).
	// The DEFAULT server: the model path (/ + /v1 → Headroom) plus the LiteLLM
	// admin (/llm) and Ollama (/ollama) management surfaces. default_server so it
	// answers localhost, host.microsandbox.internal, and any unmatched Host.
	builder.WriteString("  server {\n")
	builder.WriteString("    listen 80 default_server;\n")
	builder.WriteString("    server_name " + domain + " localhost _;\n")
	// LiteLLM admin/management surface (prefix stripped by the trailing slash).
	// BYPASSES Headroom (direct to LiteLLM).
	builder.WriteString(proxyLocation("/llm/", "http://"+litellmContainer+":4000/"))
	// Ollama HTTP API (prefix stripped). BYPASSES Headroom.
	builder.WriteString(proxyLocation("/ollama/", "http://"+ollamaContainer+":11434/"))
	// The agent CHAT path → Headroom → LiteLLM (PRESERVED). No trailing slash on
	// the target: /v1/... is forwarded to Headroom verbatim.
	builder.WriteString(proxyLocation("/v1/", proxyTargetURL))
	// Catch-all DEFAULT route: any other model-path call also goes to Headroom.
	builder.WriteString(proxyLocation("/", proxyTargetURL))
	builder.WriteString("  }\n")
	// One Host-based UI vhost per service with a UISubdomain (data-driven from the
	// registry: litellm.<domain> → the LiteLLM admin UI at /ui; valkey.<domain> → Valkey
	// Admin at root). BYPASS Headroom (they serve app UIs, not the model path). A
	// ConsolePath other than "/" is the root redirect (litellm → /ui); root-served UIs
	// (valkey-admin) get none.
	for _, vhost := range services.UIVhosts() {
		rootRedirect := vhost.ConsolePath
		if rootRedirect == "/" {
			rootRedirect = ""
		}
		builder.WriteString(proxyUIVhost(vhost.Subdomain+"."+domain, vhost.Upstream, rootRedirect))
	}
	builder.WriteString("}\n")
	return builder.String()
}

// proxyLocation renders one `location <prefix> { proxy_pass <target>; ... }` block
// tuned for LLM/SSE streaming (HTTP/1.1, buffering off, long timeouts).
func proxyLocation(prefix, target string) string {
	return "    location " + prefix + " {\n" +
		"      proxy_pass " + target + ";\n" +
		"      proxy_http_version 1.1;\n" +
		"      proxy_set_header Host $host;\n" +
		"      proxy_set_header Connection \"\";\n" +
		"      proxy_buffering off;\n" +
		"      proxy_read_timeout 3600s;\n" +
		"      proxy_send_timeout 3600s;\n" +
		"    }\n"
}

// proxyUIVhost renders a Host-based `server { server_name <name>; ... }` vhost (on
// the SHARED :80, host :18787) fronting a web UI at root, with WebSocket upgrade
// headers (the UIs use websockets). It BYPASSES Headroom — it serves the app UI,
// not the model path (the apps' model calls ride the default server's /v1 route).
// rootRedirect, when non-empty, makes `/` 302 to that path (e.g. LiteLLM's /ui).
// The block is structured so a `listen 443 ssl;` + ssl_* directives can be added
// per-vhost later (TLS termination).
func proxyUIVhost(serverName, target, rootRedirect string) string {
	var builder strings.Builder
	builder.WriteString("  server {\n")
	builder.WriteString("    listen 80;\n")
	builder.WriteString("    server_name " + serverName + ";\n")
	// Keep nginx's own redirects (e.g. trailing-slash) relative so the client's
	// host:port is preserved — an absolute redirect would drop the :18787.
	builder.WriteString("    absolute_redirect off;\n")
	if rootRedirect != "" {
		builder.WriteString("    location = / {\n")
		builder.WriteString("      return 302 " + rootRedirect + ";\n")
		builder.WriteString("    }\n")
	}
	builder.WriteString("    location / {\n")
	builder.WriteString("      proxy_pass " + target + ";\n")
	builder.WriteString("      proxy_http_version 1.1;\n")
	// Forward the ORIGINAL host INCLUDING the port ($http_host, not $host which
	// strips it) + the forwarded-* headers, so the app builds absolute redirects
	// (e.g. LiteLLM /ui) back to litellm.<domain>:18787 rather than dropping the
	// port and sending the browser to :80.
	builder.WriteString("      proxy_set_header Host $http_host;\n")
	builder.WriteString("      proxy_set_header X-Forwarded-Host $http_host;\n")
	builder.WriteString("      proxy_set_header X-Forwarded-Proto $scheme;\n")
	builder.WriteString("      proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
	builder.WriteString("      proxy_set_header Upgrade $http_upgrade;\n")
	builder.WriteString("      proxy_set_header Connection \"upgrade\";\n")
	builder.WriteString("      proxy_buffering off;\n")
	builder.WriteString("      proxy_read_timeout 3600s;\n")
	builder.WriteString("      proxy_send_timeout 3600s;\n")
	builder.WriteString("    }\n")
	builder.WriteString("  }\n")
	return builder.String()
}

// litellmRunArgs is the `<runtime> run` argv that launches LiteLLM with the
// rendered config mounted (docs.litellm.ai). Pure, so it is unit-testable.
//
// Admin-UI auth (docs.litellm.ai/docs/proxy/ui) is wired here: the username is
// inlined (it is not secret), while UI_PASSWORD, LITELLM_MASTER_KEY, and
// LITELLM_SALT_KEY are passed as **env passthrough** (`-e NAME`, no value) so
// Docker copies them from the launching process's environment — the secret values
// never appear in argv, the config, or on platform disk. The UI is secured
// whenever the password + master key are present in the environment at launch
// (exported by the user, or set for a relaunch by the setup prompt); otherwise
// LiteLLM falls back to its own default behavior. The salt key encrypts DB-backed
// model keys/credentials at rest and is generated once and kept stable.
//
// DATABASE_URL is inlined (it carries no secret — trust auth on a private
// network) so the DB-backed admin UI / virtual keys work. The container joins
// platformNetwork so it can reach aip-litellm-db by name.
//
// bindHost is now UNUSED for publishing — LiteLLM is internal-only (nginx is the
// sole host entry). The parameter is retained so the relaunch/reconcile callers
// keep a stable signature; the role-driven bindHost governs the nginx publish
// (ensureProxy), not LiteLLM.
func litellmRunArgs(configPath, bindHost, image string) []string {
	_ = bindHost // internal-only: LiteLLM no longer publishes to the host
	return []string{
		"run", "-d", "--name", litellmContainer,
		"--network", platformNetwork,
		// INTERNAL-ONLY: no host publish. LiteLLM is reached by name on aip-net
		// (aip-litellm:4000) — by Headroom (the model path) and by the nginx gateway's
		// /llm route (the admin surface). nginx (aip-proxy) is the only host entry.
		"-v", configPath + ":/app/config.yaml",
		"-e", "UI_USERNAME=" + litellmUIUsername,
		"-e", "UI_PASSWORD",
		"-e", "LITELLM_MASTER_KEY",
		// LITELLM_SALT_KEY encrypts the DB-backed model keys + provider credentials
		// at rest (store_model_in_db). Like the master key it is env passthrough
		// (name-only, value rides in the process env) so it never appears in argv or
		// on platform disk. It MUST stay stable across relaunches (a changed salt key
		// makes already-stored credentials undecryptable, docs.litellm.ai), which the
		// generate-once + preserve/persist wiring guarantees.
		"-e", "LITELLM_SALT_KEY",
		"-e", "DATABASE_URL=" + litellmDatabaseURL,
		// Reach the Presidio analyzer/anonymizer by name on the shared network so
		// the always-on PII guardrail has a backend (no secret in these values).
		"-e", "PRESIDIO_ANALYZER_API_BASE=" + presidioAnalyzerURL,
		"-e", "PRESIDIO_ANONYMIZER_API_BASE=" + presidioAnonymizerURL,
		image,
		"--config", "/app/config.yaml", "--port", "4000",
	}
}

// ensurePlatformNetwork creates the private docker network the service tier
// shares (so LiteLLM can resolve aip-litellm-db by name). Idempotent.
func ensurePlatformNetwork(prober runtime.Prober, containerRuntime string) {
	if _, err := prober.Run(containerRuntime, "network", "inspect", platformNetwork); err != nil {
		_, _ = prober.Run(containerRuntime, "network", "create", platformNetwork)
	}
}

// systemVolumeDir resolves a host SYSTEM-data volume directory under VolumesDir
// (~/.ai-platform/volumes/<name>) and creates it with the given perms. Every
// host-persisted system volume goes through here, so they all live in one
// discoverable place that `ai uninstall --purge` removes (RemoveAll
// ~/.ai-platform). Pure-ish (it touches the filesystem only to MkdirAll), and the
// path it returns is asserted in tests via VolumesDir.
func systemVolumeDir(name string, perm os.FileMode) (string, error) {
	volumesDir, err := paths.VolumesDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(volumesDir, name)
	if err := os.MkdirAll(dir, perm); err != nil {
		return "", output.Errorf(output.ExitRuntimeFailure, "create volume dir %s: %s", dir, err)
	}
	return dir, nil
}

// ensureLiteLLMDB starts the Postgres that backs LiteLLM's admin UI / virtual
// keys, unless it is already running. Trust auth on the private network (no
// password); the host port is loopback-bound at litellmDBHostPort. Idempotent.
//
// The data dir is a HOST BIND MOUNT at ~/.ai-platform/volumes/litellm-db (created
// 0700 before launch) → /var/lib/postgresql in the container — not a Docker named
// volume — so all system data lives under ~/.ai-platform and is removed by
// `ai uninstall --purge`.
//
// hardware bring-up: Postgres on a bind mount has data-dir OWNERSHIP quirks — the
// container's `postgres` UID must own (or be able to chown) the host dir, which
// macOS Docker Desktop's gRPC-FUSE mount and Linux rootless (userns-remapped UIDs)
// handle differently. Verify `initdb` succeeds on the bind mount on a provisioned
// host; some hosts may need a uid/`:Z` SELinux relabel tweak on the `-v`.
//
// Migration caveat: data in the OLD `aip-litellm-db-data` named volume does NOT
// auto-migrate — the next `ai setup` re-initdb's into the fresh bind dir;
// acceptable for this dev platform (`ai uninstall` best-effort-removes the legacy
// named volume).
func ensureLiteLLMDB(prober runtime.Prober, containerRuntime string) error {
	out, err := prober.Run(containerRuntime, "ps", "--filter", "name=^/"+litellmDBContainer+"$",
		"--filter", "status=running", "--format", "{{.Names}}")
	if err == nil && strings.TrimSpace(string(out)) == litellmDBContainer {
		return nil // already up
	}
	// Host bind-mount dir for the Postgres data (0700 — owner-only, the usual data-dir
	// perm). hardware bring-up: see the function doc on bind-mount ownership quirks.
	dbDir, err := systemVolumeDir(litellmDBVolume, 0o700)
	if err != nil {
		return err
	}
	_, _ = prober.Run(containerRuntime, "rm", "-f", litellmDBContainer) // clear any stopped one
	args := []string{
		"run", "-d", "--name", litellmDBContainer,
		"--network", platformNetwork,
		"-p", "127.0.0.1:" + litellmDBHostPort + ":5432",
		"-e", "POSTGRES_USER=" + litellmDBUser,
		"-e", "POSTGRES_DB=" + litellmDBName,
		"-e", "POSTGRES_HOST_AUTH_METHOD=trust",
		// HOST BIND MOUNT (not a named volume): the data dir lives under
		// ~/.ai-platform/volumes/litellm-db so all system data is in one discoverable
		// place removed by `ai uninstall --purge`. Postgres 18+ stores data in a
		// version-specific subdir, so the mount target is /var/lib/postgresql (NOT
		// .../data, the pre-18 convention) — otherwise the image refuses to start
		// (docker-library/postgres#1259).
		"-v", dbDir + ":/var/lib/postgresql",
		containerImage("litellm-db"),
	}
	if _, err := prober.Run(containerRuntime, args...); err != nil {
		return serviceStartError("LiteLLM database")
	}
	for attempt := 0; attempt < 20; attempt++ {
		if _, err := prober.Run(containerRuntime, "exec", litellmDBContainer, "pg_isready", "-U", litellmDBUser); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return nil // launched; LiteLLM will retry its connection as the DB finishes coming up
}

// containerRunning reports whether a container with the exact name is up.
func containerRunning(prober runtime.Prober, containerRuntime, name string) bool {
	out, err := prober.Run(containerRuntime, "ps", "--filter", "name=^/"+name+"$",
		"--filter", "status=running", "--format", "{{.Names}}")
	return err == nil && strings.TrimSpace(string(out)) == name
}

// containerPublishesHostPort reports whether a container has any host port
// binding. Used to detect a container left over from a previous topology — e.g. a
// Headroom that still host-publishes :18787 from before nginx (aip-proxy) took
// that port — so the reconcile recreates it internal-only instead of skipping it
// (idempotent "already running") and then failing when the proxy can't bind 18787.
func containerPublishesHostPort(prober runtime.Prober, containerRuntime, name string) bool {
	out, err := prober.Run(containerRuntime, "inspect", "--format",
		"{{json .HostConfig.PortBindings}}", name)
	if err != nil {
		return false
	}
	bindings := strings.TrimSpace(string(out))
	return bindings != "" && bindings != "{}" && bindings != "null"
}

// ensureOllama runs the Ollama container on the shared network, INTERNAL-ONLY (no
// host publish — reached by name on aip-net and, from the host, through the nginx
// /ollama route), persisting models under ~/.ai-platform/volumes/models on the host
// (bind-mounted). Idempotent. bindHost is unused now (no publish) but kept for a
// stable ensure* signature.
//
// Migration caveat: models previously under ~/.ai-platform/models do NOT
// auto-migrate to volumes/models — the next `ai setup` starts fresh and re-pulls;
// acceptable for this dev platform.
func ensureOllama(prober runtime.Prober, containerRuntime, bindHost string) error {
	_ = bindHost // internal-only: Ollama no longer publishes to the host
	if containerRunning(prober, containerRuntime, ollamaContainer) {
		return nil
	}
	modelsDir, err := systemVolumeDir(ollamaModelsVolume, 0o755)
	if err != nil {
		return err
	}
	_, _ = prober.Run(containerRuntime, "rm", "-f", ollamaContainer)
	args := []string{
		"run", "-d", "--name", ollamaContainer,
		"--network", platformNetwork,
		"-v", modelsDir + ":" + ollamaModelsGuest,
	}
	args = append(args, ollamaEnvArgs()...)
	args = append(args, containerImage("ollama"))
	if _, err := prober.Run(containerRuntime, args...); err != nil {
		return output.Errorf(output.ExitRuntimeFailure,
			"could not start Ollama — stop any local Ollama using port 11434, then re-run `ai setup`")
	}
	return nil
}

// ollamaEnvArgs builds the `-e OLLAMA_*` flags for the Ollama container. OLLAMA_MODELS is
// ALWAYS the platform's in-container store path (it backs the host bind mount) and is
// never taken from the environment. Every OTHER OLLAMA_-prefixed variable present in this
// process's environment — notably the ones loaded from ~/.ai-platform/.ai-platform.env at
// startup — is forwarded verbatim, so a user can tune the Ollama server
// (OLLAMA_KV_CACHE_TYPE, OLLAMA_FLASH_ATTENTION, OLLAMA_NUM_PARALLEL, …) without a code
// change; the tuned values apply on the next `ai services restart ollama` / `ai setup`
// (which recreates the container). Sorted for a deterministic, testable argv.
func ollamaEnvArgs() []string {
	args := make([]string, 0, len(ollamaEnvPairs())*2)
	for _, pair := range ollamaEnvPairs() {
		args = append(args, "-e", pair)
	}
	return args
}

// ollamaEnvPairs returns the Ollama container's env as KEY=VALUE pairs (the single
// source of truth for both ollamaEnvArgs' `-e` flags and the docker-compose renderer).
// OLLAMA_MODELS is platform-managed; every other OLLAMA_* comes from the process env.
func ollamaEnvPairs() []string {
	forwarded := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, found := strings.Cut(entry, "=")
		if !found || !strings.HasPrefix(key, "OLLAMA_") || key == "OLLAMA_MODELS" {
			continue
		}
		forwarded[key] = value
	}
	keys := make([]string, 0, len(forwarded))
	for key := range forwarded {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	pairs := []string{"OLLAMA_MODELS=" + ollamaModelsGuest}
	for _, key := range keys {
		pairs = append(pairs, key+"="+forwarded[key])
	}
	return pairs
}

// ensurePresidio runs the Presidio analyzer + anonymizer containers that back
// LiteLLM's always-on PII guardrail. Both join the shared network and are
// reachable by name on port 3000 (the image default); neither is published to
// the host. Idempotent.
func ensurePresidio(prober runtime.Prober, containerRuntime string) error {
	presidioServices := []struct{ name, image string }{
		{presidioAnalyzerContainer, containerImage("presidio-analyzer")},
		{presidioAnonymizerContainer, containerImage("presidio-anonymizer")},
	}
	for _, presidio := range presidioServices {
		if containerRunning(prober, containerRuntime, presidio.name) {
			continue
		}
		_, _ = prober.Run(containerRuntime, "rm", "-f", presidio.name)
		args := []string{
			"run", "-d", "--name", presidio.name,
			"--network", platformNetwork,
			presidio.image,
		}
		if _, err := prober.Run(containerRuntime, args...); err != nil {
			return serviceStartError("Presidio")
		}
	}
	return nil
}

// ensureValkey runs the Valkey (Redis-compatible) cache LiteLLM uses for response
// caching. Single instance, INTERNAL-ONLY (reached by name aip-valkey:6379 on the shared
// network); no persistence volume — it is a cache. Idempotent.
func ensureValkey(prober runtime.Prober, containerRuntime string) error {
	if containerRunning(prober, containerRuntime, valkeyContainer) {
		return nil
	}
	_, _ = prober.Run(containerRuntime, "rm", "-f", valkeyContainer)
	args := []string{
		"run", "-d", "--name", valkeyContainer,
		"--network", platformNetwork,
		containerImage("valkey"),
	}
	if _, err := prober.Run(containerRuntime, args...); err != nil {
		return serviceStartError("Valkey cache")
	}
	return nil
}

// ensureValkeyAdmin runs the Valkey Admin web UI, INTERNAL-ONLY on :8080 (reached through
// the nginx gateway at valkey.<domain>). It points at the aip-valkey cache with no
// auth/TLS — the cache is unauthenticated on the private network. Idempotent.
func ensureValkeyAdmin(prober runtime.Prober, containerRuntime string) error {
	if containerRunning(prober, containerRuntime, valkeyAdminContainer) {
		return nil
	}
	_, _ = prober.Run(containerRuntime, "rm", "-f", valkeyAdminContainer)
	args := []string{
		"run", "-d", "--name", valkeyAdminContainer,
		"--network", platformNetwork,
		"-e", "DEPLOYMENT_MODE=Web",
		"-e", "VALKEY_HOST=" + valkeyContainer,
		"-e", "VALKEY_PORT=6379",
		"-e", "VALKEY_TLS=false",
		containerImage("valkey-admin"),
	}
	if _, err := prober.Run(containerRuntime, args...); err != nil {
		return serviceStartError("Valkey Admin")
	}
	return nil
}

// ensureHeadroom runs the Headroom input-compression proxy in front of LiteLLM:
// the nginx gateway (aip-proxy) forwards to it by name on :8787, and it forwards
// to LiteLLM via OPENAI_TARGET_API_URL. Headroom is INTERNAL-ONLY (no host
// publish) — nginx is the host gateway entry on :18787. Pulled image (no build).
// Idempotent.
func ensureHeadroom(prober runtime.Prober, containerRuntime string) error {
	// Skip only if it is running AND already internal-only. A Headroom left over
	// from the pre-nginx topology still host-publishes :18787, which collides with
	// the aip-proxy gateway — recreate it internal-only in that case (self-heal, so
	// a plain `ai setup` migrates it instead of failing when the proxy can't bind).
	if containerRunning(prober, containerRuntime, headroomContainer) &&
		!containerPublishesHostPort(prober, containerRuntime, headroomContainer) {
		return nil
	}
	_, _ = prober.Run(containerRuntime, "rm", "-f", headroomContainer)
	args := []string{
		"run", "-d", "--name", headroomContainer,
		"--network", platformNetwork,
		"-e", "OPENAI_TARGET_API_URL=" + headroomTargetURL,
		// Headroom otherwise injects an empty `tools:[]` (its CCR retrieve-tool
		// path) into every request, which flips LiteLLM/Ollama into tool-calling
		// mode and corrupts answers regardless of prompt size. Disabling the tool
		// injection keeps compression enabled while forwarding requests faithfully.
		"-e", "HEADROOM_NO_CCR_INJECT_TOOL=1",
		containerImage("headroom"),
	}
	if _, err := prober.Run(containerRuntime, args...); err != nil {
		return serviceStartError("Headroom")
	}
	return nil
}

// ensureDNS runs the aip-dns CoreDNS resolver on the shared network, published to
// the host loopback at dnsHostPort on BOTH udp/53 and tcp/53 so microVMs (booted
// with --dns-nameserver DNSNameserver) forward their DNS there — including the
// TCP fallback path — for the attempted-egress-by-name audit
// (arch §29). It renders a Corefile to ~/.ai-platform/config/dns/Corefile with the
// `log` plugin (the audit source), a `forward` to the host's resolver, and a short
// cache. Idempotent: skips if already running, removes any stale container first.
func ensureDNS(prober runtime.Prober, containerRuntime string) error {
	if containerRunning(prober, containerRuntime, dnsContainer) {
		return nil
	}
	configDir, err := paths.ConfigDir()
	if err != nil {
		return err
	}
	dnsDir := filepath.Join(configDir, "dns")
	if err := os.MkdirAll(dnsDir, 0o755); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "create dns config dir %s: %s", dnsDir, err)
	}
	corefilePath := filepath.Join(dnsDir, "Corefile")
	corefile := ".:53 {\n    log\n    forward . " + dnsUpstreams + "\n    cache 30\n}\n"
	if err := os.WriteFile(corefilePath, []byte(corefile), 0o644); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "write Corefile %s: %s", corefilePath, err)
	}
	_, _ = prober.Run(containerRuntime, "rm", "-f", dnsContainer) // clear any stopped one
	args := []string{
		"run", "-d", "--name", dnsContainer,
		"--network", platformNetwork,
		// Publish on BOTH udp/53 and tcp/53. The Microsandbox DNS forwarder resolves
		// upstream over UDP normally but falls back to TCP (truncated/large answers,
		// EDNS); a UDP-only publish makes that TCP fallback hit a closed port, so a
		// workspace can fail to resolve public names ("Could not resolve host").
		"-p", "127.0.0.1:" + dnsHostPort + ":53/udp",
		"-p", "127.0.0.1:" + dnsHostPort + ":53/tcp",
		"-v", corefilePath + ":/Corefile",
		containerImage("dns"),
		"-conf", "/Corefile",
	}
	if _, err := prober.Run(containerRuntime, args...); err != nil {
		return serviceStartError("DNS audit resolver")
	}
	return nil
}

// ensureProxy runs the nginx reverse proxy that is the SOLE host entry to the
// service tier: microVM/host → nginx (bindHost:18787) → Headroom (:8787) → LiteLLM,
// plus the LiteLLM /llm + Ollama /ollama admin routes and the single LiteLLM admin
// UI Host-based vhost (litellm.<domain>) — ALL on the single :18787. It renders
// the nginx config (proxyNginxConf, threading the resolved domain) to
// ~/.ai-platform/config/proxy/nginx.conf, bind-mounts it at /etc/nginx/nginx.conf,
// and publishes ONLY :18787 (the LiteLLM UI is a subdomain on the same port, not a
// separate host port) on the role's bindHost (0.0.0.0 for a server, else
// loopback). Pulled image (no build).
//
// It always recreates the container (rather than skipping when running) so a change
// in the domain — which changes the rendered config — actually takes effect; nginx
// is cheap to recreate.
//
// hardware bring-up: the live end-to-end routing through these nginx routes
// (/llm, /ollama, and the litellm.<domain> UI vhost) is verified on a
// provisioned host.
func ensureProxy(prober runtime.Prober, containerRuntime, bindHost, domain string) error {
	configDir, err := paths.ConfigDir()
	if err != nil {
		return err
	}
	proxyDir := filepath.Join(configDir, "proxy")
	if err := os.MkdirAll(proxyDir, 0o755); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "create proxy config dir %s: %s", proxyDir, err)
	}
	confPath := filepath.Join(proxyDir, "nginx.conf")
	if err := os.WriteFile(confPath, []byte(proxyNginxConf(domain)), 0o644); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "write nginx.conf %s: %s", confPath, err)
	}
	_, _ = prober.Run(containerRuntime, "rm", "-f", proxyContainer) // recreate to apply config
	args := []string{
		"run", "-d", "--name", proxyContainer,
		"--network", platformNetwork,
		// nginx is the only publisher and publishes ONLY the gateway port: the LiteLLM
		// admin UI is a Host-based vhost (subdomain) on this SAME port, not a separate
		// host publish.
		"-p", bindHost + ":" + proxyHostPort + ":80",
		"-v", confPath + ":/etc/nginx/nginx.conf:ro",
		containerImage("proxy"),
	}
	if _, err := prober.Run(containerRuntime, args...); err != nil {
		return serviceStartError("gateway proxy")
	}
	return nil
}

// proxyReadinessURL is the host entry the proxy readiness probe GETs (arch §2.3).
// It targets LiteLLM's unauthenticated liveness path THROUGH the nginx gateway
// (:18787 → aip-headroom:8787 → aip-litellm), so a 200 means the whole forward
// chain is live; but proxyReachable treats ANY HTTP response (even 404/502) as
// "the proxy is up and forwarding", and only a transport error as down — so the
// probe stays meaningful even before Headroom/LiteLLM are fully healthy.
const proxyReadinessURL = "http://127.0.0.1:" + proxyHostPort + "/health/liveliness"

// proxyHTTPGet is the indirection the proxy readiness probe uses to make its HTTP
// request, so unit tests can substitute a fake without a live server. It defaults
// to a short-timeout client GET (mirroring litellm/ollama's probe style).
var proxyHTTPGet = func(url string) (*http.Response, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	return client.Get(url)
}

// proxyReachable reports whether the nginx gateway is up and forwarding: it GETs
// url and treats ANY HTTP response (any status code, even 404/502 from a not-yet-
// ready upstream) as "up and forwarding", and a transport error (e.g. connection
// refused) as down. This is the live nginx-proxy readiness check (arch §2.3),
// kept behind proxyHTTPGet so it is unit-testable.
func proxyReachable(url string) bool {
	response, err := proxyHTTPGet(url)
	if err != nil {
		return false
	}
	_ = response.Body.Close()
	return true
}

// logCaptureTailLines is how many trailing lines per container the log snapshot
// keeps (matches logs.TailLines, the reader's default tail).
const logCaptureTailLines = 200

// serviceContainers maps a logical service name (the `ai logs --service` /
// `ai services` vocabulary) to the container name(s) whose output is snapshotted
// for it. Most services own one container; a few own several — Presidio is the
// analyzer + anonymizer pair. Pure, so the mapping is unit-testable.
func serviceContainers(service string) []string {
	switch service {
	case "ollama":
		return []string{ollamaContainer}
	case "presidio":
		return []string{presidioAnalyzerContainer, presidioAnonymizerContainer}
	case "litellm":
		return []string{litellmContainer, litellmDBContainer}
	case "valkey":
		return []string{valkeyContainer}
	case "valkey-admin":
		return []string{valkeyAdminContainer}
	case "headroom":
		return []string{headroomContainer}
	case "proxy":
		return []string{proxyContainer}
	case "dns":
		return []string{dnsContainer}
	default:
		return nil
	}
}

// logFileNameFor maps a container name to its on-disk log file base name under
// ~/.ai-platform/logs. We strip the shared "aip-" prefix so the file name matches
// the `ai logs --service` vocabulary (logs.Services) for the substring filter —
// e.g. aip-litellm → litellm.log, aip-presidio-analyzer → presidio-analyzer.log
// (still matched by the "presidio" filter).
func logFileNameFor(container string) string {
	return strings.TrimPrefix(container, "aip-") + ".log"
}

// CaptureServiceLogs snapshots each RUNNING service container's recent output into
// ~/.ai-platform/logs/<container>.log, so `ai logs` and the TUI Logs view show
// real container output (arch §2.2). It walks every known logical service
// (desiredServices) → its container(s) (serviceContainers), and for each running
// container runs `docker logs --tail N --timestamps <container>` via the prober
// and writes the captured bytes to the log file. This is a point-in-time SNAPSHOT;
// continuous follow (`logs -f`) is a later enhancement. Best-effort: a probe or
// write error on one container is skipped (the rest still update), and a missing
// FollowServiceLogs streams a service's logs live (`<runtime> logs -f`) to out
// until the process is interrupted (Ctrl-C terminates the shared process group).
// It is a DIRECT streaming exec — the buffered Prober.Run cannot stream — which
// backs `ai logs --follow`. It follows the service's primary container (the
// analyzer for presidio); a companion name maps to aip-<name>.
func FollowServiceLogs(deps Deps, service string, out io.Writer) error {
	containerRuntime, err := runtime.ContainerRuntimeName(deps.Prober)
	if err != nil {
		return output.Errorf(output.ExitMissingDep, "no container runtime for --follow: %s", err)
	}
	container := "aip-" + service
	if mapped := serviceContainers(service); len(mapped) > 0 {
		container = mapped[0]
	}
	command := exec.Command(containerRuntime.Name, "logs", "-f",
		"--tail", strconv.Itoa(logCaptureTailLines), "--timestamps", container)
	command.Stdout = out
	command.Stderr = out
	if err := command.Run(); err != nil {
		// Ctrl-C (SIGINT) and the container stopping are clean ends, not failures.
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return nil
		}
		return output.Errorf(output.ExitRuntimeFailure, "follow %s logs: %s", service, err)
	}
	return nil
}

// container runtime is a no-op (nothing to capture). The prober is the SOLE docker
// touch-point — internal/logs stays a pure file reader.
func (services realServices) CaptureServiceLogs() error {
	containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
	if err != nil {
		return nil // no runtime → nothing to capture (not an error for callers)
	}
	logsDir, err := paths.LogsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "create logs dir %s: %s", logsDir, err)
	}
	tail := strconv.Itoa(logCaptureTailLines)
	for _, service := range desiredServices() {
		for _, container := range serviceContainers(service.Name) {
			if !containerRunning(services.prober, containerRuntime.Name, container) {
				continue // only snapshot running containers
			}
			out, err := services.prober.Run(containerRuntime.Name,
				"logs", "--tail", tail, "--timestamps", container)
			if err != nil {
				continue // best-effort: skip this container, keep going
			}
			logPath := filepath.Join(logsDir, logFileNameFor(container))
			_ = os.WriteFile(logPath, out, 0o644) // raw log text; best-effort
		}
	}
	return nil
}

// litellmHasDatabaseURL reports whether the running LiteLLM container already has
// DATABASE_URL wired (so a healthy-but-DB-less container is relaunched once).
func litellmHasDatabaseURL(prober runtime.Prober, containerRuntime string) bool {
	return litellmEnvSet(prober, containerRuntime, "DATABASE_URL")
}

// CurrentLiteLLMMasterKey returns the LITELLM_MASTER_KEY of the running LiteLLM
// container, or "" if unset / no container / no runtime. It is the SAME source
// RelaunchLiteLLMWithAuth and LiteLLMUISecured read, so `ai litellm password` can
// reuse an existing master key (and only mint a new one when there is none)
// rather than rotating it on every password change.
func CurrentLiteLLMMasterKey() string {
	containerRuntime, err := runtime.ContainerRuntimeName(runtime.RealProber())
	if err != nil {
		return ""
	}
	return litellmEnvValue(runtime.RealProber(), containerRuntime.Name, "LITELLM_MASTER_KEY")
}

// CurrentLiteLLMSaltKey returns the LITELLM_SALT_KEY of the running LiteLLM
// container, or "" if unset / no container / no runtime. The salt key encrypts the
// DB-backed model keys + provider credentials at rest and MUST stay stable across
// relaunches, so RelaunchLiteLLMWithAuth reuses this (only minting one when there
// is none) rather than rotating it — a rotated salt key would orphan every stored
// credential.
func CurrentLiteLLMSaltKey() string {
	containerRuntime, err := runtime.ContainerRuntimeName(runtime.RealProber())
	if err != nil {
		return ""
	}
	return litellmEnvValue(runtime.RealProber(), containerRuntime.Name, "LITELLM_SALT_KEY")
}

// resolveLiteLLMSaltKey returns the salt key to launch LiteLLM with: an explicit
// process-env value (LITELLM_SALT_KEY, e.g. from ~/.ai-platform/.ai-platform.env)
// wins; else the running container's existing salt key (kept stable); else a newly
// generated one. The value transits process memory only.
func resolveLiteLLMSaltKey() string {
	if value := os.Getenv("LITELLM_SALT_KEY"); value != "" {
		return value
	}
	if value := CurrentLiteLLMSaltKey(); value != "" {
		return value
	}
	return generateSaltKey()
}

// generateSaltKey returns a random LiteLLM salt key (`sk-` + 32 hex chars). It is
// distinct from the master key but shares the `sk-` shape LiteLLM expects.
func generateSaltKey() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "sk-aip-salt-fallback"
	}
	return "sk-" + hex.EncodeToString(buffer)
}

// LiteLLMRunning reports whether the LiteLLM gateway container is up — used by
// `ai litellm password` to fail fast (exit 3) when the platform isn't set up.
func LiteLLMRunning() bool {
	containerRuntime, err := runtime.ContainerRuntimeName(runtime.RealProber())
	if err != nil {
		return false
	}
	return containerRunning(runtime.RealProber(), containerRuntime.Name, litellmContainer)
}

// LiteLLMUISecured reports whether the LiteLLM container already has a non-empty
// UI password set, so `ai setup` does not re-prompt for it on every run.
func LiteLLMUISecured() bool {
	containerRuntime, err := runtime.ContainerRuntimeName(runtime.RealProber())
	if err != nil {
		return false
	}
	return litellmEnvSet(runtime.RealProber(), containerRuntime.Name, "UI_PASSWORD")
}

// litellmEnvSet reports whether the LiteLLM container's env has key set to a
// non-empty value (via the runtime's inspect).
func litellmEnvSet(prober runtime.Prober, containerRuntime, key string) bool {
	return litellmEnvValue(prober, containerRuntime, key) != ""
}

// litellmEnvValue returns the value of an env var on the running LiteLLM
// container, or "" if unset/absent.
func litellmEnvValue(prober runtime.Prober, containerRuntime, key string) string {
	out, err := prober.Run(containerRuntime, "inspect", "--format",
		"{{range .Config.Env}}{{println .}}{{end}}", litellmContainer)
	if err != nil {
		return ""
	}
	prefix := key + "="
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}

// preserveLiteLLMSecretsInEnv copies the running container's UI_PASSWORD,
// LITELLM_MASTER_KEY, and LITELLM_SALT_KEY into this process's environment when
// they are not already present, so an env-passthrough relaunch (litellmRunArgs)
// keeps the admin UI secured, the master key stable, AND the salt key stable
// (a changed salt key makes already-stored credentials undecryptable) rather than
// silently dropping them. Values transit process memory only — never argv or
// platform disk. MUST be called before the container is removed (a stopped
// container can still be inspected).
func preserveLiteLLMSecretsInEnv(prober runtime.Prober, containerRuntime string) {
	for _, key := range []string{"UI_PASSWORD", "LITELLM_MASTER_KEY", "LITELLM_SALT_KEY"} {
		if os.Getenv(key) != "" {
			continue // a caller-provided value (e.g. the setup prompt) wins
		}
		if value := litellmEnvValue(prober, containerRuntime, key); value != "" {
			_ = os.Setenv(key, value)
		}
	}
}

// RelaunchLiteLLMWithAuth recreates the LiteLLM container with the admin-UI
// credentials set, securing the UI immediately. The password and master key are
// passed via the process environment (not argv), so they are never written to
// disk or visible in the command line. Username is litellmUIUsername ("admin").
// Returns ErrNoContainerRuntime-wrapped errors if no runtime is present.
// currentBindHost returns the host interface the shared services should publish
// on for this host's persisted deployment role: "0.0.0.0" when the role is
// server, else "127.0.0.1" (standalone, or any absent/unreadable runtime.yaml).
// It is the bindHost source for the standalone relaunch path (RelaunchLiteLLMWithAuth),
// which does not receive a bindHost from Run; Reconcile/ensure* take the passed one.
func currentBindHost() string {
	info, err := runtime.Load()
	if err == nil && info != nil && info.Role == runtime.RoleServer {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// reconcileDomain resolves the platform base domain the nginx UI vhosts hang off
// from the persisted runtime.yaml (Info.ResolveDomain), falling back to the
// default (aip.local) when runtime.yaml is absent/unreadable. It is the domain
// source for ensureProxy in the Reconcile/Control paths, which do not receive a
// domain from their callers.
func reconcileDomain() string {
	info, err := runtime.Load()
	if err != nil || info == nil {
		return runtime.DefaultDomain
	}
	return info.ResolveDomain()
}

func RelaunchLiteLLMWithAuth(password, masterKey string) error {
	containerRuntime, err := runtime.ContainerRuntimeName(runtime.RealProber())
	if err != nil {
		return err
	}
	configPath, err := litellm.ConfigPath()
	if err != nil {
		return err
	}
	// The UI is DB-backed, so the network + Postgres must exist before relaunch.
	ensurePlatformNetwork(runtime.RealProber(), containerRuntime.Name)
	if err := ensureLiteLLMDB(runtime.RealProber(), containerRuntime.Name); err != nil {
		return err
	}
	// Ensure the mounted config exists (and is a file, not a Docker-created dir).
	if err := litellm.Render(litellm.DefaultRouting(), ""); err != nil {
		return err
	}
	// Best-effort removal of the running (likely unsecured) container.
	_ = exec.Command(containerRuntime.Name, "rm", "-f", litellmContainer).Run() // #nosec G204 — fixed args

	// #nosec G204 — fixed argv; secrets ride in the environment, not the command line.
	command := exec.Command(containerRuntime.Name, litellmRunArgs(configPath, currentBindHost(), containerImage("litellm"))...)
	command.Env = append(os.Environ(),
		"UI_PASSWORD="+password,
		"LITELLM_MASTER_KEY="+masterKey,
		// Keep the salt key stable across relaunches (reuse the running container's,
		// else mint one) so already-stored DB credentials remain decryptable.
		"LITELLM_SALT_KEY="+resolveLiteLLMSaltKey(),
	)
	if _, err := command.CombinedOutput(); err != nil {
		return serviceStartError("LiteLLM gateway")
	}
	return nil
}

// realServices reconciles, reports, and controls the host services via the
// detected container runtime and live health probes.
type realServices struct {
	prober runtime.Prober
}

// PullImages pre-pulls every service-tier image that is not already present
// locally, streaming the runtime's native pull progress to out. This is done
// BEFORE the reconcile so the subsequent `docker run -d` (whose implicit pull
// output the prober captures, invisibly) finds the image present and returns
// instantly — a multi-GB first-run pull (e.g. ollama) no longer looks hung.
// enabled is the set of opt-in optional services to include: core images are
// always pulled, but a disabled optional service's (potentially large) images are
// skipped. Already-present images are skipped (fast re-runs). Best-effort: it
// returns the first pull error but the caller treats it as non-fatal — the
// reconcile's per-service `docker run` re-pulls anything still missing.
func (services realServices) PullImages(enabled []string, out io.Writer, progress func(string)) error {
	return services.pullImages(enabled, out, progress, false)
}

// UpdateImages force-pulls every required image — UNLIKE PullImages it does NOT
// skip already-present ones, so a moved tag like `latest` is refreshed. It backs
// `ai services update`; the caller restarts the affected services afterwards to
// recreate their containers against the freshly-pulled images.
func (services realServices) UpdateImages(enabled []string, out io.Writer, progress func(string)) error {
	return services.pullImages(enabled, out, progress, true)
}

// pullImages pulls the required service-tier images, streaming native progress to
// out. When force is false it SKIPS images already present locally (the first-run
// pre-pull, fast re-runs); when force is true it pulls every one (the update path).
// Best-effort: it returns the first pull error but the caller treats it as
// non-fatal.
func (services realServices) pullImages(enabled []string, out io.Writer, progress func(string), force bool) error {
	if progress == nil {
		progress = func(string) {}
	}
	containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
	if err != nil {
		return err
	}
	var firstErr error
	for _, ref := range requiredImages(enabled) {
		if !force {
			// Present locally? Skip — `image inspect` returning an error means absent.
			if _, err := services.prober.Run(containerRuntime.Name, "image", "inspect", ref); err == nil {
				continue
			}
		}
		progress("pulling " + ref)
		// Stream the runtime's native pull progress to the user. The prober captures
		// output and cannot stream, so exec is used directly (like
		// RelaunchLiteLLMWithAuth). #nosec G204 — fixed argv: runtime + "pull" + ref.
		command := exec.Command(containerRuntime.Name, "pull", ref) // #nosec G204
		command.Stdout = out
		command.Stderr = out
		if err := command.Run(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("pull %s: %w", ref, err)
		}
	}
	return firstErr
}

// InstallPrerequisite runs the prerequisite's auto-installer, streaming the
// installer's native stdout/stderr to out so the user sees its progress (e.g. the
// curl|sh installer's own output), mirroring PullImages / RelaunchLiteLLMWithAuth's
// exec+stream pattern. The command is a fixed entry from the platform's own per-OS
// table (installCommandFor), not user input. Returns an error if the command is
// empty (a programmer error) or the installer exits non-zero.
func (services realServices) InstallPrerequisite(prereq Prerequisite, out io.Writer) error {
	if prereq.InstallCommand == "" {
		return fmt.Errorf("no auto-installer for %q on this OS", prereq.Name)
	}
	// #nosec G204 — the command is a fixed per-OS string from the platform's own
	// installCommandFor table (not user input); it is run via `sh -c` only so the
	// pipe (curl … | sh) is honored.
	command := exec.Command("sh", "-c", prereq.InstallCommand)
	command.Stdout = out
	command.Stderr = out
	if err := command.Run(); err != nil {
		return fmt.Errorf("install %s: %w", prereq.Name, err)
	}
	return nil
}

func (services realServices) Reconcile(providerConfig, bindHost string, optional []string, progress func(string)) ([]ServiceStatus, error) {
	configDir, err := paths.ConfigDir()
	if err != nil {
		return nil, err
	}
	for _, service := range desiredServices() {
		if err := os.MkdirAll(filepath.Join(configDir, service.Name), 0o755); err != nil {
			return nil, err
		}
	}
	// (LiteLLM's config is rendered by ensureLiteLLM below — passing providerConfig
	// through — so the same render happens whether launched here or by Control.)
	// Bring up the container tier on the shared network: Ollama (local models),
	// Presidio (the secret-masking guardrail backend), LiteLLM (+ its DB), the
	// Headroom compression proxy, and the nginx gateway in front of Headroom.
	// Presidio precedes LiteLLM because the LiteLLM container is launched with
	// PRESIDIO_*_API_BASE pointing at it. The prompt-injection (detect_prompt_
	// injection) and tool-firewall guardrails are in-process in LiteLLM — they need
	// no companion container.
	containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
	if err != nil {
		return nil, err
	}
	ensurePlatformNetwork(services.prober, containerRuntime.Name)
	// aip-dns first: microVMs need the resolver up before they boot (arch §29).
	progress("  • DNS resolver (aip-dns)…")
	if err := ensureDNS(services.prober, containerRuntime.Name); err != nil {
		return nil, err
	}
	progress("  • Ollama (aip-ollama, local models)…")
	if err := ensureOllama(services.prober, containerRuntime.Name, bindHost); err != nil {
		return nil, err
	}
	progress("  • Presidio (PII guardrail backend)…")
	if err := ensurePresidio(services.prober, containerRuntime.Name); err != nil {
		return nil, err
	}
	// Valkey cache before LiteLLM (LiteLLM's cache_params point at aip-valkey:6379), then
	// its admin UI.
	progress("  • Valkey cache (aip-valkey)…")
	if err := ensureValkey(services.prober, containerRuntime.Name); err != nil {
		return nil, err
	}
	progress("  • Valkey Admin UI (aip-valkey-admin)…")
	if err := ensureValkeyAdmin(services.prober, containerRuntime.Name); err != nil {
		return nil, err
	}
	progress("  • LiteLLM gateway + Postgres (waiting for it to become healthy)…")
	if err := services.ensureLiteLLM(filepath.Join(configDir, "litellm", "config.yaml"), bindHost, providerConfig); err != nil {
		return nil, err
	}
	progress("  • Headroom (compression proxy)…")
	if err := ensureHeadroom(services.prober, containerRuntime.Name); err != nil {
		return nil, err
	}
	// The platform base domain the nginx UI vhost hangs off (runtime.yaml domain,
	// default aip.local).
	domain := reconcileDomain()
	// There are currently NO optional host services to reconcile (Open WebUI moved to
	// a per-workspace in-VM app and Odysseus was removed). The optional mechanism is
	// retained (optional is still threaded through for status reporting), so a future
	// host optional service would be brought up here, BEFORE the nginx proxy.
	// nginx LAST: it is the SOLE host entry, fronting the gateway (/ + /v1 →
	// Headroom), the LiteLLM /llm + Ollama /ollama admin routes, and the LiteLLM admin
	// UI as a Host-based vhost on the same port. The UI vhost hangs off the resolved
	// platform base domain (runtime.yaml domain, default aip.local).
	progress("  • nginx reverse proxy (sole host entry → service tier)…")
	if err := ensureProxy(services.prober, containerRuntime.Name, bindHost, domain); err != nil {
		return nil, err
	}
	// Snapshot each running container's recent output so `ai logs` reflects this
	// run (arch §2.2). Best-effort — a capture failure must not fail the reconcile.
	_ = services.CaptureServiceLogs()
	return services.statusFor(optional)
}

// ensureLiteLLM starts the LiteLLM container via the detected runtime unless it
// is already healthy, then polls briefly for it to come up. Idempotent: it
// removes any stale container of the same name first.
func (services realServices) ensureLiteLLM(configPath, bindHost, providerConfig string) error {
	containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
	if err != nil {
		return err
	}
	// The DB-backed UI needs the network + Postgres regardless of LiteLLM's state.
	ensurePlatformNetwork(services.prober, containerRuntime.Name)
	if err := ensureLiteLLMDB(services.prober, containerRuntime.Name); err != nil {
		return err
	}
	// Skip the relaunch only if LiteLLM is healthy AND already wired to the DB —
	// so a pre-existing container without DATABASE_URL is relaunched once.
	if services.serviceHealthy("litellm") && litellmHasDatabaseURL(services.prober, containerRuntime.Name) {
		return nil
	}
	// Render the config we are about to mount — here (not only in Reconcile) so
	// `ai services restart` re-renders it too: it both applies the current config
	// and guarantees the file exists (a missing `-v` source makes Docker create a
	// directory → LiteLLM IsADirectoryError). providerConfig is "" for the Control
	// path (default routing) and the acceptance/real provider file for Reconcile.
	if err := litellm.Render(litellm.DefaultRouting(), providerConfig); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "render litellm config: %s", err)
	}
	// Preserve the existing UI password + master key across the relaunch so a
	// restart/re-setup does not silently unsecure the admin UI or rotate the key
	// (env passthrough would otherwise copy empty values from this process).
	preserveLiteLLMSecretsInEnv(services.prober, containerRuntime.Name)
	// Guarantee a stable salt key in the process env before the env-passthrough
	// launch: preserve copied any existing one; on the FIRST launch there is none,
	// so mint one (and keep it) — store_model_in_db credentials are encrypted with
	// it at rest. Never overwrite an existing value (rotating it orphans creds).
	if os.Getenv("LITELLM_SALT_KEY") == "" {
		_ = os.Setenv("LITELLM_SALT_KEY", generateSaltKey())
	}
	_, _ = services.prober.Run(containerRuntime.Name, "rm", "-f", litellmContainer) // best-effort cleanup
	if _, err := services.prober.Run(containerRuntime.Name, litellmRunArgs(configPath, bindHost, containerImage("litellm"))...); err != nil {
		return serviceStartError("LiteLLM gateway")
	}
	for attempt := 0; attempt < 15; attempt++ {
		if services.serviceHealthy("litellm") {
			return nil
		}
		time.Sleep(time.Second)
	}
	return nil // launched; Status() will report it as still coming up if not yet healthy
}

// Status reports the health of every known service. The enabled optional set is
// read from runtime.yaml so a not-enabled optional service is shown as
// "disabled" (it is still listed, so users can discover it).
func (services realServices) Status() ([]ServiceStatus, error) {
	// Refresh the on-disk log snapshots so `ai services status` (and its refresh
	// path) keep `ai logs` current with live container output (arch §2.2).
	// Best-effort — a capture failure must not fail a status read.
	_ = services.CaptureServiceLogs()
	return services.statusFor(enabledOptionalServices())
}

// statusFor reports the health of every known service (core + all optional),
// treating the optional services in enabled as live and any other optional
// service as "disabled" (listed but not reconciled). Core services are always
// probed. The optional-disabled state lets `ai services status` surface an
// opt-in tool the user hasn't enabled yet.
func (services realServices) statusFor(enabled []string) ([]ServiceStatus, error) {
	specs := desiredServices()
	// Endpoints render through the single nginx gateway against the platform base
	// DOMAIN (the UI subdomains hang off it, the host-CLI gateway paths resolve
	// under it). reconcileDomain resolves it from runtime.yaml (aip.local default,
	// or the server-role domain). The per-service direct ports are internal-only.
	displayDomain := reconcileDomain()
	statuses := make([]ServiceStatus, 0, len(specs))
	for _, service := range specs {
		endpoint, _ := console.EndpointForHost(service.Name, displayDomain)
		optional := isOptionalService(service.Name)
		if optional && !slices.Contains(enabled, service.Name) {
			// Not enabled: surfaced so it is discoverable, but not probed.
			statuses = append(statuses, ServiceStatus{
				Name: service.Name, Mode: service.Mode, State: "disabled", Healthy: false,
				Address: endpoint.Address, Console: endpoint.Console, Optional: true,
			})
			continue
		}
		healthy := services.serviceHealthy(service.Name)
		// Three states, so a container that is up but not yet ready (e.g. LiteLLM
		// still creating its DB views right after launch) reads "starting", not the
		// misleading "stopped": healthy → running; container(s) up but not healthy →
		// starting; nothing running → stopped.
		state := "stopped"
		switch {
		case healthy:
			state = "running"
		case services.serviceContainersUp(service.Name):
			state = "starting"
		}
		statuses = append(statuses, ServiceStatus{
			Name: service.Name, Mode: service.Mode, State: state, Healthy: healthy,
			Address: endpoint.Address, Console: endpoint.Console, Optional: optional,
		})
	}
	return statuses, nil
}

// enabledOptionalServices returns the optional services enabled on this host
// (from runtime.yaml). An absent/unreadable runtime.yaml yields no enabled
// optional services (so Status reports them "disabled" rather than guessing).
func enabledOptionalServices() []string {
	info, err := runtime.Load()
	if err != nil || info == nil {
		return nil
	}
	return info.OptionalServices
}

// serviceContainersUp reports whether ALL of a service's containers are running,
// regardless of readiness. It distinguishes "starting" (the container is up but its
// readiness probe is not yet passing — e.g. LiteLLM warming up) from "stopped" (no
// container running at all). Returns false if the service maps to no container or
// the runtime can't be resolved.
func (services realServices) serviceContainersUp(name string) bool {
	mapped := serviceContainers(name)
	if len(mapped) == 0 {
		return false
	}
	containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
	if err != nil {
		return false
	}
	for _, container := range mapped {
		if !containerRunning(services.prober, containerRuntime.Name, container) {
			return false
		}
	}
	return true
}

// serviceHealthy is the live readiness probe for one host service (the same
// checks `ai doctor` uses): LiteLLM /health, Ollama /api/version, and the
// Presidio/Headroom containers running.
func (services realServices) serviceHealthy(name string) bool {
	switch name {
	case "litellm":
		info, err := litellm.RealClient().Status()
		return err == nil && info.Healthy
	case "ollama":
		return ollama.RealProbe().Reachable() == nil
	case "presidio":
		containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
		if err != nil {
			return false
		}
		return containerRunning(services.prober, containerRuntime.Name, presidioAnalyzerContainer) &&
			containerRunning(services.prober, containerRuntime.Name, presidioAnonymizerContainer)
	case "headroom":
		containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
		if err != nil {
			return false
		}
		return containerRunning(services.prober, containerRuntime.Name, headroomContainer)
	case "proxy":
		// Live readiness probe (arch §2.3): the nginx gateway is ready once it is
		// running AND actually forwarding — an HTTP GET through the host entry
		// (:18787 → aip-headroom:8787 → aip-litellm) returns SOME response. The
		// container merely running is not enough (nginx can be up before its
		// upstream resolves). proxyReady treats any HTTP status (even 404/502) as
		// "up and forwarding" and only a transport error (connection refused) as
		// down — see proxyReachable.
		containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
		if err != nil {
			return false
		}
		if !containerRunning(services.prober, containerRuntime.Name, proxyContainer) {
			return false
		}
		return proxyReachable(proxyReadinessURL)
	case "dns":
		// The resolver is a pure forwarder; the container running is sufficient
		// (it has no HTTP health endpoint).
		containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
		if err != nil {
			return false
		}
		return containerRunning(services.prober, containerRuntime.Name, dnsContainer)
	default:
		return false
	}
}

// Control performs start/stop/restart on the host services. The platform owns
// the entire container tier (Ollama, Presidio, LiteLLM + its DB, Headroom, the
// nginx gateway), so those are started, stopped, and restarted here. An empty service name (or
// "all") acts on every platform container in dependency order. Returns the
// post-action Status.
// owningService maps a companion container that `ai services` does not manage
// independently to the logical service that owns it. It lets the unknown-service
// error point the user at the right name. There are currently NO such surfaced
// companions: the registry maps litellm-db → litellm, but that mapping is
// intentionally NOT exposed here, preserving the long-standing behavior that
// `ai services <action> litellm-db` reports a plain "unknown service" (litellm-db
// is an internal LiteLLM implementation detail with no independent service
// vocabulary). It thus returns "" for every name today; the hook is retained so a
// future multi-container optional service can surface a companion hint here.
func owningService(name string) string {
	_ = name
	return ""
}

func (services realServices) Control(action, service string) ([]ServiceStatus, error) {
	containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
	if err != nil {
		return nil, output.Errorf(output.ExitMissingDep, "no container runtime to control platform services: %s", err)
	}
	configPath, err := litellm.ConfigPath()
	if err != nil {
		return nil, err
	}
	// Publish on the interface this host's persisted role dictates (server →
	// 0.0.0.0, else loopback), so `ai services start|restart` matches `ai setup`.
	bindHost := currentBindHost()

	stopContainer := func(name string) error {
		if _, err := services.prober.Run(containerRuntime.Name, "stop", name); err != nil {
			return output.Errorf(output.ExitRuntimeFailure, "stop %s via %s: %s", name, containerRuntime.Name, err)
		}
		return nil
	}

	// The enabled optional set governs which web UIs the nginx proxy fronts (and
	// publishes) — nginx is the sole host entry, so a `proxy` start/restart must
	// render the config for whatever UIs are enabled on this host.
	enabledOptional := enabledOptionalServices()

	// The platform-owned services, in dependency order. nginx (proxy) is the SOLE
	// host entry, so it comes AFTER the optional web UIs it fronts: nginx resolves a
	// literal proxy_pass host at config-load time, so its upstreams must be up first.
	// ensure() is the idempotent launcher; stop() halts every container the service
	// owns (Presidio is two).
	type managedService struct {
		name   string
		ensure func() error
		stop   func() error
	}
	managed := []managedService{
		{"ollama",
			func() error { return ensureOllama(services.prober, containerRuntime.Name, bindHost) },
			func() error { return stopContainer(ollamaContainer) }},
		{"presidio",
			func() error { return ensurePresidio(services.prober, containerRuntime.Name) },
			func() error {
				if err := stopContainer(presidioAnalyzerContainer); err != nil {
					return err
				}
				return stopContainer(presidioAnonymizerContainer)
			}},
		{"litellm",
			func() error { return services.ensureLiteLLM(configPath, bindHost, "") },
			func() error { return stopContainer(litellmContainer) }},
		{"valkey",
			func() error { return ensureValkey(services.prober, containerRuntime.Name) },
			func() error { return stopContainer(valkeyContainer) }},
		{"valkey-admin",
			func() error { return ensureValkeyAdmin(services.prober, containerRuntime.Name) },
			func() error { return stopContainer(valkeyAdminContainer) }},
		{"headroom",
			func() error { return ensureHeadroom(services.prober, containerRuntime.Name) },
			func() error { return stopContainer(headroomContainer) }},
		{"proxy",
			func() error {
				return ensureProxy(services.prober, containerRuntime.Name, bindHost, reconcileDomain())
			},
			func() error { return stopContainer(proxyContainer) }},
		{"dns",
			func() error { return ensureDNS(services.prober, containerRuntime.Name) },
			func() error { return stopContainer(dnsContainer) }},
	}

	var targets []managedService
	switch service {
	case "", "all":
		// "all" acts only on the ENABLED set: core services always, plus any optional
		// services currently enabled (there are none today, but the gate is retained
		// so a future host optional service is handled). A named target bypasses this
		// (and is gated by ControlService).
		for _, entry := range managed {
			if isOptionalService(entry.name) && !slices.Contains(enabledOptional, entry.name) {
				continue
			}
			targets = append(targets, entry)
		}
	default:
		for _, entry := range managed {
			if entry.name == service {
				targets = []managedService{entry}
			}
		}
		if targets == nil {
			// Unreachable for an unknown name via the CLI — ControlService validates
			// (and emits the companion-container hint) before delegating here — but
			// kept as a defensive guard for direct callers.
			return nil, output.Errorf(output.ExitInvalidInput,
				"unknown service %q (expected one of: ollama, presidio, litellm, headroom, proxy, dns)", service)
		}
	}

	for _, target := range targets {
		switch action {
		case "start":
			if err := target.ensure(); err != nil {
				return nil, err
			}
		case "stop":
			if err := target.stop(); err != nil {
				return nil, err
			}
		case "restart":
			// Stop then re-launch idempotently (uniform across single- and
			// multi-container services).
			_ = target.stop()
			if err := target.ensure(); err != nil {
				return nil, err
			}
		}
	}
	return services.Status()
}

// RealDeps builds Deps wired to the actual host (used by the CLI). `ai setup`
// does not install software; missing prerequisites are detected and reported
// for the user to install, so there is no dependency installer here.
func RealDeps(goos, goarch string, now func() string) Deps {
	prober := runtime.RealProber()
	return Deps{
		GOOS:     goos,
		GOARCH:   goarch,
		Prober:   prober,
		Now:      now,
		Services: realServices{prober: prober},
		// FetchCatalog fetches + persists the models.dev catalog (offline → the saved
		// copy; never fails). hardware bring-up: the live models.dev fetch.
		FetchCatalog: func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, err := catalog.LoadOrFetch(ctx, nil, "")
			return err
		},
	}
}
