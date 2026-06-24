// Package runtime detects the container runtime for the service tier
// (Docker/Podman) and composes it with sandbox virtualization detection into
// config/runtime.yaml (repo-layout §12.5, arch §6).
//
// Detection is parameterized by GOOS/GOARCH and a Prober so it is unit-testable
// on any host. Missing dependencies surface as sentinel errors (→ exit 3);
// rootless/virtualization shortfalls are recorded and enforced by Verify
// (→ exit 4).
package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/conffile"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"github.com/jt-helsinki/ideal-robot/internal/sandbox"
)

// DefaultGatewayHost is the in-microVM address that resolves to the host machine
// (Microsandbox's host-side network stack). It is the standalone/local gateway
// host: when no AI_PLATFORM_HOST is configured, every workspace reaches the model
// gateway on the local host through this name (arch §29.2).
const DefaultGatewayHost = "host.microsandbox.internal"

// DefaultGatewayPort is the host port the in-workspace Headroom proxy reaches the
// model gateway on (arch §29.2). It is the fallback port when AIPlatformHost
// carries only a host with no ":port".
const DefaultGatewayPort = 18787

// SchemaVersion is stamped on config/runtime.yaml.
const SchemaVersion = 1

// Deployment roles (CLI §2.1). The role decides which tier runs on this host:
// standalone runs the full service tier + workspaces locally; server runs only
// the shared service tier (bound to 0.0.0.0 for remote clients, no workspaces);
// client runs only workspaces and routes to a remote server's service tier.
const (
	RoleStandalone = "standalone"
	RoleServer     = "server"
	RoleClient     = "client"
)

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

// MicrosandboxInfo mirrors runtime.yaml's microsandbox object (§12.5). It carries
// both json and yaml tags: yaml for config/runtime.yaml on disk, json for the
// --json output envelope (it rides inside setup.Report).
type MicrosandboxInfo struct {
	Available      bool   `json:"available" yaml:"available"`
	Virtualization string `json:"virtualization" yaml:"virtualization"`
}

// Info is config/runtime.yaml (§12.5). It carries both json and yaml tags: yaml
// for the on-disk file, json for the --json output envelope (setup.Report
// embeds it), so the persisted snake_case keys are preserved in both encodings.
type Info struct {
	SchemaVersion int              `json:"schema_version" yaml:"schema_version"`
	Role          string           `json:"role,omitempty" yaml:"role,omitempty"` // standalone | server | client (set by setup, not Detect)
	Detected      string           `json:"detected" yaml:"detected"`             // docker | podman
	Rootless      bool             `json:"rootless" yaml:"rootless"`
	Microsandbox  MicrosandboxInfo `json:"microsandbox" yaml:"microsandbox"`
	// OptionalServices is the set of opt-in host services this host runs (e.g.
	// "open-webui"). CORE services are always reconciled; OPTIONAL ones are
	// reconciled only when enabled here. Chosen interactively at `ai setup` and
	// persisted machine-wide so it survives across runs.
	OptionalServices []string `json:"optional_services,omitempty" yaml:"optional_services,omitempty"`
	AIPlatformHost   string   `json:"ai_platform_host" yaml:"ai_platform_host"`
	HostGateway      string   `json:"host_gateway" yaml:"host_gateway"` // guest-visible host address (arch §29.2)
	DetectedAt       string   `json:"detected_at" yaml:"detected_at"`
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

// ResolveGateway turns an AIPlatformHost value into the concrete (host, port,
// url) a workspace microVM uses to reach the model gateway (arch §29.2). The
// address format is a bare host or "host:port" — NOT a URL. Resolution:
//   - empty            → DefaultGatewayHost : DefaultGatewayPort (standalone/local)
//   - "host"           → host : DefaultGatewayPort
//   - "host:port"      → host : port (port must be a positive integer)
//
// An unparseable ":port" falls back to DefaultGatewayPort on the given host.
// url is always "http://<host>:<port>/v1" (the /v1 suffix opencode and pi need).
func ResolveGateway(aiPlatformHost string) (host string, port int, url string) {
	host = DefaultGatewayHost
	port = DefaultGatewayPort
	address := strings.TrimSpace(aiPlatformHost)
	if address != "" {
		if hostPart, portPart, ok := strings.Cut(address, ":"); ok {
			host = hostPart
			if parsed, err := strconv.Atoi(portPart); err == nil && parsed > 0 {
				port = parsed
			}
		} else {
			host = address
		}
	}
	url = fmt.Sprintf("http://%s:%d/v1", host, port)
	return host, port, url
}

// HostGateway is the SDK-PINNED override for the address a workspace reaches the
// host at (arch §29.2) — distinct from the working default. Egress + gateway
// wiring already use the verified `host.microsandbox.internal:18787`
// (egress.hostGatewayTarget / runtime.ResolveGateway); HostGateway exists only so
// a future hardware-bring-up spike can pin a backend-specific value from the
// Microsandbox SDK (HVF on macOS, KVM on Linux) and confirm it with a connectivity
// probe. Until that spike runs it reports ("", false) and callers use the default.
// It is a deliberate reserved seam, NOT dead code — do not remove without a
// runtime.yaml migration (Info.HostGateway is a persisted field).
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
	// The empty role requires the full dependency set (no role-specific tolerance),
	// matching the historical standalone behavior; setup sets the concrete role.
	return DetectForRole(goos, goarch, "", prober, detectedAt)
}

// DetectForRole probes the host like Detect but only requires the dependencies
// the deployment role actually uses (CLI §2.1): a server runs only the service
// tier (no microVM runtime, so a missing msb is tolerated and recorded as
// unavailable), and a client runs only workspaces (no service tier, so a missing
// container runtime is tolerated and recorded empty). standalone (the default,
// and the empty-role case) requires everything, exactly like Detect. The role is
// recorded on the returned Info; missing dependencies that the role does need
// still return their sentinel error.
func DetectForRole(goos, goarch, role string, prober Prober, detectedAt string) (*Info, error) {
	var name string
	var rootless bool
	containerRuntime, detectedRootless, runtimeErr := DetectContainerRuntime(goos, prober)
	if runtimeErr != nil {
		// A client runs no service tier, so a missing container runtime is fine.
		if !errors.Is(runtimeErr, ErrNoContainerRuntime) || role != RoleClient {
			return nil, runtimeErr
		}
	} else {
		name = containerRuntime.Name
		rootless = detectedRootless
	}

	detectedSandbox := sandbox.Detect(goos, goarch, sandboxAdapter{prober: prober})
	// A server runs no microVMs, so a missing msb is fine.
	if !detectedSandbox.MsbInstalled && role != RoleServer {
		return nil, ErrMsbMissing
	}

	hostGateway, _ := HostGateway(goos) // "" until pinned on hardware (§29.2)
	return &Info{
		SchemaVersion:  SchemaVersion,
		Role:           role,
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
// (The live service-tier launch builds its argv directly in internal/setup;
// this helper is the shared docker|podman run prefix.)
func (containerRuntime ContainerRuntime) RunArgs(name, imageRef string, extra ...string) []string {
	args := []string{"run", "-d", "--name", name}
	args = append(args, extra...)
	return append(args, imageRef)
}

// ContainerRuntimeName picks the service-tier container CLI by presence,
// preferring Docker then Podman, without probing rootless state. It is what the
// workspace build needs (the binary name only). Returns ErrNoContainerRuntime
// when neither is installed.
func ContainerRuntimeName(prober Prober) (ContainerRuntime, error) {
	switch {
	case hasBinary(prober, "docker"):
		return ContainerRuntime{Name: "docker"}, nil
	case hasBinary(prober, "podman"):
		return ContainerRuntime{Name: "podman"}, nil
	default:
		return ContainerRuntime{}, ErrNoContainerRuntime
	}
}

// DetectContainerRuntime resolves the container runtime and whether it runs
// rootless on this host (arch §6.1, §6.3). goos matters: Docker Desktop on macOS
// runs the engine inside a managed VM, so there is no rooted daemon on the host —
// that is rootless-equivalent for the platform's purposes.
func DetectContainerRuntime(goos string, prober Prober) (containerRuntime ContainerRuntime, rootless bool, err error) {
	containerRuntime, err = ContainerRuntimeName(prober)
	if err != nil {
		return ContainerRuntime{}, false, err
	}
	switch containerRuntime.Name {
	case "docker":
		return containerRuntime, dockerRootless(goos, prober), nil
	case "podman":
		return containerRuntime, podmanRootless(prober), nil
	default:
		return containerRuntime, false, nil
	}
}

func hasBinary(prober Prober, name string) bool {
	_, err := prober.LookPath(name)
	return err == nil
}

// dockerRootless reports whether Docker satisfies the platform's no-rooted-host-
// daemon requirement (§6.1). On macOS, Docker Desktop runs the engine inside a
// managed Linux VM — there is no rooted dockerd on the host and no host socket
// exposure — so it qualifies regardless of the engine's SecurityOptions (the
// `rootless` marker is a Linux rootless-engine flag and is absent there). On
// Linux it must be a genuine rootless engine, detected via `docker info`.
func dockerRootless(goos string, prober Prober) bool {
	if goos == "darwin" {
		return true
	}
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

// Path returns config/runtime.yaml.
func Path() (string, error) {
	configDir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "runtime.yaml"), nil
}

// Persist atomically writes config/runtime.yaml.
func Persist(info *Info) error {
	path, err := Path()
	if err != nil {
		return err
	}
	return conffile.WriteAtomic(path, info)
}

// Load reads config/runtime.yaml, returning (nil, nil) if it has not been
// detected yet (a fresh install).
func Load() (*Info, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	var info Info
	if err := conffile.Read(path, &info); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &info, nil
}
