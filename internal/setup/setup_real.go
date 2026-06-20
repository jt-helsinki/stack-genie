package setup

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/ollama"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// realFetchGatewayConfig downloads the upstream ClawPatrol gateway example HCL.
func realFetchGatewayConfig() ([]byte, error) {
	httpClient := &http.Client{Timeout: 15 * time.Second}
	response, err := httpClient.Get(gatewayConfigURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", gatewayConfigURL, response.StatusCode)
	}
	return io.ReadAll(response.Body)
}

type serviceSpec struct{ Name, Mode string }

// desiredServices is the host-service set (arch §5): Ollama (required local model
// backend, container), LiteLLM (gateway/router, container), and ClawPatrol
// (firewall + credential broker, native). Ollama is required — LiteLLM routes
// local model traffic to it (arch §14, §16). Context optimization is NOT here —
// Headroom (input compression) runs per-project inside the workspace alongside
// the Caveman skill (arch §8–10), not as a host service.
func desiredServices() []serviceSpec {
	return []serviceSpec{
		{"ollama", "container"},
		{"litellm", "container"},
		{"clawpatrol", "native"},
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
func litellmRunArgs(configPath string) []string {
	return []string{
		"run", "-d", "--name", litellmContainer,
		"-p", "4000:4000",
		"-v", configPath + ":/app/config.yaml",
		"-e", "UI_USERNAME=" + litellmUIUsername,
		"-e", "UI_PASSWORD",
		"-e", "LITELLM_MASTER_KEY",
		litellmImage,
		"--config", "/app/config.yaml", "--port", "4000",
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
	// Launch LiteLLM if it is not already up (Ollama/ClawPatrol are started by
	// their own installers; LiteLLM is the platform's to run).
	if err := services.ensureLiteLLM(filepath.Join(configDir, "litellm", "config.yaml")); err != nil {
		return nil, err
	}
	return services.Status()
}

// ensureLiteLLM starts the LiteLLM container via the detected runtime unless it
// is already healthy, then polls briefly for it to come up. Idempotent: it
// removes any stale container of the same name first.
func (services realServices) ensureLiteLLM(configPath string) error {
	if services.serviceHealthy("litellm") {
		return nil
	}
	containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
	if err != nil {
		return err
	}
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
// checks `ai doctor` uses): LiteLLM /health, Ollama /api/version, and
// `clawpatrol status`.
func (services realServices) serviceHealthy(name string) bool {
	switch name {
	case "litellm":
		info, err := litellm.RealClient().Status()
		return err == nil && info.Healthy
	case "ollama":
		return ollama.RealProbe().Reachable() == nil
	case "clawpatrol":
		_, err := services.prober.Run("clawpatrol", "status")
		return err == nil
	default:
		return false
	}
}

// Control performs start/stop/restart on the host services. Only LiteLLM's
// lifecycle is the platform's to manage; Ollama and ClawPatrol are started by
// their own installers/service managers, so naming them explicitly is rejected
// with guidance, while "all" simply skips them. Returns the post-action Status.
func (services realServices) Control(action, service string) ([]ServiceStatus, error) {
	switch service {
	case "ollama":
		return nil, output.Errorf(output.ExitInvalidInput,
			"ollama is started by its own installer; the platform does not manage its lifecycle (use Ollama's own service control, e.g. the Ollama app or `brew services`)")
	case "clawpatrol":
		return nil, output.Errorf(output.ExitInvalidInput,
			"clawpatrol runs as a native gateway managed by its own installer; the platform does not control its lifecycle")
	}

	// service is "" (all) or "litellm": act on the platform-owned LiteLLM container.
	containerRuntime, err := runtime.ContainerRuntimeName(services.prober)
	if err != nil {
		return nil, output.Errorf(output.ExitMissingDep, "no container runtime to control LiteLLM: %s", err)
	}
	configPath, err := litellm.ConfigPath()
	if err != nil {
		return nil, err
	}

	switch action {
	case "start":
		if err := services.ensureLiteLLM(configPath); err != nil {
			return nil, err
		}
	case "stop":
		if _, err := services.prober.Run(containerRuntime.Name, "stop", litellmContainer); err != nil {
			return nil, output.Errorf(output.ExitRuntimeFailure, "stop litellm via %s: %s", containerRuntime.Name, err)
		}
	case "restart":
		// Restart the existing container; if it isn't there yet, launch it fresh.
		if _, err := services.prober.Run(containerRuntime.Name, "restart", litellmContainer); err != nil {
			if err := services.ensureLiteLLM(configPath); err != nil {
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
	// The documented installers are `curl -fsSL <url> | sh` (clawpatrol.dev,
	// install.microsandbox.dev). #nosec G204 — url is a fixed in-binary constant.
	command := exec.Command("sh", "-c", "curl -fsSL "+url+" | sh")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

// realCA prepares the ClawPatrol CA location (arch §17). Generating the CA
// certificate is delegated to ClawPatrol on a provisioned host.
type realCA struct{}

func (realCA) Ensure() error {
	configDir, err := paths.ConfigDir()
	if err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(configDir, "clawpatrol", "ca"), 0o755)
}

// RealDeps builds Deps wired to the actual host (used by the CLI).
func RealDeps(goos, goarch string, now func() string) Deps {
	prober := runtime.RealProber()
	return Deps{
		GOOS:                 goos,
		GOARCH:               goarch,
		Prober:               prober,
		Now:                  now,
		Services:             realServices{prober: prober},
		CA:                   realCA{},
		DepInstaller:         realDepInstaller{},
		GatewayConfigFetcher: realFetchGatewayConfig,
	}
}
