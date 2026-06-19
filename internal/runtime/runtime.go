// Package runtime detects the container runtime for the service tier
// (Docker/Podman) and composes it with sandbox virtualization detection into
// config/runtime.json (repo-layout §12.5, arch §6).
//
// Detection is parameterized by GOOS/GOARCH and a Prober so it is unit-testable
// on any host. Missing dependencies surface as sentinel errors (→ exit 3);
// rootless/virtualization shortfalls are recorded and enforced by Verify
// (→ exit 4).
package runtime

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/jsonfile"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"github.com/jt-helsinki/ideal-robot/internal/sandbox"
)

// SchemaVersion is stamped on config/runtime.json.
const SchemaVersion = 1

// Sentinel errors. Detect returns the missing-dependency ones (exit 3); Verify
// returns the capability ones (exit 4).
var (
	ErrNoContainerRuntime        = errors.New("no container runtime (docker or podman) found")
	ErrMsbMissing                = errors.New("microsandbox (msb) is not installed")
	ErrRootlessUnavailable       = errors.New("rootless container runtime is unavailable")
	ErrVirtualizationUnavailable = errors.New("host virtualization for microVMs is unavailable")
)

// Prober abstracts host probes (binaries, command output, files) for testability.
type Prober interface {
	LookPath(file string) (string, error)
	Run(name string, args ...string) ([]byte, error)
	Exists(path string) bool
}

type realProber struct{}

func (realProber) LookPath(file string) (string, error) { return exec.LookPath(file) }
func (realProber) Run(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}
func (realProber) Exists(path string) bool { _, err := os.Stat(path); return err == nil }

// RealProber probes the actual host.
func RealProber() Prober { return realProber{} }

// MicrosandboxInfo mirrors runtime.json's microsandbox object (§12.5).
type MicrosandboxInfo struct {
	Available      bool   `json:"available"`
	Virtualization string `json:"virtualization"`
}

// Info is config/runtime.json (§12.5).
type Info struct {
	SchemaVersion  int              `json:"schema_version"`
	Detected       string           `json:"detected"` // docker | podman
	Rootless       bool             `json:"rootless"`
	Microsandbox   MicrosandboxInfo `json:"microsandbox"`
	AIPlatformHost string           `json:"ai_platform_host"`
	HostGateway    string           `json:"host_gateway"` // guest-visible host address (arch §29.2)
	DetectedAt     string           `json:"detected_at"`
}

// HostAddress returns the address a workspace microVM uses to reach the host
// (arch §29.2): the AI_PLATFORM_HOST environment override when set (e.g. the
// acceptance harness), otherwise the resolved host gateway.
func (info *Info) HostAddress() string {
	if info.AIPlatformHost != "" {
		return info.AIPlatformHost
	}
	return info.HostGateway
}

// HostGateway resolves the gateway address of Microsandbox's host-side userspace
// network stack — the address a workspace reaches the host at (arch §29.2). The
// concrete value is fixed by the Microsandbox network backend and is pinned from
// the SDK during hardware bring-up and confirmed by a connectivity probe; until
// then it reports ("", false). goos selects the backend (HVF/KVM/WSL2).
func HostGateway(goos string) (address string, pinned bool) {
	// Deferred: filled in by the §29.5 reachability spike on a provisioned host.
	return "", false
}

// sandboxAdapter adapts a runtime.Prober to a sandbox.Prober.
type sandboxAdapter struct{ prober Prober }

func (adapter sandboxAdapter) LookPath(file string) (string, error) {
	return adapter.prober.LookPath(file)
}
func (adapter sandboxAdapter) Exists(path string) bool { return adapter.prober.Exists(path) }

// Detect probes the host and builds an Info. detectedAt is an RFC 3339 UTC
// timestamp supplied by the caller. Returns ErrNoContainerRuntime / ErrMsbMissing
// for missing dependencies; rootless and virtualization shortfalls are recorded
// in the Info (see Verify), not returned as errors.
func Detect(goos, goarch string, prober Prober, detectedAt string) (*Info, error) {
	name, rootless, err := detectContainerRuntime(prober)
	if err != nil {
		return nil, err
	}
	detectedSandbox := sandbox.Detect(goos, goarch, sandboxAdapter{prober: prober})
	if !detectedSandbox.MsbInstalled {
		return nil, ErrMsbMissing
	}
	hostGateway, _ := HostGateway(goos) // "" until pinned on hardware (§29.2)
	return &Info{
		SchemaVersion:  SchemaVersion,
		Detected:       name,
		Rootless:       rootless,
		Microsandbox:   MicrosandboxInfo{Available: detectedSandbox.Available, Virtualization: detectedSandbox.Virtualization},
		AIPlatformHost: os.Getenv("AI_PLATFORM_HOST"),
		HostGateway:    hostGateway,
		DetectedAt:     detectedAt,
	}, nil
}

// Verify enforces the Slice 1 requirements: a rootless service tier and usable
// microVM virtualization (arch §6, "no silent degraded mode"). Returns a
// sentinel error (→ exit 4) or nil.
func Verify(info *Info) error {
	if !info.Rootless {
		return ErrRootlessUnavailable
	}
	if !info.Microsandbox.Available {
		return ErrVirtualizationUnavailable
	}
	return nil
}

// ContainerRuntime is the detected service-tier container CLI (arch §6.1). Docker
// and Podman are CLI-compatible across the build/run subset the platform uses, so
// this wraps the binary name and yields the matching invocations. It is the single
// source of truth for "which binary, called how" shared by the workspace image
// build (internal/workspace) and the service-tier launch (internal/setup), so the
// platform never hardcodes "docker" (Slice 6).
type ContainerRuntime struct {
	Name string // "docker" | "podman"
}

// BuildArgs returns the argv (after the binary name) that builds imageRef from
// dockerfile within contextDir. Docker and Podman share this syntax.
func (containerRuntime ContainerRuntime) BuildArgs(imageRef, dockerfile, contextDir string) []string {
	return []string{"build", "-t", imageRef, "-f", dockerfile, contextDir}
}

// RunArgs returns the argv that runs imageRef detached as the named container.
// Consumed by the service-tier launch wired during hardware bring-up.
func (containerRuntime ContainerRuntime) RunArgs(name, imageRef string, extra ...string) []string {
	args := []string{"run", "-d", "--name", name}
	args = append(args, extra...)
	return append(args, imageRef)
}

// DetectContainerRuntime probes for the service-tier container runtime, preferring
// Docker, then Podman, and reports whether it runs rootless (arch §6.1, §6.3).
// Returns ErrNoContainerRuntime when neither is installed.
func DetectContainerRuntime(prober Prober) (containerRuntime ContainerRuntime, rootless bool, err error) {
	switch {
	case hasBinary(prober, "docker"):
		return ContainerRuntime{Name: "docker"}, dockerRootless(prober), nil
	case hasBinary(prober, "podman"):
		return ContainerRuntime{Name: "podman"}, podmanRootless(prober), nil
	default:
		return ContainerRuntime{}, false, ErrNoContainerRuntime
	}
}

func detectContainerRuntime(prober Prober) (name string, rootless bool, err error) {
	containerRuntime, rootless, err := DetectContainerRuntime(prober)
	return containerRuntime.Name, rootless, err
}

func hasBinary(prober Prober, name string) bool {
	_, err := prober.LookPath(name)
	return err == nil
}

// dockerRootless reports whether the docker daemon is running rootless.
func dockerRootless(prober Prober) bool {
	output, err := prober.Run("docker", "info", "-f", "{{println .SecurityOptions}}")
	if err != nil {
		return false
	}
	return bytes.Contains(output, []byte("rootless"))
}

// podmanRootless reports whether Podman runs rootless. Podman is daemonless and
// rootless by default for non-root users (arch §6.3), so we treat an explicit
// "false" from `podman info` as rootful and otherwise default to rootless (a
// probe hiccup must not block setup on a host where Podman's default holds).
func podmanRootless(prober Prober) bool {
	output, err := prober.Run("podman", "info", "--format", "{{.Host.Security.Rootless}}")
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(output)) != "false"
}

// Path returns config/runtime.json.
func Path() (string, error) {
	configDir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "runtime.json"), nil
}

// Persist atomically writes config/runtime.json.
func Persist(info *Info) error {
	path, err := Path()
	if err != nil {
		return err
	}
	return jsonfile.WriteAtomic(path, info)
}

// Load reads config/runtime.json, returning (nil, nil) if it has not been
// detected yet (a fresh install).
func Load() (*Info, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	var info Info
	if err := jsonfile.Read(path, &info); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &info, nil
}
