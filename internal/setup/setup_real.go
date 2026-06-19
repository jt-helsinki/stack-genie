package setup

import (
	"os"
	"path/filepath"

	"github.com/jt-helsinki/ideal-robot/internal/litellm"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/versions"
)

type serviceSpec struct{ Name, Mode string }

// desiredServices is the host-service set (arch §5): LiteLLM (gateway, container)
// and ClawPatrol (firewall, native), plus Ollama (container) only when it is
// enabled in config/versions.json (default off). Context optimization is NOT
// here — Headroom (input compression) runs per-project inside the workspace
// alongside the Caveman skill (arch §8–10), not as a host service.
func desiredServices() []serviceSpec {
	specs := []serviceSpec{
		{"litellm", "container"},
		{"clawpatrol", "native"},
	}
	if ollamaEnabled() {
		specs = append(specs, serviceSpec{"ollama", "container"})
	}
	return specs
}

// ollamaEnabled reports whether the optional Ollama service is turned on in
// config/versions.json. A missing or unreadable file means off.
func ollamaEnabled() bool {
	file, err := versions.Load()
	if err != nil || file == nil {
		return false
	}
	service, ok := file.Services["ollama"]
	return ok && service.Enabled != nil && *service.Enabled
}

// realServices reconciles/reports host services via the runtime Prober.
//
// Reconcile here prepares the rendered config directories and reports status via
// host probes. Launching the LiteLLM container and registering the ClawPatrol
// gateway is wired against the real tools during hardware bring-up (Apple
// Silicon), so this file stays free of invented tool invocations.
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
	return services.Status()
}

func (services realServices) Status() ([]ServiceStatus, error) {
	specs := desiredServices()
	statuses := make([]ServiceStatus, 0, len(specs))
	for _, service := range specs {
		statuses = append(statuses, ServiceStatus{
			Name:   service.Name,
			Mode:   service.Mode,
			State:  "unknown",
			Detail: "live status wired during hardware bring-up",
		})
	}
	return statuses, nil
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
		GOOS:     goos,
		GOARCH:   goarch,
		Prober:   prober,
		Now:      now,
		Services: realServices{prober: prober},
		CA:       realCA{},
	}
}
