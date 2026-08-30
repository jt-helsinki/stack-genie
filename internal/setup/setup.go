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
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/console"
	"github.com/jt-helsinki/stack-genie/internal/doctor"
	"github.com/jt-helsinki/stack-genie/internal/layout"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/templates"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/jt-helsinki/stack-genie/internal/versions"
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
	// Optional marks an opt-in service that can be enabled
	// or disabled (`ai services enable|disable`); core services are always on. A
	// disabled optional service has State "disabled".
	Optional bool `json:"optional,omitempty"`
	// Models is the per-model config for a host-native, per-model-process service
	// (currently only vllm) — empty for every container service. It surfaces here
	// (rather than as generic ContainerStats — vLLM has no container to `docker
	// inspect`/`stats`) so the Services detail pane can show each model's
	// resolved serving config alongside the summary.
	Models []VLLMModelInfo `json:"models,omitempty"`
}

// VLLMModelInfo is one recorded vLLM model's resolved serving config, surfaced on
// ServiceStatus.Models. GPUMemoryUtilization/MaxModelLen are 0 when unset (vLLM's
// own defaults are in effect — see vllm.ServeOptions).
type VLLMModelInfo struct {
	Alias                string  `json:"alias"`
	Model                string  `json:"model"`
	Endpoint             string  `json:"endpoint,omitempty"`
	Healthy              bool    `json:"healthy"`
	GPUMemoryUtilization float64 `json:"gpu_memory_utilization,omitempty"`
	MaxModelLen          int     `json:"max_model_len,omitempty"`
	// Disabled mirrors config.ModelRuntimeChoice.Disabled — this model is
	// intentionally excluded from ensureVLLMServers' auto-start pass (`ai models
	// disable`), so a not-Healthy disabled model is expected, not "down".
	Disabled bool `json:"disabled,omitempty"`
}

// Enabled reports whether the service is currently enabled — true for every core
// service and for an optional service that is not in the "disabled" state. It lets
// the UI gate start/stop/restart (only enabled services) vs enable/disable (only
// optional ones).
func (status ServiceStatus) Enabled() bool { return !status.Optional || status.State != "disabled" }

// EndpointSuffix renders the host address and, when present, the admin-console
// hint for a service line — "  <address> · UI <console>", or "" when the service
// publishes nothing to the host. Shared by the `ai setup` and `ai services`
// renderers so the format lives in one place.
func (status ServiceStatus) EndpointSuffix() string {
	if status.Address == "" {
		return ""
	}
	suffix := "  " + ui.Value.Render(status.Address)
	if status.Console != "" {
		suffix += " · " + ui.Label.Render("UI") + " " + ui.Value.Render(status.Console)
	}
	return suffix
}

// Services reconciles, reports, and controls the host services (the LiteLLM,
// Presidio, and Headroom containers). Implementations shell out to the
// runtime/OS.
type Services interface {
	// Reconcile makes reality match the desired state (idempotent). bindHost is
	// the host interface the shared services publish on ("127.0.0.1" for a local
	// standalone host, "0.0.0.0" for a server other machines connect to). optional
	// is the set of enabled opt-in services (currently always empty — there are no
	// optional host services) — CORE services are always reconciled, optional ones
	// only when present here. progress is
	// called (never nil) with a short message before each step, so the CLI can
	// stream feedback during the slow container bring-up.
	Reconcile(providerConfig string, bindHost string, optional []string, progress func(string)) ([]ServiceStatus, error)
	// PullImages pre-pulls every service-tier image that is not already present
	// locally, streaming the runtime's native pull progress to out (so a multi-GB
	// first-run pull does not look hung behind a captured `docker run`). optional is
	// the set of opt-in optional services to include and guardrails the enabled
	// LiteLLM guardrail set — core images are always pulled, but a disabled optional
	// service's images (and the Presidio images when the secret-masking guardrail is
	// off) are skipped. progress is called (never nil) with a short message before
	// each pull. Already-present images are skipped. Best-effort: a pull error is
	// returned but is non-fatal — the subsequent `docker run` re-pulls anything still
	// missing.
	PullImages(optional []string, guardrails []string, out io.Writer, progress func(string)) error
	// UpdateImages force-pulls the latest service-tier images (UNLIKE PullImages it
	// does NOT skip already-present images — it re-pulls so a moved tag like
	// `latest` is updated), streaming native progress to out. optional + guardrails
	// gate the image set exactly like PullImages (an unselected Presidio guardrail
	// skips its images). It backs `ai services update`: after it pulls, the caller
	// restarts the affected services to recreate their containers against the
	// freshly-pulled images. Best-effort: returns the first pull error (non-fatal).
	UpdateImages(optional []string, guardrails []string, out io.Writer, progress func(string)) error
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
	// CaptureServiceLogs snapshots each RUNNING service container's recent output
	// into ~/.ai-platform/logs/<container>.log, so `ai logs` / the TUI Logs view
	// show real container output (arch §2.2). It is a point-in-time snapshot via the
	// container runtime; best-effort (per-container errors are skipped, a missing
	// runtime is a no-op).
	CaptureServiceLogs() error
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
	// FetchCatalog, when set, fetches the models.dev model catalog and persists it
	// (catalog.LoadOrFetch + Save), so the saved copy is current before the CLI
	// offers cloud-provider keys and the initial model sync runs. It is BEST-EFFORT
	// — Run calls it after the service reconcile (non-client roles) and never fails
	// setup on its error. nil = skip (e.g. tests that don't exercise the catalog).
	//
	// hardware bring-up: the LIVE models.dev fetch runs only against the network.
	FetchCatalog func() error
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
	// Domain is the platform base domain the nginx UI subdomains hang off
	// (litellm.<domain>). Empty means "leave the
	// persisted/default domain untouched" — Run does a load-modify-save so a domain
	// set via `ai domain` is preserved. The server role prompts for it at `ai setup`
	// (defaulting to localhost); standalone keeps the aip.local default.
	Domain string
	// Optional is the enabled opt-in service set for this run. nil means
	// "unspecified" — Run falls back to the persisted
	// runtime.yaml set, else the first-run default (DefaultOptionalServices). To
	// disable every optional service explicitly, set OptionalSet=true with an
	// empty Optional (the CLI's `--optional none`).
	Optional []string
	// OptionalSet distinguishes a deliberately-empty Optional ("none") from an
	// unspecified one (nil), so automation can disable every optional service.
	OptionalSet bool
	// Guardrails is the enabled LiteLLM guardrail set for this run (keys from
	// litellm.GuardrailKeys). nil means "unspecified" — Run falls back to the persisted
	// runtime.yaml set, else the default (litellm.DefaultGuardrails — Headroom only).
	// The `ai setup` picker collects it (standalone/server); a client runs no gateway.
	Guardrails []string
	// GuardrailsSet distinguishes a deliberately-empty Guardrails (every guardrail off)
	// from an unspecified one (nil).
	GuardrailsSet bool
}

// OptionalServiceNames returns the universe of opt-in service names (in
// declaration order) so the CLI can render the setup checkbox and validate the
// --optional flag without reaching into the unexported service table.
func OptionalServiceNames() []string {
	return optionalServiceNames()
}

// OptionalServiceLabel returns a short human label for an optional service, used
// in the setup checkbox. Unknown names get "". There are currently NO optional host
// services (Open WebUI moved to a per-workspace in-VM app and Odysseus was
// removed), so this returns "" for everything today.
func OptionalServiceLabel(name string) string {
	_ = name
	return ""
}

// DefaultOptionalServices is the opt-in service set enabled on a FIRST run when
// the user makes no explicit choice. There are currently NO optional host services,
// so it is empty (the optional mechanism is retained for future use).
func DefaultOptionalServices() []string {
	return nil
}

// controlActions are the valid `ai services <action>` verbs.
var controlActions = map[string]bool{"start": true, "stop": true, "restart": true}

// ServiceNames returns the names of the addressable host services (for validation
// and shell completion): the container-reconcile set (desiredServices) PLUS the
// host-native runtimes (hostNativeServiceNames — vllm) that are NOT in the
// container registry but are still controllable via `ai services start|stop|restart`.
// vLLM is not in the registry, so it is added here.
func ServiceNames() []string {
	specs := desiredServices()
	names := make([]string, 0, len(specs)+len(hostNativeServiceNames()))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	for _, name := range hostNativeServiceNames() {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// hostNativeServiceNames are the HOST-NATIVE inference runtimes: they run as host
// PROCESSES (not aip-* containers), so their start/stop/restart is a host-exec path
// (controlHostNativeService), NOT the container `Control` (`docker start/stop`). vLLM
// (per-model detached `vllm serve` loopback processes) is the sole one.
func hostNativeServiceNames() []string {
	return []string{"vllm"}
}

// isHostNativeService reports whether name is a host-native runtime (vllm),
// which is controlled via the host-exec path rather than the container runtime.
func isHostNativeService(name string) bool {
	return slices.Contains(hostNativeServiceNames(), name)
}

// ControlService backs `ai services start|stop|restart|enable|disable [service]`.
// start/stop/restart validate the action + service (empty service = all) and
// delegate to the Services implementation, but are GATED to enabled services: a
// disabled optional service must be enabled first. enable/disable toggle an
// optional service's membership in the persisted set and bring it up/down (see
// setOptionalService). Invalid action/service → exit 2.
func ControlService(deps Deps, action, service string) ([]ServiceStatus, error) {
	if action == "enable" || action == "disable" {
		// enable/disable is an optional-CONTAINER-service concept (membership in the
		// persisted set). The host-native runtimes have no such notion — they are always
		// present, controlled only by start/stop/restart — so reject it clearly (exit 2)
		// rather than fall into the "core service" message.
		if isHostNativeService(service) {
			return nil, output.Errorf(output.ExitInvalidInput,
				"%q is a host-native runtime — it has no enable/disable (use `ai services start|stop|restart %s`)",
				service, service)
		}
		return setOptionalService(deps, action == "enable", service)
	}
	if !controlActions[action] {
		return nil, output.Errorf(output.ExitInvalidInput, "unknown action %q (start|stop|restart|enable|disable)", action)
	}
	// "all" is the explicit spelling of "no service = all services".
	if service == "all" {
		service = ""
	}
	// A named host-native runtime (vllm) is a host PROCESS, not an aip-*
	// container: dispatch its lifecycle to the host-exec path instead of the
	// container `Control` (which would reject vllm as unknown).
	if service != "" && isHostNativeService(service) {
		return controlHostNativeService(deps, action, service)
	}
	if service != "" && !slices.Contains(ServiceNames(), service) {
		// A companion container managed as part of its owning logical service
		// (not on its own) would be pointed at its owner here. There are currently
		// no such surfaced companions (owningService returns "" for all names), so
		// this branch is dormant. (`ai logs --service <name>` can still tail a
		// companion container.)
		if owner := owningService(service); owner != "" {
			return nil, output.Errorf(output.ExitInvalidInput,
				"%q is managed as part of the %q service — run `ai services %s %s` (`ai logs --service %s` tails just that container)",
				service, owner, action, owner, service)
		}
		return nil, output.Errorf(output.ExitInvalidInput, "unknown service %q (one of %v, or \"all\")", service, ServiceNames())
	}
	// A disabled optional service can't be started/stopped/restarted until it is
	// enabled (`ai services enable <name>`); enabling is what brings it up.
	if service != "" && isOptionalService(service) && !optionalServiceEnabled(service) {
		return nil, output.Errorf(output.ExitInvalidInput,
			"%q is disabled — run `ai services enable %s` first", service, service)
	}
	if service == "" {
		// "all": the container tier first, then the host-native runtimes best-effort.
		// A host runtime being ABSENT (no vllm binary, no vLLM models recorded)
		// must NEVER fail `ai services <action> all` — its error is swallowed here.
		if _, err := deps.Services.Control(action, ""); err != nil {
			return nil, err
		}
		for _, name := range hostNativeServiceNames() {
			_ = applyHostNativeAction(action, name)
		}
		return deps.Services.Status()
	}
	return deps.Services.Control(action, service)
}

// hostNative*  are the injectable seams for the host-native runtime lifecycle so
// unit tests exercise ControlService's ROUTING without forking a real process.
// They default to the real (hardware bring-up) implementations in vllm_host.go. Tests
// override them (and a package TestMain neutralizes them so no container-path test
// accidentally spawns a host process).
var (
	hostNativeStartVLLM = startVLLMServersHost
	hostNativeStopVLLM  = stopVLLMServersHost
)

// controlHostNativeService applies a lifecycle action to a host-native runtime
// (vllm) via the host-exec path, then re-reads the service status (mirroring
// the container Control path, which also returns the post-action statuses). An
// unknown host-native name (defensive — ControlService validates first) is exit 2.
func controlHostNativeService(deps Deps, action, service string) ([]ServiceStatus, error) {
	if err := applyHostNativeAction(action, service); err != nil {
		return nil, err
	}
	return deps.Services.Status()
}

// applyHostNativeAction runs start/stop/restart on ONE host-native runtime through
// the injectable seams. restart is a stop (best-effort — a not-running server is not
// an error) followed by a start.
func applyHostNativeAction(action, service string) error {
	var start, stop func() error
	switch service {
	case "vllm":
		start, stop = hostNativeStartVLLM, hostNativeStopVLLM
	default:
		return output.Errorf(output.ExitInvalidInput, "unknown host-native service %q", service)
	}
	switch action {
	case "start":
		return start()
	case "stop":
		return stop()
	case "restart":
		_ = stop()
		return start()
	default:
		return output.Errorf(output.ExitInvalidInput, "unknown action %q (start|stop|restart)", action)
	}
}

// UpdateService backs `ai services update [service]` (and the TUI `p` key): it
// force-pulls the latest service-tier images (re-pulling moved tags like `latest`)
// then RESTARTS the target service(s) so their containers are recreated against
// the freshly-pulled images. An empty/"all" service updates every enabled service;
// a named one validates + updates just that service (same validation as
// ControlService). out receives the native pull progress; progress streams short
// step lines. It returns the post-restart statuses.
func UpdateService(deps Deps, service string, out io.Writer, progress func(string)) ([]ServiceStatus, error) {
	if progress == nil {
		progress = func(string) {}
	}
	// Validate the target up front (reusing the restart validation path) so an
	// unknown/companion/disabled name fails BEFORE we pull anything.
	if service == "all" {
		service = ""
	}
	if service != "" {
		if !slices.Contains(ServiceNames(), service) {
			if owner := owningService(service); owner != "" {
				return nil, output.Errorf(output.ExitInvalidInput,
					"%q is managed as part of the %q service — run `ai services update %s`", service, owner, owner)
			}
			return nil, output.Errorf(output.ExitInvalidInput, "unknown service %q (one of %v, or \"all\")", service, ServiceNames())
		}
		if isOptionalService(service) && !optionalServiceEnabled(service) {
			return nil, output.Errorf(output.ExitInvalidInput,
				"%q is disabled — run `ai services enable %s` first", service, service)
		}
	}
	// Force-pull the enabled image set (the whole set; pulling an already-current
	// image is cheap, and the per-service image subset is an internal detail).
	progress("pulling latest images…")
	if err := deps.Services.UpdateImages(enabledOptionalServices(), reconcileGuardrails(), out, progress); err != nil {
		// Non-fatal: a pull miss just means no update for that image; still restart
		// so any image that DID update is picked up.
		progress("warning: some images did not update — continuing")
	}
	// Restart to recreate the containers against the new images.
	progress("restarting to apply the updated images…")
	return ControlService(deps, "restart", service)
}

// setOptionalService enables or disables an OPTIONAL service: it updates the
// persisted optional-service set in runtime.yaml, then starts (enable) or stops
// (disable) the service's container(s) via the Services impl. Core services are
// always on and cannot be toggled (exit 2); an empty/unknown name is exit 2; a
// missing runtime.yaml is exit 3 (run `ai setup` first).
func setOptionalService(deps Deps, enable bool, service string) ([]ServiceStatus, error) {
	if service == "" || service == "all" {
		return nil, output.Errorf(output.ExitInvalidInput,
			"enable/disable need an optional service name (one of %v)", OptionalServiceNames())
	}
	if !isOptionalService(service) {
		return nil, output.Errorf(output.ExitInvalidInput,
			"%q is a core service (always enabled); only optional services (%v) can be enabled/disabled",
			service, OptionalServiceNames())
	}
	info, err := runtime.Load()
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "load runtime.yaml: %s", err)
	}
	if info == nil {
		return nil, output.Errorf(output.ExitMissingDep, "no runtime.yaml — run `ai setup` first")
	}
	info.OptionalServices = optionalSetWith(info.OptionalServices, service, enable)
	if err := runtime.Persist(info); err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "persist runtime.yaml: %s", err)
	}
	// Persisting first means the post-action Status reflects the new enabled set,
	// and the start/stop below is no longer blocked by the disabled-gate above
	// (Control is called directly, not through ControlService).
	action := "start"
	if !enable {
		action = "stop"
	}
	if _, err := deps.Services.Control(action, service); err != nil {
		return nil, err
	}
	return ServicesStatus(deps)
}

// optionalSetWith returns the optional-service set with service added (enable) or
// removed (disable), in declaration order and de-duplicated.
func optionalSetWith(current []string, service string, enable bool) []string {
	want := make(map[string]bool, len(current))
	for _, name := range current {
		want[name] = true
	}
	want[service] = enable
	result := make([]string, 0, len(optionalServiceNames()))
	for _, name := range optionalServiceNames() {
		if want[name] {
			result = append(result, name)
		}
	}
	return result
}

// optionalServiceEnabled reports whether an optional service is currently enabled
// on this host (present in runtime.yaml's optional set).
func optionalServiceEnabled(name string) bool {
	return slices.Contains(enabledOptionalServices(), name)
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
	fmt.Fprintf(&builder, "%s %s\n", ui.Label.Render("Platform:"), ui.Value.Render(report.PlatformDir))
	if report.Runtime != nil {
		runtimeInfo := report.Runtime
		available := ui.Failure.Render("unavailable")
		if runtimeInfo.Microsandbox.Available {
			available = ui.Success.Render("available")
		}
		fmt.Fprintf(&builder, "%s  %s (rootless=%s) · microVM %s (%s)\n",
			ui.Label.Render("Runtime:"),
			ui.Value.Render(runtimeInfo.Detected),
			ui.Value.Render(fmt.Sprintf("%t", runtimeInfo.Rootless)),
			ui.Value.Render(runtimeInfo.Microsandbox.Virtualization),
			available)
	}
	fmt.Fprintf(&builder, "%s    config.yaml %s · versions.yaml %s\n",
		ui.Label.Render("State:"),
		presence(report.ConfigCreated), presence(report.VersionsCreated))
	builder.WriteString(ui.Heading.Render("Services:") + "\n")
	for _, service := range report.Services {
		// Pad the plain cells to fixed width FIRST, then colour — so the invisible
		// ANSI styling codes don't throw off the column alignment.
		line := "  " + ui.Value.Render(fmt.Sprintf("%-11s", service.Name)) + " " +
			ui.Label.Render(fmt.Sprintf("%-9s", service.Mode)) + " " +
			styleServiceState(service.State)
		if service.Detail != "" {
			line += " — " + ui.Muted.Render(service.Detail)
		}
		line += service.EndpointSuffix()
		builder.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

// styleServiceState colours a service state word semantically (running/ready →
// green, starting/pending → orange [in progress], stopped/failed → red, disabled/
// unknown → muted), padded to the column width BEFORE styling so alignment is
// preserved. Mirrors cli.serviceStateLabel.
func styleServiceState(state string) string {
	padded := fmt.Sprintf("%-9s", state)
	switch state {
	case "running", "ready":
		return ui.Success.Render(padded)
	case "starting", "pending":
		return ui.Warn.Render(padded)
	case "stopped", "failed", "error", "not_installed", "unavailable":
		return ui.Failure.Render(padded)
	default: // disabled, unknown, …
		return ui.Muted.Render(padded)
	}
}

// presence renders whether a default state file was created on this run or
// already existed, reporting that it is present either way.
func presence(created bool) string {
	if created {
		return ui.Success.Render(ui.IconOK + " created")
	}
	return ui.Success.Render(ui.IconOK + " present")
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

// ResolveGuardrails resolves the enabled LiteLLM guardrail set for a setup run,
// mirroring ResolveOptional: an explicit choice (options.GuardrailsSet — the setup
// picker or a --guardrails flag, including the empty "none" set) wins; else the set
// persisted in runtime.yaml; else the default (litellm.DefaultGuardrails — Headroom
// only). Unknown keys are dropped and the result is returned in the litellm.Guardrails
// catalog order, de-duplicated — so a stale persisted key can't reference a removed
// guardrail.
func ResolveGuardrails(options Options, persisted *runtime.Info) []string {
	var chosen []string
	switch {
	case options.GuardrailsSet:
		chosen = options.Guardrails
	case persisted != nil && persisted.Guardrails != nil:
		chosen = persisted.Guardrails
	default:
		chosen = litellm.DefaultGuardrails()
	}
	enabled := make([]string, 0, len(litellm.GuardrailKeys()))
	for _, key := range litellm.GuardrailKeys() {
		if slices.Contains(chosen, key) {
			enabled = append(enabled, key)
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
	// Carry the platform base domain (the nginx UI vhosts hang off it). An explicit
	// Options.Domain wins (the server role prompts for it at `ai setup`); otherwise
	// PRESERVE a domain already set via `ai domain`/a prior run (load-modify-save —
	// the freshly-detected Info has no domain). Empty stays empty so ResolveDomain
	// falls back to aip.local for standalone.
	detected.Domain = strings.TrimSpace(options.Domain)
	if detected.Domain == "" && persisted != nil {
		detected.Domain = strings.TrimSpace(persisted.Domain)
	}
	// Resolve + persist the enabled LiteLLM guardrail set (the setup picker's / a
	// --guardrails flag's explicit choice, else the persisted set, else the default —
	// Headroom only). Persisted BEFORE the reconcile so the litellm render + the
	// Presidio-container gate (reconcileGuardrails) reflect it. Only meaningful when
	// this host runs the gateway (standalone/server); harmless on a client.
	detected.Guardrails = ResolveGuardrails(options, persisted)

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
			warnings = append(warnings, "server mode exposes LiteLLM/Headroom on 0.0.0.0 — put TLS in front and rely on LiteLLM virtual-key auth for untrusted networks")
		}
		progress("Starting host services — pulling images / launching containers (this can take a minute)…")
		serviceStatuses, err = deps.Services.Reconcile(options.ProviderConfig, bindHost, optional, progress)
		if err != nil {
			return nil, output.Errorf(output.ExitRuntimeFailure, "reconcile services: %s", err)
		}
		progress("Host services ready.")

		// 6. Fetch + persist the models.dev catalog so the saved copy is current
		// before the CLI offers cloud-provider keys and runs the initial model sync.
		// Best-effort: offline falls back to the saved copy and a failure never
		// stops setup (the catalog is only a model-picker convenience).
		if deps.FetchCatalog != nil {
			progress("Fetching the model catalog (models.dev)…")
			if err := deps.FetchCatalog(); err != nil {
				warnings = append(warnings, "model catalog: "+err.Error())
			}
		}
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

// ServiceLogTailLines is how many trailing lines per container ServiceLogTail
// returns — bounded so a busy container's full history can't flood the UI (it
// mirrors logCaptureTailLines, the on-disk snapshot's tail).
const ServiceLogTailLines = 200

// ServiceLogTail returns the recent LIVE container logs for a logical service via
// the container runtime (`<runtime> logs --tail N <container>`) — the service-tier
// analogue of Manager.WorkspaceLogTail's `msb logs --tail` for a workspace microVM.
// It is what the `ai ui` Services-tab detail view embeds beneath the service summary
// (re-polled ~2s).
//
// A service may own several containers (services.ContainerNames — Presidio's
// analyzer + anonymizer; LiteLLM + its db). For a single-container service the raw
// logs are returned as-is; for a multi-container one each container's logs are
// returned under a "── <container> ──" heading so the source of each section is
// clear. A container that is not running (no logs / unknown) contributes an empty
// section rather than failing the whole tail. tail defaults to ServiceLogTailLines
// when <= 0.
//
// hardware bring-up: the live `<runtime> logs` round-trip runs only against a
// running engine; the argv construction + multi-container layout are unit-tested
// against a fake prober.
func ServiceLogTail(deps Deps, service string, tail int) (string, error) {
	if service == "vllm" {
		// Host-native: no container, so serviceContainers is empty. vLLM's per-model
		// output is redirected to <storeDir>/<alias>.log at process start (see
		// vllm.RealRunner.Start), so tail those files instead of a container log.
		if tail <= 0 {
			tail = ServiceLogTailLines
		}
		return vllmHostLogTail(tail)
	}
	containers := serviceContainers(service)
	if len(containers) == 0 {
		if isDesiredService(service) {
			// A known host-native service (e.g. vllm) has no container to tail — it
			// is not an error, there is simply nothing to show.
			return "", nil
		}
		return "", output.Errorf(output.ExitInvalidInput, "unknown service %q", service)
	}
	containerRuntime, err := runtime.ContainerRuntimeName(deps.Prober)
	if err != nil {
		return "", output.Errorf(output.ExitMissingDep, "no container runtime for logs: %s", err)
	}
	if tail <= 0 {
		tail = ServiceLogTailLines
	}
	tailArg := strconv.Itoa(tail)
	// Single container: the raw log text, no heading (the detail view already labels
	// the pane with the service name).
	if len(containers) == 1 {
		out, runErr := deps.Prober.Run(containerRuntime.Name, "logs", "--tail", tailArg, containers[0])
		if runErr != nil {
			// A stopped/absent container is not an error for the viewer — it just has
			// nothing to show yet (the same tolerance CaptureServiceLogs uses).
			return "", nil
		}
		return string(out), nil
	}
	// Multiple containers: concatenate per-container sections under a heading so the
	// origin of each block is clear (e.g. presidio's analyzer + anonymizer).
	var combined strings.Builder
	for index, container := range containers {
		if index > 0 {
			combined.WriteString("\n")
		}
		combined.WriteString("── " + container + " ──\n")
		out, runErr := deps.Prober.Run(containerRuntime.Name, "logs", "--tail", tailArg, container)
		if runErr != nil {
			continue // best-effort: a not-running companion contributes an empty section
		}
		combined.Write(out)
	}
	return combined.String(), nil
}

// ContainerStats is a live resource snapshot of ONE container backing a service.
// The rate fields are the container runtime's OWN pre-formatted strings (e.g.
// "3.20%", "180MiB / 512MiB", "1.2kB / 0B") so the UI renders them verbatim. A
// container that is not running yields Found=false (State carries why) and empty
// rate fields.
type ContainerStats struct {
	Container  string `json:"container"`   // container name (e.g. aip-litellm)
	ID         string `json:"id"`          // short (12-char) container id
	State      string `json:"state"`       // running | exited | not found | …
	Found      bool   `json:"found"`       // the container exists AND is running
	CPUPercent string `json:"cpu_percent"` // "3.20%"
	MemUsage   string `json:"mem_usage"`   // "180MiB / 512MiB"
	MemPercent string `json:"mem_percent"` // "35.20%"
	NetIO      string `json:"net_io"`      // "1.2kB / 0B"  (rx / tx)
	BlockIO    string `json:"block_io"`    // "10MB / 4MB"  (read / write)
	Pids       string `json:"pids"`        // "12"
	Ports      string `json:"ports"`       // "4000/tcp" (comma-joined, from inspect)
	StartedAt  string `json:"started_at"`  // RFC3339 last-start time ("" if never)
	Uptime     string `json:"uptime"`      // now-StartedAt, rounded ("" if not running)
}

// dockerStatsLine is the shape of one `<runtime> stats --no-stream --format '{{json .}}'`
// object (docker + nerdctl share these field names).
type dockerStatsLine struct {
	CPUPerc  string `json:"CPUPerc"`
	MemUsage string `json:"MemUsage"`
	MemPerc  string `json:"MemPerc"`
	NetIO    string `json:"NetIO"`
	BlockIO  string `json:"BlockIO"`
	PIDs     string `json:"PIDs"`
}

// ServiceStats returns a live resource snapshot per container of a logical service
// (a service may own several — Presidio's analyzer+anonymizer, LiteLLM + its db), via
// the container runtime's `stats --no-stream` + `inspect`. It backs the `ai ui`
// Services detail's Service sub-tab (polled ~2s), the service-tier analogue of the
// workspace Metrics stream. A stopped/absent container yields a Found=false entry
// rather than failing the whole call. Unknown service → exit 2; no runtime → exit 3.
//
// hardware bring-up: the live `<runtime> stats`/`inspect` round-trips run only against
// a running engine; the argv + JSON parsing are unit-tested against a fake prober.
func ServiceStats(deps Deps, service string) ([]ContainerStats, error) {
	containers := serviceContainers(service)
	if len(containers) == 0 {
		if isDesiredService(service) {
			// A known host-native service (e.g. vllm) runs no container — no stats to
			// report, but not an error.
			return nil, nil
		}
		return nil, output.Errorf(output.ExitInvalidInput, "unknown service %q", service)
	}
	containerRuntime, err := runtime.ContainerRuntimeName(deps.Prober)
	if err != nil {
		return nil, output.Errorf(output.ExitMissingDep, "no container runtime for stats: %s", err)
	}
	now := statsNow(deps.Now)
	stats := make([]ContainerStats, 0, len(containers))
	for _, container := range containers {
		stats = append(stats, containerStats(deps.Prober, containerRuntime.Name, container, now))
	}
	return stats, nil
}

// containerStats inspects one container for its id/state/ports/start-time, then — when
// running — layers on the runtime's live stats line. Best-effort: any failed probe
// leaves that field empty rather than erroring (a stopped container is normal).
func containerStats(prober runtime.Prober, containerRuntime, name string, now time.Time) ContainerStats {
	stats := ContainerStats{Container: name, State: "not found"}
	out, err := prober.Run(containerRuntime, "inspect", "--format",
		"{{.Id}}\t{{.State.Status}}\t{{.State.StartedAt}}\t{{json .NetworkSettings.Ports}}", name)
	if err != nil {
		return stats // absent — Found stays false
	}
	fields := strings.SplitN(strings.TrimSpace(string(out)), "\t", 4)
	if len(fields) == 4 {
		stats.ID = shortContainerID(fields[0])
		stats.State = fields[1]
		stats.StartedAt = fields[2]
		stats.Ports = parseContainerPorts(fields[3])
		if started, perr := time.Parse(time.RFC3339Nano, fields[2]); perr == nil && !started.IsZero() && !now.IsZero() && stats.State == "running" {
			stats.Uptime = now.Sub(started).Round(time.Second).String()
		}
	}
	if stats.State != "running" {
		return stats
	}
	stats.Found = true
	statsOut, serr := prober.Run(containerRuntime, "stats", "--no-stream", "--format", "{{json .}}", name)
	if serr != nil {
		return stats
	}
	var line dockerStatsLine
	if json.Unmarshal([]byte(strings.TrimSpace(string(statsOut))), &line) == nil {
		stats.CPUPercent = line.CPUPerc
		stats.MemUsage = line.MemUsage
		stats.MemPercent = line.MemPerc
		stats.NetIO = line.NetIO
		stats.BlockIO = line.BlockIO
		stats.Pids = line.PIDs
	}
	return stats
}

// statsNow resolves the reference time for uptime: the injected Now (RFC3339, for
// determinism) when set + parseable, else the wall clock; a zero time skips uptime.
func statsNow(nowFn func() string) time.Time {
	if nowFn != nil {
		if parsed, err := time.Parse(time.RFC3339, nowFn()); err == nil {
			return parsed
		}
	}
	return time.Now()
}

// shortContainerID truncates a full container id to the conventional 12 chars.
func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// parseContainerPorts renders the inspect `.NetworkSettings.Ports` JSON map as a
// comma-joined, sorted list of container ports ("4000/tcp"), appending "→<hostPort>"
// when a port is published to the host. Empty/null map → "".
func parseContainerPorts(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || raw == "{}" {
		return ""
	}
	var ports map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	if json.Unmarshal([]byte(raw), &ports) != nil {
		return ""
	}
	entries := make([]string, 0, len(ports))
	for port, bindings := range ports {
		entry := port
		if len(bindings) > 0 && bindings[0].HostPort != "" {
			entry += "→" + bindings[0].HostPort
		}
		entries = append(entries, entry)
	}
	slices.Sort(entries)
	return strings.Join(entries, ", ")
}
