package doctor

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
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
func (prober fakeProber) Run(_ string, _ ...string) ([]byte, error) {
	return nil, exec.ErrNotFound
}
func (prober fakeProber) Exists(path string) bool { return prober.files[path] }

func checkByName(report Report, name string) Check {
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	return Check{}
}

// healthyServices is the service list a fully-up platform would supply (the CLI
// maps it from setup.ServicesStatus). It mirrors the real per-provider set,
// including the optional open-webui/odysseus, so the SERVICES section is covered.
func healthyServices() []Service {
	return []Service{
		{Name: "ollama", State: "running", Healthy: true},
		{Name: "presidio", State: "running", Healthy: true},
		{Name: "litellm", State: "running", Healthy: true},
		{Name: "headroom", State: "running", Healthy: true},
		{Name: "proxy", State: "running", Healthy: true},
		{Name: "dns", State: "running", Healthy: true},
		{Name: "open-webui", State: "disabled", Healthy: false, Optional: true},
		{Name: "odysseus", State: "disabled", Healthy: false, Optional: true},
	}
}

func TestServicesSectionListsEveryService(test *testing.T) {
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober:   fakeProber{bins: map[string]bool{"docker": true, "msb": true}},
		Services: healthyServices(),
	}
	report := Run(deps)
	// Every service supplied appears as a check — including the optional ones.
	for _, name := range []string{"ollama", "litellm", "headroom", "proxy", "dns", "open-webui", "odysseus"} {
		if checkByName(report, name).Name == "" {
			test.Errorf("doctor SERVICES section is missing %q", name)
		}
	}
	// A disabled optional service is a non-error warning, not a failure.
	openWebUI := checkByName(report, "open-webui")
	if openWebUI.Status != StatusWarn {
		test.Errorf("disabled open-webui status = %q, want warn", openWebUI.Status)
	}
	if !report.OK {
		test.Error("a disabled optional service must not fail the report")
	}
}

func TestOptionalServiceUnreachableWarns(test *testing.T) {
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{bins: map[string]bool{"docker": true, "msb": true}},
		Services: []Service{
			{Name: "open-webui", State: "stopped", Healthy: false, Optional: true},
		},
	}
	report := Run(deps)
	if got := checkByName(report, "open-webui").Status; got != StatusWarn {
		test.Errorf("unreachable optional open-webui → status = %q, want warn", got)
	}
	if !report.OK {
		test.Error("an unreachable optional service must not fail the report")
	}
}

func TestRequiredServiceDownIsError(test *testing.T) {
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{bins: map[string]bool{"docker": true, "msb": true}},
		Services: []Service{
			{Name: "ollama", State: "stopped", Healthy: false},
		},
	}
	report := Run(deps)
	if got := checkByName(report, "ollama").Status; got != StatusError {
		test.Errorf("down required ollama → status = %q, want error", got)
	}
	if report.OK {
		test.Error("a down required service must fail the report")
	}
}

func TestRunAllHealthy(test *testing.T) {
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober:   fakeProber{bins: map[string]bool{"docker": true, "msb": true}},
		Services: healthyServices(),
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

func TestDomainSectionStandaloneUpToDate(test *testing.T) {
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{bins: map[string]bool{"docker": true, "msb": true}},
		Domain: &DomainInfo{
			Domain: "aip.local", Role: "standalone", Standalone: true,
			HostsPresent: true, HostsUpToDate: true,
			URLs: []DomainURL{{Service: "litellm", Host: "litellm.aip.local", URL: "http://litellm.aip.local:18787"}},
		},
	}
	report := Run(deps)
	if checkByName(report, "platform domain").Detail != "aip.local" {
		test.Errorf("platform domain detail: %+v", checkByName(report, "platform domain"))
	}
	if checkByName(report, "UI litellm.aip.local").Status != StatusOK {
		test.Errorf("UI URL check missing/not ok: %+v", checkByName(report, "UI litellm.aip.local"))
	}
	if checkByName(report, "UI subdomain resolution").Status != StatusOK {
		test.Errorf("resolution should be ok when present + up to date: %+v", checkByName(report, "UI subdomain resolution"))
	}
}

func TestDomainSectionStandaloneMissingWarns(test *testing.T) {
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{bins: map[string]bool{"docker": true, "msb": true}},
		Domain: &DomainInfo{Domain: "aip.local", Role: "standalone", Standalone: true},
	}
	report := Run(deps)
	resolution := checkByName(report, "UI subdomain resolution")
	if resolution.Status != StatusWarn {
		test.Errorf("missing hosts block should warn: %+v", resolution)
	}
	// A warning must NOT fail the report (doctor always exits 0 with a report).
	if !report.OK {
		test.Error("a hosts-resolution warning must not fail the report")
	}
}

func TestDomainSectionServerReminder(test *testing.T) {
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{bins: map[string]bool{"docker": true, "msb": true}},
		Domain: &DomainInfo{
			Domain: "aip.example.com", Role: "server",
			ServerReminder:    "create DNS records (*.aip.example.com) → this server's IP, and terminate TLS at nginx",
			ServerCredentials: "LiteLLM admin UI — http://litellm.aip.example.com:18787/ui — ai litellm password",
		},
	}
	report := Run(deps)
	resolution := checkByName(report, "UI subdomain resolution")
	if resolution.Status != StatusWarn || !strings.Contains(resolution.Suggestion, "DNS") {
		test.Errorf("server reminder missing: %+v", resolution)
	}
	credentials := checkByName(report, "UI admin access")
	if credentials.Status != StatusWarn || !strings.Contains(credentials.Suggestion, "ai litellm password") {
		test.Errorf("server credentials check missing: %+v", credentials)
	}
}

func TestHumanShowsServiceEndpoints(test *testing.T) {
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober:   fakeProber{bins: map[string]bool{"docker": true, "msb": true}},
		Services: healthyServices(),
	}
	rendered := Run(deps).Human()
	// A healthy litellm shows its nginx subdomain address and admin-UI URL on the
	// check line (the gateway port, NOT the internal-only :14000).
	if !strings.Contains(rendered, "litellm") ||
		!strings.Contains(rendered, "http://litellm.localhost:18787 (UI http://litellm.localhost:18787/ui)") {
		test.Errorf("litellm endpoint missing from doctor output:\n%s", rendered)
	}
	// Ollama shows its host-CLI gateway path (no UI), NOT the internal-only :11434.
	if !strings.Contains(rendered, "ollama") || !strings.Contains(rendered, "http://localhost:18787/ollama") {
		test.Errorf("ollama address missing from doctor output:\n%s", rendered)
	}
}

func TestWorkspaceSectionOmittedWhenAbsent(test *testing.T) {
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober:   fakeProber{bins: map[string]bool{"docker": true, "msb": true}},
		Services: healthyServices(),
	}
	report := Run(deps)
	for _, name := range []string{"workspace rootless", "workspace virtualization", "workspace runtime"} {
		if checkByName(report, name).Name != "" {
			test.Errorf("workspace section should be omitted when Workspace is nil, found %q", name)
		}
	}
}

func TestWorkspaceSectionRendersWhenPresent(test *testing.T) {
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober:   fakeProber{bins: map[string]bool{"docker": true, "msb": true}},
		Services: healthyServices(),
		Workspace: &WorkspaceRuntime{
			Project: "demo", Rootless: true, Virtualization: "hvf", Available: true,
		},
	}
	report := Run(deps)
	if got := checkByName(report, "workspace rootless").Status; got != StatusOK {
		test.Errorf("workspace rootless = %q, want ok", got)
	}
	if got := checkByName(report, "workspace virtualization").Status; got != StatusOK {
		test.Errorf("workspace virtualization = %q, want ok", got)
	}
	if !report.OK {
		test.Error("a healthy workspace section must not fail the report")
	}
}

// A missing-runtime / virtualization shortfall is folded into error Checks rather
// than aborting (the old `ai workspace doctor` exited 3/4 here).
func TestWorkspaceSectionFoldsFailuresIntoChecks(test *testing.T) {
	missingRuntime := Run(Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober:    fakeProber{bins: map[string]bool{"docker": true, "msb": true}},
		Workspace: &WorkspaceRuntime{Project: "demo", RuntimeErr: errors.New("no container runtime")},
	})
	if got := checkByName(missingRuntime, "workspace runtime").Status; got != StatusError {
		test.Errorf("missing runtime → workspace runtime check = %q, want error", got)
	}
	if missingRuntime.OK {
		test.Error("a missing workspace runtime must fail the report (as a check, not a non-zero exit)")
	}

	shortfall := Run(Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{bins: map[string]bool{"docker": true, "msb": true}},
		Workspace: &WorkspaceRuntime{
			Project: "demo", Rootless: false, Available: false,
			VerifyErr: errors.New("virtualization unavailable"),
		},
	})
	if got := checkByName(shortfall, "workspace rootless").Status; got != StatusError {
		test.Errorf("non-rootless → workspace rootless check = %q, want error", got)
	}
	if got := checkByName(shortfall, "workspace virtualization").Status; got != StatusError {
		test.Errorf("unavailable virt → workspace virtualization check = %q, want error", got)
	}
}

func TestRunMissingDepsError(test *testing.T) {
	deps := Deps{
		GOOS: "linux", GOARCH: "amd64",
		Prober: fakeProber{}, // nothing installed, no /dev/kvm
	}
	report := Run(deps)
	if report.OK {
		test.Fatal("expected not-OK with missing deps")
	}
	for _, name := range []string{"container runtime", "microsandbox runtime", "host virtualization"} {
		check := checkByName(report, name)
		if check.Status != StatusError {
			test.Errorf("%s status = %q, want error", name, check.Status)
		}
		if check.Suggestion == "" {
			test.Errorf("%s should carry a repair suggestion", name)
		}
	}
}

// TestUnsupportedOSReportsVirtualizationUnavailable: only macOS and Linux are
// supported hosts, so any other GOOS reports virtualization unavailable.
func TestUnsupportedOSReportsVirtualizationUnavailable(test *testing.T) {
	deps := Deps{
		GOOS: "plan9", GOARCH: "amd64",
		Prober: fakeProber{bins: map[string]bool{"docker": true, "msb": true}},
	}
	virtualization := checkByName(Run(deps), "host virtualization")
	if virtualization.Status != StatusError {
		test.Fatalf("unsupported OS should report virtualization unavailable: %+v", virtualization)
	}
}

func TestRootlessCheckMacDockerDesktop(test *testing.T) {
	// Docker Desktop on macOS reports no "rootless" SecurityOption but runs in a
	// VM — the rootless check must pass (§6.1).
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{bins: map[string]bool{"docker": true, "msb": true}},
	}
	if got := checkByName(Run(deps), "rootless service tier").Status; got != StatusOK {
		test.Fatalf("rootless service tier on macOS Docker Desktop = %q, want ok", got)
	}
}

func TestMissingDepSuggestionsAreCopyPasteable(test *testing.T) {
	deps := Deps{
		GOOS: "linux", GOARCH: "amd64",
		Prober: fakeProber{}, // nothing installed
	}
	report := Run(deps)
	wants := map[string]string{
		"microsandbox runtime": "curl -sSL https://get.microsandbox.dev | sh",
		"container runtime":    "https://get.docker.com",
	}
	for name, fragment := range wants {
		if got := checkByName(report, name).Suggestion; !strings.Contains(got, fragment) {
			test.Errorf("%s suggestion %q should contain %q", name, got, fragment)
		}
	}
}
