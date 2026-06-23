package setup

import (
	"errors"
	"io"
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
	reconciled    bool
	provider      string
	bindHost      string
	optional      []string
	pulledEnabled []string
	installed     bool
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
func (services *fakeServices) Status() ([]ServiceStatus, error) {
	return []ServiceStatus{{Name: "litellm", Mode: "container", State: "running", Healthy: true}}, nil
}
func (services *fakeServices) Control(action, service string) ([]ServiceStatus, error) {
	return []ServiceStatus{{Name: "litellm", Mode: "container", State: action + "ed"}}, nil
}
func (services *fakeServices) InstallPrerequisite(_ Prerequisite, _ io.Writer) error {
	services.installed = true
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
		"-p", "127.0.0.1:14000:4000",
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

// TestLiteLLMRunArgsBindHost asserts the host port is published on the bindHost
// the role dictates: loopback for standalone, 0.0.0.0 for a server.
func TestLiteLLMRunArgsBindHost(test *testing.T) {
	standalone := strings.Join(litellmRunArgs("/cfg/config.yaml", "127.0.0.1", containerImage("litellm")), " ")
	if !strings.Contains(standalone, "-p 127.0.0.1:14000:4000") {
		test.Errorf("standalone bind: want -p 127.0.0.1:14000:4000 in %s", standalone)
	}
	server := strings.Join(litellmRunArgs("/cfg/config.yaml", "0.0.0.0", containerImage("litellm")), " ")
	if !strings.Contains(server, "-p 0.0.0.0:14000:4000") {
		test.Errorf("server bind: want -p 0.0.0.0:14000:4000 in %s", server)
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
	if err := ensureProxy(prober, "docker", "127.0.0.1"); err != nil {
		test.Fatal(err)
	}
	confPath := filepath.Join(home, ".ai-platform", "config", "proxy", "nginx.conf")
	content, err := os.ReadFile(confPath)
	if err != nil {
		test.Fatalf("nginx.conf not written: %v", err)
	}
	rendered := string(content)
	if !strings.Contains(rendered, "proxy_pass http://aip-headroom:8787;") {
		test.Errorf("nginx.conf must reverse-proxy to Headroom:\n%s", rendered)
	}
	if !strings.Contains(rendered, "proxy_buffering off;") {
		test.Errorf("nginx.conf must disable buffering for SSE streaming:\n%s", rendered)
	}
	// nginx takes over host :18787 and forwards to Headroom on :80 internally.
	launch := strings.Join(runArgsFor(prober), " ")
	if !strings.Contains(launch, "-p 127.0.0.1:18787:80") {
		test.Errorf("proxy must publish the gateway port 18787: %s", launch)
	}
	if !strings.Contains(launch, confPath+":/etc/nginx/nginx.conf:ro") {
		test.Errorf("proxy did not bind-mount nginx.conf: %s", launch)
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
// the companions are internal-only (no -p host publish), and the app publishes
// :7000, mounts the host Docker socket, and routes models through aip-proxy.
func TestEnsureOdysseusGroupRunArgs(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	prober := &recordingProber{}
	if err := ensureOdysseus(prober, "docker", "127.0.0.1"); err != nil {
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

	// The app: publishes :7000, mounts the Docker socket, routes via aip-proxy.
	appArgs := runArgsForContainer(prober, odysseusContainer)
	if appArgs == nil {
		test.Fatalf("aip-odysseus was not launched: %v", prober.calls)
	}
	app := strings.Join(appArgs, " ")
	if !strings.Contains(app, "-p 127.0.0.1:7000:7000") {
		test.Errorf("odysseus must publish :7000: %s", app)
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
