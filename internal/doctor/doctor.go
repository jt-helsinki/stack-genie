// Package doctor implements `ai doctor [name]` (CLI §10.1, §12.1): it runs the
// platform's dependency, service, and (when inside a workspace) runtime health
// checks and reports each with an actionable repair suggestion. Detection reuses
// internal/runtime + internal/sandbox, so it is unit-testable on any host via an
// injected prober. The service list is supplied by the CLI layer (from
// setup.ServicesStatus) so this package does not import internal/setup — that
// would form an import cycle (internal/setup imports internal/doctor).
package doctor

import (
	"fmt"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/console"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/sandbox"
)

// Status is a single check's outcome.
type Status string

const (
	StatusOK    Status = "ok"
	StatusWarn  Status = "warn"
	StatusError Status = "error"
)

// Check is one dependency/health result.
type Check struct {
	Name       string `json:"name"`
	Status     Status `json:"status"`
	Detail     string `json:"detail,omitempty"`
	Suggestion string `json:"suggestion,omitempty"` // how to install / repair
	DocsURL    string `json:"docs_url,omitempty"`   // web address for the software
}

// Report is the full `ai doctor` result. OK is false if any check errored.
type Report struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

// Human renders the report as a readable check list with status glyphs and
// repair suggestions (used for non-JSON `ai doctor` output).
func (report Report) Human() string {
	glyphs := map[Status]string{StatusOK: "✓", StatusWarn: "!", StatusError: "✗"}
	var builder strings.Builder
	for _, check := range report.Checks {
		detail := check.Detail
		// For service checks show the host-reachable address — and, for services
		// that have one, the admin-console URL — so the user can find each
		// service's endpoint + UI. This is distinct from the "↳ suggestion" /
		// "web:" lines below, which guide FAILING software checks.
		if endpoint, ok := console.EndpointFor(check.Name); ok && endpoint.Address != "" {
			detail += " — " + endpoint.Address
			if endpoint.Console != "" {
				detail += " (UI " + endpoint.Console + ")"
			}
		}
		_, _ = fmt.Fprintf(&builder, "%s  %-22s %s\n", glyphs[check.Status], check.Name, detail)
		if check.Suggestion != "" {
			_, _ = fmt.Fprintf(&builder, "       ↳ %s\n", check.Suggestion)
		}
		if check.Status != StatusOK && check.DocsURL != "" {
			_, _ = fmt.Fprintf(&builder, "       web: %s\n", check.DocsURL)
		}
	}
	if report.OK {
		builder.WriteString("\nAll checks passed.")
	} else {
		builder.WriteString("\nProblems found — see the suggestions above.")
	}
	return builder.String()
}

// Service is the doctor-layer view of one managed service, supplied by the CLI
// from setup.ServiceStatus. It is a local mirror (not setup.ServiceStatus) so
// this package avoids importing internal/setup, which would create an import
// cycle. Run turns each into a Check in the SERVICES section.
type Service struct {
	Name     string
	State    string // running | stopped | disabled | not_installed | unavailable | ready | unknown
	Healthy  bool
	Optional bool
	Detail   string
}

// WorkspaceRuntime is the per-workspace runtime posture supplied by the CLI when
// the user is inside (or names) a workspace. It mirrors the fields the old
// `ai workspace doctor` reported; Run turns it into the WORKSPACE section.
type WorkspaceRuntime struct {
	// Project names the resolved workspace (for the section heading detail).
	Project string
	// RuntimeErr, when non-nil, is a missing-runtime failure (no container
	// runtime / Microsandbox) — surfaced as an error Check rather than aborting.
	RuntimeErr error
	// VerifyErr, when non-nil, is a rootless/virtualization shortfall — surfaced
	// as an error Check.
	VerifyErr error
	Rootless  bool
	// Virtualization names the workspace virtualization backend (hvf, kvm, …).
	Virtualization string
	// Available reports whether the microVM virtualization is usable.
	Available bool
}

// DomainURL is one UI subdomain's display URL for the DOMAIN section.
type DomainURL struct {
	Service string
	Host    string
	URL     string
}

// DomainInfo is the platform UI-subdomain posture supplied by the CLI (built from
// uihosts + the persisted runtime.yaml). Run renders it as the DOMAIN section: the
// resolved base domain, the UI URLs, and — for standalone — whether the /etc/hosts
// block is present + up to date; for a server, the DNS reminder.
type DomainInfo struct {
	// Domain is the resolved platform base domain (Info.ResolveDomain()).
	Domain string
	// Role is the deployment role (standalone/server/client) that decides how the
	// UI subdomains resolve.
	Role string
	// URLs are the active UI subdomain display URLs.
	URLs []DomainURL
	// Standalone is true when this host manages /etc/hosts (the resolution-status
	// check applies). HostsPresent/HostsUpToDate are the hostsfile.Status result.
	Standalone    bool
	HostsPresent  bool
	HostsUpToDate bool
	// ServerReminder is the one-line DNS/TLS reminder shown on a server.
	ServerReminder string
	// ServerCredentials, when set (server role), is the per-UI admin-access guide
	// (each UI's URL + how to set/rotate its password) rendered as its own check.
	ServerCredentials string
}

// Deps are the injectable dependencies of Run.
type Deps struct {
	GOOS, GOARCH string
	Prober       runtime.Prober
	// Services is the full managed-service list (from setup.ServicesStatus,
	// mapped by the CLI). Run renders each as a Check in the SERVICES section, so
	// `ai doctor` lists every service — including any optional ones — without this
	// package importing internal/setup.
	Services []Service
	// Domain, when non-nil, adds the DOMAIN section (the platform base domain, the
	// UI URLs, and the standalone /etc/hosts resolution status or the server DNS
	// reminder). nil omits it.
	Domain *DomainInfo
	// Workspace, when non-nil, adds the WORKSPACE-runtime section (the user is
	// inside a workspace directory or named one). When nil the section is omitted.
	Workspace *WorkspaceRuntime
}

// Run executes the platform health checks: platform dependencies, then every
// managed service, then (when supplied) the per-workspace runtime posture. It
// always runs to completion and the report's OK is false if any check errored.
func Run(deps Deps) Report {
	detectedSandbox := sandbox.Detect(deps.GOOS, deps.GOARCH, deps.Prober)
	checks := []Check{
		containerRuntimeCheck(deps.GOOS, deps.Prober),
		rootlessCheck(deps.GOOS, deps.Prober),
		microsandboxCheck(detectedSandbox),
		virtualizationCheck(deps.GOOS, detectedSandbox),
	}
	for _, service := range deps.Services {
		checks = append(checks, serviceCheck(service))
	}
	if deps.Domain != nil {
		checks = append(checks, domainChecks(*deps.Domain)...)
	}
	if deps.Workspace != nil {
		checks = append(checks, workspaceChecks(*deps.Workspace)...)
	}
	report := Report{OK: true, Checks: checks}
	for _, check := range checks {
		if check.Status == StatusError {
			report.OK = false
		}
	}
	return report
}

// serviceCheck renders one managed service as a Check. A disabled optional
// service is a non-error "disabled" warning; a stopped/unhealthy required
// service is an error; everything else maps from State + Healthy. The check name
// matches the service name so Human() can show its console-registry endpoint.
func serviceCheck(service Service) Check {
	if service.Optional && service.State == "disabled" {
		return Check{
			Name: service.Name, Status: StatusWarn,
			Detail:     "optional; disabled",
			Suggestion: "run `ai services enable " + service.Name + "` to turn it on",
		}
	}
	if service.Healthy {
		detail := "running"
		if service.State == "ready" {
			detail = "ready"
		}
		if service.Detail != "" {
			detail = service.Detail
		}
		return Check{Name: service.Name, Status: StatusOK, Detail: detail}
	}
	// Not healthy. Optional services warn (never fail the report); required
	// services error.
	detail := service.State
	if detail == "" {
		detail = "not reachable"
	}
	if service.Optional {
		return Check{
			Name: service.Name, Status: StatusWarn,
			Detail:     "optional; " + detail,
			Suggestion: "run `ai services start " + service.Name + "` (or `ai setup`) to start it",
		}
	}
	return Check{
		Name: service.Name, Status: StatusError,
		Detail:     detail,
		Suggestion: "run `ai setup` (or `ai services start " + service.Name + "`) to start it",
	}
}

// workspaceChecks renders the per-workspace runtime posture as Checks (the old
// `ai workspace doctor` content). A missing runtime or a rootless/virtualization
// shortfall is folded into an error Check rather than a non-zero exit, so the
// consolidated doctor still runs to completion and exits 0 with a full report.
func workspaceChecks(workspace WorkspaceRuntime) []Check {
	if workspace.RuntimeErr != nil {
		// Missing container runtime or Microsandbox.
		return []Check{{
			Name: "workspace runtime", Status: StatusError,
			Detail:     workspace.RuntimeErr.Error(),
			Suggestion: "install the missing runtime, then `ai setup`",
		}}
	}
	rootless := Check{Name: "workspace rootless", Status: StatusOK, Detail: "rootless, unprivileged"}
	if !workspace.Rootless {
		rootless = Check{
			Name: "workspace rootless", Status: StatusError,
			Detail:     "container runtime runs as root",
			Suggestion: "run the container runtime rootless (the platform never runs privileged)",
		}
	}
	virt := Check{
		Name: "workspace virtualization", Status: StatusOK,
		Detail: "microvm (" + workspace.Virtualization + ")",
	}
	if !workspace.Available || workspace.VerifyErr != nil {
		detail := "microVM virtualization unavailable"
		if workspace.VerifyErr != nil {
			detail = workspace.VerifyErr.Error()
		}
		virt = Check{
			Name: "workspace virtualization", Status: StatusError,
			Detail:     detail,
			Suggestion: "enable hardware virtualization for the microVM runtime (Apple Silicon HVF / Linux KVM)",
		}
	}
	return []Check{rootless, virt}
}

// domainChecks renders the platform UI-subdomain posture: the resolved base
// domain, one info check per UI URL, and the resolution status — standalone shows
// whether the /etc/hosts block is present + up to date (a warning when it is
// missing/stale, with a `ai setup` repair hint); a server shows the DNS/TLS
// reminder. These are informational/warn only — they never fail the report.
func domainChecks(info DomainInfo) []Check {
	checks := []Check{{
		Name: "platform domain", Status: StatusOK, Detail: info.Domain,
	}}
	for _, url := range info.URLs {
		checks = append(checks, Check{
			Name: "UI " + url.Host, Status: StatusOK, Detail: url.URL,
		})
	}
	switch {
	case info.Standalone:
		if info.HostsPresent && info.HostsUpToDate {
			checks = append(checks, Check{
				Name: "UI subdomain resolution", Status: StatusOK,
				Detail: "/etc/hosts block present and up to date",
			})
		} else {
			detail := "/etc/hosts block missing"
			if info.HostsPresent {
				detail = "/etc/hosts block out of date"
			}
			checks = append(checks, Check{
				Name: "UI subdomain resolution", Status: StatusWarn,
				Detail:     detail,
				Suggestion: "run `ai setup` to update /etc/hosts (or add the block manually with sudo)",
			})
		}
	case info.ServerReminder != "":
		checks = append(checks, Check{
			Name: "UI subdomain resolution", Status: StatusWarn,
			Detail:     "server mode — DNS + TLS are operator-managed",
			Suggestion: info.ServerReminder,
		})
		if info.ServerCredentials != "" {
			checks = append(checks, Check{
				Name: "UI admin access", Status: StatusWarn,
				Detail:     "server mode is network-exposed — secure each UI",
				Suggestion: info.ServerCredentials,
			})
		}
	}
	return checks
}

func containerRuntimeCheck(goos string, prober runtime.Prober) Check {
	switch {
	case installed(prober, "docker"):
		return Check{Name: "container runtime", Status: StatusOK, Detail: "docker"}
	case installed(prober, "podman"):
		return Check{Name: "container runtime", Status: StatusOK, Detail: "podman"}
	default:
		return Check{
			Name: "container runtime", Status: StatusError,
			Detail:     "neither docker nor podman found",
			Suggestion: containerRuntimeSuggestion(goos),
			DocsURL:    "https://docs.docker.com/get-docker/  (or Podman: https://podman.io/get-started)",
		}
	}
}

func installed(prober runtime.Prober, binary string) bool {
	_, err := prober.LookPath(binary)
	return err == nil
}

// containerRuntimeSuggestion gives an OS-specific, copy-pasteable install command
// for a rootless container runtime (the service tier, §6.1).
func containerRuntimeSuggestion(goos string) string {
	switch goos {
	case "darwin":
		return "install a rootless container runtime: brew install --cask docker (or brew install podman), then `ai setup`"
	case "linux":
		return "install a rootless container runtime: curl -fsSL https://get.docker.com | sh (or your distro's podman), then `ai setup`"
	default:
		return "install Docker or Podman (rootless), then `ai setup`"
	}
}

// rootlessCheck verifies the service tier runs without a rooted host daemon
// (§6.1). When no container runtime is present the container-runtime check
// already reports it, so this stays a warning to avoid a duplicate error.
func rootlessCheck(goos string, prober runtime.Prober) Check {
	containerRuntime, rootless, err := runtime.DetectContainerRuntime(goos, prober)
	if err != nil {
		return Check{Name: "rootless service tier", Status: StatusWarn, Detail: "no container runtime"}
	}
	if rootless {
		return Check{Name: "rootless service tier", Status: StatusOK, Detail: containerRuntime.Name + " rootless"}
	}
	return Check{
		Name: "rootless service tier", Status: StatusError,
		Detail:     containerRuntime.Name + " runs as root",
		Suggestion: "run the container runtime rootless (Linux: enable rootless mode, or use Podman)",
		DocsURL:    "https://docs.docker.com/engine/security/rootless/",
	}
}

func microsandboxCheck(detected sandbox.Info) Check {
	if detected.MsbInstalled {
		return Check{Name: "microsandbox runtime", Status: StatusOK, Detail: "msb installed"}
	}
	return Check{
		Name: "microsandbox runtime", Status: StatusError,
		Detail:     "msb not found",
		Suggestion: "install Microsandbox: curl -sSL https://get.microsandbox.dev | sh",
		DocsURL:    "https://docs.microsandbox.dev  (installer: https://get.microsandbox.dev)",
	}
}

func virtualizationCheck(goos string, detected sandbox.Info) Check {
	if detected.Available {
		return Check{Name: "host virtualization", Status: StatusOK, Detail: detected.Virtualization}
	}
	detail := "no usable virtualization"
	if detected.Virtualization != "" {
		detail = detected.Virtualization + " unavailable"
	}
	return Check{
		Name: "host virtualization", Status: StatusError, Detail: detail,
		Suggestion: virtualizationSuggestion(goos),
	}
}

// virtualizationSuggestion gives the OS-specific remedy. Supported hosts are
// macOS (Apple Silicon / HVF) and Linux (KVM) (arch §6.2).
func virtualizationSuggestion(goos string) string {
	switch goos {
	case "linux":
		return "enable hardware virtualization (KVM) so /dev/kvm is present"
	case "darwin":
		return "Apple Silicon is required for the microVM runtime (Intel Macs are unsupported)"
	default:
		return "unsupported OS — this platform requires macOS (Apple Silicon) or Linux (KVM)"
	}
}
