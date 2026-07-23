package setup

import (
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/paths"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/versions"
)

// --- health / container-state prober ---------------------------------------

// healthProber reports a fixed set of containers as running (via `ps`) and returns
// a canned `exec` (valkey-cli ping) response, so serviceHealthy / serviceContainersUp
// can be exercised without a live runtime. LookPath resolves any binary so
// ContainerRuntimeName picks docker.
type healthProber struct {
	running map[string]bool
	ping    string
}

func (prober healthProber) LookPath(file string) (string, error) { return "/usr/bin/" + file, nil }
func (prober healthProber) Exists(string) bool                   { return false }
func (prober healthProber) Run(_ string, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, nil
	}
	switch args[0] {
	case "ps":
		for _, arg := range args {
			if filterName, ok := strings.CutPrefix(arg, "name=^/"); ok {
				container := strings.TrimSuffix(filterName, "$")
				if prober.running[container] {
					return []byte(container + "\n"), nil
				}
			}
		}
		return nil, nil
	case "exec":
		return []byte(prober.ping), nil
	}
	return nil, nil
}

// TestServiceHealthyContainerBranches drives the container-only readiness branches
// (presidio needs BOTH containers, headroom/dns/redisinsight need one) plus the
// unknown-service default.
func TestServiceHealthyContainerBranches(test *testing.T) {
	cases := []struct {
		name    string
		running map[string]bool
		want    bool
	}{
		{"presidio", map[string]bool{presidioAnalyzerContainer: true, presidioAnonymizerContainer: true}, true},
		{"presidio", map[string]bool{presidioAnalyzerContainer: true}, false}, // anonymizer down
		{"presidio", map[string]bool{}, false},
		{"headroom", map[string]bool{headroomContainer: true}, true},
		{"headroom", map[string]bool{}, false},
		{"dns", map[string]bool{dnsContainer: true}, true},
		{"dns", map[string]bool{}, false},
		{"redisinsight", map[string]bool{redisInsightContainer: true}, true},
		{"redisinsight", map[string]bool{}, false},
		{"bogus-service", map[string]bool{}, false}, // default branch
	}
	for _, testCase := range cases {
		services := realServices{prober: healthProber{running: testCase.running}}
		if got := services.serviceHealthy(testCase.name); got != testCase.want {
			test.Errorf("serviceHealthy(%q) with running=%v = %v, want %v",
				testCase.name, testCase.running, got, testCase.want)
		}
	}
}

// TestServiceHealthyValkeyPing: valkey is healthy only when its container is up AND
// `valkey-cli ping` returns PONG.
func TestServiceHealthyValkeyPing(test *testing.T) {
	up := realServices{prober: healthProber{running: map[string]bool{valkeyContainer: true}, ping: "PONG\n"}}
	if !up.serviceHealthy("valkey") {
		test.Error("valkey with a PONG reply should be healthy")
	}
	noPong := realServices{prober: healthProber{running: map[string]bool{valkeyContainer: true}, ping: "LOADING"}}
	if noPong.serviceHealthy("valkey") {
		test.Error("valkey without a PONG reply must not be healthy")
	}
	down := realServices{prober: healthProber{running: map[string]bool{}, ping: "PONG\n"}}
	if down.serviceHealthy("valkey") {
		test.Error("valkey with no container running must not be healthy")
	}
}

// TestServiceHealthyProxyBranch: the proxy is healthy only when its container is up
// AND the readiness GET reaches it (any HTTP response). A not-running container is
// down without ever issuing the GET.
func TestServiceHealthyProxyBranch(test *testing.T) {
	original := proxyHTTPGet
	defer func() { proxyHTTPGet = original }()
	proxyHTTPGet = func(string) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
	}

	up := realServices{prober: healthProber{running: map[string]bool{proxyContainer: true}}}
	if !up.serviceHealthy("proxy") {
		test.Error("proxy running + reachable should be healthy")
	}

	// Container running but the gateway is not forwarding (transport error) → down.
	proxyHTTPGet = func(string) (*http.Response, error) { return nil, errors.New("connection refused") }
	if up.serviceHealthy("proxy") {
		test.Error("proxy running but unreachable must not be healthy")
	}

	down := realServices{prober: healthProber{running: map[string]bool{}}}
	if down.serviceHealthy("proxy") {
		test.Error("proxy with no container running must not be healthy")
	}
}

// (The litellm/ollama serviceHealthy branches probe a real gateway/Ollama endpoint,
// so their result is host-dependent — deliberately not asserted here.)

// TestServiceContainersUp: all mapped containers running → true; one missing → false;
// an unknown service (no containers) → false.
func TestServiceContainersUp(test *testing.T) {
	// litellm maps to aip-litellm + aip-litellm-db (both must be up).
	bothUp := realServices{prober: healthProber{running: map[string]bool{
		litellmContainer: true, litellmDBContainer: true,
	}}}
	if !bothUp.serviceContainersUp("litellm") {
		test.Error("litellm containersUp should be true when both containers run")
	}
	partial := realServices{prober: healthProber{running: map[string]bool{litellmContainer: true}}}
	if partial.serviceContainersUp("litellm") {
		test.Error("litellm containersUp must be false when the DB container is down")
	}
	if bothUp.serviceContainersUp("nope") {
		test.Error("an unknown service maps to no container → false")
	}
}

// TestStatusForStartingAndRunningStates: statusFor maps a healthy service to
// "running", a service whose containers are up but not ready to "starting", and a
// service with nothing running to "stopped". Uses dns (container-only readiness →
// running), valkey (container up but no PONG → starting), and headroom (nothing
// running → stopped). It also surfaces the postgres line from the litellm-db
// container. The litellm/ollama states probe a real gateway/Ollama, so they are
// deliberately not asserted (host-dependent).
func TestStatusForStartingAndRunningStates(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	// dns up → healthy → running; valkey up but ping is not PONG → starting;
	// headroom absent → stopped; litellm-db up → postgres running.
	services := realServices{prober: healthProber{
		running: map[string]bool{
			dnsContainer:       true,
			valkeyContainer:    true,
			litellmDBContainer: true,
		},
		ping: "LOADING", // valkey container up but not yet answering PONG
	}}
	statuses, err := services.statusFor(nil)
	if err != nil {
		test.Fatal(err)
	}
	stateOf := func(name string) string {
		for _, status := range statuses {
			if status.Name == name {
				return status.State
			}
		}
		return "<absent>"
	}
	if got := stateOf("dns"); got != "running" {
		test.Errorf("dns state = %q, want running", got)
	}
	if got := stateOf("valkey"); got != "starting" {
		test.Errorf("valkey state = %q, want starting (container up, no PONG yet)", got)
	}
	if got := stateOf("headroom"); got != "stopped" {
		test.Errorf("headroom state = %q, want stopped (nothing running)", got)
	}
	// The postgres line reflects the litellm-db container being up.
	if got := stateOf("postgres"); got != "running" {
		test.Errorf("postgres state = %q, want running (db container up)", got)
	}
}

// --- currentBindHost / reconcileGuardrails ---------------------------------

func TestCurrentBindHost(test *testing.T) {
	// Absent runtime.yaml → loopback default.
	test.Setenv("HOME", test.TempDir())
	if got := currentBindHost(); got != "127.0.0.1" {
		test.Errorf("no runtime.yaml bindHost = %q, want 127.0.0.1", got)
	}
	// Standalone → loopback.
	if err := runtime.Persist(&runtime.Info{Role: runtime.RoleStandalone}); err != nil {
		test.Fatal(err)
	}
	if got := currentBindHost(); got != "127.0.0.1" {
		test.Errorf("standalone bindHost = %q, want 127.0.0.1", got)
	}
	// Server → all interfaces.
	if err := runtime.Persist(&runtime.Info{Role: runtime.RoleServer}); err != nil {
		test.Fatal(err)
	}
	if got := currentBindHost(); got != "0.0.0.0" {
		test.Errorf("server bindHost = %q, want 0.0.0.0", got)
	}
}

func TestReconcileGuardrails(test *testing.T) {
	// Absent runtime.yaml → the default set (Headroom only).
	test.Setenv("HOME", test.TempDir())
	got := reconcileGuardrails()
	if len(got) != 1 || got[0] != litellm.GuardrailHeadroom {
		test.Errorf("no runtime.yaml guardrails = %v, want the default [%q]", got, litellm.GuardrailHeadroom)
	}
	// A persisted non-nil set is honoured verbatim.
	chosen := []string{litellm.GuardrailSecretMasking, litellm.GuardrailToolFirewall}
	if err := runtime.Persist(&runtime.Info{Guardrails: chosen}); err != nil {
		test.Fatal(err)
	}
	if got := reconcileGuardrails(); len(got) != 2 {
		test.Errorf("persisted guardrails = %v, want the 2 persisted", got)
	}
}

// --- versionsImageRef -------------------------------------------------------

func TestVersionsImageRef(test *testing.T) {
	file := &versions.File{Services: map[string]versions.Service{
		"litellm":  {Image: "ghcr.io/x/litellm", Tag: "v1"},
		"no-tag":   {Image: "ghcr.io/x/notag", Tag: ""},
		"no-image": {Image: "", Tag: "v2"},
	}}
	if got := versionsImageRef(file, "litellm"); got != "ghcr.io/x/litellm:v1" {
		test.Errorf("complete entry = %q, want ghcr.io/x/litellm:v1", got)
	}
	if got := versionsImageRef(file, "no-tag"); got != "" {
		test.Errorf("missing tag should yield empty, got %q", got)
	}
	if got := versionsImageRef(file, "no-image"); got != "" {
		test.Errorf("missing image should yield empty, got %q", got)
	}
	if got := versionsImageRef(file, "absent"); got != "" {
		test.Errorf("absent service should yield empty, got %q", got)
	}
}

// TestContainerImageFallsBackToDefault: with no versions.yaml on disk, containerImage
// resolves from the built-in default pin (repo:tag form) for a known service.
func TestContainerImageFallsBackToDefault(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	ref := containerImage("litellm")
	if ref == "" || !strings.Contains(ref, ":") {
		test.Errorf("containerImage(litellm) = %q, want a default repo:tag ref", ref)
	}
	if got := containerImage("not-a-service"); got != "" {
		test.Errorf("unknown service image = %q, want empty", got)
	}
}

// --- systemVolumeDir --------------------------------------------------------

func TestSystemVolumeDir(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	dir, err := systemVolumeDir("some-vol", 0o700)
	if err != nil {
		test.Fatalf("systemVolumeDir: %v", err)
	}
	volumesDir, err := paths.VolumesDir()
	if err != nil {
		test.Fatal(err)
	}
	want := filepath.Join(volumesDir, "some-vol")
	if dir != want {
		test.Errorf("systemVolumeDir path = %q, want %q", dir, want)
	}
}

// --- parseContainerPorts ----------------------------------------------------

func TestParseContainerPorts(test *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"null", "null", ""},
		{"empty-map", "{}", ""},
		{"invalid-json", "not-json", ""},
		{"unpublished", `{"4000/tcp":null}`, "4000/tcp"},
		{"published", `{"4000/tcp":[{"HostIp":"127.0.0.1","HostPort":"18787"}]}`, "4000/tcp→18787"},
		{"sorted-multi", `{"53/udp":[],"53/tcp":[{"HostIp":"","HostPort":"15353"}]}`, "53/tcp→15353, 53/udp"},
	}
	for _, testCase := range cases {
		if got := parseContainerPorts(testCase.raw); got != testCase.want {
			test.Errorf("parseContainerPorts(%s) = %q, want %q", testCase.name, got, testCase.want)
		}
	}
}

// --- ServicesStatus branches -----------------------------------------------

// TestServicesStatusWithoutRuntimeYAML: with no persisted runtime.yaml the
// microsandbox runtime line is omitted (only the service-tier statuses remain).
func TestServicesStatusWithoutRuntimeYAML(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()
	statuses, err := ServicesStatus(deps)
	if err != nil {
		test.Fatal(err)
	}
	for _, status := range statuses {
		if status.Name == "microsandbox" {
			test.Fatal("microsandbox line must be absent without a runtime.yaml")
		}
	}
	if len(statuses) == 0 {
		test.Fatal("service-tier statuses should still be returned")
	}
}

// statusErrServices makes Status fail so the ServicesStatus error path is exercised.
type statusErrServices struct{ *fakeServices }

func (services statusErrServices) Status() ([]ServiceStatus, error) {
	return nil, errors.New("status boom")
}

func TestServicesStatusServiceErrorPropagates(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, base := healthyDeps()
	deps.Services = statusErrServices{fakeServices: base}
	if _, err := ServicesStatus(deps); err == nil {
		test.Fatal("a Services.Status error must propagate")
	}
}

// --- UpdateService warning path --------------------------------------------

// updateErrServices makes UpdateImages fail so UpdateService's non-fatal warning
// branch runs (it still restarts to pick up any image that did update).
type updateErrServices struct{ *fakeServices }

func (services updateErrServices) UpdateImages(_ []string, _ []string, _ io.Writer, _ func(string)) error {
	return errors.New("pull failed")
}

func TestUpdateServiceContinuesOnPullError(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, base := healthyDeps()
	deps.Services = updateErrServices{fakeServices: base}

	var steps []string
	statuses, err := UpdateService(deps, "litellm", io.Discard, func(msg string) { steps = append(steps, msg) })
	if err != nil {
		test.Fatalf("a pull error must not fail UpdateService: %v", err)
	}
	if base.controlAction != "restart" || base.controlService != "litellm" {
		test.Errorf("UpdateService should still restart on a pull error, got %s %s", base.controlAction, base.controlService)
	}
	if len(statuses) == 0 {
		test.Error("UpdateService should return post-restart statuses")
	}
	var warned bool
	for _, step := range steps {
		if strings.Contains(step, "did not update") {
			warned = true
		}
	}
	if !warned {
		test.Errorf("expected a non-fatal pull-warning step, got %v", steps)
	}
}

// --- pure renderers: styleServiceState / presence / Human -------------------

func TestStyleServiceState(test *testing.T) {
	// Every state must render its own word (padded + styled) without dropping text.
	for _, state := range []string{"running", "ready", "starting", "pending", "stopped", "failed", "error", "not_installed", "unavailable", "disabled", "unknown"} {
		if got := styleServiceState(state); !strings.Contains(got, state) {
			test.Errorf("styleServiceState(%q) = %q, want it to contain the state word", state, got)
		}
	}
}

func TestPresence(test *testing.T) {
	if got := presence(true); !strings.Contains(got, "created") {
		test.Errorf("presence(true) = %q, want it to say created", got)
	}
	if got := presence(false); !strings.Contains(got, "present") {
		test.Errorf("presence(false) = %q, want it to say present", got)
	}
}

// TestReportHumanRendersRuntimeBlock covers the Runtime + state lines of Human for
// both the microVM-available and unavailable cases.
func TestReportHumanRendersRuntimeBlock(test *testing.T) {
	available := &Report{
		PlatformDir:     "/home/u/.ai-platform",
		ConfigCreated:   true,
		VersionsCreated: false,
		Runtime: &runtime.Info{
			Detected:     "docker",
			Rootless:     true,
			Microsandbox: runtime.MicrosandboxInfo{Available: true, Virtualization: "hvf"},
		},
	}
	rendered := available.Human()
	for _, fragment := range []string{"Runtime:", "docker", "hvf", "available", "config.yaml", "versions.yaml"} {
		if !strings.Contains(rendered, fragment) {
			test.Errorf("Human() missing %q:\n%s", fragment, rendered)
		}
	}

	unavailable := &Report{
		PlatformDir: "/home/u/.ai-platform",
		Runtime:     &runtime.Info{Detected: "podman", Microsandbox: runtime.MicrosandboxInfo{Available: false, Virtualization: "none"}},
	}
	if got := unavailable.Human(); !strings.Contains(got, "unavailable") {
		test.Errorf("Human() for an unavailable microVM should say unavailable:\n%s", got)
	}
}
