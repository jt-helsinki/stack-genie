package doctor

import (
	"errors"
	"os/exec"
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

func TestRunAllHealthy(test *testing.T) {
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{bins: map[string]bool{"docker": true, "msb": true, "clawpatrol": true}},
		Model:  fakeModel{healthy: true},
	}
	report := Run(deps)
	if !report.OK {
		test.Fatalf("expected healthy, got %+v", report)
	}
	if got := checkByName(report, "container runtime").Detail; got != "docker" {
		test.Errorf("container detail = %q", got)
	}
	if checkByName(report, "host virtualization").Detail != "hvf" {
		test.Errorf("virtualization: %+v", checkByName(report, "host virtualization"))
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
