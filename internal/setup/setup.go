// Package setup implements `ai setup` (CLI §2.1, arch §5): the single,
// idempotent control-plane action that preflights the host, initializes
// ~/.ai-platform, persists the detected runtime, writes default config/versions,
// and reconciles the host services.
//
// External-tool actions (starting services) are behind the Services interface so
// the orchestration is unit-tested with fakes and the real impl (setup_real.go)
// runs on a provisioned host.
package setup

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/console"
	"github.com/jt-helsinki/ideal-robot/internal/doctor"
	"github.com/jt-helsinki/ideal-robot/internal/layout"
	"github.com/jt-helsinki/ideal-robot/internal/output"
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
	// Address is the host-reachable URL/host:port the user can hit (from the
	// console registry); Console is the admin-UI URL when the service has one.
	// Both empty for services that publish nothing to the host (e.g. Presidio,
	// the microVM runtime).
	Address string `json:"address,omitempty"`
	Console string `json:"console,omitempty"`
}

// EndpointSuffix renders the host address and, when present, the admin-console
// hint for a service line — "  <address> · UI <console>", or "" when the service
// publishes nothing to the host. Shared by the `ai setup` and `ai services`
// renderers so the format lives in one place.
func (status ServiceStatus) EndpointSuffix() string {
	if status.Address == "" {
		return ""
	}
	suffix := "  " + status.Address
	if status.Console != "" {
		suffix += " · UI " + status.Console
	}
	return suffix
}

// Services reconciles, reports, and controls the host services (the LiteLLM,
// Ollama, Presidio, and Headroom containers). Implementations shell out to the
// runtime/OS.
type Services interface {
	// Reconcile makes reality match the desired state (idempotent). bindHost is
	// the host interface the shared services publish on ("127.0.0.1" for a local
	// standalone host, "0.0.0.0" for a server other machines connect to). optional
	// is the set of enabled opt-in services (e.g. ["open-webui"]) — CORE services
	// are always reconciled, optional ones only when present here. progress is
	// called (never nil) with a short message before each step, so the CLI can
	// stream feedback during the slow container bring-up.
	Reconcile(providerConfig string, bindHost string, optional []string, progress func(string)) ([]ServiceStatus, error)
	// PullImages pre-pulls every service-tier image that is not already present
	// locally, streaming the runtime's native pull progress to out (so a multi-GB
	// first-run pull does not look hung behind a captured `docker run`). enabled is
	// the set of opt-in optional services to include — core images are always
	// pulled, but a disabled optional service's (potentially large) images are
	// skipped. progress is called (never nil) with a short message before each
	// pull. Already-present images are skipped. Best-effort: a pull error is
	// returned but is non-fatal — the subsequent `docker run` re-pulls anything
	// still missing.
	PullImages(enabled []string, out io.Writer, progress func(string)) error
	// Status reports current health without mutating anything.
	Status() ([]ServiceStatus, error)
	// Control performs a lifecycle action (start|stop|restart) on one service,
	// or all of them when service is "". Returns the resulting statuses.
	Control(action, service string) ([]ServiceStatus, error)
	// InstallPrerequisite runs the prerequisite's auto-installer (prereq.InstallCommand,
	// a fixed per-OS command from the platform's own table), streaming the installer's
	// native stdout/stderr to out so the user sees its progress. Only valid when
	// prereq.InstallCommand != ""; an empty command is a programmer error.
	InstallPrerequisite(prereq Prerequisite, out io.Writer) error
}

// `ai setup` never installs host software. Missing prerequisites are detected
// (MissingPrerequisites) and reported with install instructions + a web address
// for the user to install themselves and re-run (PrerequisiteError).

// Deps are the injectable dependencies of Run / ServicesStatus.
type Deps struct {
	GOOS, GOARCH string
	Prober       runtime.Prober
	Now          func() string // RFC 3339 UTC timestamp
	Services     Services
	// Progress, when set, is called with a short human message before each setup
	// step so the CLI can stream feedback (the work is otherwise silent for the
	// several seconds it takes to launch + health-check the containers). nil = no
	// progress (e.g. --json / tests).
	Progress func(string)
}

// Options configure a setup run.
type Options struct {
	ProviderConfig string // --provider-config: points LiteLLM at a provider config
	Upgrade        bool   // --upgrade: re-pin versions.yaml to this binary's defaults
	// Mode is the deployment role: "" (resolve from runtime.yaml, else standalone),
	// standalone, server, or client (CLI §2.1).
	Mode string
	// ServerAddr is the remote service-tier address a client routes to (CLI §2.1).
	// Ignored for standalone/server.
	ServerAddr string
	// Optional is the enabled opt-in service set for this run (e.g.
	// ["open-webui"]). nil means "unspecified" — Run falls back to the persisted
	// runtime.yaml set, else the first-run default (DefaultOptionalServices). To
	// disable every optional service explicitly, set OptionalSet=true with an
	// empty Optional (the CLI's `--optional none`).
	Optional []string
	// OptionalSet distinguishes a deliberately-empty Optional ("none") from an
	// unspecified one (nil), so automation can disable every optional service.
	OptionalSet bool
}

// OptionalServiceNames returns the universe of opt-in service names (in
// declaration order) so the CLI can render the setup checkbox and validate the
// --optional flag without reaching into the unexported service table.
func OptionalServiceNames() []string {
	return optionalServiceNames()
}

// OptionalServiceLabel returns a short human label for an optional service, used
// in the setup checkbox (e.g. open-webui → "chat UI"). Unknown names get "".
func OptionalServiceLabel(name string) string {
	switch name {
	case "open-webui":
		return "chat UI"
	case "odysseus":
		return "AI workspace (mounts the Docker socket = full host-Docker control)"
	default:
		return ""
	}
}

// DefaultOptionalServices is the opt-in service set enabled on a FIRST run when
// the user makes no explicit choice — open-webui, preserving the historical
// always-on behavior. It is overridden by an explicit choice (the setup prompt /
// --optional flag) and, once persisted, by the runtime.yaml set.
func DefaultOptionalServices() []string {
	return []string{"open-webui"}
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
	// "all" is the explicit spelling of "no service = all services".
	if service == "all" {
		service = ""
	}
	if service != "" && !slices.Contains(ServiceNames(), service) {
		// A companion container (Odysseus's chromadb/searxng/ntfy) is managed as
		// part of its owning logical service, not on its own — point the user
		// there. (`ai logs --service <name>` can still tail that one container.)
		if owner := owningService(service); owner != "" {
			return nil, output.Errorf(output.ExitInvalidInput,
				"%q is managed as part of the %q service — run `ai services %s %s` (`ai logs --service %s` tails just that container)",
				service, owner, action, owner, service)
		}
		return nil, output.Errorf(output.ExitInvalidInput, "unknown service %q (one of %v, or \"all\")", service, ServiceNames())
	}
	return deps.Services.Control(action, service)
}

// Report is the result of a successful setup.
type Report struct {
	PlatformDir     string          `json:"platform_dir"`
	Runtime         *runtime.Info   `json:"runtime"`
	ConfigCreated   bool            `json:"config_created"`
	VersionsCreated bool            `json:"versions_created"`
	Services        []ServiceStatus `json:"services"`
	// Warnings carries non-blocking prerequisite gaps. Surfaced via the
	// envelope's warnings, not the data payload.
	Warnings []string `json:"-"`
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
	fmt.Fprintf(&builder, "State:    config.yaml %s · versions.yaml %s\n",
		presence(report.ConfigCreated), presence(report.VersionsCreated))
	builder.WriteString("Services:\n")
	for _, service := range report.Services {
		line := fmt.Sprintf("  %-11s %-9s %-9s", service.Name, service.Mode, service.State)
		if service.Detail != "" {
			line += " — " + service.Detail
		}
		line += service.EndpointSuffix()
		builder.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

// presence renders whether a default state file was created on this run or
// already existed, reporting that it is present either way.
func presence(created bool) string {
	if created {
		return "✓ created"
	}
	return "✓ present"
}

// Prerequisite is one external dependency `ai setup` needs. Blocking ones must be
// satisfied before setup can proceed; non-blocking ones are surfaced as warnings.
type Prerequisite struct {
	Name       string `json:"name"`
	Detail     string `json:"detail,omitempty"`
	Suggestion string `json:"suggestion,omitempty"` // how to install / repair
	DocsURL    string `json:"docs_url,omitempty"`   // web address for the software
	Blocking   bool   `json:"blocking"`
	// InstallCommand is the exact shell command that auto-installs this
	// prerequisite on this host's OS, or "" when there is no clean auto-installer
	// (e.g. Docker Desktop on macOS, or a host-capability gap like virtualization /
	// rootless posture). When "", the prerequisite is instruct-only — the CLI prints
	// Suggestion + DocsURL and exits rather than offering to install it.
	InstallCommand string `json:"install_command,omitempty"`
}

// installCommandFor returns the auto-install command for a prerequisite on the
// given OS, or "" when there is no clean auto-installer. The commands are the
// platform's own verified per-OS table — do not invent others:
//   - microsandbox runtime: `curl -sSL https://get.microsandbox.dev | sh` (macOS + Linux).
//   - container runtime: `curl -fsSL https://get.docker.com | sh` (Linux only; needs
//     sudo). On macOS Docker Desktop is a GUI app → no auto-installer ("").
//   - everything else (host virtualization, rootless service tier): host
//     capability/config → never auto-installable ("").
func installCommandFor(name, goos string) string {
	switch name {
	case "microsandbox runtime":
		if goos == "darwin" || goos == "linux" {
			return "curl -sSL https://get.microsandbox.dev | sh"
		}
		return ""
	case "container runtime":
		if goos == "linux" {
			return "curl -fsSL https://get.docker.com | sh"
		}
		return ""
	default:
		return ""
	}
}

// blockingPrereqs are the checks that must pass before setup proceeds: a missing
// container runtime / Microsandbox / virtualization / rootless posture stops
// setup.
var blockingPrereqs = map[string]bool{
	"container runtime":     true,
	"microsandbox runtime":  true,
	"host virtualization":   true,
	"rootless service tier": true,
}

// blockingPrereqsForRole returns the prerequisite names that must pass before
// setup proceeds for a given deployment role (CLI §2.1). A role only blocks on
// the tier it actually runs:
//   - standalone runs everything → the full set.
//   - server runs only the service tier → container runtime + rootless only
//     (no microVM runtime / virtualization).
//   - client runs only workspaces → microVM runtime + virtualization only
//     (no container runtime / rootless service tier).
//
// Prerequisites not in the returned set are non-blocking for the role (surfaced
// as warnings), so a missing one does not stop setup.
func blockingPrereqsForRole(role string) map[string]bool {
	switch role {
	case runtime.RoleServer:
		return map[string]bool{
			"container runtime":     true,
			"rootless service tier": true,
		}
	case runtime.RoleClient:
		return map[string]bool{
			"microsandbox runtime": true,
			"host virtualization":  true,
		}
	default: // standalone
		return blockingPrereqs
	}
}

// installablePrograms are the prerequisites whose absence is a missing dependency
// (exit 3) rather than a host-capability failure (exit 4).
var installablePrograms = map[string]bool{
	"container runtime":    true,
	"microsandbox runtime": true,
}

// MissingPrerequisites scans the host (reusing the doctor checks) and returns the
// unmet prerequisites, each with an install/repair suggestion and, where one
// exists for this OS, the auto-install command. LiteLLM is excluded — it is a
// service setup starts, not a prerequisite.
func MissingPrerequisites(deps Deps, role string) []Prerequisite {
	blocking := blockingPrereqsForRole(role)
	report := doctor.Run(doctor.Deps{GOOS: deps.GOOS, GOARCH: deps.GOARCH, Prober: deps.Prober})
	var missing []Prerequisite
	for _, check := range report.Checks {
		if check.Name == "litellm" || check.Status != doctor.StatusError {
			continue
		}
		missing = append(missing, Prerequisite{
			Name:           check.Name,
			Detail:         check.Detail,
			Suggestion:     check.Suggestion,
			DocsURL:        check.DocsURL,
			Blocking:       blocking[check.Name],
			InstallCommand: installCommandFor(check.Name, deps.GOOS),
		})
	}
	return missing
}

// PrerequisiteError formats every missing prerequisite into one actionable error
// (the human message lists each with its install command; the structured list
// rides in error.details). The exit code is 3 when an installable program is
// missing, else 4 (a host-capability shortfall).
func PrerequisiteError(missing []Prerequisite) *output.Error {
	code := output.ExitRuntimeFailure
	for _, prereq := range missing {
		if installablePrograms[prereq.Name] {
			code = output.ExitMissingDep
			break
		}
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "preflight: %d prerequisite(s) not satisfied — install the missing software and re-run `ai setup`:", len(missing))
	for _, prereq := range missing {
		fmt.Fprintf(&builder, "\n  ✗ %s — %s", prereq.Name, prereq.Detail)
		if prereq.Suggestion != "" {
			fmt.Fprintf(&builder, "\n      how to install: %s", prereq.Suggestion)
		}
		if prereq.DocsURL != "" {
			fmt.Fprintf(&builder, "\n      web:            %s", prereq.DocsURL)
		}
	}
	return output.Errorf(code, "%s", builder.String()).WithDetails(missing)
}

// RoleFor resolves the effective deployment role for a setup run (CLI §2.1),
// using the same precedence Run() applies: the explicit option (options.Mode),
// else the role persisted in runtime.yaml, else the default (standalone). A failed
// load is treated as "absent" — setup is the thing that writes runtime.yaml. An
// unrecognized role is an invalid-input error (exit 2).
func RoleFor(options Options) (string, error) {
	effectiveRole := options.Mode
	if effectiveRole == "" {
		if persisted, _ := runtime.Load(); persisted != nil {
			effectiveRole = persisted.Role
		}
	}
	if effectiveRole == "" {
		effectiveRole = runtime.RoleStandalone
	}
	switch effectiveRole {
	case runtime.RoleStandalone, runtime.RoleServer, runtime.RoleClient:
		return effectiveRole, nil
	default:
		return "", output.Errorf(output.ExitInvalidInput,
			"invalid setup mode %q (one of standalone|server|client)", effectiveRole)
	}
}

// ResolveOptional resolves the enabled optional-service set for a setup run,
// applying the precedence: an explicit choice (options.OptionalSet — the setup
// prompt or the --optional flag, including the empty "none" set) wins; else the
// set persisted in runtime.yaml; else the first-run default
// (DefaultOptionalServices). Unknown names are dropped so a stale persisted entry
// (a retired optional service) cannot break the reconcile. The returned slice is
// in optionalServices declaration order and de-duplicated.
func ResolveOptional(options Options, persisted *runtime.Info) []string {
	var chosen []string
	switch {
	case options.OptionalSet:
		chosen = options.Optional
	case persisted != nil && persisted.OptionalServices != nil:
		chosen = persisted.OptionalServices
	default:
		chosen = DefaultOptionalServices()
	}
	enabled := make([]string, 0, len(chosen))
	for _, name := range optionalServiceNames() {
		if slices.Contains(chosen, name) {
			enabled = append(enabled, name)
		}
	}
	return enabled
}

// Run performs `ai setup`. Returned errors are *output.Error carrying the exit
// code (§18): missing deps → 3, capability/other failures → 4. Idempotent.
func Run(options Options, deps Deps) (*Report, error) {
	// 1. Preflight: scan every prerequisite and report all missing ones at once,
	// each with install instructions + a web address. `ai setup` never installs
	// software itself — the user installs what's missing and re-runs. Blocking
	// gaps stop setup (exit 3 if a program is missing, else 4); non-blocking gaps
	// become warnings.
	var warnings []string

	// progress is never nil so callees can call it directly.
	progress := deps.Progress
	if progress == nil {
		progress = func(string) {}
	}

	// 0. Resolve the effective deployment role + server address (CLI §2.1).
	persisted, _ := runtime.Load()
	effectiveRole, err := RoleFor(options)
	if err != nil {
		return nil, err
	}
	effectiveServerAddr := options.ServerAddr
	if effectiveServerAddr == "" && persisted != nil {
		effectiveServerAddr = persisted.AIPlatformHost
	}
	if effectiveRole != runtime.RoleClient {
		effectiveServerAddr = "" // only a client routes to a remote server
	}

	progress("Checking prerequisites…")
	missing := MissingPrerequisites(deps, effectiveRole)
	for _, prereq := range missing {
		if prereq.Blocking {
			return nil, PrerequisiteError(missing)
		}
		warning := prereq.Name + ": " + prereq.Detail
		if prereq.Suggestion != "" {
			warning += " — " + prereq.Suggestion
		}
		warnings = append(warnings, warning)
	}

	detected, err := runtime.DetectForRole(deps.GOOS, deps.GOARCH, effectiveRole, deps.Prober, deps.Now())
	if err != nil {
		// Defensive: the prerequisite scan should already have caught this.
		return nil, output.Errorf(output.ExitMissingDep, "preflight: %s", err)
	}
	// Persist the resolved role + remote server address so they survive across
	// runs and are available to later steps (e.g. `ai gateway`, workspace wiring).
	detected.Role = effectiveRole
	detected.AIPlatformHost = effectiveServerAddr
	// Resolve + persist the enabled optional-service set (explicit choice, else
	// the persisted set, else the first-run default) so it survives across runs
	// and Status can report not-enabled optional services as "disabled".
	optional := ResolveOptional(options, persisted)
	detected.OptionalServices = optional

	// 2. Initialize the host layout and install the environment templates.
	progress("Initializing ~/.ai-platform and installing templates…")
	platformDir, err := layout.Ensure()
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "init layout: %s", err)
	}
	if err := templates.Install(); err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "install templates: %s", err)
	}

	// 3. Persist the detected runtime.
	if err := runtime.Persist(detected); err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "write runtime.yaml: %s", err)
	}

	// 4. Defaults: global config + pinned versions (idempotent).
	configCreated, err := config.EnsureGlobalDefault()
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "write config.yaml: %s", err)
	}
	// --upgrade re-pins versions.yaml to this binary's defaults (overwrite);
	// otherwise the pins are written only when absent.
	var versionsCreated bool
	if options.Upgrade {
		if err := versions.WriteDefault(); err != nil {
			return nil, output.Errorf(output.ExitRuntimeFailure, "upgrade versions.yaml: %s", err)
		}
		versionsCreated = true
	} else if versionsCreated, err = versions.EnsureDefault(); err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "write versions.yaml: %s", err)
	}

	// 5. Reconcile host services to the desired state — but only when this host
	// runs the service tier. A client runs no local Docker tier: it routes to the
	// configured server, so the reconcile is skipped (the layout/config/versions
	// above are still useful on a client). standalone binds the shared services to
	// loopback (local-only); server binds them to 0.0.0.0 so other machines connect.
	var serviceStatuses []ServiceStatus
	switch effectiveRole {
	case runtime.RoleClient:
		note := fmt.Sprintf("client mode: services run on the configured server (%s) — run `ai gateway show` to verify", effectiveServerAddr)
		progress(note)
		warnings = append(warnings, note)
	default:
		bindHost := "127.0.0.1"
		if effectiveRole == runtime.RoleServer {
			bindHost = "0.0.0.0"
			warnings = append(warnings, "server mode exposes LiteLLM/Headroom/Ollama/open-webui on 0.0.0.0 — put TLS in front and rely on LiteLLM virtual-key auth for untrusted networks")
		}
		progress("Starting host services — pulling images / launching containers (this can take a minute)…")
		serviceStatuses, err = deps.Services.Reconcile(options.ProviderConfig, bindHost, optional, progress)
		if err != nil {
			return nil, output.Errorf(output.ExitRuntimeFailure, "reconcile services: %s", err)
		}
		progress("Host services ready.")
	}

	return &Report{
		PlatformDir:     platformDir,
		Runtime:         detected,
		ConfigCreated:   configCreated,
		VersionsCreated: versionsCreated,
		Services:        serviceStatuses,
		Warnings:        warnings,
	}, nil
}

// ServicesStatus backs `ai services status`: the workspace runtime (from the
// persisted runtime.yaml) plus each host service's current health.
func ServicesStatus(deps Deps) ([]ServiceStatus, error) {
	statuses := []ServiceStatus{}

	detected, err := runtime.Load()
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "load runtime.yaml: %s", err)
	}
	if detected != nil {
		state := "unavailable"
		if detected.Microsandbox.Available {
			state = "ready"
		}
		endpoint, _ := console.EndpointFor("microsandbox")
		statuses = append(statuses, ServiceStatus{
			Name: "microsandbox", Mode: "runtime", State: state,
			Healthy: detected.Microsandbox.Available, Detail: detected.Microsandbox.Virtualization,
			Address: endpoint.Address, Console: endpoint.Console,
		})
	}

	serviceStatuses, err := deps.Services.Status()
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "service status: %s", err)
	}
	return append(statuses, serviceStatuses...), nil
}
