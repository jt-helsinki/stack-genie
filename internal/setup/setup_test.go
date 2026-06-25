package setup

import (
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/conffile"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
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
	reconciled     bool
	provider       string
	bindHost       string
	optional       []string
	pulledEnabled  []string
	updatedImages  bool
	updatedEnabled []string
	installed      bool
	capturedLogs   bool
	controlAction  string
	controlService string
}

func (services *fakeServices) Reconcile(providerConfig, bindHost string, optional []string, progress func(string)) ([]ServiceStatus, error) {
	services.reconciled = true
	services.provider = providerConfig
	services.bindHost = bindHost
	services.optional = optional
	if progress != nil {
		progress("reconciling (fake)")
	}
	statuses := []ServiceStatus{{Name: "litellm", Mode: "container", State: "running", Healthy: true}}
	// Mirror the real impl's optional gating so callers can assert which optional
	// services were brought up.
	for _, name := range optionalServiceNames() {
		state := "disabled"
		if slicesContains(optional, name) {
			state = "running"
		}
		statuses = append(statuses, ServiceStatus{Name: name, Mode: "container", State: state, Healthy: state == "running"})
	}
	return statuses, nil
}

// slicesContains is a tiny local helper so the fake mirrors the real gating
// without importing slices just for the test.
func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func (services *fakeServices) PullImages(enabled []string, _ io.Writer, progress func(string)) error {
	services.pulledEnabled = enabled
	if progress != nil {
		progress("pulling (fake)")
	}
	return nil
}
func (services *fakeServices) UpdateImages(enabled []string, _ io.Writer, progress func(string)) error {
	services.updatedImages = true
	services.updatedEnabled = enabled
	if progress != nil {
		progress("updating (fake)")
	}
	return nil
}
func (services *fakeServices) Status() ([]ServiceStatus, error) {
	return []ServiceStatus{{Name: "litellm", Mode: "container", State: "running", Healthy: true}}, nil
}
func (services *fakeServices) Control(action, service string) ([]ServiceStatus, error) {
	services.controlAction = action
	services.controlService = service
	return []ServiceStatus{{Name: "litellm", Mode: "container", State: action + "ed"}}, nil
}
func (services *fakeServices) InstallPrerequisite(_ Prerequisite, _ io.Writer) error {
	services.installed = true
	return nil
}
func (services *fakeServices) CaptureServiceLogs() error {
	services.capturedLogs = true
	return nil
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
		filepath.Join(home, ".ai-platform", "config", "runtime.yaml"),
		filepath.Join(home, ".ai-platform", "config", "config.yaml"),
		filepath.Join(home, ".ai-platform", "config", "versions.yaml"),
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
		"microsandbox runtime", "get.microsandbox.dev",
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

func TestRoleForPrecedence(test *testing.T) {
	// An explicit option always wins, even over a persisted role.
	test.Setenv("HOME", test.TempDir())
	if role, err := RoleFor(Options{Mode: runtime.RoleServer}); err != nil || role != runtime.RoleServer {
		test.Fatalf("explicit option: role=%q err=%v, want %q", role, err, runtime.RoleServer)
	}

	// No option, no persisted runtime.yaml → standalone default.
	if role, err := RoleFor(Options{}); err != nil || role != runtime.RoleStandalone {
		test.Fatalf("default: role=%q err=%v, want %q", role, err, runtime.RoleStandalone)
	}

	// No option but a persisted role → the persisted role.
	if err := runtime.Persist(&runtime.Info{Role: runtime.RoleClient}); err != nil {
		test.Fatal(err)
	}
	if role, err := RoleFor(Options{}); err != nil || role != runtime.RoleClient {
		test.Fatalf("persisted: role=%q err=%v, want %q", role, err, runtime.RoleClient)
	}

	// An unrecognized explicit role is an invalid-input error (exit 2).
	if _, err := RoleFor(Options{Mode: "bogus"}); exitCodeOf(test, err) != output.ExitInvalidInput {
		test.Fatalf("invalid role should be exit %d", output.ExitInvalidInput)
	}
}

func TestPrerequisiteInstallCommandPerOS(test *testing.T) {
	cases := []struct {
		name string
		goos string
		want string
	}{
		{"microsandbox runtime", "darwin", "curl -sSL https://get.microsandbox.dev | sh"},
		{"microsandbox runtime", "linux", "curl -sSL https://get.microsandbox.dev | sh"},
		{"container runtime", "linux", "curl -fsSL https://get.docker.com | sh"},
		{"container runtime", "darwin", ""}, // Docker Desktop is a GUI app — instruct only
		{"host virtualization", "linux", ""},
		{"host virtualization", "darwin", ""},
		{"rootless service tier", "linux", ""},
	}
	for _, testCase := range cases {
		if got := installCommandFor(testCase.name, testCase.goos); got != testCase.want {
			test.Errorf("installCommandFor(%q,%q) = %q, want %q", testCase.name, testCase.goos, got, testCase.want)
		}
	}
}

func TestMissingPrerequisitesPopulatesInstallCommand(test *testing.T) {
	// Linux, nothing installed: msb + docker are missing AND auto-installable.
	deps := Deps{GOOS: "linux", GOARCH: "amd64", Prober: fakeProber{}}
	byName := map[string]Prerequisite{}
	for _, prereq := range MissingPrerequisites(deps, runtime.RoleStandalone) {
		byName[prereq.Name] = prereq
	}
	if cmd := byName["microsandbox runtime"].InstallCommand; cmd != "curl -sSL https://get.microsandbox.dev | sh" {
		test.Errorf("microsandbox InstallCommand = %q", cmd)
	}
	if cmd := byName["container runtime"].InstallCommand; cmd != "curl -fsSL https://get.docker.com | sh" {
		test.Errorf("container runtime InstallCommand = %q", cmd)
	}
	if cmd := byName["host virtualization"].InstallCommand; cmd != "" {
		test.Errorf("host virtualization should be instruct-only, got %q", cmd)
	}
}

func TestMissingPrerequisitesNoneWhenSatisfied(test *testing.T) {
	// A healthy host (docker + msb + virtualization) has no missing prerequisites,
	// so the interactive preflight would attempt no install.
	deps, _ := healthyDeps()
	if missing := MissingPrerequisites(deps, runtime.RoleStandalone); len(missing) != 0 {
		test.Fatalf("expected no missing prerequisites on a healthy host, got %+v", missing)
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

// owningService points companion containers (Odysseus's chromadb/searxng/ntfy,
// which `ai logs --service` accepts individually) at their managing logical
// service, and returns "" for a service that is managed on its own or unknown.
func TestOwningServiceMapsCompanionsToOdysseus(test *testing.T) {
	for _, name := range []string{"chromadb", "searxng", "ntfy"} {
		if got := owningService(name); got != "odysseus" {
			test.Errorf("owningService(%q) = %q, want odysseus", name, got)
		}
	}
	for _, name := range []string{"odysseus", "ollama", "litellm", "presidio", "", "bogus"} {
		if got := owningService(name); got != "" {
			test.Errorf("owningService(%q) = %q, want empty", name, got)
		}
	}
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

// TestUpdateService: it force-pulls the images then RESTARTS the target — and
// validates the target up front (unknown name → exit 2, before any pull).
func TestUpdateService(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, services := healthyDeps()

	// Unknown service → exit 2, and no pull happened.
	if _, err := UpdateService(deps, "nope", io.Discard, nil); exitCodeOf(test, err) != output.ExitInvalidInput {
		test.Fatalf("unknown service should be exit 2, got %v", err)
	}
	if services.updatedImages {
		test.Fatal("a validation failure must not pull images")
	}

	// Valid service → force-pull (UpdateImages) then restart (Control).
	statuses, err := UpdateService(deps, "litellm", io.Discard, nil)
	if err != nil {
		test.Fatalf("update litellm: %v", err)
	}
	if !services.updatedImages {
		test.Error("UpdateService must force-pull the images (UpdateImages)")
	}
	if services.controlAction != "restart" || services.controlService != "litellm" {
		test.Errorf("update should restart the service, got %s %s", services.controlAction, services.controlService)
	}
	if len(statuses) == 0 {
		test.Error("update should return the post-restart statuses")
	}

	// "all" updates every service (empty target to Control).
	if _, err := UpdateService(deps, "all", io.Discard, nil); err != nil {
		test.Fatalf("update all: %v", err)
	}
	if services.controlService != "" {
		test.Errorf("update all should restart every service (empty target), got %q", services.controlService)
	}
}

// TestServicesEnableDisableOptional: enable/disable toggle an optional service's
// membership in runtime.yaml and start/stop its container(s).
func TestServicesEnableDisableOptional(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, services := healthyDeps()
	if _, err := Run(Options{}, deps); err != nil {
		test.Fatal(err)
	}
	// After setup the default optional set is enabled (open-webui), odysseus off.
	// Disable open-webui: it leaves the persisted set and its container is stopped.
	if _, err := ControlService(deps, "disable", "open-webui"); err != nil {
		test.Fatalf("disable open-webui: %v", err)
	}
	info, _ := runtime.Load()
	if slicesContains(info.OptionalServices, "open-webui") {
		test.Errorf("open-webui should be removed from the optional set: %v", info.OptionalServices)
	}
	if services.controlAction != "stop" || services.controlService != "open-webui" {
		test.Errorf("disable should stop open-webui, got %s %s", services.controlAction, services.controlService)
	}
	// Enable odysseus: it joins the set and its container is started.
	if _, err := ControlService(deps, "enable", "odysseus"); err != nil {
		test.Fatalf("enable odysseus: %v", err)
	}
	info, _ = runtime.Load()
	if !slicesContains(info.OptionalServices, "odysseus") {
		test.Errorf("odysseus should be in the optional set: %v", info.OptionalServices)
	}
	if services.controlAction != "start" || services.controlService != "odysseus" {
		test.Errorf("enable should start odysseus, got %s %s", services.controlAction, services.controlService)
	}
}

// TestControlServiceGatesAndValidatesToggle: start/stop/restart are gated to
// enabled services, and enable/disable only apply to optional services.
func TestControlServiceGatesAndValidatesToggle(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()
	if _, err := Run(Options{}, deps); err != nil {
		test.Fatal(err)
	}
	// odysseus is disabled by default → it cannot be started until enabled.
	if _, err := ControlService(deps, "start", "odysseus"); exitCodeOf(test, err) != output.ExitInvalidInput {
		test.Fatalf("start on a disabled optional should be exit 2, got %v", err)
	}
	// A core service cannot be enabled/disabled (always on).
	if _, err := ControlService(deps, "enable", "litellm"); exitCodeOf(test, err) != output.ExitInvalidInput {
		test.Fatalf("enabling a core service should be exit 2, got %v", err)
	}
	// enable/disable need a service name.
	if _, err := ControlService(deps, "enable", ""); exitCodeOf(test, err) != output.ExitInvalidInput {
		test.Fatalf("enable with no service should be exit 2, got %v", err)
	}
}

func TestRunUpgradeRepinsVersions(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()

	// First run creates versions.yaml; then corrupt a pin.
	if _, err := Run(Options{}, deps); err != nil {
		test.Fatal(err)
	}
	path, _ := versions.Path()
	if err := conffile.WriteAtomic(path, &versions.File{SchemaVersion: 1, Services: map[string]versions.Service{"litellm": {Mode: "container", Image: "stale"}}}); err != nil {
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
		test.Fatal("--upgrade should have re-pinned versions.yaml to defaults")
	}
	if _, ok := file.Services["ollama"]; !ok {
		test.Fatalf("upgraded versions.yaml missing default services: %+v", file.Services)
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
		"microsandbox runtime", "get.microsandbox.dev", "docs.microsandbox.dev", "how to install", "web:",
	} {
		if !strings.Contains(platformErr.Message, fragment) {
			test.Errorf("message missing %q (should instruct, not install):\n%s", fragment, platformErr.Message)
		}
	}
}

func TestLiteLLMRunArgs(test *testing.T) {
	args := litellmRunArgs("/cfg/litellm/config.yaml", "127.0.0.1", containerImage("litellm"))
	want := []string{
		"run", "-d", "--name", "aip-litellm",
		"--network", "aip-net",
		// INTERNAL-ONLY: no host publish — reached by name on aip-net; nginx fronts it.
		"-v", "/cfg/litellm/config.yaml:/app/config.yaml",
		"-e", "UI_USERNAME=admin",
		"-e", "UI_PASSWORD",
		"-e", "LITELLM_MASTER_KEY",
		"-e", "DATABASE_URL=postgresql://litellm@aip-litellm-db:5432/litellm",
		"-e", "PRESIDIO_ANALYZER_API_BASE=http://aip-presidio-analyzer:3000",
		"-e", "PRESIDIO_ANONYMIZER_API_BASE=http://aip-presidio-anonymizer:3000",
		containerImage("litellm"),
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

// TestLiteLLMRunArgsInternalOnly asserts LiteLLM no longer publishes ANY host port
// (nginx is the sole host entry) — regardless of the role's bindHost.
func TestLiteLLMRunArgsInternalOnly(test *testing.T) {
	for _, bindHost := range []string{"127.0.0.1", "0.0.0.0"} {
		launch := strings.Join(litellmRunArgs("/cfg/config.yaml", bindHost, containerImage("litellm")), " ")
		if strings.Contains(launch, "-p ") || strings.Contains(launch, "14000") {
			test.Errorf("litellm must be internal-only (no host publish) for bindHost %s: %s", bindHost, launch)
		}
	}
}

// TestBlockingPrereqsForRole pins the role → blocking-prerequisite mapping:
// standalone requires everything, server drops the microVM runtime + virtualization,
// client drops the container runtime + rootless service tier.
func TestBlockingPrereqsForRole(test *testing.T) {
	standalone := blockingPrereqsForRole(runtime.RoleStandalone)
	for _, name := range []string{"container runtime", "rootless service tier", "microsandbox runtime", "host virtualization"} {
		if !standalone[name] {
			test.Errorf("standalone should block on %q", name)
		}
	}
	server := blockingPrereqsForRole(runtime.RoleServer)
	if !server["container runtime"] || !server["rootless service tier"] {
		test.Errorf("server should block on the service tier: %v", server)
	}
	if server["microsandbox runtime"] || server["host virtualization"] {
		test.Errorf("server must NOT block on the microVM runtime/virtualization: %v", server)
	}
	client := blockingPrereqsForRole(runtime.RoleClient)
	if !client["microsandbox runtime"] || !client["host virtualization"] {
		test.Errorf("client should block on the microVM runtime: %v", client)
	}
	if client["container runtime"] || client["rootless service tier"] {
		test.Errorf("client must NOT block on the service tier: %v", client)
	}
}

// TestClientModePassesPreflightWithoutDocker: a client only needs the microVM
// runtime + virtualization, so a host with NO container runtime still passes
// preflight, and the service-tier reconcile is skipped entirely.
func TestClientModePassesPreflightWithoutDocker(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, services := healthyDeps()
	// msb + virtualization present, but no docker/podman at all.
	deps.Prober = fakeProber{bins: map[string]bool{"msb": true}}

	report, err := Run(Options{Mode: runtime.RoleClient, ServerAddr: "gateway.example:14000"}, deps)
	if err != nil {
		test.Fatalf("client setup should pass preflight without docker: %v", err)
	}
	if services.reconciled {
		test.Error("client mode must NOT reconcile the local service tier")
	}
	if len(report.Services) != 0 {
		test.Errorf("client mode Report.Services should be empty, got %+v", report.Services)
	}
	if report.Runtime.Role != runtime.RoleClient {
		test.Errorf("role not persisted to report: %+v", report.Runtime)
	}
	if report.Runtime.AIPlatformHost != "gateway.example:14000" {
		test.Errorf("server address not recorded: %+v", report.Runtime)
	}
	// Role + server address survive on disk for later runs/steps.
	persisted, err := runtime.Load()
	if err != nil || persisted == nil {
		test.Fatalf("load runtime.yaml: %v", err)
	}
	if persisted.Role != runtime.RoleClient || persisted.AIPlatformHost != "gateway.example:14000" {
		test.Errorf("runtime.yaml did not persist client role/address: %+v", persisted)
	}
}

// TestServerModePassesPreflightWithoutMsb: a server only needs the service tier,
// so a host with NO microsandbox still passes preflight and reconciles, binding
// the shared services to 0.0.0.0.
func TestServerModePassesPreflightWithoutMsb(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, services := healthyDeps()
	// docker present + rootless, but no msb.
	deps.Prober = fakeProber{
		bins:      map[string]bool{"docker": true},
		dockerOut: "[name=seccomp name=rootless]",
	}

	report, err := Run(Options{Mode: runtime.RoleServer}, deps)
	if err != nil {
		test.Fatalf("server setup should pass preflight without msb: %v", err)
	}
	if !services.reconciled {
		test.Error("server mode must reconcile the service tier")
	}
	if services.bindHost != "0.0.0.0" {
		test.Errorf("server reconcile bindHost = %q, want 0.0.0.0", services.bindHost)
	}
	if report.Runtime.Role != runtime.RoleServer {
		test.Errorf("role not persisted: %+v", report.Runtime)
	}
}

// TestStandaloneBindsLoopback: the default role reconciles with a loopback bind.
func TestStandaloneBindsLoopback(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, services := healthyDeps()
	if _, err := Run(Options{}, deps); err != nil {
		test.Fatal(err)
	}
	if services.bindHost != "127.0.0.1" {
		test.Errorf("standalone reconcile bindHost = %q, want 127.0.0.1", services.bindHost)
	}
}

// TestInvalidModeRejected: an unknown role is exit 2.
func TestInvalidModeRejected(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()
	_, err := Run(Options{Mode: "bogus"}, deps)
	if got := exitCodeOf(test, err); got != output.ExitInvalidInput {
		test.Fatalf("invalid mode exit = %d, want %d", got, output.ExitInvalidInput)
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
	for _, name := range []string{"ollama", "presidio", "litellm", "headroom", "proxy", "open-webui"} {
		if !hasService(desiredServices(), name) {
			test.Errorf("required service %q missing from desiredServices: %+v", name, desiredServices())
		}
	}
}

// serviceIndex returns the position of a service in desiredServices (or -1).
func serviceIndex(specs []serviceSpec, name string) int {
	for index, spec := range specs {
		if spec.Name == name {
			return index
		}
	}
	return -1
}

// TestDesiredServicesOrder pins the reconcile order that the guardrail/gateway
// wiring depends on: Presidio (the secret-masking guardrail backend) starts BEFORE
// litellm (LiteLLM is launched with PRESIDIO_*_API_BASE pointing at it), and the
// nginx proxy starts AFTER headroom (it forwards to Headroom).
func TestDesiredServicesOrder(test *testing.T) {
	specs := desiredServices()
	presidio := serviceIndex(specs, "presidio")
	litellm := serviceIndex(specs, "litellm")
	headroom := serviceIndex(specs, "headroom")
	proxy := serviceIndex(specs, "proxy")
	if presidio >= litellm {
		test.Errorf("presidio must be before litellm: presidio=%d litellm=%d", presidio, litellm)
	}
	if headroom >= proxy {
		test.Errorf("proxy must be after headroom: headroom=%d proxy=%d", headroom, proxy)
	}
}

// TestRequiredImagesCoversEveryService asserts requiredImages returns the
// containerImage ref (repo:tag form) for every service-tier container when all
// optional services are enabled, including the ones that have no Status line —
// litellm-db is the notable omission from desiredServices and must be present
// here, alongside proxy and dns. With all optional enabled, Odysseus's four
// images appear too.
func TestRequiredImagesCoversEveryService(test *testing.T) {
	images := requiredImages(optionalServiceNames()) // all optional enabled
	have := make(map[string]bool, len(images))
	for _, ref := range images {
		if !strings.Contains(ref, ":") {
			test.Errorf("image ref %q is not in image:tag form", ref)
		}
		have[ref] = true
	}
	for _, service := range []string{
		"ollama", "presidio-analyzer", "presidio-anonymizer",
		"litellm", "litellm-db",
		"headroom", "proxy", "open-webui", "dns",
		"odysseus", "chromadb", "searxng", "ntfy",
	} {
		ref := containerImage(service)
		if ref == "" {
			test.Fatalf("containerImage(%q) returned empty — fixture/default missing", service)
		}
		if !have[ref] {
			test.Errorf("requiredImages missing %q (%s); got %v", service, ref, images)
		}
	}
}

// TestRequiredImagesGatesOptionalImages: a disabled optional service's images are
// NOT pulled, but become required once it is enabled. Core images are always present.
func TestRequiredImagesGatesOptionalImages(test *testing.T) {
	odysseusImages := []string{
		containerImage("odysseus"), containerImage("chromadb"),
		containerImage("searxng"), containerImage("ntfy"),
	}
	// Disabled: none of Odysseus's images appear.
	disabled := requiredImages(nil)
	for _, ref := range odysseusImages {
		if slicesContains(disabled, ref) {
			test.Errorf("disabled odysseus image %q must NOT be in requiredImages: %v", ref, disabled)
		}
	}
	// Core image (ollama) is always present, even with nothing optional enabled.
	if !slicesContains(disabled, containerImage("ollama")) {
		test.Errorf("core ollama image must always be required: %v", disabled)
	}
	// Enabled: Odysseus's four images all appear.
	enabled := requiredImages([]string{"odysseus"})
	for _, ref := range odysseusImages {
		if !slicesContains(enabled, ref) {
			test.Errorf("enabled odysseus image %q must be in requiredImages: %v", ref, enabled)
		}
	}
}

// recordingProber records every Run invocation's args so config-render + run-arg
// shapes can be asserted (mirrors how ensureDNS is exercised). Containers are
// reported as not running so the ensure* paths render config + run.
type recordingProber struct{ calls [][]string }

func (prober *recordingProber) LookPath(file string) (string, error) { return "/usr/bin/" + file, nil }
func (prober *recordingProber) Run(name string, args ...string) ([]byte, error) {
	prober.calls = append(prober.calls, append([]string{name}, args...))
	return nil, nil // "ps" returns empty → containerRunning is false
}
func (prober *recordingProber) Exists(string) bool { return false }

// runArgsFor returns the argv of the recorded `<runtime> run` call (the launch),
// or nil if none was recorded.
func runArgsFor(prober *recordingProber) []string {
	for _, call := range prober.calls {
		if len(call) >= 2 && call[1] == "run" {
			return call
		}
	}
	return nil
}

// staleHeadroomProber reports Headroom as RUNNING with a stale host-port binding
// (the pre-nginx :18787 publish), so ensureHeadroom must recreate it internal-only.
type staleHeadroomProber struct{ calls [][]string }

func (prober *staleHeadroomProber) LookPath(file string) (string, error) {
	return "/usr/bin/" + file, nil
}
func (prober *staleHeadroomProber) Exists(string) bool { return false }
func (prober *staleHeadroomProber) Run(name string, args ...string) ([]byte, error) {
	prober.calls = append(prober.calls, append([]string{name}, args...))
	if len(args) > 0 && args[0] == "ps" {
		return []byte("aip-headroom\n"), nil // running
	}
	if len(args) > 0 && args[0] == "inspect" {
		return []byte(`{"8787/tcp":[{"HostIp":"127.0.0.1","HostPort":"18787"}]}`), nil // stale publish
	}
	return nil, nil
}

// TestEnsureHeadroomRecreatesStalePublishedContainer: a Headroom still holding the
// old host :18787 publish must be recreated internal-only (self-heal) rather than
// skipped — otherwise `ai setup` fails when the nginx proxy can't bind 18787.
func TestEnsureHeadroomRecreatesStalePublishedContainer(test *testing.T) {
	prober := &staleHeadroomProber{}
	if err := ensureHeadroom(prober, "docker"); err != nil {
		test.Fatal(err)
	}
	var removed, launched bool
	var launchArgs []string
	for _, call := range prober.calls {
		if len(call) >= 3 && call[1] == "rm" && call[len(call)-1] == "aip-headroom" {
			removed = true
		}
		if len(call) >= 2 && call[1] == "run" {
			launched = true
			launchArgs = call
		}
	}
	if !removed || !launched {
		test.Fatalf("stale headroom not recreated: removed=%v launched=%v", removed, launched)
	}
	if launch := strings.Join(launchArgs, " "); strings.Contains(launch, "18787") || strings.Contains(launch, "-p ") {
		test.Errorf("recreated headroom must be internal-only (no 18787 publish): %s", launch)
	}
}

func TestEnsureProxyRendersGatewayConfig(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	prober := &recordingProber{}
	if err := ensureProxy(prober, "docker", "127.0.0.1", "aip.local", nil); err != nil {
		test.Fatal(err)
	}
	confPath := filepath.Join(home, ".ai-platform", "config", "proxy", "nginx.conf")
	content, err := os.ReadFile(confPath)
	if err != nil {
		test.Fatalf("nginx.conf not written: %v", err)
	}
	rendered := string(content)
	// The default server matches the domain, localhost, and the catch-all.
	if !strings.Contains(rendered, "server_name aip.local localhost _;") {
		test.Errorf("default server must match the domain + localhost + _:\n%s", rendered)
	}
	if !strings.Contains(rendered, "listen 80 default_server;") {
		test.Errorf("default server must be the default_server on :80:\n%s", rendered)
	}
	// The agent CHAT path (/v1) stays routed to Headroom — preserved unchanged.
	if !strings.Contains(rendered, "location /v1/ {") || !strings.Contains(rendered, "proxy_pass http://aip-headroom:8787;") {
		test.Errorf("nginx.conf must keep the /v1 → Headroom model path:\n%s", rendered)
	}
	// The LiteLLM admin surface is fronted on /llm (prefix stripped → :4000).
	if !strings.Contains(rendered, "location /llm/ {") || !strings.Contains(rendered, "proxy_pass http://aip-litellm:4000/;") {
		test.Errorf("nginx.conf must front the LiteLLM admin surface on /llm:\n%s", rendered)
	}
	// The Ollama HTTP API is fronted on /ollama (prefix stripped → :11434).
	if !strings.Contains(rendered, "location /ollama/ {") || !strings.Contains(rendered, "proxy_pass http://aip-ollama:11434/;") {
		test.Errorf("nginx.conf must front the Ollama API on /ollama:\n%s", rendered)
	}
	// /llm and /ollama must be matched BEFORE the catch-all `location /`.
	llmIdx := strings.Index(rendered, "location /llm/ {")
	ollamaIdx := strings.Index(rendered, "location /ollama/ {")
	catchAllIdx := strings.Index(rendered, "location / {")
	if llmIdx < 0 || ollamaIdx < 0 || catchAllIdx < 0 || llmIdx > catchAllIdx || ollamaIdx > catchAllIdx {
		test.Errorf("specific /llm and /ollama prefixes must precede the catch-all:\n%s", rendered)
	}
	if !strings.Contains(rendered, "proxy_buffering off;") {
		test.Errorf("nginx.conf must disable buffering for SSE streaming:\n%s", rendered)
	}
	// The LiteLLM admin UI is ALWAYS served as the litellm.<domain> vhost (it is a
	// core service) → :4000 at root, bypassing Headroom.
	if !strings.Contains(rendered, "server_name litellm.aip.local;") || !strings.Contains(rendered, "proxy_pass http://aip-litellm:4000;") {
		test.Errorf("nginx.conf must serve the litellm.<domain> vhost:\n%s", rendered)
	}
	// With no optional services enabled, no chat/odysseus vhosts render.
	if strings.Contains(rendered, "server_name chat.aip.local;") || strings.Contains(rendered, "server_name odysseus.aip.local;") {
		test.Errorf("no optional UI vhosts should render when none are enabled:\n%s", rendered)
	}
	// nginx takes over host :18787 and forwards on :80 internally; the UIs are
	// subdomains on the SAME port, so no separate UI ports are published.
	launch := strings.Join(runArgsFor(prober), " ")
	if !strings.Contains(launch, "-p 127.0.0.1:18787:80") {
		test.Errorf("proxy must publish the gateway port 18787: %s", launch)
	}
	if strings.Contains(launch, "18090") || strings.Contains(launch, ":7000") || strings.Contains(launch, ":8080") {
		test.Errorf("no separate UI ports should be published (UIs are subdomains): %s", launch)
	}
	if !strings.Contains(launch, confPath+":/etc/nginx/nginx.conf:ro") {
		test.Errorf("proxy did not bind-mount nginx.conf: %s", launch)
	}
}

// TestEnsureProxyFrontsEnabledUIs: with open-webui + odysseus enabled, nginx
// renders their Host-based UI vhosts (chat./odysseus.<domain>) on the SAME
// gateway port — no separate host ports (the containers are internal-only).
func TestEnsureProxyFrontsEnabledUIs(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	prober := &recordingProber{}
	if err := ensureProxy(prober, "0.0.0.0", "0.0.0.0", "aip.example.com", []string{"open-webui", "odysseus"}); err != nil {
		test.Fatal(err)
	}
	confPath := filepath.Join(home, ".ai-platform", "config", "proxy", "nginx.conf")
	content, err := os.ReadFile(confPath)
	if err != nil {
		test.Fatalf("nginx.conf not written: %v", err)
	}
	rendered := string(content)
	if !strings.Contains(rendered, "server_name chat.aip.example.com;") || !strings.Contains(rendered, "proxy_pass http://aip-open-webui:8080;") {
		test.Errorf("nginx.conf must serve the chat.<domain> vhost → Open WebUI:\n%s", rendered)
	}
	if !strings.Contains(rendered, "server_name odysseus.aip.example.com;") || !strings.Contains(rendered, "proxy_pass http://aip-odysseus:7000;") {
		test.Errorf("nginx.conf must serve the odysseus.<domain> vhost → Odysseus:\n%s", rendered)
	}
	// WebSocket upgrade headers for the UI vhosts.
	if !strings.Contains(rendered, "proxy_set_header Upgrade $http_upgrade;") {
		test.Errorf("UI vhosts must carry the websocket Upgrade header:\n%s", rendered)
	}
	// nginx publishes ONLY the gateway port (on the server bindHost 0.0.0.0) — the
	// UIs are subdomains on the same port.
	launch := strings.Join(runArgsFor(prober), " ")
	if !strings.Contains(launch, "-p 0.0.0.0:18787:80") {
		test.Errorf("proxy must publish the gateway port on 0.0.0.0: %s", launch)
	}
	if strings.Contains(launch, ":18090:") || strings.Contains(launch, ":7000:") || strings.Contains(launch, ":8080") {
		test.Errorf("no separate UI ports should be published (UIs are subdomains): %s", launch)
	}
}

// TestProxyNginxConfThreadsDomain: the rendered vhost server_names use the given
// domain, the LiteLLM admin UI vhost redirects / → /ui, and the UI-serving vhosts
// bypass Headroom (proxy_pass to the app, not aip-headroom).
func TestProxyNginxConfThreadsDomain(test *testing.T) {
	rendered := proxyNginxConf("dev.example.com", []string{"open-webui"})
	if !strings.Contains(rendered, "server_name dev.example.com localhost _;") {
		test.Errorf("default server must use the threaded domain:\n%s", rendered)
	}
	if !strings.Contains(rendered, "server_name litellm.dev.example.com;") {
		test.Errorf("litellm vhost must use the threaded domain:\n%s", rendered)
	}
	if !strings.Contains(rendered, "server_name chat.dev.example.com;") {
		test.Errorf("chat vhost must use the threaded domain:\n%s", rendered)
	}
	// LiteLLM admin UI: / → /ui redirect.
	if !strings.Contains(rendered, "return 302 /ui;") {
		test.Errorf("litellm vhost should redirect / → /ui:\n%s", rendered)
	}
	// The UI vhosts BYPASS Headroom (they proxy to the app, not aip-headroom).
	chatBlock := rendered[strings.Index(rendered, "server_name chat.dev.example.com;"):]
	if strings.Contains(chatBlock[:strings.Index(chatBlock, "}")], "aip-headroom") {
		test.Errorf("the chat UI vhost must bypass Headroom:\n%s", rendered)
	}
	// disabled odysseus does not render.
	if strings.Contains(rendered, "odysseus.dev.example.com") {
		test.Errorf("disabled odysseus vhost should not render:\n%s", rendered)
	}
}

// --- optional-services framework -------------------------------------------

// TestReconcileGatesOptionalOpenWebUI: open-webui is brought up when enabled and
// skipped when not, while core services are unaffected (asserted via the fake's
// per-service status, which mirrors the real impl's gating).
func TestReconcileGatesOptionalOpenWebUI(test *testing.T) {
	services := &fakeServices{}
	enabledStatuses, err := services.Reconcile("", "127.0.0.1", []string{"open-webui"}, nil)
	if err != nil {
		test.Fatal(err)
	}
	if stateOf(enabledStatuses, "open-webui") != "running" {
		test.Errorf("open-webui should be running when enabled: %+v", enabledStatuses)
	}
	if !slicesContains(services.optional, "open-webui") {
		test.Errorf("Reconcile did not receive the enabled optional set: %+v", services.optional)
	}

	disabled := &fakeServices{}
	disabledStatuses, err := disabled.Reconcile("", "127.0.0.1", nil, nil)
	if err != nil {
		test.Fatal(err)
	}
	if stateOf(disabledStatuses, "open-webui") != "disabled" {
		test.Errorf("open-webui should be disabled when not enabled: %+v", disabledStatuses)
	}
}

// stateOf returns the State of a named service in statuses, or "".
func stateOf(statuses []ServiceStatus, name string) string {
	for _, status := range statuses {
		if status.Name == name {
			return status.State
		}
	}
	return ""
}

// TestCoreOptionalSplit pins the core/optional partition: open-webui and odysseus
// are the optional services; the rest are core; desiredServices is their union.
func TestCoreOptionalSplit(test *testing.T) {
	for _, optional := range []string{"open-webui", "odysseus"} {
		if !isOptionalService(optional) {
			test.Errorf("%q must be an optional service", optional)
		}
	}
	for _, core := range []string{"ollama", "presidio", "litellm", "headroom", "proxy", "dns"} {
		if isOptionalService(core) {
			test.Errorf("%q must be a core service, not optional", core)
		}
		if !hasService(coreServices(), core) {
			test.Errorf("%q missing from coreServices", core)
		}
	}
	if got := optionalServiceNames(); len(got) != 2 || got[0] != "open-webui" || got[1] != "odysseus" {
		test.Errorf("optionalServiceNames = %v, want [open-webui odysseus]", got)
	}
	// desiredServices is core + all optional (every name addressable).
	for _, name := range []string{"ollama", "presidio", "litellm", "headroom", "proxy", "dns", "open-webui", "odysseus"} {
		if !hasService(desiredServices(), name) {
			test.Errorf("%q missing from desiredServices: %+v", name, desiredServices())
		}
	}
}

// TestResolveOptionalPrecedence pins the enabled-set precedence: an explicit set
// (incl. "none") wins; else the persisted set; else the first-run default.
func TestResolveOptionalPrecedence(test *testing.T) {
	// First run, no explicit choice, no persisted set → default ([open-webui]).
	if got := ResolveOptional(Options{}, nil); len(got) != 1 || got[0] != "open-webui" {
		test.Errorf("default first-run set = %v, want [open-webui]", got)
	}
	// Explicit "none" (OptionalSet with an empty slice) → empty.
	if got := ResolveOptional(Options{OptionalSet: true, Optional: []string{}}, nil); len(got) != 0 {
		test.Errorf("explicit none = %v, want []", got)
	}
	// Explicit choice wins over the persisted set.
	persisted := &runtime.Info{OptionalServices: []string{}}
	if got := ResolveOptional(Options{OptionalSet: true, Optional: []string{"open-webui"}}, persisted); len(got) != 1 {
		test.Errorf("explicit choice should win: %v", got)
	}
	// No explicit choice → the persisted set (here, deliberately empty).
	if got := ResolveOptional(Options{}, persisted); len(got) != 0 {
		test.Errorf("persisted empty set should be honored: %v", got)
	}
	// Unknown persisted names are dropped (a retired optional service).
	stale := &runtime.Info{OptionalServices: []string{"open-webui", "retired-tool"}}
	if got := ResolveOptional(Options{}, stale); len(got) != 1 || got[0] != "open-webui" {
		test.Errorf("unknown names should be dropped: %v", got)
	}
}

// TestRunPersistsDefaultOptional: a first run with no choice persists the default
// ([open-webui]) to runtime.yaml and reconciles it.
func TestRunPersistsDefaultOptional(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, services := healthyDeps()
	report, err := Run(Options{}, deps)
	if err != nil {
		test.Fatal(err)
	}
	if !slicesContains(report.Runtime.OptionalServices, "open-webui") {
		test.Errorf("first run should default to [open-webui], got %v", report.Runtime.OptionalServices)
	}
	if !slicesContains(services.optional, "open-webui") {
		test.Errorf("Reconcile should receive [open-webui], got %v", services.optional)
	}
	persisted, err := runtime.Load()
	if err != nil || persisted == nil {
		test.Fatalf("load runtime.yaml: %v", err)
	}
	if !slicesContains(persisted.OptionalServices, "open-webui") {
		test.Errorf("runtime.yaml did not persist [open-webui]: %v", persisted.OptionalServices)
	}
}

// TestRunOptionalNoneDisables: --optional none (OptionalSet + empty) persists an
// empty optional set and reconciles no optional services.
func TestRunOptionalNoneDisables(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, services := healthyDeps()
	report, err := Run(Options{OptionalSet: true, Optional: []string{}}, deps)
	if err != nil {
		test.Fatal(err)
	}
	if len(report.Runtime.OptionalServices) != 0 {
		test.Errorf("--optional none should persist no optional services, got %v", report.Runtime.OptionalServices)
	}
	if len(services.optional) != 0 {
		test.Errorf("Reconcile should receive no optional services, got %v", services.optional)
	}
}

// TestRunOptionalHonoursExplicitSet: an explicit set is honored and persisted.
func TestRunOptionalHonoursExplicitSet(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, services := healthyDeps()
	report, err := Run(Options{OptionalSet: true, Optional: []string{"open-webui"}}, deps)
	if err != nil {
		test.Fatal(err)
	}
	if !slicesContains(report.Runtime.OptionalServices, "open-webui") {
		test.Errorf("explicit set not persisted: %v", report.Runtime.OptionalServices)
	}
	if !slicesContains(services.optional, "open-webui") {
		test.Errorf("explicit set not reconciled: %v", services.optional)
	}
}

// TestStatusForShowsDisabledOptional: a not-enabled optional service is listed
// with State "disabled" so users can discover it.
func TestStatusForShowsDisabledOptional(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	services := realServices{prober: fakeProber{}}
	statuses, err := services.statusFor(nil) // nothing enabled
	if err != nil {
		test.Fatal(err)
	}
	if stateOf(statuses, "open-webui") != "disabled" {
		test.Errorf("not-enabled open-webui should be \"disabled\": %+v", statuses)
	}
	// And it IS probed (not "disabled") when enabled.
	enabled, err := services.statusFor([]string{"open-webui"})
	if err != nil {
		test.Fatal(err)
	}
	if stateOf(enabled, "open-webui") == "disabled" {
		test.Errorf("enabled open-webui should be probed, not \"disabled\": %+v", enabled)
	}
}

// TestStatusForDisplayHostByRole: in the server role (services bound 0.0.0.0,
// LAN-reachable) statusFor renders endpoints against the machine hostname so a
// remote client gets a reachable address; every other role keeps localhost. This
// is display-only — it does not change any container bind.
func TestStatusForDisplayHostByRole(test *testing.T) {
	addressOf := func(statuses []ServiceStatus, name string) string {
		for _, status := range statuses {
			if status.Name == name {
				return status.Address
			}
		}
		return ""
	}
	consoleOf := func(statuses []ServiceStatus, name string) string {
		for _, status := range statuses {
			if status.Name == name {
				return status.Console
			}
		}
		return ""
	}

	// Stub the hostname source so the assertion is exact and deterministic.
	original := osHostname
	osHostname = func() (string, error) { return "build-host.lan", nil }
	defer func() { osHostname = original }()

	// Standalone role → localhost (loopback display).
	test.Setenv("HOME", test.TempDir())
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, Role: runtime.RoleStandalone}); err != nil {
		test.Fatal(err)
	}
	services := realServices{prober: fakeProber{}}
	standalone, err := services.statusFor(nil)
	if err != nil {
		test.Fatal(err)
	}
	if got := addressOf(standalone, "litellm"); got != "http://localhost:14000" {
		test.Errorf("standalone litellm address = %q, want http://localhost:14000", got)
	}
	if got := addressOf(standalone, "dns"); got != "127.0.0.1:15353/udp" {
		test.Errorf("standalone dns address = %q, want loopback unchanged", got)
	}

	// Server role → machine hostname in both Address and Console; dns stays loopback.
	test.Setenv("HOME", test.TempDir())
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, Role: runtime.RoleServer}); err != nil {
		test.Fatal(err)
	}
	server, err := services.statusFor(nil)
	if err != nil {
		test.Fatal(err)
	}
	if got := addressOf(server, "litellm"); got != "http://build-host.lan:14000" {
		test.Errorf("server litellm address = %q, want http://build-host.lan:14000", got)
	}
	if got := consoleOf(server, "litellm"); got != "http://build-host.lan:14000/ui" {
		test.Errorf("server litellm console = %q, want http://build-host.lan:14000/ui", got)
	}
	if got := addressOf(server, "litellm"); strings.Contains(got, "localhost") {
		test.Errorf("server role must not display localhost: %q", got)
	}
	if got := addressOf(server, "dns"); got != "127.0.0.1:15353/udp" {
		test.Errorf("server dns address = %q, want loopback unchanged", got)
	}
}

// TestEnsureHeadroomIsInternalOnly: Headroom no longer publishes the gateway port
// 18787 — nginx (aip-proxy) owns it now. The launch must carry no host publish.
func TestEnsureHeadroomIsInternalOnly(test *testing.T) {
	prober := &recordingProber{}
	if err := ensureHeadroom(prober, "docker"); err != nil {
		test.Fatal(err)
	}
	launch := strings.Join(runArgsFor(prober), " ")
	if strings.Contains(launch, "18787") || strings.Contains(launch, "-p ") {
		test.Errorf("headroom must be internal-only (no 18787 publish): %s", launch)
	}
	if !strings.Contains(launch, "OPENAI_TARGET_API_URL=") {
		test.Errorf("headroom run missing OPENAI_TARGET_API_URL: %s", launch)
	}
}

// runArgsForContainer returns the argv of the recorded `<runtime> run --name
// <container>` call, or nil if none was recorded.
// TestEnsureOpenWebUIAuthByRole pins the role-based UI-auth policy
// (runtime.RequireUIAuth): a server renders WEBUI_AUTH=true (network-exposed,
// login required), while standalone/client render WEBUI_AUTH=false (open).
func TestEnsureOpenWebUIAuthByRole(test *testing.T) {
	cases := []struct {
		name        string
		requireAuth bool
		wantAuth    string
	}{
		{"server", true, "WEBUI_AUTH=true"},
		{"standalone", false, "WEBUI_AUTH=false"},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			test.Setenv("HOME", test.TempDir())
			prober := &recordingProber{}
			if err := ensureOpenWebUI(prober, "docker", "127.0.0.1", "aip.local", testCase.requireAuth); err != nil {
				test.Fatal(err)
			}
			args := runArgsForContainer(prober, openWebUIContainer)
			if args == nil {
				test.Fatalf("open-webui was not launched: %v", prober.calls)
			}
			launch := strings.Join(args, " ")
			if !strings.Contains(launch, testCase.wantAuth) {
				test.Errorf("launch missing %q: %s", testCase.wantAuth, launch)
			}
			// open-webui is internal-only (no host publish) regardless of role.
			if strings.Contains(launch, "-p ") {
				test.Errorf("open-webui must be internal-only (no host publish): %s", launch)
			}
		})
	}
}

func runArgsForContainer(prober *recordingProber, container string) []string {
	for _, call := range prober.calls {
		if len(call) < 4 || call[1] != "run" {
			continue
		}
		for index := 2; index < len(call)-1; index++ {
			if call[index] == "--name" && call[index+1] == container {
				return call
			}
		}
	}
	return nil
}

// TestEnsureOdysseusGroupRunArgs: ensureOdysseus brings up all four containers;
// ALL of them are internal-only (no -p host publish — nginx fronts the app UI),
// the app mounts the host Docker socket, and routes models through aip-proxy.
func TestEnsureOdysseusGroupRunArgs(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	prober := &recordingProber{}
	if err := ensureOdysseus(prober, "docker", "127.0.0.1", "aip.local", false); err != nil {
		test.Fatal(err)
	}

	// Companions are internal-only: launched, but with no host publish.
	for _, companion := range []string{chromadbContainer, searxngContainer, ntfyContainer} {
		args := runArgsForContainer(prober, companion)
		if args == nil {
			test.Fatalf("%s was not launched: %v", companion, prober.calls)
		}
		launch := strings.Join(args, " ")
		if strings.Contains(launch, "-p ") {
			test.Errorf("%s must be internal-only (no host publish): %s", companion, launch)
		}
		if !strings.Contains(launch, "--network "+platformNetwork) {
			test.Errorf("%s must join %s: %s", companion, platformNetwork, launch)
		}
	}
	// ntfy needs the explicit `serve` command.
	if ntfy := strings.Join(runArgsForContainer(prober, ntfyContainer), " "); !strings.HasSuffix(ntfy, " serve") {
		test.Errorf("ntfy must run `serve`: %s", ntfy)
	}
	// SearXNG must carry a secret.
	if searx := strings.Join(runArgsForContainer(prober, searxngContainer), " "); !strings.Contains(searx, "SEARXNG_SECRET=") {
		test.Errorf("searxng must set SEARXNG_SECRET: %s", searx)
	}

	// The app: INTERNAL-ONLY (nginx fronts its UI), mounts the Docker socket,
	// routes via aip-proxy.
	appArgs := runArgsForContainer(prober, odysseusContainer)
	if appArgs == nil {
		test.Fatalf("aip-odysseus was not launched: %v", prober.calls)
	}
	app := strings.Join(appArgs, " ")
	if strings.Contains(app, "-p ") {
		test.Errorf("odysseus must be internal-only (no host publish — nginx fronts it): %s", app)
	}
	if !strings.Contains(app, "/var/run/docker.sock:/var/run/docker.sock") {
		test.Errorf("odysseus must mount the host Docker socket: %s", app)
	}
	if !strings.Contains(app, "OLLAMA_BASE_URL=http://aip-proxy/v1") {
		test.Errorf("odysseus must route models through aip-proxy: %s", app)
	}
	if !strings.Contains(app, "CHROMADB_HOST=aip-chromadb") || !strings.Contains(app, "SEARXNG_INSTANCE=http://aip-searxng:8080") {
		test.Errorf("odysseus must point at its companions by name: %s", app)
	}
	if !strings.Contains(app, "APP_BIND=0.0.0.0") {
		test.Errorf("odysseus must bind 0.0.0.0 inside the container: %s", app)
	}
}

// TestSearxngSecretPersists: the SearXNG secret is generated once and reused on
// later runs (so signed cookies stay valid).
func TestSearxngSecretPersists(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	first, err := searxngSecret()
	if err != nil || first == "" {
		test.Fatalf("searxngSecret() = (%q,%v)", first, err)
	}
	second, err := searxngSecret()
	if err != nil {
		test.Fatal(err)
	}
	if first != second {
		test.Errorf("searxng secret not stable across runs: %q vs %q", first, second)
	}
}

// TestReconcileGatesOptionalOdysseus: odysseus is brought up when enabled and
// skipped when not (via the fake's per-service status mirroring the real gating).
func TestReconcileGatesOptionalOdysseus(test *testing.T) {
	services := &fakeServices{}
	enabledStatuses, err := services.Reconcile("", "127.0.0.1", []string{"odysseus"}, nil)
	if err != nil {
		test.Fatal(err)
	}
	if stateOf(enabledStatuses, "odysseus") != "running" {
		test.Errorf("odysseus should be running when enabled: %+v", enabledStatuses)
	}

	disabled := &fakeServices{}
	disabledStatuses, err := disabled.Reconcile("", "127.0.0.1", nil, nil)
	if err != nil {
		test.Fatal(err)
	}
	if stateOf(disabledStatuses, "odysseus") != "disabled" {
		test.Errorf("odysseus should be disabled when not enabled: %+v", disabledStatuses)
	}
}

// --- TASK B: live service log capture --------------------------------------

// logCaptureProber is a fake that reports docker present, reports a fixed set of
// containers as RUNNING (via `ps`), and returns canned `docker logs` output for
// any container — so CaptureServiceLogs can be exercised without a live daemon.
// It records its calls so the test can assert the `docker logs` invocation shape.
type logCaptureProber struct {
	running map[string]bool // container name → running
	calls   [][]string
}

func (prober *logCaptureProber) LookPath(file string) (string, error) {
	if file == "docker" {
		return "/usr/bin/docker", nil
	}
	return "", exec.ErrNotFound
}
func (prober *logCaptureProber) Exists(string) bool { return false }
func (prober *logCaptureProber) Run(name string, args ...string) ([]byte, error) {
	prober.calls = append(prober.calls, append([]string{name}, args...))
	if len(args) == 0 {
		return nil, nil
	}
	switch args[0] {
	case "ps":
		// `ps --filter name=^/<name>$ ... --format {{.Names}}` → the name if running.
		for _, arg := range args {
			if filterName, found := strings.CutPrefix(arg, "name=^/"); found {
				container := strings.TrimSuffix(filterName, "$")
				if prober.running[container] {
					return []byte(container + "\n"), nil
				}
			}
		}
		return nil, nil
	case "logs":
		// The container name is the last arg; return canned output keyed to it.
		container := args[len(args)-1]
		return []byte("LOG-" + container + "\n"), nil
	}
	return nil, nil
}

// TestCaptureServiceLogsWritesRunningContainers: a running container's `docker
// logs` output is snapshotted to ~/.ai-platform/logs/<name>.log (aip- prefix
// stripped), a stopped container is skipped, and the `docker logs` call carries
// --tail/--timestamps.
func TestCaptureServiceLogsWritesRunningContainers(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	prober := &logCaptureProber{running: map[string]bool{
		litellmContainer:          true, // aip-litellm running
		presidioAnalyzerContainer: true, // aip-presidio-analyzer running
		// aip-litellm-db, aip-presidio-anonymizer, and everything else are stopped.
	}}
	services := realServices{prober: prober}
	if err := services.CaptureServiceLogs(); err != nil {
		test.Fatal(err)
	}

	logsDir := filepath.Join(home, ".ai-platform", "logs")
	// Running containers are captured to <stripped-name>.log with their canned output.
	for container, wantBase := range map[string]string{
		litellmContainer:          "litellm.log",
		presidioAnalyzerContainer: "presidio-analyzer.log",
	} {
		content, err := os.ReadFile(filepath.Join(logsDir, wantBase))
		if err != nil {
			test.Fatalf("expected %s to be written: %v", wantBase, err)
		}
		if got := strings.TrimSpace(string(content)); got != "LOG-"+container {
			test.Errorf("%s content = %q, want %q", wantBase, got, "LOG-"+container)
		}
	}
	// A stopped container (the litellm DB) must NOT be captured.
	if _, err := os.Stat(filepath.Join(logsDir, "litellm-db.log")); !os.IsNotExist(err) {
		test.Errorf("stopped aip-litellm-db must not be captured (err=%v)", err)
	}

	// The `docker logs` call must use --tail and --timestamps.
	var sawLogsCall bool
	for _, call := range prober.calls {
		if len(call) >= 2 && call[0] == "docker" && call[1] == "logs" {
			sawLogsCall = true
			joined := strings.Join(call, " ")
			if !strings.Contains(joined, "--tail") || !strings.Contains(joined, "--timestamps") {
				test.Errorf("docker logs call missing --tail/--timestamps: %s", joined)
			}
		}
	}
	if !sawLogsCall {
		test.Error("CaptureServiceLogs made no `docker logs` call")
	}
}

// TestServiceContainersMapping: the logical service → container(s) mapping covers
// the multi-container services (presidio is two; odysseus is four) and the simple
// 1:1 ones, and the file-name derivation strips the aip- prefix.
func TestServiceContainersMapping(test *testing.T) {
	cases := map[string][]string{
		"litellm":  {litellmContainer, litellmDBContainer},
		"presidio": {presidioAnalyzerContainer, presidioAnonymizerContainer},
		"odysseus": {odysseusContainer, chromadbContainer, searxngContainer, ntfyContainer},
		"dns":      {dnsContainer},
		"unknown":  nil,
	}
	for service, want := range cases {
		got := serviceContainers(service)
		if len(got) != len(want) {
			test.Errorf("serviceContainers(%q) = %v, want %v", service, got, want)
			continue
		}
		for index := range want {
			if got[index] != want[index] {
				test.Errorf("serviceContainers(%q)[%d] = %q, want %q", service, index, got[index], want[index])
			}
		}
	}
	if got := logFileNameFor("aip-presidio-analyzer"); got != "presidio-analyzer.log" {
		test.Errorf("logFileNameFor(aip-presidio-analyzer) = %q, want presidio-analyzer.log", got)
	}
}

// TestCaptureServiceLogsNoRuntimeIsNoOp: with no container runtime present,
// CaptureServiceLogs is a no-op (no error, no files) — capture must never fail a
// reconcile/status on a host without docker.
func TestCaptureServiceLogsNoRuntimeIsNoOp(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	services := realServices{prober: fakeProber{}} // no docker in LookPath
	if err := services.CaptureServiceLogs(); err != nil {
		test.Fatalf("CaptureServiceLogs with no runtime should be a no-op, got %v", err)
	}
}

// --- TASK A: nginx proxy readiness probe -----------------------------------

// TestProxyReachableUpAndDown: the readiness probe treats ANY HTTP response (even
// a 404/502 from a not-ready upstream) as "up and forwarding", and a transport
// error (connection refused) as down — exercised behind the proxyHTTPGet seam so
// no live server is needed.
func TestProxyReachableUpAndDown(test *testing.T) {
	original := proxyHTTPGet
	defer func() { proxyHTTPGet = original }()

	// Any HTTP response (even 404) → up.
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusBadGateway} {
		proxyHTTPGet = func(string) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		if !proxyReachable("http://127.0.0.1:18787/health/liveliness") {
			test.Errorf("HTTP %d should be treated as up/forwarding", status)
		}
	}

	// Transport error (connection refused) → down.
	proxyHTTPGet = func(string) (*http.Response, error) {
		return nil, errors.New("dial tcp 127.0.0.1:18787: connect: connection refused")
	}
	if proxyReachable("http://127.0.0.1:18787/health/liveliness") {
		test.Error("a transport error should be treated as down")
	}
}
