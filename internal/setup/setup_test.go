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
	"github.com/jt-helsinki/ideal-robot/internal/paths"
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

func (services *fakeServices) Reconcile(providerConfig string) ([]ServiceStatus, error) {
	services.reconciled = true
	services.provider = providerConfig
	return []ServiceStatus{{Name: "litellm", Mode: "container", State: "running", Healthy: true}}, nil
}
func (services *fakeServices) Status() ([]ServiceStatus, error) {
	return []ServiceStatus{{Name: "litellm", Mode: "container", State: "running", Healthy: true}}, nil
}
func (services *fakeServices) Control(action, service string) ([]ServiceStatus, error) {
	return []ServiceStatus{{Name: "litellm", Mode: "container", State: action + "ed"}}, nil
}

type fakeCA struct{ ensured bool }

func (certificateAuthority *fakeCA) Ensure() error {
	certificateAuthority.ensured = true
	return nil
}

// healthyDeps returns Deps that pass preflight (Apple Silicon, rootless docker,
// msb installed) with fresh fakes.
func healthyDeps() (Deps, *fakeServices, *fakeCA) {
	services := &fakeServices{}
	certificateAuthority := &fakeCA{}
	return Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{
			bins:      map[string]bool{"docker": true, "msb": true},
			dockerOut: "[name=seccomp name=rootless]",
		},
		Now:      func() string { return "2026-06-18T00:00:00Z" },
		Services: services,
		CA:       certificateAuthority,
		GatewayConfigFetcher: func() ([]byte, error) {
			return []byte("gateway {\n  dashboard_listen = \"127.0.0.1:8080\"\n  state_dir        = \"/opt/clawpatrol\"\n}\n"), nil
		},
	}, services, certificateAuthority
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
	deps, services, certificateAuthority := healthyDeps()

	report, err := Run(Options{ProviderConfig: "prov.yaml"}, deps)
	if err != nil {
		test.Fatal(err)
	}
	if !services.reconciled || services.provider != "prov.yaml" {
		test.Fatalf("services not reconciled with provider config: %+v", services)
	}
	if !certificateAuthority.ensured {
		test.Fatal("CA.Ensure was not called")
	}
	if !report.ConfigCreated || !report.VersionsCreated || !report.CAReady {
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
	deps, _, _ := healthyDeps()
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
		"clawpatrol", "clawpatrol.dev/install.sh",
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
	deps, _, _ := healthyDeps()
	deps.Prober = fakeProber{bins: map[string]bool{"msb": true}} // no docker

	_, err := Run(Options{}, deps)
	if got := exitCodeOf(test, err); got != output.ExitMissingDep {
		test.Fatalf("exit = %d, want %d", got, output.ExitMissingDep)
	}
}

func TestRunPreflightRootlessUnavailable(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _, _ := healthyDeps()
	// A rooted Linux docker engine ("rootless" absent from SecurityOptions). On
	// macOS/Windows, Docker Desktop is rootless-equivalent (VM), so this scenario
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
	deps, _, _ := healthyDeps()

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
	deps, _, _ := healthyDeps()
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

func hasService(specs []serviceSpec, name string) bool {
	for _, spec := range specs {
		if spec.Name == name {
			return true
		}
	}
	return false
}

func TestEnsureGatewayConfigSeedsWithDefaults(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _, _ := healthyDeps() // fetcher returns an example with state_dir=/opt/clawpatrol

	created, warning, err := ensureGatewayConfig(deps)
	if err != nil {
		test.Fatal(err)
	}
	if !created || warning != "" {
		test.Fatalf("expected created with no warning, got created=%v warning=%q", created, warning)
	}

	dir, _ := paths.ClawPatrolDir()
	contents, err := os.ReadFile(filepath.Join(dir, "gateway.hcl"))
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(string(contents), `"`+dir+`"`) {
		test.Fatalf("state_dir not defaulted to %s:\n%s", dir, contents)
	}
	if strings.Contains(string(contents), "/opt/clawpatrol") {
		test.Fatalf("example state_dir should have been replaced:\n%s", contents)
	}
	if !strings.Contains(string(contents), "127.0.0.1:8123") || strings.Contains(string(contents), "127.0.0.1:8080") {
		test.Fatalf("dashboard_listen should default to :8123:\n%s", contents)
	}
}

func TestEnsureGatewayConfigSkipsWhenPresent(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	dir, _ := paths.ClawPatrolDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		test.Fatal(err)
	}
	sentinel := []byte("# my hand edits\n")
	if err := os.WriteFile(filepath.Join(dir, "gateway.hcl"), sentinel, 0o644); err != nil {
		test.Fatal(err)
	}

	deps, _, _ := healthyDeps()
	deps.GatewayConfigFetcher = func() ([]byte, error) {
		test.Fatal("fetcher must not run when the config already exists")
		return nil, nil
	}

	created, _, err := ensureGatewayConfig(deps)
	if err != nil {
		test.Fatal(err)
	}
	if created {
		test.Fatal("must not recreate an existing config")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "gateway.hcl"))
	if string(got) != string(sentinel) {
		test.Fatalf("existing config must be left untouched, got:\n%s", got)
	}
}

func TestControlServiceValidation(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _, _ := healthyDeps()

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
}

func TestRunUpgradeRepinsVersions(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _, _ := healthyDeps()

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

func TestDesiredServicesAreRequired(test *testing.T) {
	// Ollama, LiteLLM, and ClawPatrol are all required host services (Ollama is
	// the local model backend LiteLLM routes to, arch §14/§16).
	for _, name := range []string{"ollama", "litellm", "clawpatrol"} {
		if !hasService(desiredServices(), name) {
			test.Errorf("required service %q missing from desiredServices: %+v", name, desiredServices())
		}
	}
}
