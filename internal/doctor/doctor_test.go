package doctor

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/litellm"
)

type fakeProber struct {
	bins  map[string]bool
	files map[string]bool
}

func (prober fakeProber) LookPath(file string) (string, error) {
	if prober.bins[file] {
		return "/usr/bin/" + file, nil
	}
	return "", exec.ErrNotFound
}
func (prober fakeProber) Run(string, ...string) ([]byte, error) { return nil, exec.ErrNotFound }
func (prober fakeProber) Exists(path string) bool               { return prober.files[path] }

type fakeModel struct {
	healthy bool
	err     error
}

func (model fakeModel) Status() (litellm.StatusInfo, error) {
	return litellm.StatusInfo{Healthy: model.healthy}, model.err
}
func (fakeModel) Test(string) (litellm.TestResult, error) { return litellm.TestResult{}, nil }

func checkByName(report Report, name string) Check {
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	return Check{}
}

type fakeOllama struct{ err error }

func (probe fakeOllama) Reachable() error { return probe.err }

func TestRunAllHealthy(test *testing.T) {
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{bins: map[string]bool{"docker": true, "msb": true, "clawpatrol": true}},
		Model:  fakeModel{healthy: true},
		Ollama: fakeOllama{},
	}
	report := Run(deps)
	if !report.OK {
		test.Fatalf("expected healthy, got %+v", report)
	}
	if checkByName(report, "ollama").Status != StatusOK {
		test.Errorf("ollama: %+v", checkByName(report, "ollama"))
	}
	if got := checkByName(report, "container runtime").Detail; got != "docker" {
		test.Errorf("container detail = %q", got)
	}
	if checkByName(report, "host virtualization").Detail != "hvf" {
		test.Errorf("virtualization: %+v", checkByName(report, "host virtualization"))
	}
}

func TestOllamaRequiredCheck(test *testing.T) {
	base := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{bins: map[string]bool{"docker": true, "msb": true, "clawpatrol": true}},
		Model:  fakeModel{healthy: true},
	}

	// No probe → warn (not yet checked), but the report stays OK.
	report := Run(base)
	if got := checkByName(report, "ollama").Status; got != StatusWarn {
		test.Errorf("nil probe → ollama status = %q, want warn", got)
	}
	if !report.OK {
		test.Error("a not-yet-checked Ollama must not fail the report")
	}

	// Required but unreachable → error (it is on the model path).
	base.Ollama = fakeOllama{err: errors.New("connection refused")}
	report = Run(base)
	if got := checkByName(report, "ollama").Status; got != StatusError {
		test.Errorf("unreachable ollama → status = %q, want error", got)
	}
	if report.OK {
		test.Error("an unreachable required Ollama must fail the report")
	}
}

func TestRunMissingDepsError(test *testing.T) {
	deps := Deps{
		GOOS: "linux", GOARCH: "amd64",
		Prober: fakeProber{}, // nothing installed, no /dev/kvm
		Model:  fakeModel{healthy: false},
	}
	report := Run(deps)
	if report.OK {
		test.Fatal("expected not-OK with missing deps")
	}
	for _, name := range []string{"container runtime", "microsandbox runtime", "host virtualization", "clawpatrol"} {
		check := checkByName(report, name)
		if check.Status != StatusError {
			test.Errorf("%s status = %q, want error", name, check.Status)
		}
		if check.Suggestion == "" {
			test.Errorf("%s should carry a repair suggestion", name)
		}
	}
	// LiteLLM unreachable is a warning, not an error.
	if checkByName(report, "litellm").Status != StatusWarn {
		test.Errorf("litellm should warn, got %q", checkByName(report, "litellm").Status)
	}
}

// TestWindowsVirtualizationReportsWSL2 is the [S7] requirement that `doctor`
// clearly reports when WSL2 nested virtualization is unavailable on Windows.
func TestWindowsVirtualizationReportsWSL2(test *testing.T) {
	deps := Deps{
		GOOS: "windows", GOARCH: "amd64",
		Prober: fakeProber{bins: map[string]bool{"docker": true, "msb": true}}, // no /dev/kvm
		Model:  fakeModel{healthy: true},
	}
	virtualization := checkByName(Run(deps), "host virtualization")
	if virtualization.Status != StatusError {
		test.Fatalf("windows without nested virt should error: %+v", virtualization)
	}
	if !strings.Contains(virtualization.Suggestion, "WSL2") {
		test.Errorf("windows suggestion should name WSL2: %q", virtualization.Suggestion)
	}
}

func TestRootlessCheckMacDockerDesktop(test *testing.T) {
	// Docker Desktop on macOS reports no "rootless" SecurityOption but runs in a
	// VM — the rootless check must pass (§6.1).
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{bins: map[string]bool{"docker": true, "msb": true}},
		Model:  fakeModel{healthy: true},
	}
	if got := checkByName(Run(deps), "rootless service tier").Status; got != StatusOK {
		test.Fatalf("rootless service tier on macOS Docker Desktop = %q, want ok", got)
	}
}

func TestMissingDepSuggestionsAreCopyPasteable(test *testing.T) {
	deps := Deps{
		GOOS: "linux", GOARCH: "amd64",
		Prober: fakeProber{}, // nothing installed
		Model:  fakeModel{healthy: false},
	}
	report := Run(deps)
	wants := map[string]string{
		"microsandbox runtime": "curl -fsSL https://install.microsandbox.dev | sh",
		"clawpatrol":           "curl -fsSL https://clawpatrol.dev/install.sh | sh",
		"container runtime":    "https://get.docker.com",
	}
	for name, fragment := range wants {
		if got := checkByName(report, name).Suggestion; !strings.Contains(got, fragment) {
			test.Errorf("%s suggestion %q should contain %q", name, got, fragment)
		}
	}
}

func TestLitellmErrorIsWarn(test *testing.T) {
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{bins: map[string]bool{"docker": true, "msb": true, "clawpatrol": true}},
		Model:  fakeModel{err: errors.New("connection refused")},
	}
	report := Run(deps)
	if !report.OK { // a warn does not flip OK
		test.Fatalf("warn should not make report not-OK: %+v", report)
	}
	if checkByName(report, "litellm").Status != StatusWarn {
		test.Errorf("litellm should warn on error")
	}
}
