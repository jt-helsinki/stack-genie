// Package doctor implements `ai doctor` (CLI §10.1): it runs the platform's
// dependency and health checks and reports each with an actionable repair
// suggestion. Detection reuses internal/runtime + internal/sandbox, so it is
// unit-testable on any host via an injected prober and model client.
package doctor

import (
	"fmt"
	"strings"

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

// Human renders the report as a readable check list with status glyphs and
// repair suggestions (used for non-JSON `ai doctor` output).
func (report Report) Human() string {
	glyphs := map[Status]string{StatusOK: "✓", StatusWarn: "!", StatusError: "✗"}
	var builder strings.Builder
	for _, check := range report.Checks {
		_, _ = fmt.Fprintf(&builder, "%s  %-22s %s\n", glyphs[check.Status], check.Name, check.Detail)
		if check.Suggestion != "" {
			_, _ = fmt.Fprintf(&builder, "       ↳ %s\n", check.Suggestion)
		}
	}
	if report.OK {
		builder.WriteString("\nAll checks passed.")
	} else {
		builder.WriteString("\nProblems found — see the suggestions above.")
	}
	return builder.String()
}

// OllamaProbe reports whether the optional Ollama service is reachable. It is
// injectable so the check is unit-testable; nil means "not checked" (the live
// probe is wired during hardware bring-up).
type OllamaProbe interface{ Reachable() error }

// Deps are the injectable dependencies of Run.
type Deps struct {
	GOOS, GOARCH string
	Prober       runtime.Prober
	Model        litellm.Client
	Ollama       OllamaProbe
}

// Run executes the platform health checks.
func Run(deps Deps) Report {
	detectedSandbox := sandbox.Detect(deps.GOOS, deps.GOARCH, deps.Prober)
	checks := []Check{
		containerRuntimeCheck(deps.GOOS, deps.Prober),
		rootlessCheck(deps.GOOS, deps.Prober),
		microsandboxCheck(detectedSandbox),
		virtualizationCheck(deps.GOOS, detectedSandbox),
		clawpatrolCheck(deps.Prober),
		litellmCheck(deps.Model),
		ollamaCheck(deps.Ollama),
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
		}
	}
}

// containerRuntimeSuggestion gives an OS-specific, copy-pasteable install command
// for a rootless container runtime (the service tier, §6.1).
func containerRuntimeSuggestion(goos string) string {
	switch goos {
	case "darwin":
		return "install a rootless container runtime: brew install --cask docker (or brew install podman), then `ai setup`"
	case "linux":
		return "install a rootless container runtime: curl -fsSL https://get.docker.com | sh (or your distro's podman), then `ai setup`"
	case "windows":
		return "install Docker Desktop with the WSL2 backend (or Podman), then `ai setup`"
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
	}
}

// ollamaCheck reports the Ollama service state. Ollama is required — LiteLLM
// routes local model traffic to it (arch §14, §16) — so an unreachable Ollama is
// an error. The probe is injectable; nil means "not checked yet" (the live probe
// is wired during hardware bring-up).
func ollamaCheck(probe OllamaProbe) Check {
	if probe == nil {
		return Check{
			Name: "ollama", Status: StatusWarn,
			Detail:     "required; not checked",
			Suggestion: "run `ai setup` to start Ollama (live health check lands with hardware bring-up)",
		}
	}
	if err := probe.Reachable(); err != nil {
		return Check{
			Name: "ollama", Status: StatusError,
			Detail:     "required but not reachable",
			Suggestion: "run `ai setup` to start the Ollama container",
		}
	}
	return Check{Name: "ollama", Status: StatusOK, Detail: "reachable"}
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
	if !installed(prober, "clawpatrol") {
		return Check{
			Name: "clawpatrol", Status: StatusError,
			Detail:     "clawpatrol not found",
			Suggestion: "install ClawPatrol: curl -fsSL https://clawpatrol.dev/install.sh | sh",
		}
	}
	// `clawpatrol status` reports gateway/device health — whether join/login ran,
	// the CA is trusted, and the tunnel is healthy (clawpatrol.dev/docs/cli).
	// Exit 0 means the gateway is up; otherwise it is installed but not running.
	if _, err := prober.Run("clawpatrol", "status"); err != nil {
		return Check{
			Name: "clawpatrol", Status: StatusWarn,
			Detail:     "installed but gateway not running/healthy",
			Suggestion: "run `ai setup` to start the ClawPatrol gateway",
		}
	}
	return Check{Name: "clawpatrol", Status: StatusOK, Detail: "running"}
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
