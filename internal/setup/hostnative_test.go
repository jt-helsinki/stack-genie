package setup

import (
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/output"
)

// TestMain neutralizes the host-native runtime lifecycle seams for the WHOLE setup
// test package: applyHostNativeAction routes through hostNativeStart*/Stop*, and the
// container-tier / all-path tests must NEVER fork a real `ollama serve` / `vllm serve`
// just because those binaries happen to be installed on the test host. Tests that
// specifically exercise routing override these seams (with a cleanup restore); the
// tests that exercise the REAL start/stop functions call them directly (they are not
// the seams), controlling the lower-level primitives (spawnHostOllama, pkillHostOllama,
// ollamaBinaryLookup, vllmDetect, …).
func TestMain(m *testing.M) {
	hostNativeStartOllama = func() error { return nil }
	hostNativeStopOllama = func() error { return nil }
	hostNativeStartVLLM = func() error { return nil }
	hostNativeStopVLLM = func() error { return nil }
	os.Exit(m.Run())
}

// ServiceNames must expose BOTH host-native runtimes (so neither is an "unknown
// service"), with ollama listed exactly once (it is also in the container registry
// for status) and vllm added (it is not in the registry at all).
func TestServiceNamesIncludesHostNative(test *testing.T) {
	names := ServiceNames()
	if !slices.Contains(names, "ollama") {
		test.Errorf("ServiceNames must include ollama: %v", names)
	}
	if !slices.Contains(names, "vllm") {
		test.Errorf("ServiceNames must include vllm: %v", names)
	}
	count := 0
	for _, name := range names {
		if name == "ollama" {
			count++
		}
	}
	if count != 1 {
		test.Errorf("ollama must appear exactly once in ServiceNames, got %d: %v", count, names)
	}
}

// ControlService routes a named host-native runtime to the host-exec path
// (controlHostNativeService → the hostNative* seams), NEVER the container Control.
func TestControlServiceRoutesHostNative(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, services := healthyDeps()

	var ollamaStarted, vllmStopped, vllmStarted bool
	swapHostNativeSeams(test)
	hostNativeStartOllama = func() error { ollamaStarted = true; return nil }
	hostNativeStopVLLM = func() error { vllmStopped = true; return nil }
	hostNativeStartVLLM = func() error { vllmStarted = true; return nil }

	if _, err := ControlService(deps, "start", "ollama"); err != nil {
		test.Fatalf("start ollama should route to host-native, got %v", err)
	}
	if !ollamaStarted {
		test.Error("start ollama must call the host-native ollama starter")
	}
	if services.controlService == "ollama" {
		test.Error("start ollama must NOT reach the container Control")
	}

	// restart vllm → the host-native path does stop then start; the container Control
	// must never see "vllm" (which it rejects as unknown).
	if _, err := ControlService(deps, "restart", "vllm"); err != nil {
		test.Fatalf("restart vllm should route to host-native, got %v", err)
	}
	if !vllmStopped || !vllmStarted {
		test.Errorf("restart vllm must stop then start (stopped=%v started=%v)", vllmStopped, vllmStarted)
	}
	if services.controlService == "vllm" {
		test.Error("restart vllm must NOT reach the container Control")
	}
}

// enable/disable is a container-optional concept — a host-native runtime rejects it
// with a clear exit-2 error rather than the "core service" message or a crash.
func TestControlServiceHostNativeEnableDisableRejected(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()
	for _, action := range []string{"enable", "disable"} {
		for _, service := range []string{"ollama", "vllm"} {
			_, err := ControlService(deps, action, service)
			if exitCodeOf(test, err) != output.ExitInvalidInput {
				test.Errorf("%s %s must be exit 2, got %v", action, service, err)
			}
			if err != nil && !strings.Contains(err.Error(), "host-native") {
				test.Errorf("%s %s error should explain it is host-native, got %v", action, service, err)
			}
		}
	}
}

// `ai services <action> all` must NEVER fail because a host runtime is absent: even
// when both host-native controllers error, the all-path swallows them and returns the
// container statuses.
func TestControlServiceAllNeverFailsOnMissingHostRuntime(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, services := healthyDeps()

	swapHostNativeSeams(test)
	hostNativeStartOllama = func() error { return errors.New("no ollama binary") }
	hostNativeStartVLLM = func() error { return output.Errorf(output.ExitMissingDep, "vLLM not installed") }

	if _, err := ControlService(deps, "start", "all"); err != nil {
		test.Fatalf("`start all` must not fail on a missing host runtime, got %v", err)
	}
	if services.controlService != "" {
		test.Errorf("`start all` must call the container Control with the all-target (empty), got %q", services.controlService)
	}
}

// startHostOllamaService is a no-op success when Ollama is already reachable — it must
// NOT spawn a second process.
func TestStartHostOllamaAlreadyUp(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fakeOllamaHTTP(test, http.StatusOK)
	spawned := false
	swapOllamaSpawn(test)
	spawnHostOllama = func(string, []string) error { spawned = true; return nil }

	if err := startHostOllamaService(); err != nil {
		test.Fatalf("already-reachable ollama should succeed, got %v", err)
	}
	if spawned {
		test.Error("already-reachable ollama must not be spawned again")
	}
}

// startHostOllamaService errors (exit 3) with install guidance when the `ollama` binary
// is absent — it must not attempt a spawn.
func TestStartHostOllamaBinaryAbsent(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fakeOllamaHTTP(test, 0) // down
	swapOllamaLookup(test)
	ollamaBinaryLookup = func() (string, bool) { return "", false }
	spawned := false
	swapOllamaSpawn(test)
	spawnHostOllama = func(string, []string) error { spawned = true; return nil }

	err := startHostOllamaService()
	if exitCodeOf(test, err) != output.ExitMissingDep {
		test.Fatalf("absent ollama binary must be exit 3, got %v", err)
	}
	if !strings.Contains(err.Error(), "install") {
		test.Errorf("error must carry install guidance, got %v", err)
	}
	if spawned {
		test.Error("no spawn must be attempted when the binary is absent")
	}
}

// startHostOllamaService spawns a detached `ollama serve` when down but installed, then
// confirms reachability (the spawn flips the probe, so the poll returns immediately).
func TestStartHostOllamaSpawns(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	reachable := false
	original := hostOllamaHTTPGet
	test.Cleanup(func() { hostOllamaHTTPGet = original })
	hostOllamaHTTPGet = func(string) (*http.Response, error) {
		if reachable {
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}
		return nil, errors.New("down")
	}
	swapOllamaLookup(test)
	ollamaBinaryLookup = func() (string, bool) { return "/usr/local/bin/ollama", true }
	swapOllamaSpawn(test)
	spawnCount := 0
	spawnHostOllama = func(string, []string) error { spawnCount++; reachable = true; return nil }

	if err := startHostOllamaService(); err != nil {
		test.Fatalf("installed-but-down ollama should spawn and succeed, got %v", err)
	}
	if spawnCount != 1 {
		test.Errorf("expected exactly one spawn, got %d", spawnCount)
	}
}

// stopHostOllamaService best-effort kills the host `ollama serve` via the pkill seam.
func TestStopHostOllamaCallsPkill(test *testing.T) {
	killed := false
	original := pkillHostOllama
	test.Cleanup(func() { pkillHostOllama = original })
	pkillHostOllama = func() error { killed = true; return nil }

	if err := stopHostOllamaService(); err != nil {
		test.Fatalf("stop should be best-effort success, got %v", err)
	}
	if !killed {
		test.Error("stop must invoke the pkill seam")
	}
}

// startVLLMServersHost errors (exit 3) with install guidance when vLLM is not installed.
func TestStartVLLMHostNotInstalled(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fakeVLLMDetect(test, false)
	err := startVLLMServersHost()
	if exitCodeOf(test, err) != output.ExitMissingDep {
		test.Fatalf("uninstalled vLLM must be exit 3, got %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "vllm") {
		test.Errorf("error must mention vLLM/install guidance, got %v", err)
	}
}

// startVLLMServersHost succeeds (nothing to do) when vLLM is installed but no runtime=vllm
// models are recorded.
func TestStartVLLMHostInstalledNoModels(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fakeVLLMDetect(test, true)
	if err := startVLLMServersHost(); err != nil {
		test.Fatalf("installed vLLM with no models should succeed, got %v", err)
	}
}

// stopVLLMServersHost stops each recorded vLLM server by its loopback port.
func TestStopVLLMHostByPort(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	recordVLLMChoice(test, "my-vllm", "mlx-community/foo", "http://127.0.0.1:8101/v1")

	var stoppedPorts []int
	originalStop := vllmStopByPort
	originalPkill := vllmPkill
	test.Cleanup(func() { vllmStopByPort = originalStop; vllmPkill = originalPkill })
	pkillCalled := false
	vllmStopByPort = func(port int) error { stoppedPorts = append(stoppedPorts, port); return nil }
	vllmPkill = func() error { pkillCalled = true; return nil }

	if err := stopVLLMServersHost(); err != nil {
		test.Fatalf("stop vllm should be best-effort success, got %v", err)
	}
	if !slices.Contains(stoppedPorts, 8101) {
		test.Errorf("stop must target the recorded port 8101, got %v", stoppedPorts)
	}
	if pkillCalled {
		test.Error("with a recorded port, the pkill fallback must NOT be used")
	}
}

// stopVLLMServersHost falls back to pkill when there are no recorded ports.
func TestStopVLLMHostPkillFallback(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	originalPkill := vllmPkill
	test.Cleanup(func() { vllmPkill = originalPkill })
	pkillCalled := false
	vllmPkill = func() error { pkillCalled = true; return nil }

	if err := stopVLLMServersHost(); err != nil {
		test.Fatalf("stop vllm (no models) should succeed, got %v", err)
	}
	if !pkillCalled {
		test.Error("with no recorded ports, stop must use the pkill fallback")
	}
}

// swapHostNativeSeams captures the four host-native lifecycle seams and restores them
// on cleanup (the caller then overrides whichever ones it needs).
func swapHostNativeSeams(test *testing.T) {
	test.Helper()
	startOllama, stopOllama := hostNativeStartOllama, hostNativeStopOllama
	startVLLM, stopVLLM := hostNativeStartVLLM, hostNativeStopVLLM
	test.Cleanup(func() {
		hostNativeStartOllama, hostNativeStopOllama = startOllama, stopOllama
		hostNativeStartVLLM, hostNativeStopVLLM = startVLLM, stopVLLM
	})
}

// swapOllamaSpawn captures the ollama spawn seam and restores it on cleanup.
func swapOllamaSpawn(test *testing.T) {
	test.Helper()
	original := spawnHostOllama
	test.Cleanup(func() { spawnHostOllama = original })
}

// swapOllamaLookup captures the ollama binary-lookup seam and restores it on cleanup.
func swapOllamaLookup(test *testing.T) {
	test.Helper()
	original := ollamaBinaryLookup
	test.Cleanup(func() { ollamaBinaryLookup = original })
}
