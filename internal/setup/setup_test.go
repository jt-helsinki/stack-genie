package setup

import (
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/conffile"
	"github.com/jt-helsinki/stack-genie/internal/envfile"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/paths"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/versions"
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
	reconciled        bool
	provider          string
	bindHost          string
	optional          []string
	pulledEnabled     []string
	pulledGuardrails  []string
	updatedImages     bool
	updatedEnabled    []string
	updatedGuardrails []string
	installed         bool
	capturedLogs      bool
	controlAction     string
	controlService    string
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
func (services *fakeServices) PullImages(optional []string, guardrails []string, _ io.Writer, progress func(string)) error {
	services.pulledEnabled = optional
	services.pulledGuardrails = guardrails
	if progress != nil {
		progress("pulling (fake)")
	}
	return nil
}
func (services *fakeServices) UpdateImages(optional []string, guardrails []string, _ io.Writer, progress func(string)) error {
	services.updatedImages = true
	services.updatedEnabled = optional
	services.updatedGuardrails = guardrails
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

// Phase E: Run invokes the catalog fetch (best-effort) after the service reconcile
// on a non-client role, and a fetch error becomes a warning rather than failing.
func TestRunFetchesCatalogBestEffort(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()
	fetched := 0
	deps.FetchCatalog = func() error { fetched++; return nil }

	report, err := Run(Options{}, deps)
	if err != nil {
		test.Fatal(err)
	}
	if fetched != 1 {
		test.Fatalf("FetchCatalog called %d times, want 1 (after the reconcile)", fetched)
	}
	_ = report
}

func TestRunCatalogFetchErrorIsWarningNotFatal(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()
	deps.FetchCatalog = func() error { return errors.New("models.dev unreachable") }

	report, err := Run(Options{}, deps)
	if err != nil {
		test.Fatalf("a catalog fetch error must not fail setup: %v", err)
	}
	found := false
	for _, warning := range report.Warnings {
		if strings.Contains(warning, "model catalog") && strings.Contains(warning, "unreachable") {
			found = true
		}
	}
	if !found {
		test.Fatalf("expected a model-catalog warning, got %v", report.Warnings)
	}
}

// A client role runs no local service tier, so the catalog fetch is skipped.
func TestRunClientRoleSkipsCatalogFetch(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	services := &fakeServices{}
	deps := Deps{
		GOOS: "darwin", GOARCH: "arm64",
		Prober: fakeProber{
			bins:      map[string]bool{"msb": true},
			dockerOut: "[name=seccomp name=rootless]",
		},
		Now:      func() string { return "2026-06-18T00:00:00Z" },
		Services: services,
	}
	fetched := 0
	deps.FetchCatalog = func() error { fetched++; return nil }

	if _, err := Run(Options{Mode: runtime.RoleClient, ServerAddr: "10.0.0.5"}, deps); err != nil {
		test.Fatal(err)
	}
	if fetched != 0 {
		test.Fatalf("client role should NOT fetch the catalog, called %d times", fetched)
	}
}

func TestRunPersistsOptionsDomain(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()

	report, err := Run(Options{Mode: runtime.RoleServer, Domain: "aip.example.com"}, deps)
	if err != nil {
		test.Fatal(err)
	}
	if report.Runtime.Domain != "aip.example.com" {
		test.Fatalf("report domain = %q, want aip.example.com", report.Runtime.Domain)
	}
	persisted, err := runtime.Load()
	if err != nil || persisted == nil {
		test.Fatalf("load runtime.yaml: %v", err)
	}
	if persisted.Domain != "aip.example.com" {
		test.Fatalf("persisted domain = %q, want aip.example.com", persisted.Domain)
	}
}

func TestRunPreservesExistingDomainWhenUnset(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()

	// First run sets a domain (e.g. via `ai domain` / a prior server setup).
	if _, err := Run(Options{Mode: runtime.RoleServer, Domain: "set.example.com"}, deps); err != nil {
		test.Fatal(err)
	}
	// A subsequent run with NO Options.Domain must NOT clobber it (load-modify-save).
	report, err := Run(Options{Mode: runtime.RoleServer}, deps)
	if err != nil {
		test.Fatal(err)
	}
	if report.Runtime.Domain != "set.example.com" {
		test.Fatalf("domain not preserved: got %q, want set.example.com", report.Runtime.Domain)
	}
	persisted, err := runtime.Load()
	if err != nil || persisted == nil {
		test.Fatalf("load runtime.yaml: %v", err)
	}
	if persisted.Domain != "set.example.com" {
		test.Fatalf("persisted domain not preserved: got %q", persisted.Domain)
	}
}

func TestRunStandaloneLeavesDomainEmptyForDefault(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()

	report, err := Run(Options{}, deps)
	if err != nil {
		test.Fatal(err)
	}
	// Standalone with no domain stays empty so ResolveDomain falls back to aip.local.
	if report.Runtime.Domain != "" {
		test.Fatalf("standalone domain = %q, want empty (default aip.local)", report.Runtime.Domain)
	}
	if report.Runtime.ResolveDomain() != runtime.DefaultDomain {
		test.Fatalf("resolved domain = %q, want %q", report.Runtime.ResolveDomain(), runtime.DefaultDomain)
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
	test.Setenv("LITELLM_SALT_KEY", "")
	prober := fakeProber{dockerOut: "UI_USERNAME=admin\nUI_PASSWORD=hunter2\nLITELLM_MASTER_KEY=sk-abc\nLITELLM_SALT_KEY=sk-salt-xyz\nOTHER=x\n"}
	preserveLiteLLMSecretsInEnv(prober, "docker")
	if got := os.Getenv("UI_PASSWORD"); got != "hunter2" {
		test.Errorf("UI_PASSWORD = %q, want preserved hunter2", got)
	}
	if got := os.Getenv("LITELLM_MASTER_KEY"); got != "sk-abc" {
		test.Errorf("LITELLM_MASTER_KEY = %q, want preserved sk-abc", got)
	}
	// The salt key must be preserved too (a rotated salt orphans stored credentials).
	if got := os.Getenv("LITELLM_SALT_KEY"); got != "sk-salt-xyz" {
		test.Errorf("LITELLM_SALT_KEY = %q, want preserved sk-salt-xyz", got)
	}
}

// TestGenerateSaltKey verifies a fresh salt key has the sk- shape and is non-empty
// and unique across calls.
func TestGenerateSaltKey(test *testing.T) {
	first := generateSaltKey()
	second := generateSaltKey()
	if !strings.HasPrefix(first, "sk-") || len(first) <= len("sk-") {
		test.Errorf("generateSaltKey() = %q, want sk-<hex>", first)
	}
	if first == second {
		test.Errorf("generateSaltKey() produced identical keys %q", first)
	}
}

// TestResolveLiteLLMSaltKeyPrefersEnv verifies an explicit process-env salt key
// wins over generation (the env-file / exported value path).
func TestResolveLiteLLMSaltKeyPrefersEnv(test *testing.T) {
	test.Setenv("LITELLM_SALT_KEY", "sk-from-env")
	if got := resolveLiteLLMSaltKey(); got != "sk-from-env" {
		test.Errorf("resolveLiteLLMSaltKey() = %q, want the explicit env value", got)
	}
}

// TestGenerateMasterKey verifies a fresh master key has the sk- shape, is
// non-empty, and is unique across calls.
func TestGenerateMasterKey(test *testing.T) {
	first := generateMasterKey()
	second := generateMasterKey()
	if !strings.HasPrefix(first, "sk-") || len(first) <= len("sk-") {
		test.Errorf("generateMasterKey() = %q, want sk-<hex>", first)
	}
	if first == second {
		test.Errorf("generateMasterKey() produced identical keys %q", first)
	}
}

// TestPersistLiteLLMInfraKeysWritesMasterAndSalt verifies the master + salt keys in
// the process env are written to the 0600 env file (so they survive a container-down
// relaunch) while the UI password is deliberately NOT persisted.
func TestPersistLiteLLMInfraKeysWritesMasterAndSalt(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Setenv("LITELLM_MASTER_KEY", "sk-master-abc")
	test.Setenv("LITELLM_SALT_KEY", "sk-salt-xyz")
	test.Setenv("UI_PASSWORD", "hunter2")

	persistLiteLLMInfraKeys()

	path, err := envfile.Path()
	if err != nil {
		test.Fatalf("envfile.Path() error: %s", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		test.Fatalf("read env file: %s", err)
	}
	content := string(data)
	if !strings.Contains(content, "sk-master-abc") {
		test.Errorf("env file missing master key; got:\n%s", content)
	}
	if !strings.Contains(content, "sk-salt-xyz") {
		test.Errorf("env file missing salt key; got:\n%s", content)
	}
	if strings.Contains(content, "hunter2") || strings.Contains(content, "UI_PASSWORD") {
		test.Errorf("UI password must NOT be auto-persisted; got:\n%s", content)
	}
	// The file must be 0600 (it holds secrets).
	info, err := os.Stat(path)
	if err != nil {
		test.Fatalf("stat env file: %s", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		test.Errorf("env file perm = %o, want 600", perm)
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

// owningService points companion containers at their managing logical service. No
// such surfaced companions exist today (Open WebUI moved to a per-workspace in-VM
// app and Odysseus — which owned chromadb/searxng/ntfy — was removed), so it
// returns "" for every name. The hook is retained for a future multi-container
// optional service.
func TestOwningServiceReturnsEmpty(test *testing.T) {
	for _, name := range []string{"chromadb", "searxng", "ntfy", "litellm", "ollama", "presidio", "litellm-db", "", "bogus"} {
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

// TestNoOptionalServicesToToggle: there are currently NO optional host services
// (Open WebUI moved to a per-workspace in-VM app and Odysseus was removed), so the
// universe of optional services is empty and enable/disable have nothing to toggle.
func TestNoOptionalServicesToToggle(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if got := optionalServiceNames(); len(got) != 0 {
		test.Fatalf("optionalServiceNames() = %v, want empty (no optional host services)", got)
	}
	deps, _ := healthyDeps()
	if _, err := Run(Options{}, deps); err != nil {
		test.Fatal(err)
	}
	// No optional services are enabled after setup.
	info, _ := runtime.Load()
	if len(info.OptionalServices) != 0 {
		test.Errorf("no optional services should be enabled, got %v", info.OptionalServices)
	}
	// enable/disable a non-optional (core) name → exit 2 (it is not optional).
	if _, err := ControlService(deps, "enable", "litellm"); exitCodeOf(test, err) != output.ExitInvalidInput {
		test.Fatalf("enabling a core service should be exit 2, got %v", err)
	}
	// enable/disable with no service name → exit 2.
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
	if _, ok := file.Services["dns"]; !ok {
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
		// Always present: reach the host-native Ollama through the host gateway.
		"--add-host=host.docker.internal:host-gateway",
		// INTERNAL-ONLY: no host publish — reached by name on aip-net; nginx fronts it.
		"-v", "/cfg/litellm/config.yaml:/app/config.yaml",
		"-e", "UI_USERNAME=admin",
		"-e", "UI_PASSWORD",
		"-e", "LITELLM_MASTER_KEY",
		"-e", "LITELLM_SALT_KEY",
		"-e", "DATABASE_URL=postgresql://litellm@aip-litellm-db:5432/litellm",
		"-e", "PRESIDIO_ANALYZER_API_BASE=http://aip-presidio-analyzer:3000",
		"-e", "PRESIDIO_ANONYMIZER_API_BASE=http://aip-presidio-anonymizer:3000",
		"-e", "REDIS_HOST=aip-valkey",
		"-e", "REDIS_PORT=6379",
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
	if strings.Contains(joined, "UI_PASSWORD=") || strings.Contains(joined, "LITELLM_MASTER_KEY=") ||
		strings.Contains(joined, "LITELLM_SALT_KEY=") {
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
				Address: "http://litellm.aip.local:18787", Console: "http://litellm.aip.local:18787/ui/login"},
			{Name: "ollama", Mode: "container", State: "running", Healthy: true,
				Address: "http://aip.local:18787/ollama"},
			{Name: "presidio", Mode: "container", State: "running", Healthy: true},
		},
	}
	rendered := report.Human()
	// litellm shows both its address and the admin UI URL.
	if !strings.Contains(rendered, "http://litellm.aip.local:18787 · UI http://litellm.aip.local:18787/ui/login") {
		test.Errorf("litellm address+UI missing:\n%s", rendered)
	}
	// ollama shows its address only (no UI).
	if !strings.Contains(rendered, "http://aip.local:18787/ollama") {
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
	for _, name := range []string{"ollama", "presidio", "litellm", "headroom", "proxy", "dns"} {
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
// wiring depends on: Presidio (the secret-masking guardrail backend) AND Headroom
// (the compression guardrail backend) both start BEFORE litellm (LiteLLM is launched
// with PRESIDIO_*_API_BASE and calls Headroom at aip-headroom:8787/v1/compress), and
// the nginx proxy starts AFTER litellm (it forwards the model path to LiteLLM).
func TestDesiredServicesOrder(test *testing.T) {
	specs := desiredServices()
	presidio := serviceIndex(specs, "presidio")
	litellm := serviceIndex(specs, "litellm")
	headroom := serviceIndex(specs, "headroom")
	proxy := serviceIndex(specs, "proxy")
	if presidio >= litellm {
		test.Errorf("presidio must be before litellm: presidio=%d litellm=%d", presidio, litellm)
	}
	if headroom >= litellm {
		test.Errorf("headroom must be before litellm (LiteLLM's guardrail calls it): headroom=%d litellm=%d", headroom, litellm)
	}
	if litellm >= proxy {
		test.Errorf("proxy must be after litellm: litellm=%d proxy=%d", litellm, proxy)
	}
}

// TestRequiredImagesCoversEveryService asserts requiredImages returns the
// containerImage ref (repo:tag form) for every service-tier container, including
// the ones that have no Status line — litellm-db is the notable omission from
// desiredServices and must be present here, alongside proxy and dns. There are no
// optional host services, so this is the full set. Every guardrail is enabled
// (GuardrailKeys) so the Presidio images are included.
func TestRequiredImagesCoversEveryService(test *testing.T) {
	images := requiredImages(optionalServiceNames(), litellm.GuardrailKeys()) // all optional (none) + all guardrails
	have := make(map[string]bool, len(images))
	for _, ref := range images {
		if !strings.Contains(ref, ":") {
			test.Errorf("image ref %q is not in image:tag form", ref)
		}
		have[ref] = true
	}
	// Ollama is host-native (no aip-ollama container), so its image is deliberately
	// NOT in this set.
	for _, service := range []string{
		"presidio-analyzer", "presidio-anonymizer",
		"litellm", "litellm-db",
		"headroom", "proxy", "dns",
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

// TestRequiredImagesSkipsPresidioWhenGuardrailOff: with the secret-masking guardrail
// NOT selected (the default), the Presidio images are NOT pulled — no point consuming
// the bandwidth/disk for a guardrail that isn't rendered. The core images still pull.
func TestRequiredImagesSkipsPresidioWhenGuardrailOff(test *testing.T) {
	images := requiredImages(optionalServiceNames(), litellm.DefaultGuardrails()) // Headroom only
	presidio := containerImage("presidio-analyzer")
	for _, ref := range images {
		if ref == presidio {
			test.Errorf("Presidio image %q must NOT be pulled when secret-masking is off: %v", presidio, images)
		}
	}
	// Sanity: a core image (litellm) is still present.
	if !slices.Contains(images, containerImage("litellm")) {
		test.Errorf("core litellm image must still be pulled: %v", images)
	}
	// And WITH secret-masking enabled, Presidio IS pulled.
	withMasking := requiredImages(optionalServiceNames(), []string{litellm.GuardrailSecretMasking})
	if !slices.Contains(withMasking, presidio) {
		test.Errorf("Presidio image must be pulled when secret-masking is on: %v", withMasking)
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
	if err := ensureProxy(prober, "docker", "127.0.0.1", "aip.local"); err != nil {
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
	// The agent CHAT path (/v1) now routes to LiteLLM DIRECTLY (LiteLLM calls
	// Headroom as an in-process compression guardrail — nginx no longer hops to it).
	if !strings.Contains(rendered, "location /v1/ {") || !strings.Contains(rendered, "proxy_pass http://aip-litellm:4000;") {
		test.Errorf("nginx.conf must route the /v1 model path to LiteLLM directly:\n%s", rendered)
	}
	// Headroom must NOT appear as an nginx upstream anymore.
	if strings.Contains(rendered, "aip-headroom") {
		test.Errorf("nginx.conf must not reference aip-headroom (it is a LiteLLM guardrail now):\n%s", rendered)
	}
	// The LiteLLM admin surface is fronted on /llm (prefix stripped → :4000).
	if !strings.Contains(rendered, "location /llm/ {") || !strings.Contains(rendered, "proxy_pass http://aip-litellm:4000/;") {
		test.Errorf("nginx.conf must front the LiteLLM admin surface on /llm:\n%s", rendered)
	}
	// The Ollama HTTP API is fronted on /ollama (prefix stripped → :11434). Ollama is
	// host-native, so the upstream is the host gateway (host.docker.internal).
	if !strings.Contains(rendered, "location /ollama/ {") || !strings.Contains(rendered, "proxy_pass http://host.docker.internal:11434/;") {
		test.Errorf("nginx.conf must front the host-native Ollama API on /ollama:\n%s", rendered)
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
	// litellm is the ONLY host UI vhost — no chat/odysseus vhosts render anymore.
	if strings.Contains(rendered, "server_name chat.aip.local;") || strings.Contains(rendered, "server_name odysseus.aip.local;") {
		test.Errorf("no chat/odysseus UI vhosts should render (Open WebUI is in-VM; Odysseus removed):\n%s", rendered)
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

// TestProxyNginxConfThreadsDomain: the rendered vhost server_names use the given
// domain, the LiteLLM admin UI vhost redirects / → /ui and bypasses Headroom, and
// litellm is the ONLY host UI vhost (no chat./odysseus.).
func TestProxyNginxConfThreadsDomain(test *testing.T) {
	rendered := proxyNginxConf("dev.example.com")
	if !strings.Contains(rendered, "server_name dev.example.com localhost _;") {
		test.Errorf("default server must use the threaded domain:\n%s", rendered)
	}
	if !strings.Contains(rendered, "server_name litellm.dev.example.com;") {
		test.Errorf("litellm vhost must use the threaded domain:\n%s", rendered)
	}
	// LiteLLM admin UI: / → /ui/login redirect.
	if !strings.Contains(rendered, "return 302 /ui/login;") {
		test.Errorf("litellm vhost should redirect / → /ui/login:\n%s", rendered)
	}
	// The litellm UI vhost proxies to :4000 directly (Headroom is not an upstream).
	litellmBlock := rendered[strings.Index(rendered, "server_name litellm.dev.example.com;"):]
	if strings.Contains(litellmBlock[:strings.Index(litellmBlock, "}")], "aip-headroom") {
		test.Errorf("the litellm UI vhost must not reference aip-headroom:\n%s", rendered)
	}
	// No chat./odysseus. vhosts render anymore.
	if strings.Contains(rendered, "chat.dev.example.com") || strings.Contains(rendered, "odysseus.dev.example.com") {
		test.Errorf("no chat/odysseus vhost should render:\n%s", rendered)
	}
}

// --- optional-services framework -------------------------------------------

// There are currently NO optional host services (Open WebUI moved to a
// per-workspace in-VM app and Odysseus was removed). The optional MECHANISM is
// retained, so these tests pin the empty-set behaviour.

// TestNoOptionalServices pins that the optional set is empty and every remaining
// service is core; desiredServices == coreServices.
func TestNoOptionalServices(test *testing.T) {
	if got := optionalServiceNames(); len(got) != 0 {
		test.Errorf("optionalServiceNames = %v, want empty (no optional host services)", got)
	}
	for _, core := range []string{"ollama", "presidio", "litellm", "headroom", "proxy", "dns"} {
		if isOptionalService(core) {
			test.Errorf("%q must be a core service, not optional", core)
		}
		if !hasService(coreServices(), core) {
			test.Errorf("%q missing from coreServices", core)
		}
		if !hasService(desiredServices(), core) {
			test.Errorf("%q missing from desiredServices: %+v", core, desiredServices())
		}
	}
	// desiredServices == coreServices when there are no optional services.
	if len(desiredServices()) != len(coreServices()) {
		test.Errorf("desiredServices (%d) should equal coreServices (%d) with no optional services",
			len(desiredServices()), len(coreServices()))
	}
}

// TestResolveOptionalPrecedence pins the enabled-set precedence with no optional
// services: every path resolves to the empty set (an unknown persisted/explicit
// name is dropped because the optional universe is empty).
func TestResolveOptionalPrecedence(test *testing.T) {
	// First run, no explicit choice, no persisted set → empty (no default optional).
	if got := ResolveOptional(Options{}, nil); len(got) != 0 {
		test.Errorf("default first-run set = %v, want []", got)
	}
	// Explicit "none" (OptionalSet with an empty slice) → empty.
	if got := ResolveOptional(Options{OptionalSet: true, Optional: []string{}}, nil); len(got) != 0 {
		test.Errorf("explicit none = %v, want []", got)
	}
	// An explicit name with no matching optional service is dropped → empty.
	if got := ResolveOptional(Options{OptionalSet: true, Optional: []string{"bogus"}}, nil); len(got) != 0 {
		test.Errorf("unknown explicit name should be dropped: %v", got)
	}
	// Unknown persisted names are dropped (a retired optional service) → empty.
	stale := &runtime.Info{OptionalServices: []string{"open-webui", "retired-tool"}}
	if got := ResolveOptional(Options{}, stale); len(got) != 0 {
		test.Errorf("retired persisted names should be dropped: %v", got)
	}
}

// TestResolveGuardrailsPrecedence pins the guardrail selection precedence: explicit
// choice (incl. empty "none") > persisted > default (Headroom only). Unknown keys are
// dropped and the result follows the litellm.Guardrails catalog order.
func TestResolveGuardrailsPrecedence(test *testing.T) {
	// First run, no explicit choice, no persisted set → the default (Headroom only).
	got := ResolveGuardrails(Options{}, nil)
	if len(got) != 1 || got[0] != litellm.GuardrailHeadroom {
		test.Errorf("default first-run guardrails = %v, want [%q]", got, litellm.GuardrailHeadroom)
	}
	// Explicit "none" (GuardrailsSet with an empty slice) → empty (every guardrail off).
	if got := ResolveGuardrails(Options{GuardrailsSet: true, Guardrails: []string{}}, nil); len(got) != 0 {
		test.Errorf("explicit none = %v, want []", got)
	}
	// An explicit choice wins over both persisted and default.
	explicit := ResolveGuardrails(Options{GuardrailsSet: true, Guardrails: []string{litellm.GuardrailToolFirewall}}, &runtime.Info{Guardrails: []string{litellm.GuardrailHeadroom}})
	if len(explicit) != 1 || explicit[0] != litellm.GuardrailToolFirewall {
		test.Errorf("explicit choice = %v, want [%q]", explicit, litellm.GuardrailToolFirewall)
	}
	// A persisted set (no explicit choice) is honoured verbatim.
	persistedSet := ResolveGuardrails(Options{}, &runtime.Info{Guardrails: []string{litellm.GuardrailSecretMasking, litellm.GuardrailHeadroom}})
	if len(persistedSet) != 2 {
		test.Errorf("persisted set = %v, want the 2 persisted guardrails", persistedSet)
	}
	// Unknown keys (a retired guardrail) are dropped.
	if got := ResolveGuardrails(Options{GuardrailsSet: true, Guardrails: []string{"bogus", litellm.GuardrailHeadroom}}, nil); len(got) != 1 || got[0] != litellm.GuardrailHeadroom {
		test.Errorf("unknown key should be dropped: %v", got)
	}
}

// TestRunPersistsNoOptional: a first run persists no optional services (there are
// none) and reconciles none.
func TestRunPersistsNoOptional(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, services := healthyDeps()
	report, err := Run(Options{}, deps)
	if err != nil {
		test.Fatal(err)
	}
	if len(report.Runtime.OptionalServices) != 0 {
		test.Errorf("first run should persist no optional services, got %v", report.Runtime.OptionalServices)
	}
	if len(services.optional) != 0 {
		test.Errorf("Reconcile should receive no optional services, got %v", services.optional)
	}
	persisted, err := runtime.Load()
	if err != nil || persisted == nil {
		test.Fatalf("load runtime.yaml: %v", err)
	}
	if len(persisted.OptionalServices) != 0 {
		test.Errorf("runtime.yaml should persist no optional services: %v", persisted.OptionalServices)
	}
	// A first run with no guardrail choice persists the default (Headroom only).
	if len(persisted.Guardrails) != 1 || persisted.Guardrails[0] != litellm.GuardrailHeadroom {
		test.Errorf("first run should persist the default guardrails [%q], got %v", litellm.GuardrailHeadroom, persisted.Guardrails)
	}
}

// TestRunPersistsChosenGuardrails: an explicit guardrail selection is persisted to
// runtime.yaml verbatim (so the litellm render + Presidio gate pick it up on the next
// reconcile).
func TestRunPersistsChosenGuardrails(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()
	chosen := []string{litellm.GuardrailHeadroom, litellm.GuardrailToolFirewall}
	report, err := Run(Options{GuardrailsSet: true, Guardrails: chosen}, deps)
	if err != nil {
		test.Fatal(err)
	}
	if len(report.Runtime.Guardrails) != 2 {
		test.Errorf("report should carry the 2 chosen guardrails, got %v", report.Runtime.Guardrails)
	}
	persisted, err := runtime.Load()
	if err != nil || persisted == nil {
		test.Fatalf("load runtime.yaml: %v", err)
	}
	if len(persisted.Guardrails) != 2 {
		test.Errorf("runtime.yaml should persist the 2 chosen guardrails, got %v", persisted.Guardrails)
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

// TestStatusForHasNoOptional: with no optional services, statusFor lists only the
// core services and none of the CONTAINER-TIER services is marked Optional (the
// enable/disable optional-service set is empty). The host-native `vllm` summary line
// is legitimately opt-in — it carries Optional purely so `ai doctor` WARNS (not
// errors) when it is down — so it is excluded from this container-tier invariant.
func TestStatusForHasNoOptional(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	services := realServices{prober: fakeProber{}}
	statuses, err := services.statusFor(nil)
	if err != nil {
		test.Fatal(err)
	}
	for _, status := range statuses {
		if status.Name == "vllm" {
			continue // host-native opt-in backend; Optional is a doctor-severity marker
		}
		if status.Optional {
			test.Errorf("no service should be Optional, got %q", status.Name)
		}
	}
}

// TestStatusForPresidioDisabledWhenGuardrailOff: with no persisted guardrail set
// (default = Headroom only, secret-masking OFF), statusFor reports Presidio as
// "disabled" — intentionally not running, not a "stopped"/unhealthy error — while
// remaining a core (non-optional) service.
func TestStatusForPresidioDisabledWhenGuardrailOff(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	services := realServices{prober: fakeProber{}}
	statuses, err := services.statusFor(nil)
	if err != nil {
		test.Fatal(err)
	}
	var seen bool
	for _, status := range statuses {
		if status.Name != "presidio" {
			continue
		}
		seen = true
		if status.State != "disabled" {
			test.Errorf("presidio state = %q, want disabled (secret-masking off)", status.State)
		}
		if status.Optional {
			test.Errorf("presidio must stay a core (non-optional) service even when disabled")
		}
	}
	if !seen {
		test.Fatal("presidio missing from statusFor output")
	}
}

// TestStatusForListsPostgres: the Postgres backing LiteLLM (aip-litellm-db) is surfaced
// as its own "postgres" status line, positioned immediately after litellm — users
// expect to SEE it in the services list even though it is managed with litellm.
func TestStatusForListsPostgres(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	services := realServices{prober: fakeProber{}}
	statuses, err := services.statusFor(nil)
	if err != nil {
		test.Fatal(err)
	}
	litellmIdx, postgresIdx := -1, -1
	for index, status := range statuses {
		switch status.Name {
		case "litellm":
			litellmIdx = index
		case "postgres":
			postgresIdx = index
		}
	}
	if postgresIdx < 0 {
		test.Fatalf("postgres missing from statusFor output: %+v", statuses)
	}
	if litellmIdx < 0 || postgresIdx != litellmIdx+1 {
		test.Errorf("postgres must appear immediately after litellm: litellm=%d postgres=%d", litellmIdx, postgresIdx)
	}
}

// TestStatusForDisplayDomain: statusFor renders host-reachable endpoints through
// the single nginx gateway against the platform base DOMAIN — UI services as
// <subdomain>.<domain>:18787 vhosts, ollama as the host-CLI gateway path
// <domain>:18787/ollama, the proxy on <domain>:18787 — never the old direct
// per-service ports (which are internal-only now). dns stays loopback.
func TestStatusForDisplayDomain(test *testing.T) {
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

	// Default domain (aip.local) when none is persisted.
	test.Setenv("HOME", test.TempDir())
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, Role: runtime.RoleStandalone}); err != nil {
		test.Fatal(err)
	}
	services := realServices{prober: fakeProber{}}
	standalone, err := services.statusFor(nil)
	if err != nil {
		test.Fatal(err)
	}
	if got := consoleOf(standalone, "litellm"); got != "http://litellm.aip.local:18787/ui/login" {
		test.Errorf("standalone litellm console = %q, want http://litellm.aip.local:18787/ui/login", got)
	}
	if got := addressOf(standalone, "ollama"); got != "http://aip.local:18787/ollama" {
		test.Errorf("standalone ollama address = %q, want http://aip.local:18787/ollama", got)
	}
	if got := addressOf(standalone, "proxy"); got != "http://aip.local:18787" {
		test.Errorf("standalone proxy address = %q, want http://aip.local:18787", got)
	}
	// No host-side use of the old direct ports anywhere.
	for _, name := range []string{"litellm", "ollama", "proxy"} {
		got := addressOf(standalone, name)
		for _, deadPort := range []string{":14000", ":11434", ":18090", ":7000"} {
			if strings.Contains(got, deadPort) {
				test.Errorf("%s address %q must not use the internal-only port %s", name, got, deadPort)
			}
		}
	}
	if got := addressOf(standalone, "dns"); got != "127.0.0.1:15353/udp" {
		test.Errorf("standalone dns address = %q, want loopback unchanged", got)
	}

	// A configured domain is woven into the subdomain URLs.
	test.Setenv("HOME", test.TempDir())
	if err := runtime.Persist(&runtime.Info{SchemaVersion: runtime.SchemaVersion, Role: runtime.RoleServer, Domain: "build-host.lan"}); err != nil {
		test.Fatal(err)
	}
	server, err := services.statusFor(nil)
	if err != nil {
		test.Fatal(err)
	}
	if got := consoleOf(server, "litellm"); got != "http://litellm.build-host.lan:18787/ui/login" {
		test.Errorf("server litellm console = %q, want http://litellm.build-host.lan:18787/ui/login", got)
	}
	if got := addressOf(server, "dns"); got != "127.0.0.1:15353/udp" {
		test.Errorf("server dns address = %q, want loopback unchanged", got)
	}
}

// TestEnsureHeadroomIsInternalOnly: Headroom no longer publishes the gateway port
// 18787 — nginx (aip-proxy) owns it now. The launch must carry no host publish. It
// is a standalone compression SERVICE LiteLLM calls as a guardrail, so it must NOT
// carry OPENAI_TARGET_API_URL (that would loop litellm→headroom→litellm) and MUST
// disable telemetry.
func TestEnsureHeadroomIsInternalOnly(test *testing.T) {
	prober := &recordingProber{}
	if err := ensureHeadroom(prober, "docker"); err != nil {
		test.Fatal(err)
	}
	launch := strings.Join(runArgsFor(prober), " ")
	if strings.Contains(launch, "18787") || strings.Contains(launch, "-p ") {
		test.Errorf("headroom must be internal-only (no 18787 publish): %s", launch)
	}
	if strings.Contains(launch, "OPENAI_TARGET_API_URL") {
		test.Errorf("headroom must NOT set OPENAI_TARGET_API_URL (guardrail, not a proxy): %s", launch)
	}
	if !strings.Contains(launch, "HEADROOM_TELEMETRY=off") {
		test.Errorf("headroom run missing HEADROOM_TELEMETRY=off: %s", launch)
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
		// `ps --filter name=<name> ... --format {{.Names}}` → the name if running.
		for _, arg := range args {
			if container, found := strings.CutPrefix(arg, "name="); found {
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
// the multi-container services (presidio is two) and the simple 1:1 ones, and the
// file-name derivation strips the aip- prefix.
func TestServiceContainersMapping(test *testing.T) {
	cases := map[string][]string{
		"litellm":  {litellmContainer, litellmDBContainer},
		"presidio": {presidioAnalyzerContainer, presidioAnonymizerContainer},
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

// capturingProber records every Run argv so volume-mount tests can assert the
// host bind paths the ensure* funcs pass to `docker run`. It reports the litellm-db
// container as NOT running (empty ps output) so ensureLiteLLMDB proceeds to launch.
type capturingProber struct {
	runs [][]string
}

func (prober *capturingProber) LookPath(file string) (string, error) {
	return "/usr/bin/" + file, nil
}

func (prober *capturingProber) Run(name string, args ...string) ([]byte, error) {
	prober.runs = append(prober.runs, append([]string{name}, args...))
	// pg_isready (the readiness loop) → succeed immediately so the test is fast.
	if len(args) > 0 && args[0] == "exec" {
		return nil, nil
	}
	return nil, nil
}
func (prober *capturingProber) Exists(string) bool { return false }

// runContainsVolume reports whether any captured `run` argv carries `-v src:...`
// with the given host source path, returning the full mount spec it found.
func runContainsVolume(runs [][]string, hostSrc string) (string, bool) {
	for _, run := range runs {
		for index := 0; index < len(run)-1; index++ {
			if run[index] == "-v" && strings.HasPrefix(run[index+1], hostSrc+":") {
				return run[index+1], true
			}
		}
	}
	return "", false
}

// TestEnsureLiteLLMDBBindMountUnderVolumesDir asserts Postgres is launched with a
// HOST BIND MOUNT under ~/.ai-platform/volumes/litellm-db (not a docker named
// volume), targeting /var/lib/postgresql, and that the host dir is created.
func TestEnsureLiteLLMDBBindMountUnderVolumesDir(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	prober := &capturingProber{}
	if err := ensureLiteLLMDB(prober, "docker"); err != nil {
		test.Fatalf("ensureLiteLLMDB: %v", err)
	}

	volumesDir, err := paths.VolumesDir()
	if err != nil {
		test.Fatalf("VolumesDir: %v", err)
	}
	wantSrc := filepath.Join(volumesDir, "litellm-db")
	spec, ok := runContainsVolume(prober.runs, wantSrc)
	if !ok {
		test.Fatalf("no -v bind mount with host source %q in runs: %v", wantSrc, prober.runs)
	}
	if spec != wantSrc+":/var/lib/postgresql" {
		test.Errorf("db mount = %q, want %q", spec, wantSrc+":/var/lib/postgresql")
	}
	// Must NOT use the old docker NAMED volume.
	if named, found := runContainsVolume(prober.runs, "aip-litellm-db-data"); found {
		test.Errorf("must not use the legacy named volume, found %q", named)
	}
	if info, err := os.Stat(wantSrc); err != nil || !info.IsDir() {
		test.Errorf("expected the db bind dir %q to be created: %v", wantSrc, err)
	}
}

// TestOllamaEnvPairsForwardsPrefixed verifies the Ollama env (rendered for the
// compose debug artifact + documented for the host process) carries every OLLAMA_*
// var from the process env (i.e. from ~/.ai-platform/.ai-platform.env) EXCEPT
// OLLAMA_MODELS, which stays the platform-managed store path (not user-overridable).
func TestHostOllamaEnvPairsForwardsPrefixed(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Setenv("OLLAMA_FLASH_ATTENTION", "1")
	test.Setenv("OLLAMA_KV_CACHE_TYPE", "q8_0")
	test.Setenv("OLLAMA_MODELS", "/should/not/win") // platform-managed; must be ignored
	test.Setenv("NOT_OLLAMA", "nope")

	joined := strings.Join(hostOllamaEnvPairs(), " ")
	for _, want := range []string{"OLLAMA_FLASH_ATTENTION=1", "OLLAMA_KV_CACHE_TYPE=q8_0", "OLLAMA_MODELS=" + hostOllamaModelsDir()} {
		if !strings.Contains(joined, want) {
			test.Errorf("host ollama env pairs missing %q: %q", want, joined)
		}
	}
	if strings.Contains(joined, "/should/not/win") {
		test.Errorf("OLLAMA_MODELS must NOT be overridable from the environment: %q", joined)
	}
	if strings.Contains(joined, "NOT_OLLAMA") {
		test.Errorf("non-OLLAMA_ vars must not be forwarded: %q", joined)
	}
}

// TestOllamaContextLengthDefault verifies the platform sets a generous default context
// window (so agent CLIs' prompt + tools don't starve generation), and that a user-forwarded
// OLLAMA_CONTEXT_LENGTH overrides it without a duplicate entry.
func TestOllamaContextLengthDefault(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	// Default applied when unset.
	joined := strings.Join(hostOllamaEnvPairs(), " ")
	if !strings.Contains(joined, "OLLAMA_CONTEXT_LENGTH="+defaultOllamaContextLength) {
		test.Errorf("ollama env pairs missing default context length: %q", joined)
	}

	// User override wins and is not duplicated.
	test.Setenv("OLLAMA_CONTEXT_LENGTH", "65536")
	joined = strings.Join(hostOllamaEnvPairs(), " ")
	if !strings.Contains(joined, "OLLAMA_CONTEXT_LENGTH=65536") {
		test.Errorf("user OLLAMA_CONTEXT_LENGTH must win: %q", joined)
	}
	if strings.Contains(joined, "OLLAMA_CONTEXT_LENGTH="+defaultOllamaContextLength) {
		test.Errorf("default must be dropped when the user sets OLLAMA_CONTEXT_LENGTH: %q", joined)
	}
	if count := strings.Count(joined, "OLLAMA_CONTEXT_LENGTH="); count != 1 {
		test.Errorf("OLLAMA_CONTEXT_LENGTH must appear once, got %d: %q", count, joined)
	}
}
