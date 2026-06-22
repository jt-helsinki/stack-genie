package setup

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/jsonfile"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/versions"
)

// --- fakes ------------------------------------------------------------------

type fakeProber struct {
	bins      map[string]bool
	files     map[string]bool
	dockerOut string
}

func (prober fakeProber) LookPath(file string) (string, error) {
	if prober.bins[file] {
		return "/usr/bin/" + file, nil
	}
	return "", exec.ErrNotFound
}
func (prober fakeProber) Run(name string, _ ...string) ([]byte, error) {
	if name == "docker" {
		return []byte(prober.dockerOut), nil
	}
	return nil, exec.ErrNotFound
}
func (prober fakeProber) Exists(path string) bool { return prober.files[path] }

type fakeServices struct {
	reconciled bool
	provider   string
}

func (services *fakeServices) Reconcile(providerConfig string, progress func(string)) ([]ServiceStatus, error) {
	services.reconciled = true
	services.provider = providerConfig
	if progress != nil {
		progress("reconciling (fake)")
	}
	return []ServiceStatus{{Name: "litellm", Mode: "container", State: "running", Healthy: true}}, nil
}
func (services *fakeServices) Status() ([]ServiceStatus, error) {
	return []ServiceStatus{{Name: "litellm", Mode: "container", State: "running", Healthy: true}}, nil
}
func (services *fakeServices) Control(action, service string) ([]ServiceStatus, error) {
	return []ServiceStatus{{Name: "litellm", Mode: "container", State: action + "ed"}}, nil
}

// healthyDeps returns Deps that pass preflight (Apple Silicon, rootless docker,
// msb installed) with fresh fakes.
func healthyDeps() (Deps, *fakeServices) {
	services := &fakeServices{}
	return Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{
			bins:      map[string]bool{"docker": true, "msb": true},
			dockerOut: "[name=seccomp name=rootless]",
		},
		Now:      func() string { return "2026-06-18T00:00:00Z" },
		Services: services,
	}, services
}

func exitCodeOf(test *testing.T, err error) int {
	test.Helper()
	var platformErr *output.Error
	if !errors.As(err, &platformErr) {
		test.Fatalf("error is not *output.Error: %v", err)
	}
	return platformErr.Code
}

// --- tests ------------------------------------------------------------------

func TestRunHappyPath(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	deps, services := healthyDeps()

	report, err := Run(Options{ProviderConfig: "prov.yaml"}, deps)
	if err != nil {
		test.Fatal(err)
	}
	if !services.reconciled || services.provider != "prov.yaml" {
		test.Fatalf("services not reconciled with provider config: %+v", services)
	}
	if !report.ConfigCreated || !report.VersionsCreated {
		test.Fatalf("report flags: %+v", report)
	}
	for _, path := range []string{
		filepath.Join(home, ".ai-platform", "config", "runtime.json"),
		filepath.Join(home, ".ai-platform", "config", "config.yaml"),
		filepath.Join(home, ".ai-platform", "config", "versions.json"),
		filepath.Join(home, ".ai-platform", "logs"),
	} {
		if _, err := os.Stat(path); err != nil {
			test.Errorf("expected %s to exist: %v", path, err)
		}
	}
}

func TestRunPreflightListsAllMissingPrerequisites(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()
	deps.GOOS, deps.GOARCH = "linux", "amd64"
	deps.Prober = fakeProber{} // nothing installed, no /dev/kvm

	_, err := Run(Options{}, deps)
	if got := exitCodeOf(test, err); got != output.ExitMissingDep {
		test.Fatalf("exit = %d, want %d (a missing program)", got, output.ExitMissingDep)
	}

	var platformErr *output.Error
	if !errors.As(err, &platformErr) {
		test.Fatalf("want *output.Error, got %T", err)
	}
	// The message lists every missing prerequisite with its install command.
	for _, fragment := range []string{
		"container runtime", "https://get.docker.com",
		"microsandbox runtime", "install.microsandbox.dev",
		"host virtualization",
	} {
		if !strings.Contains(platformErr.Message, fragment) {
			test.Errorf("error message missing %q:\n%s", fragment, platformErr.Message)
		}
	}
	// The structured list rides in error.details for --json consumers.
	details, ok := platformErr.Details.([]Prerequisite)
	if !ok || len(details) < 3 {
		test.Fatalf("details should be a []Prerequisite with the missing items, got %#v", platformErr.Details)
	}
}

func TestRunPreflightMissingDep(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()
	deps.Prober = fakeProber{bins: map[string]bool{"msb": true}} // no docker

	_, err := Run(Options{}, deps)
	if got := exitCodeOf(test, err); got != output.ExitMissingDep {
		test.Fatalf("exit = %d, want %d", got, output.ExitMissingDep)
	}
}

func TestRunPreflightRootlessUnavailable(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()
	// A rooted Linux docker engine ("rootless" absent from SecurityOptions). On
	// macOS, Docker Desktop is rootless-equivalent (VM), so this scenario
	// is Linux-specific.
	deps.GOOS, deps.GOARCH = "linux", "amd64"
	deps.Prober = fakeProber{
		bins:      map[string]bool{"docker": true, "msb": true},
		dockerOut: "[name=seccomp]", // not rootless
	}
	_, err := Run(Options{}, deps)
	if got := exitCodeOf(test, err); got != output.ExitRuntimeFailure {
		test.Fatalf("exit = %d, want %d", got, output.ExitRuntimeFailure)
	}
}

func TestRunIdempotent(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()

	first, err := Run(Options{}, deps)
	if err != nil {
		test.Fatal(err)
	}
	if !first.ConfigCreated || !first.VersionsCreated {
		test.Fatalf("first run should create defaults: %+v", first)
	}
	second, err := Run(Options{}, deps)
	if err != nil {
		test.Fatal(err)
	}
	if second.ConfigCreated || second.VersionsCreated {
		test.Fatalf("second run must not recreate defaults: %+v", second)
	}
}

func TestServicesStatusIncludesMicrosandbox(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()
	if _, err := Run(Options{}, deps); err != nil {
		test.Fatal(err)
	}

	statuses, err := ServicesStatus(deps)
	if err != nil {
		test.Fatal(err)
	}
	var foundMicrosandbox bool
	for _, status := range statuses {
		if status.Name == "microsandbox" {
			foundMicrosandbox = true
			if status.Mode != "runtime" || !status.Healthy || status.Detail != "hvf" {
				test.Fatalf("microsandbox status: %+v", status)
			}
		}
	}
	if !foundMicrosandbox {
		test.Fatal("microsandbox runtime missing from services status")
	}
}

func TestPreserveLiteLLMSecretsInEnv(test *testing.T) {
	test.Setenv("UI_PASSWORD", "")
	test.Setenv("LITELLM_MASTER_KEY", "")
	prober := fakeProber{dockerOut: "UI_USERNAME=admin\nUI_PASSWORD=hunter2\nLITELLM_MASTER_KEY=sk-abc\nOTHER=x\n"}
	preserveLiteLLMSecretsInEnv(prober, "docker")
	if got := os.Getenv("UI_PASSWORD"); got != "hunter2" {
		test.Errorf("UI_PASSWORD = %q, want preserved hunter2", got)
	}
	if got := os.Getenv("LITELLM_MASTER_KEY"); got != "sk-abc" {
		test.Errorf("LITELLM_MASTER_KEY = %q, want preserved sk-abc", got)
	}
}

func TestPreserveLiteLLMSecretsDoesNotOverrideCaller(test *testing.T) {
	test.Setenv("UI_PASSWORD", "caller-set")
	prober := fakeProber{dockerOut: "UI_PASSWORD=from-container\n"}
	preserveLiteLLMSecretsInEnv(prober, "docker")
	if got := os.Getenv("UI_PASSWORD"); got != "caller-set" {
		test.Errorf("a caller-provided value must win, got %q", got)
	}
}

func hasService(specs []serviceSpec, name string) bool {
	for _, spec := range specs {
		if spec.Name == name {
			return true
		}
	}
	return false
}

func TestControlServiceValidation(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()

	if _, err := ControlService(deps, "bounce", ""); exitCodeOf(test, err) != output.ExitInvalidInput {
		test.Fatalf("unknown action should be exit 2, got %v", err)
	}
	if _, err := ControlService(deps, "start", "nope"); exitCodeOf(test, err) != output.ExitInvalidInput {
		test.Fatalf("unknown service should be exit 2, got %v", err)
	}
	// Valid action + known service → delegated to the Services impl.
	statuses, err := ControlService(deps, "restart", "litellm")
	if err != nil || len(statuses) == 0 {
		test.Fatalf("valid control should delegate: statuses=%+v err=%v", statuses, err)
	}
	// Empty service (all) is valid too.
	if _, err := ControlService(deps, "start", ""); err != nil {
		test.Fatalf("control-all should be valid: %v", err)
	}
	// The literal "all" keyword is accepted (same as no service).
	if _, err := ControlService(deps, "restart", "all"); err != nil {
		test.Fatalf("\"all\" should be accepted: %v", err)
	}
}

func TestRunUpgradeRepinsVersions(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()

	// First run creates versions.json; then corrupt a pin.
	if _, err := Run(Options{}, deps); err != nil {
		test.Fatal(err)
	}
	path, _ := versions.Path()
	if err := jsonfile.WriteAtomic(path, &versions.File{SchemaVersion: 1, Services: map[string]versions.Service{"litellm": {Mode: "container", Image: "stale"}}}); err != nil {
		test.Fatal(err)
	}

	// --upgrade overwrites it with this binary's defaults.
	if _, err := Run(Options{Upgrade: true}, deps); err != nil {
		test.Fatal(err)
	}
	file, err := versions.Load()
	if err != nil || file == nil {
		test.Fatalf("load versions: %v", err)
	}
	if file.Services["litellm"].Image == "stale" {
		test.Fatal("--upgrade should have re-pinned versions.json to defaults")
	}
	if _, ok := file.Services["ollama"]; !ok {
		test.Fatalf("upgraded versions.json missing default services: %+v", file.Services)
	}
}

// TestPreflightReportsMsbWithInstructionsNotInstalling verifies `ai setup` does
// NOT install msb — when it's missing it is reported as a prerequisite with its
// install instructions + web address, not auto-installed.
func TestPreflightReportsMsbWithInstructionsNotInstalling(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()
	deps.GOOS, deps.GOARCH = "linux", "amd64"
	// docker present + rootless, virtualization ok, but msb absent.
	deps.Prober = fakeProber{
		bins:      map[string]bool{"docker": true},
		files:     map[string]bool{"/dev/kvm": true},
		dockerOut: "[name=rootless]",
	}

	_, err := Run(Options{}, deps)
	var platformErr *output.Error
	if !errors.As(err, &platformErr) {
		test.Fatalf("want *output.Error for missing msb, got %T", err)
	}
	for _, fragment := range []string{
		"microsandbox runtime", "install.microsandbox.dev", "docs.microsandbox.dev", "how to install", "web:",
	} {
		if !strings.Contains(platformErr.Message, fragment) {
			test.Errorf("message missing %q (should instruct, not install):\n%s", fragment, platformErr.Message)
		}
	}
}

func TestLiteLLMRunArgs(test *testing.T) {
	args := litellmRunArgs("/cfg/litellm/config.yaml")
	want := []string{
		"run", "-d", "--name", "aip-litellm",
		"--network", "aip-net",
		"-p", "14000:4000",
		"-v", "/cfg/litellm/config.yaml:/app/config.yaml",
		"-e", "UI_USERNAME=admin",
		"-e", "UI_PASSWORD",
		"-e", "LITELLM_MASTER_KEY",
		"-e", "DATABASE_URL=postgresql://litellm@aip-litellm-db:5432/litellm",
		"-e", "PRESIDIO_ANALYZER_API_BASE=http://aip-presidio-analyzer:3000",
		"-e", "PRESIDIO_ANONYMIZER_API_BASE=http://aip-presidio-anonymizer:3000",
		"ghcr.io/berriai/litellm:main-latest",
		"--config", "/app/config.yaml", "--port", "4000",
	}
	if len(args) != len(want) {
		test.Fatalf("args = %v, want %v", args, want)
	}
	for index := range want {
		if args[index] != want[index] {
			test.Errorf("args[%d] = %q, want %q", index, args[index], want[index])
		}
	}
	// The secrets are env passthrough (name-only) — their values must NOT appear
	// in argv (they ride in the process environment instead).
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "UI_PASSWORD=") || strings.Contains(joined, "LITELLM_MASTER_KEY=") {
		test.Errorf("secret values must not be inlined in argv: %v", args)
	}
}

func TestReportHumanShowsAddressAndConsole(test *testing.T) {
	report := &Report{
		PlatformDir: "/home/u/.ai-platform",
		Services: []ServiceStatus{
			{Name: "litellm", Mode: "container", State: "running", Healthy: true,
				Address: "http://localhost:14000", Console: "http://localhost:14000/ui"},
			{Name: "ollama", Mode: "container", State: "running", Healthy: true,
				Address: "http://localhost:11434"},
			{Name: "presidio", Mode: "container", State: "running", Healthy: true},
		},
	}
	rendered := report.Human()
	// litellm shows both its address and the admin UI URL.
	if !strings.Contains(rendered, "http://localhost:14000 · UI http://localhost:14000/ui") {
		test.Errorf("litellm address+UI missing:\n%s", rendered)
	}
	// ollama shows its address only (no UI).
	if !strings.Contains(rendered, "http://localhost:11434") {
		test.Errorf("ollama address missing:\n%s", rendered)
	}
	// presidio (no host endpoint) shows neither an address nor a UI hint.
	for _, presidioLine := range strings.Split(rendered, "\n") {
		if strings.Contains(presidioLine, "presidio") && strings.Contains(presidioLine, "http") {
			test.Errorf("presidio line should have no address/UI: %q", presidioLine)
		}
	}
}

func TestDesiredServicesAreRequired(test *testing.T) {
	// Ollama, Presidio, LiteLLM, and Headroom are all required host services
	// (Ollama is the local model backend LiteLLM routes to, arch §14/§16).
	for _, name := range []string{"ollama", "presidio", "litellm", "headroom", "open-webui"} {
		if !hasService(desiredServices(), name) {
			test.Errorf("required service %q missing from desiredServices: %+v", name, desiredServices())
		}
	}
}
