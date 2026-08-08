package setup

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/output"
)

// TestMain neutralizes the host-native runtime lifecycle seams for the WHOLE setup
// test package: applyHostNativeAction routes through hostNativeStartVLLM/StopVLLM, and
// the container-tier / all-path tests must NEVER fork a real `vllm serve` just because
// that binary happens to be installed on the test host. Tests that specifically
// exercise routing override these seams (with a cleanup restore); the tests that
// exercise the REAL start/stop functions call them directly (controlling the
// lower-level primitives: vllmDetect, vllmStopByPort, …).
func TestMain(m *testing.M) {
	hostNativeStartVLLM = func() error { return nil }
	hostNativeStopVLLM = func() error { return nil }
	// A reconcile-path test must never trigger the real (large) vLLM / hf pip install
	// just because the host lacks them; the reconcile routes through these seams.
	installVLLMFn = func(...string) error { return nil }
	installHFFn = func() error { return nil }
	os.Exit(m.Run())
}

// ServiceNames must expose the host-native vLLM runtime (so it is never an "unknown
// service"), listed exactly once (it is also in the registry for status/logs).
func TestServiceNamesIncludesHostNative(test *testing.T) {
	names := ServiceNames()
	if !slices.Contains(names, "vllm") {
		test.Errorf("ServiceNames must include vllm: %v", names)
	}
	count := 0
	for _, name := range names {
		if name == "vllm" {
			count++
		}
	}
	if count != 1 {
		test.Errorf("vllm must appear exactly once in ServiceNames, got %d: %v", count, names)
	}
}

// ControlService routes the vLLM runtime to the host-exec path
// (controlHostNativeService → the hostNative* seams), NEVER the container Control.
func TestControlServiceRoutesHostNative(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, services := healthyDeps()

	var vllmStopped, vllmStarted bool
	swapHostNativeSeams(test)
	hostNativeStopVLLM = func() error { vllmStopped = true; return nil }
	hostNativeStartVLLM = func() error { vllmStarted = true; return nil }

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

// enable/disable is a container-optional concept — the host-native vLLM runtime rejects
// it with a clear exit-2 error rather than the "core service" message or a crash.
func TestControlServiceHostNativeEnableDisableRejected(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()
	for _, action := range []string{"enable", "disable"} {
		_, err := ControlService(deps, action, "vllm")
		if exitCodeOf(test, err) != output.ExitInvalidInput {
			test.Errorf("%s vllm must be exit 2, got %v", action, err)
		}
		if err != nil && !strings.Contains(err.Error(), "host-native") {
			test.Errorf("%s vllm error should explain it is host-native, got %v", action, err)
		}
	}
}

// `ai services <action> all` must NEVER fail because a host runtime is absent: even
// when the host-native controller errors, the all-path swallows it and returns the
// container statuses.
func TestControlServiceAllNeverFailsOnMissingHostRuntime(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, services := healthyDeps()

	swapHostNativeSeams(test)
	hostNativeStartVLLM = func() error { return output.Errorf(output.ExitMissingDep, "vLLM not installed") }

	if _, err := ControlService(deps, "start", "all"); err != nil {
		test.Fatalf("`start all` must not fail on a missing host runtime, got %v", err)
	}
	if services.controlService != "" {
		test.Errorf("`start all` must call the container Control with the all-target (empty), got %q", services.controlService)
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

// swapHostNativeSeams captures the host-native lifecycle seams and restores them on
// cleanup (the caller then overrides whichever ones it needs).
func swapHostNativeSeams(test *testing.T) {
	test.Helper()
	startVLLM, stopVLLM := hostNativeStartVLLM, hostNativeStopVLLM
	test.Cleanup(func() {
		hostNativeStartVLLM, hostNativeStopVLLM = startVLLM, stopVLLM
	})
}
