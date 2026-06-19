// Package doctor implements `ai doctor` (CLI §10.1): it runs the platform's
// dependency and health checks and reports each with an actionable repair
// suggestion. Detection reuses internal/runtime + internal/sandbox, so it is
// unit-testable on any host via an injected prober and model client.
package doctor

import (
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
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
	Suggestion string `json:"suggestion,omitempty"`
}

// Report is the full `ai doctor` result. OK is false if any check errored.
type Report struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

// Deps are the injectable dependencies of Run.
type Deps struct {
	GOOS, GOARCH string
	Prober       runtime.Prober
	Model        litellm.Client
}

// Run executes the Slice 1 checks. (Caveman/Headroom checks land with Slice 2.)
func Run(deps Deps) Report {
	detectedSandbox := sandbox.Detect(deps.GOOS, deps.GOARCH, deps.Prober)
	checks := []Check{
		containerRuntimeCheck(deps.Prober),
		microsandboxCheck(detectedSandbox),
		virtualizationCheck(deps.GOOS, detectedSandbox),
		clawpatrolCheck(deps.Prober),
		litellmCheck(deps.Model),
	}
	report := Report{OK: true, Checks: checks}
	for _, check := range checks {
		if check.Status == StatusError {
			report.OK = false
		}
	}
	return report
}

func installed(prober runtime.Prober, binary string) bool {
	_, err := prober.LookPath(binary)
	return err == nil
}

func containerRuntimeCheck(prober runtime.Prober) Check {
	switch {
	case installed(prober, "docker"):
		return Check{Name: "container runtime", Status: StatusOK, Detail: "docker"}
	case installed(prober, "podman"):
		return Check{Name: "container runtime", Status: StatusOK, Detail: "podman"}
	default:
		return Check{
			Name: "container runtime", Status: StatusError,
			Detail:     "neither docker nor podman found",
			Suggestion: "install Docker (or Podman) and run `ai setup`",
		}
	}
}

func microsandboxCheck(detected sandbox.Info) Check {
	if detected.MsbInstalled {
		return Check{Name: "microsandbox runtime", Status: StatusOK, Detail: "msb installed"}
	}
	return Check{
		Name: "microsandbox runtime", Status: StatusError,
		Detail:     "msb not found",
		Suggestion: "install Microsandbox: curl -fsSL https://install.microsandbox.dev | sh",
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

// virtualizationSuggestion gives the OS-specific remedy. On Windows the
// WSL2-nested-virtualization requirement is at-risk, so `doctor` must call it out
// clearly (arch §6.2, Slice 7).
func virtualizationSuggestion(goos string) string {
	switch goos {
	case "windows":
		return "enable WSL2 with nested virtualization (Microsandbox microVMs need /dev/kvm inside the WSL2 guest); see `ai doctor` docs"
	case "linux":
		return "enable hardware virtualization (KVM) so /dev/kvm is present"
	case "darwin":
		return "Apple Silicon is required for the microVM runtime (Intel Macs are unsupported)"
	default:
		return "macOS needs Apple Silicon; Linux needs /dev/kvm; Windows needs WSL2 nested virtualization"
	}
}

func clawpatrolCheck(prober runtime.Prober) Check {
	if installed(prober, "clawpatrol") {
		return Check{
			Name: "clawpatrol", Status: StatusOK,
			Detail: "installed (gateway health + CA-trust checks land with hardware bring-up)",
		}
	}
	return Check{
		Name: "clawpatrol", Status: StatusError,
		Detail:     "clawpatrol not found",
		Suggestion: "install ClawPatrol and run `ai setup`",
	}
}

func litellmCheck(client litellm.Client) Check {
	if client == nil {
		return Check{Name: "litellm", Status: StatusWarn, Detail: "not checked"}
	}
	status, err := client.Status()
	if err != nil || !status.Healthy {
		return Check{
			Name: "litellm", Status: StatusWarn, Detail: "not reachable",
			Suggestion: "run `ai setup` to start the LiteLLM gateway",
		}
	}
	return Check{Name: "litellm", Status: StatusOK, Detail: "reachable"}
}
