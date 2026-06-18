// Package setup implements `ai setup` (CLI §2.1, arch §5): the single,
// idempotent control-plane action that preflights the host, initializes
// ~/.ai-platform, persists the detected runtime, writes default config/versions,
// ensures the ClawPatrol CA, and reconciles the host services.
//
// External-tool actions (starting services, generating the CA) are behind the
// Services and CA interfaces so the orchestration is unit-tested with fakes and
// the real impls (setup_real.go) run on a provisioned host.
package setup

import (
	"errors"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/layout"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/versions"
)

// ServiceStatus describes one managed service (or the workspace runtime).
type ServiceStatus struct {
	Name    string `json:"name"`
	Mode    string `json:"mode"`  // container | native | runtime
	State   string `json:"state"` // running | stopped | not_installed | unavailable | unknown
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail,omitempty"`
}

// Services reconciles and reports the host services (LiteLLM container, ClawPatrol
// native gateway, optional Ollama). Implementations shell out to the runtime/OS.
type Services interface {
	// Reconcile makes reality match the desired state (idempotent).
	Reconcile(providerConfig string) ([]ServiceStatus, error)
	// Status reports current health without mutating anything.
	Status() ([]ServiceStatus, error)
}

// CA ensures the ClawPatrol TLS-interception CA exists (arch §17). The root is
// installed into each workspace at workspace start (M5), not here.
type CA interface {
	Ensure() error
}

// Deps are the injectable dependencies of Run / ServicesStatus.
type Deps struct {
	GOOS, GOARCH string
	Prober       runtime.Prober
	Now          func() string // RFC 3339 UTC timestamp
	Services     Services
	CA           CA
}

// Options configure a setup run.
type Options struct {
	ProviderConfig string // --provider-config: points LiteLLM at a provider config
}

// Report is the result of a successful setup.
type Report struct {
	PlatformDir     string          `json:"platform_dir"`
	Runtime         *runtime.Info   `json:"runtime"`
	ConfigCreated   bool            `json:"config_created"`
	VersionsCreated bool            `json:"versions_created"`
	CAReady         bool            `json:"ca_ready"`
	Services        []ServiceStatus `json:"services"`
}

// Run performs `ai setup`. Returned errors are *output.Error carrying the exit
// code (§18): missing deps → 3, capability/other failures → 4. Idempotent.
func Run(opts Options, d Deps) (*Report, error) {
	// 1. Preflight: detect deps (exit 3) then verify capabilities (exit 4).
	info, err := runtime.Detect(d.GOOS, d.GOARCH, d.Prober, d.Now())
	if err != nil {
		if errors.Is(err, runtime.ErrNoContainerRuntime) || errors.Is(err, runtime.ErrMsbMissing) {
			return nil, output.Errorf(output.ExitMissingDep, "preflight: %s", err)
		}
		return nil, output.Errorf(output.ExitRuntimeFailure, "preflight: %s", err)
	}
	if err := runtime.Verify(info); err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "preflight: %s", err)
	}

	// 2. Initialize the host layout.
	root, err := layout.Ensure()
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "init layout: %s", err)
	}

	// 3. Persist the detected runtime.
	if err := runtime.Persist(info); err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "write runtime.json: %s", err)
	}

	// 4. Defaults: global config + pinned versions (idempotent).
	cfgCreated, err := config.EnsureGlobalDefault()
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "write config.yaml: %s", err)
	}
	verCreated, err := versions.EnsureDefault()
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "write versions.json: %s", err)
	}

	// 5. ClawPatrol TLS-interception CA (arch §17).
	if err := d.CA.Ensure(); err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "ensure ClawPatrol CA: %s", err)
	}

	// 6. Reconcile host services to the desired state.
	svcs, err := d.Services.Reconcile(opts.ProviderConfig)
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "reconcile services: %s", err)
	}

	return &Report{
		PlatformDir:     root,
		Runtime:         info,
		ConfigCreated:   cfgCreated,
		VersionsCreated: verCreated,
		CAReady:         true,
		Services:        svcs,
	}, nil
}

// ServicesStatus backs `ai services status`: the workspace runtime (from the
// persisted runtime.json) plus each host service's current health.
func ServicesStatus(d Deps) ([]ServiceStatus, error) {
	out := []ServiceStatus{}

	info, err := runtime.Load()
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "load runtime.json: %s", err)
	}
	if info != nil {
		state := "unavailable"
		if info.Microsandbox.Available {
			state = "ready"
		}
		out = append(out, ServiceStatus{
			Name: "microsandbox", Mode: "runtime", State: state,
			Healthy: info.Microsandbox.Available, Detail: info.Microsandbox.Virtualization,
		})
	}

	svcs, err := d.Services.Status()
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "service status: %s", err)
	}
	return append(out, svcs...), nil
}
