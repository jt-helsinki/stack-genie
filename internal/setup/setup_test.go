package setup

import (
	"errors"
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
	reconciled bool
	provider   string
	bindHost   string
}

func (services *fakeServices) Reconcile(providerConfig, bindHost string, progress func(string)) ([]ServiceStatus, error) {
	services.reconciled = true
	services.provider = providerConfig
	services.bindHost = bindHost
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
		"microsandbox runtime", "install.microsandbox.dev", "docs.microsandbox.dev", "how to install", "web:",
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
		"-e", "LLM_GUARD_API_BASE=http://aip-llm-guard:8000",
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
	for _, name := range []string{"ollama", "presidio", "llm-guard", "litellm", "headroom", "proxy", "open-webui"} {
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
// wiring depends on: LLM Guard starts AFTER presidio and BEFORE litellm (LiteLLM's
// callback points at it), and the nginx proxy starts AFTER headroom (it forwards
// to Headroom).
func TestDesiredServicesOrder(test *testing.T) {
	specs := desiredServices()
	presidio := serviceIndex(specs, "presidio")
	llmGuard := serviceIndex(specs, "llm-guard")
	litellm := serviceIndex(specs, "litellm")
	headroom := serviceIndex(specs, "headroom")
	proxy := serviceIndex(specs, "proxy")
	if presidio >= llmGuard || llmGuard >= litellm {
		test.Errorf("llm-guard must be after presidio and before litellm: presidio=%d llm-guard=%d litellm=%d", presidio, llmGuard, litellm)
	}
	if headroom >= proxy {
		test.Errorf("proxy must be after headroom: headroom=%d proxy=%d", headroom, proxy)
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

func TestEnsureLLMGuardRendersSecurityScannersOnly(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	prober := &recordingProber{}
	if err := ensureLLMGuard(prober, "docker"); err != nil {
		test.Fatal(err)
	}
	scannersPath := filepath.Join(home, ".ai-platform", "config", "llm-guard", "scanners.yml")
	content, err := os.ReadFile(scannersPath)
	if err != nil {
		test.Fatalf("scanners.yml not written: %v", err)
	}
	rendered := string(content)
	for _, want := range []string{"input_scanners:", "output_scanners:", "PromptInjection", "Secrets", "Regex", "Bearer "} {
		if !strings.Contains(rendered, want) {
			test.Errorf("scanners.yml missing %q:\n%s", want, rendered)
		}
	}
	// Security-only: the prompt-corrupting scanners must NOT be present.
	for _, forbidden := range []string{"Anonymize", "Toxicity", "BanTopics", "Sentiment", "Language"} {
		if strings.Contains(rendered, forbidden) {
			test.Errorf("scanners.yml must not enable %q (corrupts coding prompts):\n%s", forbidden, rendered)
		}
	}
	// The run mounts the rendered scanners and uses the internal-only image (no -p).
	launch := strings.Join(runArgsFor(prober), " ")
	if !strings.Contains(launch, scannersPath+":/home/user/app/config/scanners.yml") {
		test.Errorf("llm-guard run did not bind-mount scanners.yml: %s", launch)
	}
	if !strings.Contains(launch, containerImage("llm-guard")) {
		test.Errorf("llm-guard run did not use %s: %s", containerImage("llm-guard"), launch)
	}
	if strings.Contains(launch, "-p ") {
		test.Errorf("llm-guard must be internal-only (no host publish): %s", launch)
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
