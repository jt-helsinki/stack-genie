package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

type serviceSpec struct{ Name, Mode string }

// desiredServices is the host-service set (arch §5), all containers: Ollama
// (required local model backend), Presidio (PII guardrail backend), LiteLLM
// (gateway/router), and Headroom (input compression proxy in front of LiteLLM).
// Ollama is required — LiteLLM routes local model traffic to it (arch §14, §16).
// Headroom runs as a shared host-side proxy (agents point at :8787, it forwards
// to LiteLLM); the per-project Caveman skill handles output compression inside
// the workspace (arch §8–10).
func desiredServices() []serviceSpec {
	return []serviceSpec{
		{"ollama", "container"},
		{"presidio", "container"},
		{"litellm", "container"},
		{"headroom", "container"},
	}
}

// litellm container naming + image (digest pinned in versions.json during
// release; until then the documented rolling tag).
const (
	litellmContainer = "aip-litellm"
	litellmImage     = "ghcr.io/berriai/litellm:main-latest"
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
	litellmDBImage     = "postgres:18.4-alpine3.24"
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
	ollamaImage       = "ollama/ollama:latest"
	ollamaModelsGuest = "/models" // where ~/.ai-platform/models is mounted in the container

	// Headroom is the input-compression proxy in front of LiteLLM. Official image
	// (no build): agents point at :8787, it forwards to LiteLLM via OPENAI_TARGET_API_URL.
	headroomContainer = "aip-headroom"
	headroomImage     = "ghcr.io/chopratejas/headroom:slim"
	headroomTargetURL = "http://" + litellmContainer + ":4000"

	// Presidio backs LiteLLM's always-on PII guardrail (arch §17). The analyzer
	// detects PII and the anonymizer masks it; LiteLLM reaches both by name on the
	// shared network via PRESIDIO_*_API_BASE (port 3000, the image default). They
	// are internal only — not published to the host.
	presidioAnalyzerContainer   = "aip-presidio-analyzer"
	presidioAnonymizerContainer = "aip-presidio-anonymizer"
	presidioAnalyzerImage       = "mcr.microsoft.com/presidio-analyzer:latest"
	presidioAnonymizerImage     = "mcr.microsoft.com/presidio-anonymizer:latest"
	presidioAnalyzerURL         = "http://" + presidioAnalyzerContainer + ":3000"
	presidioAnonymizerURL       = "http://" + presidioAnonymizerContainer + ":3000"
)

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
func litellmRunArgs(configPath string) []string {
	return []string{
		"run", "-d", "--name", litellmContainer,
		"--network", platformNetwork,
		"-p", "4000:4000",
		"-v", configPath + ":/app/config.yaml",
		"-e", "UI_USERNAME=" + litellmUIUsername,
		"-e", "UI_PASSWORD",
		"-e", "LITELLM_MASTER_KEY",
		"-e", "DATABASE_URL=" + litellmDatabaseURL,
		// Reach the Presidio analyzer/anonymizer by name on the shared network so
		// the always-on PII guardrail has a backend (no secret in these values).
		"-e", "PRESIDIO_ANALYZER_API_BASE=" + presidioAnalyzerURL,
		"-e", "PRESIDIO_ANONYMIZER_API_BASE=" + presidioAnonymizerURL,
		litellmImage,
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
		litellmDBImage,
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

// ensureOllama runs the Ollama container on the shared network, publishing :11434
// and persisting models under ~/.ai-platform/models on the host (bind-mounted).
// Idempotent. Replaces a native Ollama — any native instance bound to :11434 must
// be stopped first.
func ensureOllama(prober runtime.Prober, containerRuntime string) error {
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
		"-p", "11434:11434",
		"-v", modelsDir + ":" + ollamaModelsGuest,
		"-e", "OLLAMA_MODELS=" + ollamaModelsGuest,
		ollamaImage,
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
		{presidioAnalyzerContainer, presidioAnalyzerImage},
		{presidioAnonymizerContainer, presidioAnonymizerImage},
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
// agents send to :8787, it forwards to LiteLLM via OPENAI_TARGET_API_URL. Pulled
// image (no build). Idempotent.
func ensureHeadroom(prober runtime.Prober, containerRuntime string) error {
	if containerRunning(prober, containerRuntime, headroomContainer) {
		return nil
	}
	_, _ = prober.Run(containerRuntime, "rm", "-f", headroomContainer)
	args := []string{
		"run", "-d", "--name", headroomContainer,
		"--network", platformNetwork,
		"-p", "8787:8787",
		"-e", "OPENAI_TARGET_API_URL=" + headroomTargetURL,
		// Headroom otherwise injects an empty `tools:[]` (its CCR retrieve-tool
		// path) into every request, which flips LiteLLM/Ollama into tool-calling
		// mode and corrupts answers regardless of prompt size. Disabling the tool
		// injection keeps compression enabled while forwarding requests faithfully.
		"-e", "HEADROOM_NO_CCR_INJECT_TOOL=1",
		headroomImage,
	}
	if _, err := prober.Run(containerRuntime, args...); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "launch headroom via %s: %s", containerRuntime, err)
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
	// Best-effort removal of the running (likely unsecured) container.
	_ = exec.Command(containerRuntime.Name, "rm", "-f", litellmContainer).Run() // #nosec G204 — fixed args

	// #nosec G204 — fixed argv; secrets ride in the environment, not the command line.
	command := exec.Command(containerRuntime.Name, litellmRunArgs(configPath)...)
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

func (services realServices) Reconcile(providerConfig string) ([]ServiceStatus, error) {
	configDir, err := paths.ConfigDir()
	if err != nil {
		return nil, err
	}
	for _, service := range desiredServices() {
		if err := os.MkdirAll(filepath.Join(configDir, service.Name), 0o755); err != nil {
			return nil, err
		}
	}
	// Render the LiteLLM gateway config from default routing (placeholders only),
	// or pass through a provided provider config (e.g. the acceptance harness).
	if err := litellm.Render(litellm.DefaultRouting(), providerConfig); err != nil {
		return nil, err
	}
	// Bring up the container tier on the shared network: Ollama (local models),
	// Presidio (PII guardrail backend), LiteLLM (+ its DB), and the Headroom
	// compression proxy in front of LiteLLM. Presidio precedes LiteLLM because the
	// LiteLLM container is launched with PRESIDIO_*_API_BASE pointing at it.
	containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
	if err != nil {
		return nil, err
	}
	ensurePlatformNetwork(services.prober, containerRuntime.Name)
	if err := ensureOllama(services.prober, containerRuntime.Name); err != nil {
		return nil, err
	}
	if err := ensurePresidio(services.prober, containerRuntime.Name); err != nil {
		return nil, err
	}
	if err := services.ensureLiteLLM(filepath.Join(configDir, "litellm", "config.yaml")); err != nil {
		return nil, err
	}
	if err := ensureHeadroom(services.prober, containerRuntime.Name); err != nil {
		return nil, err
	}
	return services.Status()
}

// ensureLiteLLM starts the LiteLLM container via the detected runtime unless it
// is already healthy, then polls briefly for it to come up. Idempotent: it
// removes any stale container of the same name first.
func (services realServices) ensureLiteLLM(configPath string) error {
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
	// Preserve the existing UI password + master key across the relaunch so a
	// restart/re-setup does not silently unsecure the admin UI or rotate the key
	// (env passthrough would otherwise copy empty values from this process).
	preserveLiteLLMSecretsInEnv(services.prober, containerRuntime.Name)
	_, _ = services.prober.Run(containerRuntime.Name, "rm", "-f", litellmContainer) // best-effort cleanup
	if _, err := services.prober.Run(containerRuntime.Name, litellmRunArgs(configPath)...); err != nil {
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

func (services realServices) Status() ([]ServiceStatus, error) {
	specs := desiredServices()
	statuses := make([]ServiceStatus, 0, len(specs))
	for _, service := range specs {
		healthy := services.serviceHealthy(service.Name)
		state := "stopped"
		if healthy {
			state = "running"
		}
		statuses = append(statuses, ServiceStatus{
			Name: service.Name, Mode: service.Mode, State: state, Healthy: healthy,
		})
	}
	return statuses, nil
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
	default:
		return false
	}
}

// Control performs start/stop/restart on the host services. The platform owns
// the entire container tier (Ollama, Presidio, LiteLLM + its DB, Headroom), so
// those are started, stopped, and restarted here. An empty service name (or
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
			func() error { return ensureOllama(services.prober, containerRuntime.Name) },
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
			func() error { return services.ensureLiteLLM(configPath) },
			func() error { return stopContainer(litellmContainer) }},
		{"headroom",
			func() error { return ensureHeadroom(services.prober, containerRuntime.Name) },
			func() error { return stopContainer(headroomContainer) }},
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
				"unknown service %q (expected one of: ollama, presidio, litellm, headroom)", service)
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

// realDepInstaller runs a dependency's official one-line installer, streaming its
// output so the user sees progress.
type realDepInstaller struct{}

func (realDepInstaller) Install(binary string) error {
	url, ok := installerURL(binary)
	if !ok {
		return fmt.Errorf("no installer known for %q", binary)
	}
	// The documented installer is `curl -fsSL <url> | sh`
	// (install.microsandbox.dev). #nosec G204 — url is a fixed in-binary constant.
	command := exec.Command("sh", "-c", "curl -fsSL "+url+" | sh")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

// RealDeps builds Deps wired to the actual host (used by the CLI).
func RealDeps(goos, goarch string, now func() string) Deps {
	prober := runtime.RealProber()
	return Deps{
		GOOS:         goos,
		GOARCH:       goarch,
		Prober:       prober,
		Now:          now,
		Services:     realServices{prober: prober},
		DepInstaller: realDepInstaller{},
	}
}
