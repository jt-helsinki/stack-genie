package setup

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/console"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/versions"
)

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
// the workspace (arch §8–10). These are reconciled on every `ai setup`.
func coreServices() []serviceSpec {
	return []serviceSpec{
		{"ollama", "container"},
		{"presidio", "container"},
		{"litellm", "container"},
		{"headroom", "container"},
		// nginx reverse proxy: the gateway entry on host :18787, in front of Headroom.
		{"proxy", "container"},
		{"dns", "container"},
	}
}

// optionalServices is the opt-in host-service set: services that run on the
// host, OUTSIDE the workspace microVM sandbox, and so are reconciled only when
// the user has explicitly enabled them at `ai setup`. They are an additive,
// data-driven list — adding a new optional service is just another entry here
// (plus its ensure*/stop wiring). Currently: open-webui (the chat UI).
func optionalServices() []serviceSpec {
	return []serviceSpec{
		{"open-webui", "container"},
	}
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

// requiredImages returns the image reference (repo:tag) for EVERY container in
// the service tier, resolved via containerImage with the same versions-keys
// containerImage expects. desiredServices lists the services but NOT litellm-db
// (it has no health line / Status entry), so it is added explicitly; and its
// "presidio" entry is one logical service backed by TWO images
// (presidio-analyzer + presidio-anonymizer, the actual versions keys), so it is
// expanded here. Pure and testable. Duplicate refs are de-duplicated so a shared
// image is only pulled once.
func requiredImages() []string {
	serviceKeys := []string{"litellm-db"}
	for _, service := range desiredServices() {
		switch service.Name {
		case "presidio":
			serviceKeys = append(serviceKeys, "presidio-analyzer", "presidio-anonymizer")
		default:
			serviceKeys = append(serviceKeys, service.Name)
		}
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
	litellmDBVolume    = "aip-litellm-db-data"
	litellmDBUser      = "litellm"
	litellmDBName      = "litellm"
	litellmDatabaseURL = "postgresql://" + litellmDBUser + "@" + litellmDBContainer + ":5432/" + litellmDBName
	// Published on the host at 5442 (not the default 5432) and bound to loopback,
	// to avoid clashing with any other Postgres on the machine. LiteLLM itself
	// reaches the DB over the private network (5432), not this host port.
	litellmDBHostPort = "5442"

	// Ollama runs as a container on aip-net (so LiteLLM reaches it by name) and
	// publishes :11434 to the host. Models persist on the host under
	// ~/.ai-platform/models (bind-mounted to ollamaModelsGuest, with OLLAMA_MODELS
	// pointing there) so they are visible on disk and removed with the rest of
	// platform state on `ai uninstall --purge`. Replaces a native Ollama — stop any
	// native instance bound to 11434 first.
	ollamaContainer   = "aip-ollama"
	ollamaModelsGuest = "/models" // where ~/.ai-platform/models is mounted in the container

	// Headroom is the input-compression proxy in front of LiteLLM. Official image
	// (no build). It is now INTERNAL-ONLY on aip-net at :8787 (no host publish) —
	// the nginx reverse proxy (aip-proxy) is the gateway entry on host :18787 and
	// forwards to Headroom by name; Headroom forwards to LiteLLM via
	// OPENAI_TARGET_API_URL.
	headroomContainer = "aip-headroom"
	headroomTargetURL = "http://" + litellmContainer + ":4000"

	// aip-proxy is the nginx reverse proxy that is the gateway ENTRY on the host:
	// microVM → nginx (host :18787) → Headroom (compress) → LiteLLM. nginx takes
	// over the established gateway port 18787 (what resolveGateway returns), so this
	// is transparent to workspaces; Headroom no longer publishes to the host. nginx
	// terminates TLS later (the future HTTPS endpoint). Pinned minor tag.
	proxyContainer = "aip-proxy"
	proxyHostPort  = "18787"
	proxyTargetURL = "http://" + headroomContainer + ":8787"

	// Open WebUI is the optional chat UI for the platform, routed through LiteLLM
	// as an OpenAI-compatible gateway. Pulled image (no build): it listens on :8080
	// in the container, published to the host at openWebUIHostPort, and persists its
	// data on a named volume. The built-in Ollama backend and the login wall are
	// disabled (single-user local UI); all model traffic goes via LiteLLM.
	openWebUIContainer = "aip-open-webui"
	// Published on the host at a deliberately non-standard port (18090, not 8090)
	// to avoid clashing with common dev servers; the container still listens on 8080.
	openWebUIHostPort  = "18090"
	openWebUIVolume    = "aip-open-webui-data"
	openWebUITargetURL = "http://" + litellmContainer + ":4000/v1"

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
	// to public upstreams and caches briefly. Published to the host LOOPBACK at
	// dnsHostPort so msb's netstack can forward guest DNS to it; not exposed off
	// the machine. The image reference is resolved from versions.yaml (CoreDNS).
	dnsContainer = "aip-dns"
	dnsHostPort  = "15353"
	// DNSNameserver is the guest-facing target passed to `msb create
	// --dns-nameserver` (a fixed platform setting). msb's netstack forwards guest
	// DNS to this host loopback address.
	DNSNameserver = "127.0.0.1:" + dnsHostPort
	// dnsUpstreams are the public resolvers CoreDNS forwards to (Cloudflare +
	// Google). Audit-only; not security-sensitive.
	dnsUpstreams = "1.1.1.1 8.8.8.8"
)

// proxyNginxConf is the nginx reverse-proxy config rendered to
// ~/.ai-platform/config/proxy/nginx.conf and bind-mounted at /etc/nginx/nginx.conf.
// It reverse-proxies / → Headroom (aip-headroom:8787) and is tuned for LLM
// traffic: HTTP/1.1 + an empty Connection header + disabled response buffering so
// streamed (SSE) responses flush promptly, and long read/send timeouts for slow
// generations. A `server { listen 443 ssl; ... }` block is the future HTTPS
// termination point (out of scope now).
const proxyNginxConf = `events {}
http {
  server {
    listen 80;
    location / {
      proxy_pass http://aip-headroom:8787;
      proxy_http_version 1.1;
      proxy_set_header Host $host;
      proxy_set_header Connection "";
      proxy_buffering off;
      proxy_read_timeout 3600s;
      proxy_send_timeout 3600s;
    }
  }
}
`

// litellmRunArgs is the `<runtime> run` argv that launches LiteLLM with the
// rendered config mounted (docs.litellm.ai). Pure, so it is unit-testable.
//
// Admin-UI auth (docs.litellm.ai/docs/proxy/ui) is wired here: the username is
// inlined (it is not secret), while UI_PASSWORD and LITELLM_MASTER_KEY are passed
// as **env passthrough** (`-e NAME`, no value) so Docker copies them from the
// launching process's environment — the secret values never appear in argv, the
// config, or on platform disk. The UI is secured whenever those two are present
// in the environment at launch (exported by the user, or set for a relaunch by
// the setup prompt); otherwise LiteLLM falls back to its own default behavior.
//
// DATABASE_URL is inlined (it carries no secret — trust auth on a private
// network) so the DB-backed admin UI / virtual keys work. The container joins
// platformNetwork so it can reach aip-litellm-db by name.
// bindHost is the host interface a shared service publishes on. The role decides
// it (CLI §2.1): standalone (the default and the absent-role case) keeps the
// shared services loopback-bound — local microVMs reach the loopback-bound nginx
// gateway via msb's netstack, so this is a security tightening with no functional
// loss — while server binds 0.0.0.0 so other machines can connect.
func litellmRunArgs(configPath, bindHost, image string) []string {
	return []string{
		"run", "-d", "--name", litellmContainer,
		"--network", platformNetwork,
		// Host port is deliberately non-standard (14000, not 4000) to avoid clashing
		// with common dev servers; the container still listens on 4000 (--port 4000).
		"-p", bindHost + ":14000:4000",
		"-v", configPath + ":/app/config.yaml",
		"-e", "UI_USERNAME=" + litellmUIUsername,
		"-e", "UI_PASSWORD",
		"-e", "LITELLM_MASTER_KEY",
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

// ensureLiteLLMDB starts the Postgres that backs LiteLLM's admin UI / virtual
// keys, unless it is already running. Trust auth on the private network (no
// password); the host port is loopback-bound at litellmDBHostPort. Idempotent.
func ensureLiteLLMDB(prober runtime.Prober, containerRuntime string) error {
	out, err := prober.Run(containerRuntime, "ps", "--filter", "name=^/"+litellmDBContainer+"$",
		"--filter", "status=running", "--format", "{{.Names}}")
	if err == nil && strings.TrimSpace(string(out)) == litellmDBContainer {
		return nil // already up
	}
	_, _ = prober.Run(containerRuntime, "rm", "-f", litellmDBContainer) // clear any stopped one
	args := []string{
		"run", "-d", "--name", litellmDBContainer,
		"--network", platformNetwork,
		"-p", "127.0.0.1:" + litellmDBHostPort + ":5432",
		"-e", "POSTGRES_USER=" + litellmDBUser,
		"-e", "POSTGRES_DB=" + litellmDBName,
		"-e", "POSTGRES_HOST_AUTH_METHOD=trust",
		// Postgres 18+ stores data in a version-specific subdir, so the volume is
		// mounted at /var/lib/postgresql (NOT .../data, the pre-18 convention) —
		// otherwise the image refuses to start (docker-library/postgres#1259).
		"-v", litellmDBVolume + ":/var/lib/postgresql",
		containerImage("litellm-db"),
	}
	if _, err := prober.Run(containerRuntime, args...); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "launch litellm db via %s: %s", containerRuntime, err)
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

// ensureOllama runs the Ollama container on the shared network, publishing :11434
// and persisting models under ~/.ai-platform/models on the host (bind-mounted).
// Idempotent. Replaces a native Ollama — any native instance bound to :11434 must
// be stopped first.
func ensureOllama(prober runtime.Prober, containerRuntime, bindHost string) error {
	if containerRunning(prober, containerRuntime, ollamaContainer) {
		return nil
	}
	platformDir, err := paths.PlatformDir()
	if err != nil {
		return err
	}
	modelsDir := filepath.Join(platformDir, "models")
	if err := os.MkdirAll(modelsDir, 0o755); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "create ollama models dir %s: %s", modelsDir, err)
	}
	_, _ = prober.Run(containerRuntime, "rm", "-f", ollamaContainer)
	args := []string{
		"run", "-d", "--name", ollamaContainer,
		"--network", platformNetwork,
		"-p", bindHost + ":11434:11434",
		"-v", modelsDir + ":" + ollamaModelsGuest,
		"-e", "OLLAMA_MODELS=" + ollamaModelsGuest,
		containerImage("ollama"),
	}
	if _, err := prober.Run(containerRuntime, args...); err != nil {
		return output.Errorf(output.ExitRuntimeFailure,
			"launch ollama via %s: %s (stop any native Ollama bound to :11434 first)", containerRuntime, err)
	}
	return nil
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
			return output.Errorf(output.ExitRuntimeFailure, "launch %s via %s: %s", presidio.name, containerRuntime, err)
		}
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
		return output.Errorf(output.ExitRuntimeFailure, "launch headroom via %s: %s", containerRuntime, err)
	}
	return nil
}

// ensureOpenWebUI runs the Open WebUI chat UI, routed through LiteLLM as an
// OpenAI-compatible gateway: agents/users hit :18090 on the host and the UI sends
// model traffic to LiteLLM via OPENAI_API_BASE_URL (which includes /v1). The
// built-in Ollama backend and the login wall are disabled. The gateway key is the
// LiteLLM master key when one is set: it is reused from the running LiteLLM
// container (litellmEnvValue) and passed as **env passthrough** (`-e
// OPENAI_API_KEY`, no value) so it never appears in argv or on platform disk —
// exactly like the LiteLLM secret handling. When LiteLLM has no master key the key
// is omitted (LiteLLM then accepts unauthenticated requests). Pulled image (no
// build). Idempotent.
func ensureOpenWebUI(prober runtime.Prober, containerRuntime, bindHost string) error {
	if containerRunning(prober, containerRuntime, openWebUIContainer) {
		return nil
	}
	_, _ = prober.Run(containerRuntime, "rm", "-f", openWebUIContainer)
	args := []string{
		"run", "-d", "--name", openWebUIContainer,
		"--network", platformNetwork,
		"-p", bindHost + ":" + openWebUIHostPort + ":8080",
		"-v", openWebUIVolume + ":/app/backend/data",
		"-e", "OPENAI_API_BASE_URL=" + openWebUITargetURL,
		"-e", "ENABLE_OLLAMA_API=false",
		"-e", "WEBUI_AUTH=false",
	}
	// Reuse the running LiteLLM container's master key, passing it via env
	// passthrough so the value stays out of argv (and platform disk).
	if key := litellmEnvValue(prober, containerRuntime, "LITELLM_MASTER_KEY"); key != "" {
		_ = os.Setenv("OPENAI_API_KEY", key)
		args = append(args, "-e", "OPENAI_API_KEY")
	}
	args = append(args, containerImage("open-webui"))
	if _, err := prober.Run(containerRuntime, args...); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "launch open-webui via %s: %s", containerRuntime, err)
	}
	return nil
}

// ensureDNS runs the aip-dns CoreDNS resolver on the shared network, published to
// the host loopback at dnsHostPort/udp so microVMs (booted with --dns-nameserver
// DNSNameserver) forward their DNS there for the attempted-egress-by-name audit
// (arch §29). It renders a Corefile to ~/.ai-platform/config/dns/Corefile with the
// `log` plugin (the audit source), a `forward` to public upstreams, and a short
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
		"-p", "127.0.0.1:" + dnsHostPort + ":53/udp",
		"-v", corefilePath + ":/Corefile",
		containerImage("dns"),
		"-conf", "/Corefile",
	}
	if _, err := prober.Run(containerRuntime, args...); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "launch aip-dns via %s: %s", containerRuntime, err)
	}
	return nil
}

// ensureProxy runs the nginx reverse proxy that is the gateway entry on the host:
// microVM → nginx (bindHost:18787) → Headroom (:8787) → LiteLLM. It renders the
// nginx config to ~/.ai-platform/config/proxy/nginx.conf, bind-mounts it at
// /etc/nginx/nginx.conf, and publishes the established gateway port 18787 on the
// role's bindHost (0.0.0.0 for a server — the eventual public HTTPS endpoint —
// else loopback). Pulled image (no build). Idempotent: skips if already running,
// removes any stale container first.
func ensureProxy(prober runtime.Prober, containerRuntime, bindHost string) error {
	if containerRunning(prober, containerRuntime, proxyContainer) {
		return nil
	}
	configDir, err := paths.ConfigDir()
	if err != nil {
		return err
	}
	proxyDir := filepath.Join(configDir, "proxy")
	if err := os.MkdirAll(proxyDir, 0o755); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "create proxy config dir %s: %s", proxyDir, err)
	}
	confPath := filepath.Join(proxyDir, "nginx.conf")
	if err := os.WriteFile(confPath, []byte(proxyNginxConf), 0o644); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "write nginx.conf %s: %s", confPath, err)
	}
	_, _ = prober.Run(containerRuntime, "rm", "-f", proxyContainer) // clear any stopped one
	args := []string{
		"run", "-d", "--name", proxyContainer,
		"--network", platformNetwork,
		"-p", bindHost + ":" + proxyHostPort + ":80",
		"-v", confPath + ":/etc/nginx/nginx.conf:ro",
		containerImage("proxy"),
	}
	if _, err := prober.Run(containerRuntime, args...); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "launch aip-proxy via %s: %s", containerRuntime, err)
	}
	return nil
}

// litellmHasDatabaseURL reports whether the running LiteLLM container already has
// DATABASE_URL wired (so a healthy-but-DB-less container is relaunched once).
func litellmHasDatabaseURL(prober runtime.Prober, containerRuntime string) bool {
	return litellmEnvSet(prober, containerRuntime, "DATABASE_URL")
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

// preserveLiteLLMSecretsInEnv copies the running container's UI_PASSWORD and
// LITELLM_MASTER_KEY into this process's environment when they are not already
// present, so an env-passthrough relaunch (litellmRunArgs) keeps the admin UI
// secured and the master key stable rather than silently dropping them. Values
// transit process memory only — never argv or platform disk. MUST be called
// before the container is removed (a stopped container can still be inspected).
func preserveLiteLLMSecretsInEnv(prober runtime.Prober, containerRuntime string) {
	for _, key := range []string{"UI_PASSWORD", "LITELLM_MASTER_KEY"} {
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
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("relaunch litellm: %s: %s", err, string(output))
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
// Already-present images are skipped (fast re-runs). Best-effort: it returns the
// first pull error but the caller treats it as non-fatal — the reconcile's
// per-service `docker run` re-pulls anything still missing.
func (services realServices) PullImages(out io.Writer, progress func(string)) error {
	if progress == nil {
		progress = func(string) {}
	}
	containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
	if err != nil {
		return err
	}
	var firstErr error
	for _, ref := range requiredImages() {
		// Present locally? Skip — `image inspect` returning an error means absent.
		if _, err := services.prober.Run(containerRuntime.Name, "image", "inspect", ref); err == nil {
			continue
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
	progress("  • LiteLLM gateway + Postgres (waiting for it to become healthy)…")
	if err := services.ensureLiteLLM(filepath.Join(configDir, "litellm", "config.yaml"), bindHost, providerConfig); err != nil {
		return nil, err
	}
	progress("  • Headroom (compression proxy)…")
	if err := ensureHeadroom(services.prober, containerRuntime.Name); err != nil {
		return nil, err
	}
	progress("  • nginx reverse proxy (gateway entry → Headroom)…")
	if err := ensureProxy(services.prober, containerRuntime.Name, bindHost); err != nil {
		return nil, err
	}
	// Optional services: reconciled ONLY when enabled. These run on the host,
	// outside the workspace sandbox, so they are opt-in (chosen at `ai setup`).
	if slices.Contains(optional, "open-webui") {
		progress("  • open-webui (chat UI → LiteLLM)…")
		if err := ensureOpenWebUI(services.prober, containerRuntime.Name, bindHost); err != nil {
			return nil, err
		}
	}
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
	_, _ = services.prober.Run(containerRuntime.Name, "rm", "-f", litellmContainer) // best-effort cleanup
	if _, err := services.prober.Run(containerRuntime.Name, litellmRunArgs(configPath, bindHost, containerImage("litellm"))...); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "launch litellm via %s: %s", containerRuntime.Name, err)
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
	return services.statusFor(enabledOptionalServices())
}

// statusFor reports the health of every known service (core + all optional),
// treating the optional services in enabled as live and any other optional
// service as "disabled" (listed but not reconciled). Core services are always
// probed. The optional-disabled state lets `ai services status` surface an
// opt-in tool the user hasn't enabled yet.
func (services realServices) statusFor(enabled []string) ([]ServiceStatus, error) {
	specs := desiredServices()
	statuses := make([]ServiceStatus, 0, len(specs))
	for _, service := range specs {
		endpoint, _ := console.EndpointFor(service.Name)
		if isOptionalService(service.Name) && !slices.Contains(enabled, service.Name) {
			// Not enabled: surfaced so it is discoverable, but not probed.
			statuses = append(statuses, ServiceStatus{
				Name: service.Name, Mode: service.Mode, State: "disabled", Healthy: false,
				Address: endpoint.Address, Console: endpoint.Console,
			})
			continue
		}
		healthy := services.serviceHealthy(service.Name)
		state := "stopped"
		if healthy {
			state = "running"
		}
		statuses = append(statuses, ServiceStatus{
			Name: service.Name, Mode: service.Mode, State: state, Healthy: healthy,
			Address: endpoint.Address, Console: endpoint.Console,
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
		// The nginx gateway is ready once the container is running (the live HTTP
		// readiness probe is a hardware bring-up seam — see docs/HARDWARE-BRINGUP.md).
		containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
		if err != nil {
			return false
		}
		return containerRunning(services.prober, containerRuntime.Name, proxyContainer)
	case "open-webui":
		// Open WebUI takes a while to boot; the container running is sufficient for
		// readiness here (the live /health probe is `ai doctor`'s job).
		containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
		if err != nil {
			return false
		}
		return containerRunning(services.prober, containerRuntime.Name, openWebUIContainer)
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

	// The platform-owned services, in dependency order (Ollama + Presidio before
	// the gateway, the compression proxy last). ensure() is the idempotent
	// launcher; stop() halts every container the service owns (Presidio is two).
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
		{"headroom",
			func() error { return ensureHeadroom(services.prober, containerRuntime.Name) },
			func() error { return stopContainer(headroomContainer) }},
		{"proxy",
			func() error { return ensureProxy(services.prober, containerRuntime.Name, bindHost) },
			func() error { return stopContainer(proxyContainer) }},
		{"open-webui",
			func() error { return ensureOpenWebUI(services.prober, containerRuntime.Name, bindHost) },
			func() error { return stopContainer(openWebUIContainer) }},
		{"dns",
			func() error { return ensureDNS(services.prober, containerRuntime.Name) },
			func() error { return stopContainer(dnsContainer) }},
	}

	var targets []managedService
	switch service {
	case "", "all":
		targets = managed
	default:
		for _, entry := range managed {
			if entry.name == service {
				targets = []managedService{entry}
			}
		}
		if targets == nil {
			return nil, output.Errorf(output.ExitInvalidInput,
				"unknown service %q (expected one of: ollama, presidio, litellm, headroom, proxy, open-webui, dns)", service)
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
	}
}
