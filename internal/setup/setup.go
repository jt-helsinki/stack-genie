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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/doctor"
	"github.com/jt-helsinki/ideal-robot/internal/layout"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/templates"
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

// Services reconciles, reports, and controls the host services (LiteLLM + Ollama
// containers, ClawPatrol native gateway). Implementations shell out to the
// runtime/OS.
type Services interface {
	// Reconcile makes reality match the desired state (idempotent).
	Reconcile(providerConfig string) ([]ServiceStatus, error)
	// Status reports current health without mutating anything.
	Status() ([]ServiceStatus, error)
	// Control performs a lifecycle action (start|stop|restart) on one service,
	// or all of them when service is "". Returns the resulting statuses.
	Control(action, service string) ([]ServiceStatus, error)
}

// CA ensures the ClawPatrol TLS-interception CA exists (arch §17). The root is
// installed into each workspace at workspace start (M5), not here.
type CA interface {
	Ensure() error
}

// DepInstaller installs a missing host dependency by binary name (msb,
// clawpatrol) via the tool's official installer. Injectable for tests.
type DepInstaller interface {
	Install(binary string) error
}

// installableDeps are the host programs `ai setup` auto-installs when absent
// (detect-if-installed), each with its official one-line installer. Docker/Podman
// are intentionally NOT here — too heavy and a user choice; they stay a prereq.
var installableDeps = []struct{ Binary, URL string }{
	{"msb", "https://install.microsandbox.dev"},
	{"clawpatrol", "https://clawpatrol.dev/install.sh"},
}

// installerURL returns the official installer URL for a known dependency binary.
func installerURL(binary string) (string, bool) {
	for _, dep := range installableDeps {
		if dep.Binary == binary {
			return dep.URL, true
		}
	}
	return "", false
}

// ensureDependencies installs the auto-installable host programs that are not yet
// on PATH (msb, ClawPatrol), skipping any already present. Best-effort: an
// installer failure or a not-yet-on-PATH binary becomes a note, not a hard fail —
// the prerequisite scan still gates anything truly missing.
func ensureDependencies(deps Deps) []string {
	var notes []string
	for _, dep := range installableDeps {
		if _, err := deps.Prober.LookPath(dep.Binary); err == nil {
			continue // already installed
		}
		if deps.DepInstaller == nil {
			continue
		}
		if err := deps.DepInstaller.Install(dep.Binary); err != nil {
			notes = append(notes, fmt.Sprintf("could not auto-install %s: %s", dep.Binary, err))
			continue
		}
		if _, err := deps.Prober.LookPath(dep.Binary); err == nil {
			notes = append(notes, "installed "+dep.Binary)
		} else {
			notes = append(notes, dep.Binary+" installed — ensure its bin dir is on PATH, then re-run `ai setup`")
		}
	}
	return notes
}

// Deps are the injectable dependencies of Run / ServicesStatus.
type Deps struct {
	GOOS, GOARCH string
	Prober       runtime.Prober
	Now          func() string // RFC 3339 UTC timestamp
	Services     Services
	CA           CA
	// DepInstaller installs missing auto-installable host programs (msb, ClawPatrol).
	DepInstaller DepInstaller
	// GatewayConfigFetcher downloads the ClawPatrol gateway example HCL (seeded
	// into ~/.clawpatrol/gateway.hcl on first setup). Injectable for tests.
	GatewayConfigFetcher func() ([]byte, error)
}

// Options configure a setup run.
type Options struct {
	ProviderConfig string // --provider-config: points LiteLLM at a provider config
	Upgrade        bool   // --upgrade: re-pin versions.json to this binary's defaults
}

// controlActions are the valid `ai services <action>` verbs.
var controlActions = map[string]bool{"start": true, "stop": true, "restart": true}

// ServiceNames returns the names of the desired host services (for validation
// and shell completion).
func ServiceNames() []string {
	specs := desiredServices()
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

// ControlService backs `ai services start|stop|restart [service]`: it validates
// the action and service (empty service = all) then delegates to the Services
// implementation. Invalid action/service → exit 2.
func ControlService(deps Deps, action, service string) ([]ServiceStatus, error) {
	if !controlActions[action] {
		return nil, output.Errorf(output.ExitInvalidInput, "unknown action %q (start|stop|restart)", action)
	}
	if service != "" && !slices.Contains(ServiceNames(), service) {
		return nil, output.Errorf(output.ExitInvalidInput, "unknown service %q (one of %v)", service, ServiceNames())
	}
	return deps.Services.Control(action, service)
}

// Report is the result of a successful setup.
type Report struct {
	PlatformDir     string        `json:"platform_dir"`
	Runtime         *runtime.Info `json:"runtime"`
	ConfigCreated   bool          `json:"config_created"`
	VersionsCreated bool          `json:"versions_created"`
	CAReady         bool          `json:"ca_ready"`
	// GatewayConfigCreated is true when this run seeded ~/.clawpatrol/gateway.hcl.
	GatewayConfigCreated bool            `json:"gateway_config_created"`
	Services             []ServiceStatus `json:"services"`
	// Warnings carries non-blocking prerequisite gaps (e.g. ClawPatrol not yet
	// installed). Surfaced via the envelope's warnings, not the data payload.
	Warnings []string `json:"-"`
}

// gatewayConfigURL is the upstream ClawPatrol gateway example, seeded into
// ~/.clawpatrol/gateway.hcl on first setup (clawpatrol.dev/docs/configure-gateway).
const gatewayConfigURL = "https://raw.githubusercontent.com/denoland/clawpatrol/refs/heads/main/examples/gateway.example.hcl"

// gateway HCL assignments rewritten with sensible local defaults (only the
// assignment lines, never the comment references).
var (
	stateDirAssignment        = regexp.MustCompile(`(?m)^(\s*state_dir\s*=\s*)"[^"]*"`)
	dashboardListenAssignment = regexp.MustCompile(`(?m)^(\s*dashboard_listen\s*=\s*)"[^"]*"`)
)

// dashboardListen is the platform default for the ClawPatrol dashboard — 8123 is
// less likely to collide with other apps than the upstream example's 8080.
const dashboardListen = "127.0.0.1:8123"

// ensureGatewayConfig seeds ~/.clawpatrol/gateway.hcl from the upstream example
// on first setup and applies sensible local defaults (state_dir → ~/.clawpatrol).
// It is idempotent: if the file already exists it is left untouched (no download,
// no rewrite — local edits are respected). A download failure is non-fatal and
// returned as a warning so setup does not hard-fail offline.
func ensureGatewayConfig(deps Deps) (created bool, warning string, err error) {
	dir, err := paths.ClawPatrolDir()
	if err != nil {
		return false, "", err
	}
	configPath := filepath.Join(dir, "gateway.hcl")
	if _, statErr := os.Stat(configPath); statErr == nil {
		return false, "", nil // exists — skip (don't download, don't overwrite)
	} else if !os.IsNotExist(statErr) {
		return false, "", statErr
	}

	contents, fetchErr := deps.GatewayConfigFetcher()
	if fetchErr != nil {
		return false, fmt.Sprintf("ClawPatrol gateway config not seeded: %s", fetchErr), nil
	}
	contents = stateDirAssignment.ReplaceAll(contents, []byte(`${1}"`+dir+`"`))
	contents = dashboardListenAssignment.ReplaceAll(contents, []byte(`${1}"`+dashboardListen+`"`))

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, "", err
	}
	if err := os.WriteFile(configPath, contents, 0o644); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// Human renders the setup result as a readable summary (non-JSON output).
func (report *Report) Human() string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Platform: %s\n", report.PlatformDir)
	if report.Runtime != nil {
		runtimeInfo := report.Runtime
		available := "unavailable"
		if runtimeInfo.Microsandbox.Available {
			available = "available"
		}
		fmt.Fprintf(&builder, "Runtime:  %s (rootless=%t) · microVM %s (%s)\n",
			runtimeInfo.Detected, runtimeInfo.Rootless, runtimeInfo.Microsandbox.Virtualization, available)
	}
	fmt.Fprintf(&builder, "State:    config=%t versions=%t ca=%t gateway=%t\n",
		report.ConfigCreated, report.VersionsCreated, report.CAReady, report.GatewayConfigCreated)
	builder.WriteString("Services:\n")
	for _, service := range report.Services {
		line := fmt.Sprintf("  %-11s %-9s %s", service.Name, service.Mode, service.State)
		if service.Detail != "" {
			line += " — " + service.Detail
		}
		builder.WriteString(line + "\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

// Prerequisite is one external dependency `ai setup` needs. Blocking ones must be
// satisfied before setup can proceed; non-blocking ones are surfaced as warnings.
type Prerequisite struct {
	Name       string `json:"name"`
	Detail     string `json:"detail,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
	Blocking   bool   `json:"blocking"`
}

// blockingPrereqs are the checks that must pass before setup proceeds. ClawPatrol
// is intentionally non-blocking (its integration is wired during hardware
// bring-up); a missing container runtime / Microsandbox / virtualization /
// rootless posture stops setup.
var blockingPrereqs = map[string]bool{
	"container runtime":     true,
	"microsandbox runtime":  true,
	"host virtualization":   true,
	"rootless service tier": true,
}

// installablePrograms are the prerequisites whose absence is a missing dependency
// (exit 3) rather than a host-capability failure (exit 4).
var installablePrograms = map[string]bool{
	"container runtime":    true,
	"microsandbox runtime": true,
}

// missingPrerequisites scans the host (reusing the doctor checks) and returns the
// unmet prerequisites, each with an install/repair suggestion. LiteLLM is excluded
// — it is a service setup starts, not a prerequisite.
func missingPrerequisites(deps Deps) []Prerequisite {
	report := doctor.Run(doctor.Deps{GOOS: deps.GOOS, GOARCH: deps.GOARCH, Prober: deps.Prober})
	var missing []Prerequisite
	for _, check := range report.Checks {
		if check.Name == "litellm" || check.Status != doctor.StatusError {
			continue
		}
		missing = append(missing, Prerequisite{
			Name:       check.Name,
			Detail:     check.Detail,
			Suggestion: check.Suggestion,
			Blocking:   blockingPrereqs[check.Name],
		})
	}
	return missing
}

// prerequisiteError formats every missing prerequisite into one actionable error
// (the human message lists each with its install command; the structured list
// rides in error.details). The exit code is 3 when an installable program is
// missing, else 4 (a host-capability shortfall).
func prerequisiteError(missing []Prerequisite) *output.Error {
	code := output.ExitRuntimeFailure
	for _, prereq := range missing {
		if installablePrograms[prereq.Name] {
			code = output.ExitMissingDep
			break
		}
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "preflight: %d prerequisite(s) not satisfied:", len(missing))
	for _, prereq := range missing {
		fmt.Fprintf(&builder, "\n  - %s: %s", prereq.Name, prereq.Detail)
		if prereq.Suggestion != "" {
			fmt.Fprintf(&builder, "\n      → %s", prereq.Suggestion)
		}
	}
	return output.Errorf(code, "%s", builder.String()).WithDetails(missing)
}

// Run performs `ai setup`. Returned errors are *output.Error carrying the exit
// code (§18): missing deps → 3, capability/other failures → 4. Idempotent.
func Run(options Options, deps Deps) (*Report, error) {
	// 1. Preflight: scan every prerequisite and report all missing ones at once,
	// each with an install command. Blocking gaps stop setup (exit 3 if a program
	// is missing, else 4); non-blocking gaps (e.g. ClawPatrol) become warnings.
	// Auto-install the host programs we can (msb, ClawPatrol) before the scan, so
	// a fresh host doesn't fail preflight just for a missing installable dep.
	warnings := ensureDependencies(deps)

	missing := missingPrerequisites(deps)
	for _, prereq := range missing {
		if prereq.Blocking {
			return nil, prerequisiteError(missing)
		}
		warning := prereq.Name + ": " + prereq.Detail
		if prereq.Suggestion != "" {
			warning += " — " + prereq.Suggestion
		}
		warnings = append(warnings, warning)
	}

	detected, err := runtime.Detect(deps.GOOS, deps.GOARCH, deps.Prober, deps.Now())
	if err != nil {
		// Defensive: the prerequisite scan should already have caught this.
		return nil, output.Errorf(output.ExitMissingDep, "preflight: %s", err)
	}

	// 2. Initialize the host layout and install the environment templates.
	platformDir, err := layout.Ensure()
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "init layout: %s", err)
	}
	if err := templates.Install(); err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "install templates: %s", err)
	}

	// 3. Persist the detected runtime.
	if err := runtime.Persist(detected); err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "write runtime.json: %s", err)
	}

	// 4. Defaults: global config + pinned versions (idempotent).
	configCreated, err := config.EnsureGlobalDefault()
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "write config.yaml: %s", err)
	}
	// --upgrade re-pins versions.json to this binary's defaults (overwrite);
	// otherwise the pins are written only when absent.
	var versionsCreated bool
	if options.Upgrade {
		if err := versions.WriteDefault(); err != nil {
			return nil, output.Errorf(output.ExitRuntimeFailure, "upgrade versions.json: %s", err)
		}
		versionsCreated = true
	} else if versionsCreated, err = versions.EnsureDefault(); err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "write versions.json: %s", err)
	}

	// 5. ClawPatrol TLS-interception CA (arch §17).
	if err := deps.CA.Ensure(); err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "ensure ClawPatrol CA: %s", err)
	}

	// 5b. Seed ~/.clawpatrol/gateway.hcl from the upstream example (first run only).
	gatewayCreated, gatewayWarning, err := ensureGatewayConfig(deps)
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "ensure ClawPatrol gateway config: %s", err)
	}
	if gatewayWarning != "" {
		warnings = append(warnings, gatewayWarning)
	}

	// 6. Reconcile host services to the desired state.
	serviceStatuses, err := deps.Services.Reconcile(options.ProviderConfig)
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "reconcile services: %s", err)
	}

	return &Report{
		PlatformDir:          platformDir,
		Runtime:              detected,
		ConfigCreated:        configCreated,
		VersionsCreated:      versionsCreated,
		CAReady:              true,
		GatewayConfigCreated: gatewayCreated,
		Services:             serviceStatuses,
		Warnings:             warnings,
	}, nil
}

// ServicesStatus backs `ai services status`: the workspace runtime (from the
// persisted runtime.json) plus each host service's current health.
func ServicesStatus(deps Deps) ([]ServiceStatus, error) {
	statuses := []ServiceStatus{}

	detected, err := runtime.Load()
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "load runtime.json: %s", err)
	}
	if detected != nil {
		state := "unavailable"
		if detected.Microsandbox.Available {
			state = "ready"
		}
		statuses = append(statuses, ServiceStatus{
			Name: "microsandbox", Mode: "runtime", State: state,
			Healthy: detected.Microsandbox.Available, Detail: detected.Microsandbox.Virtualization,
		})
	}

	serviceStatuses, err := deps.Services.Status()
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "service status: %s", err)
	}
	return append(statuses, serviceStatuses...), nil
}
