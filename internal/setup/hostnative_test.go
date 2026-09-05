package setup

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/omlx"
	"github.com/jt-helsinki/stack-genie/internal/output"
)

// TestMain neutralizes the host-native runtime lifecycle seams for the WHOLE setup
// test package: applyHostNativeAction routes through hostNativeStartOmlx/StopOmlx, and
// the container-tier / all-path tests must NEVER fork a real `omlx serve` just because
// that binary happens to be installed on the test host. Tests that specifically
// exercise routing override these seams (with a cleanup restore); the tests that
// exercise the REAL start/stop functions call them directly (controlling the
// lower-level primitives: omlxDetectFn, omlxManagerFn, omlxSyncModelsFn).
func TestMain(m *testing.M) {
	hostNativeStartOmlx = func() error { return nil }
	hostNativeStopOmlx = func() error { return nil }
	// A reconcile-path test must never trigger the real (large) omlx pip install just
	// because the host lacks it; the reconcile routes through this seam.
	installOmlxFn = func() error { return nil }
	os.Exit(m.Run())
}

// fakeOmlxDetect swaps omlxDetectFn to report installed (restored on cleanup).
func fakeOmlxDetect(test *testing.T, installed bool) {
	test.Helper()
	original := omlxDetectFn
	test.Cleanup(func() { omlxDetectFn = original })
	omlxDetectFn = func() bool { return installed }
}

// ServiceNames must expose the host-native omlx runtime (so it is never an "unknown
// service"), listed exactly once (it is also in the registry for status/logs).
func TestServiceNamesIncludesHostNative(test *testing.T) {
	names := ServiceNames()
	if !slices.Contains(names, "omlx") {
		test.Errorf("ServiceNames must include omlx: %v", names)
	}
	count := 0
	for _, name := range names {
		if name == "omlx" {
			count++
		}
	}
	if count != 1 {
		test.Errorf("omlx must appear exactly once in ServiceNames, got %d: %v", count, names)
	}
}

// ControlService routes the omlx runtime to the host-exec path
// (controlHostNativeService → the hostNative* seams), NEVER the container Control.
func TestControlServiceRoutesHostNative(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, services := healthyDeps()

	var omlxStopped, omlxStarted bool
	swapHostNativeSeams(test)
	hostNativeStopOmlx = func() error { omlxStopped = true; return nil }
	hostNativeStartOmlx = func() error { omlxStarted = true; return nil }

	// restart omlx → the host-native path does stop then start; the container Control
	// must never see "omlx" (which it rejects as unknown).
	if _, err := ControlService(deps, "restart", "omlx"); err != nil {
		test.Fatalf("restart omlx should route to host-native, got %v", err)
	}
	if !omlxStopped || !omlxStarted {
		test.Errorf("restart omlx must stop then start (stopped=%v started=%v)", omlxStopped, omlxStarted)
	}
	if services.controlService == "omlx" {
		test.Error("restart omlx must NOT reach the container Control")
	}
}

// enable/disable is a container-optional concept — the host-native omlx runtime rejects
// it with a clear exit-2 error rather than the "core service" message or a crash.
func TestControlServiceHostNativeEnableDisableRejected(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	deps, _ := healthyDeps()
	for _, action := range []string{"enable", "disable"} {
		_, err := ControlService(deps, action, "omlx")
		if exitCodeOf(test, err) != output.ExitInvalidInput {
			test.Errorf("%s omlx must be exit 2, got %v", action, err)
		}
		if err != nil && !strings.Contains(err.Error(), "host-native") {
			test.Errorf("%s omlx error should explain it is host-native, got %v", action, err)
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
	hostNativeStartOmlx = func() error { return output.Errorf(output.ExitMissingDep, "omlx not installed") }

	if _, err := ControlService(deps, "start", "all"); err != nil {
		test.Fatalf("`start all` must not fail on a missing host runtime, got %v", err)
	}
	if services.controlService != "" {
		test.Errorf("`start all` must call the container Control with the all-target (empty), got %q", services.controlService)
	}
}

// startOmlxServerHost errors (exit 3) with install guidance when omlx is not installed.
func TestStartOmlxHostNotInstalled(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fakeOmlxDetect(test, false)
	err := startOmlxServerHost()
	if exitCodeOf(test, err) != output.ExitMissingDep {
		test.Fatalf("uninstalled omlx must be exit 3, got %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "omlx") {
		test.Errorf("error must mention omlx/install guidance, got %v", err)
	}
}

// startOmlxServerHost starts the server (adopting an already-healthy fake so no
// real process is spawned), then best-effort syncs LiteLLM.
func TestStartOmlxHostStartsAndSyncs(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fakeOmlxDetect(test, true)

	originalManager, originalSync := omlxManagerFn, omlxSyncModelsFn
	test.Cleanup(func() { omlxManagerFn, omlxSyncModelsFn = originalManager, originalSync })

	omlxManagerFn = func() *omlx.Manager {
		return omlx.NewManager(omlx.Config{Probe: func() bool { return true }})
	}
	var syncCalled bool
	omlxSyncModelsFn = func() (litellm.SyncResult, error) {
		syncCalled = true
		return litellm.SyncResult{Added: []string{"omlx/qwen"}}, nil
	}

	if err := startOmlxServerHost(); err != nil {
		test.Fatalf("startOmlxServerHost: %v", err)
	}
	if !syncCalled {
		test.Error("startOmlxServerHost must sync LiteLLM after the server is healthy")
	}
}

// startOmlxServerHost still succeeds even when the LiteLLM sync fails — a sync
// failure is reported (via progress) but must not fail the start.
func TestStartOmlxHostSyncFailureIsSwallowed(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fakeOmlxDetect(test, true)

	originalManager, originalSync := omlxManagerFn, omlxSyncModelsFn
	test.Cleanup(func() { omlxManagerFn, omlxSyncModelsFn = originalManager, originalSync })

	omlxManagerFn = func() *omlx.Manager {
		return omlx.NewManager(omlx.Config{Probe: func() bool { return true }})
	}
	omlxSyncModelsFn = func() (litellm.SyncResult, error) {
		return litellm.SyncResult{}, output.Errorf(output.ExitRuntimeFailure, "gateway unreachable")
	}

	if err := startOmlxServerHost(); err != nil {
		test.Fatalf("a sync failure must not fail the start, got %v", err)
	}
}

// swapHostNativeSeams captures the host-native lifecycle seams and restores them on
// cleanup (the caller then overrides whichever ones it needs).
func swapHostNativeSeams(test *testing.T) {
	test.Helper()
	startOmlx, stopOmlx := hostNativeStartOmlx, hostNativeStopOmlx
	test.Cleanup(func() {
		hostNativeStartOmlx, hostNativeStopOmlx = startOmlx, stopOmlx
	})
}
