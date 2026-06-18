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

func (realProber) LookPath(f string) (string, error) { return exec.LookPath(f) }
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
	DetectedAt     string           `json:"detected_at"`
}

// sandboxProber adapts a runtime.Prober to a sandbox.Prober.
type sandboxProber struct{ p Prober }

func (s sandboxProber) LookPath(f string) (string, error) { return s.p.LookPath(f) }
func (s sandboxProber) Exists(path string) bool           { return s.p.Exists(path) }

// Detect probes the host and builds an Info. detectedAt is an RFC 3339 UTC
// timestamp supplied by the caller. Returns ErrNoContainerRuntime / ErrMsbMissing
// for missing dependencies; rootless and virtualization shortfalls are recorded
// in the Info (see Verify), not returned as errors.
func Detect(goos, goarch string, p Prober, detectedAt string) (*Info, error) {
	name, rootless, err := detectContainerRuntime(p)
	if err != nil {
		return nil, err
	}
	sb := sandbox.Detect(goos, goarch, sandboxProber{p})
	if !sb.MsbInstalled {
		return nil, ErrMsbMissing
	}
	return &Info{
		SchemaVersion:  SchemaVersion,
		Detected:       name,
		Rootless:       rootless,
		Microsandbox:   MicrosandboxInfo{Available: sb.Available, Virtualization: sb.Virtualization},
		AIPlatformHost: os.Getenv("AI_PLATFORM_HOST"),
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

func detectContainerRuntime(p Prober) (name string, rootless bool, err error) {
	switch {
	case hasBinary(p, "docker"):
		return "docker", dockerRootless(p), nil
	case hasBinary(p, "podman"):
		// Podman is rootless by default; the full Runtime impl lands in Slice 6.
		return "podman", true, nil
	default:
		return "", false, ErrNoContainerRuntime
	}
}

func hasBinary(p Prober, name string) bool {
	_, err := p.LookPath(name)
	return err == nil
}

// dockerRootless reports whether the docker daemon is running rootless.
func dockerRootless(p Prober) bool {
	out, err := p.Run("docker", "info", "-f", "{{println .SecurityOptions}}")
	if err != nil {
		return false
	}
	return bytes.Contains(out, []byte("rootless"))
}

// Path returns config/runtime.json.
func Path() (string, error) {
	c, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(c, "runtime.json"), nil
}

// Persist atomically writes config/runtime.json.
func Persist(info *Info) error {
	p, err := Path()
	if err != nil {
		return err
	}
	return jsonfile.WriteAtomic(p, info)
}

// Load reads config/runtime.json, returning (nil, nil) if it has not been
// detected yet (a fresh install).
func Load() (*Info, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	var info Info
	if err := jsonfile.Read(p, &info); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &info, nil
}
