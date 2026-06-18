// Package sandbox detects the Microsandbox microVM runtime and host
// virtualization (arch §6.2). Detection is parameterized by GOOS/GOARCH and a
// prober so it is unit-testable on any host.
package sandbox

// Prober abstracts host probes so detection can be faked in tests.
type Prober interface {
	LookPath(file string) (string, error)
	Exists(path string) bool
}

// Info is the detected sandbox-runtime state.
type Info struct {
	MsbInstalled   bool
	Virtualization string // "hvf" | "kvm" | "wsl2" | ""
	Available      bool   // virtualization usable for microVMs
}

// Detect reports whether the Microsandbox runtime (`msb`) is installed and
// whether the host can run microVMs:
//
//   - macOS: requires Apple Silicon (Apple Hypervisor); Intel is unsupported
//   - Linux: requires /dev/kvm
//   - Windows: WSL2 with nested virtualization (exposes /dev/kvm in the guest)
func Detect(goos, goarch string, p Prober) Info {
	var info Info
	if _, err := p.LookPath("msb"); err == nil {
		info.MsbInstalled = true
	}
	switch goos {
	case "darwin":
		if goarch == "arm64" {
			info.Virtualization = "hvf"
			info.Available = true
		}
		// Intel macOS: no virtualization (arch §6.2).
	case "linux":
		info.Virtualization = "kvm"
		info.Available = p.Exists("/dev/kvm")
	case "windows":
		info.Virtualization = "wsl2"
		info.Available = p.Exists("/dev/kvm")
	}
	return info
}
