package setup

import (
	"os"
	"path/filepath"

	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// desiredServices is the Slice 1 host-service set (arch §5). Headroom is added in
// Slice 2; Ollama only when enabled.
var desiredServices = []struct{ Name, Mode string }{
	{"litellm", "container"},
	{"clawpatrol", "native"},
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

func (s realServices) Reconcile(_ string) ([]ServiceStatus, error) {
	cfg, err := paths.ConfigDir()
	if err != nil {
		return nil, err
	}
	for _, svc := range desiredServices {
		if err := os.MkdirAll(filepath.Join(cfg, svc.Name), 0o755); err != nil {
			return nil, err
		}
	}
	return s.Status()
}

func (s realServices) Status() ([]ServiceStatus, error) {
	out := make([]ServiceStatus, 0, len(desiredServices))
	for _, svc := range desiredServices {
		out = append(out, ServiceStatus{
			Name:   svc.Name,
			Mode:   svc.Mode,
			State:  "unknown",
			Detail: "live status wired during hardware bring-up",
		})
	}
	return out, nil
}

// realCA prepares the ClawPatrol CA location (arch §17). Generating the CA
// certificate is delegated to ClawPatrol on a provisioned host.
type realCA struct{}

func (realCA) Ensure() error {
	cfg, err := paths.ConfigDir()
	if err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(cfg, "clawpatrol", "ca"), 0o755)
}

// RealDeps builds Deps wired to the actual host (used by the CLI).
func RealDeps(goos, goarch string, now func() string) Deps {
	p := runtime.RealProber()
	return Deps{
		GOOS:     goos,
		GOARCH:   goarch,
		Prober:   p,
		Now:      now,
		Services: realServices{prober: p},
		CA:       realCA{},
	}
}
