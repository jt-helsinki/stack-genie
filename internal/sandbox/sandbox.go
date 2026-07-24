// Package sandbox detects the Microsandbox microVM runtime and host
// virtualization (arch §6.2). Detection is parameterized by GOOS/GOARCH and a
// prober so it is unit-testable on any host.
package sandbox

import (
	"path/filepath"

	"github.com/jt-helsinki/stack-genie/internal/paths"
)

// Prober abstracts host probes so detection can be faked in tests.
type Prober interface {
	LookPath(file string) (string, error)
	Exists(path string) bool
}

// msbInstalled reports whether an `msb` CLI is available: on PATH, or as the
// platform-managed copy at ~/.ai-platform/bin/msb (which the platform pins + downloads
// to match the embedded SDK FFI). Read-only — it never installs.
func msbInstalled(prober Prober) bool {
	if _, err := prober.LookPath("msb"); err == nil {
		return true
	}
	if binDir, err := paths.BinDir(); err == nil {
		return prober.Exists(filepath.Join(binDir, "msb"))
	}
	return false
}

// Info is the detected sandbox-runtime state.
type Info struct {
	MsbInstalled   bool
	Virtualization string // "hvf" | "kvm" | ""
	Available      bool   // virtualization usable for microVMs
}

// Detect reports whether the Microsandbox runtime (`msb`) is installed and
// whether the host can run microVMs. Supported hosts are macOS and Linux:
//
//   - macOS: requires Apple Silicon (Apple Hypervisor); Intel is unsupported
//   - Linux: requires /dev/kvm
//
// Any other GOOS is unsupported (Available stays false).
func Detect(goos, goarch string, prober Prober) Info {
	var info Info
	// msb is considered installed if it is on PATH OR the platform-managed copy exists at
	// ~/.ai-platform/bin/msb (the platform pins + downloads that build to keep it in
	// lockstep with the embedded SDK FFI, so a host need not have a PATH `msb` at all).
	info.MsbInstalled = msbInstalled(prober)
	switch goos {
	case "darwin":
		if goarch == "arm64" {
			info.Virtualization = "hvf"
			info.Available = true
		}
		// Intel macOS: no virtualization (arch §6.2).
	case "linux":
		info.Virtualization = "kvm"
		info.Available = prober.Exists("/dev/kvm")
		// Any other GOOS: unsupported — Available stays false.
	}
	return info
}
