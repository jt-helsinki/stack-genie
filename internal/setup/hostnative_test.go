package setup

import (
	"errors"
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
	hostNativeStartColibri = func() error { return nil }
	hostNativeStopColibri = func() error { return nil }
	// A reconcile-path test must never trigger the real (large) omlx pip install, or
	// the real colibri archive download, just because the test host lacks either;
	// the reconcile routes through these seams.
	installOmlxFn = func() error { return nil }
	installColibriFn = func() error { return nil }
	// Run() calls EnsureNonoInstalled before the prerequisite gate; a test must never
	// trigger the real brew/curl nono install just because the test host lacks it
	// (nonoDetectFn itself is left as the real exec.LookPath check — cheap and
	// side-effect-free either way, mirroring omlxDetectFn's precedent above).
	installNonoFn = func() error { return nil }
	// startOmlxServerHost calls ensureOmlxAPIKey, which falls back to storeCredentialFn/
	// findCredentialFn on a cache miss — a test must never shell out to the real OS
	// keychain (security/secret-tool), which can mutate the developer's real login
	// keychain or block on a GUI permission prompt. Tests that specifically exercise
	// keychain behavior override these (with a cleanup restore).
	storeCredentialFn = func(string, string) error { return nil }
	findCredentialFn = func(string) (string, error) { return "", errors.New("not found (test stub)") }
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

// ensureOmlxAPIKey is a regression test for a real bug: omlx REFUSES to bind to a
// non-loopback host (RealRunner.Start always passes --host 0.0.0.0) without an API
// key, so the server process exited immediately on every `ai setup`/`ai services
// start omlx` — startOmlxServerHost must mint one (and set it in this process's
// env, so RealRunner.Start's os.Environ() passthrough carries it to the child) the
// first time it runs, from a keychain miss.
func TestStartOmlxHostMintsAPIKeyWhenAbsent(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	_ = os.Unsetenv("OMLX_API_KEY")
	test.Cleanup(func() { _ = os.Unsetenv("OMLX_API_KEY") }) // ensureOmlxAPIKey sets it as a side effect
	fakeOmlxDetect(test, true)
	originalManager, originalSync := omlxManagerFn, omlxSyncModelsFn
	test.Cleanup(func() { omlxManagerFn, omlxSyncModelsFn = originalManager, originalSync })
	omlxManagerFn = func() *omlx.Manager {
		return omlx.NewManager(omlx.Config{Probe: func() bool { return true }})
	}
	omlxSyncModelsFn = func() (litellm.SyncResult, error) { return litellm.SyncResult{}, nil }

	origStore, origFind := storeCredentialFn, findCredentialFn
	test.Cleanup(func() { storeCredentialFn, findCredentialFn = origStore, origFind })
	findCredentialFn = func(string) (string, error) { return "", errors.New("not found") }
	var stored string
	storeCredentialFn = func(name, value string) error {
		if name == "OMLX_API_KEY" {
			stored = value
		}
		return nil
	}

	if err := startOmlxServerHost(); err != nil {
		test.Fatalf("startOmlxServerHost: %v", err)
	}
	if stored == "" {
		test.Fatal("startOmlxServerHost must mint and store an OMLX_API_KEY when none exists")
	}
	if got := os.Getenv("OMLX_API_KEY"); got != stored {
		test.Errorf("OMLX_API_KEY env = %q, want the minted+stored key %q (RealRunner.Start needs it in this process's env)", got, stored)
	}
}

// TestOmlxAPIKeyConfigured pins the read-only check `ai setup` uses, captured
// BEFORE the reconcile runs, to decide whether to offer the one-time admin
// password prompt (checking after would always see the reconcile's own
// auto-minted key and never offer it).
func TestOmlxAPIKeyConfigured(test *testing.T) {
	origFind := findCredentialFn
	test.Cleanup(func() { findCredentialFn = origFind })

	findCredentialFn = func(string) (string, error) { return "", errors.New("not found") }
	if OmlxAPIKeyConfigured() {
		test.Error("OmlxAPIKeyConfigured() = true, want false when the keychain has no entry")
	}
	findCredentialFn = func(string) (string, error) { return "omlx-existing-key", nil }
	if !OmlxAPIKeyConfigured() {
		test.Error("OmlxAPIKeyConfigured() = false, want true when the keychain already has a key")
	}
}

// TestSetOmlxAPIKey pins `ai setup`'s password-prompt write path: the chosen value
// lands in BOTH this process's env (so a subsequent restart's os.Environ()
// passthrough carries it to the omlx subprocess) AND the keychain (so it survives
// past this process).
func TestSetOmlxAPIKey(test *testing.T) {
	_ = os.Unsetenv("OMLX_API_KEY")
	test.Cleanup(func() { _ = os.Unsetenv("OMLX_API_KEY") })
	origStore := storeCredentialFn
	test.Cleanup(func() { storeCredentialFn = origStore })
	var stored string
	storeCredentialFn = func(name, value string) error {
		if name == "OMLX_API_KEY" {
			stored = value
		}
		return nil
	}

	SetOmlxAPIKey("my-chosen-password")

	if got := os.Getenv("OMLX_API_KEY"); got != "my-chosen-password" {
		test.Errorf("OMLX_API_KEY env = %q, want %q", got, "my-chosen-password")
	}
	if stored != "my-chosen-password" {
		test.Errorf("stored keychain value = %q, want %q", stored, "my-chosen-password")
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

// TestOmlxContainerAPIBaseUsesHostGateway pins the fix for a real bug: LiteLLM's
// omlx/* registration must use hostGatewayName (the LiteLLM CONTAINER's route back
// to the host), never omlx.BaseURL()'s 127.0.0.1 — that address means the
// CONTAINER's own loopback from inside LiteLLM, which has nothing listening on it,
// so every chat completion failed with "Connection error" regardless of what omlx
// itself bound to.
func TestOmlxContainerAPIBaseUsesHostGateway(test *testing.T) {
	got := omlxContainerAPIBase()
	want := "http://host.docker.internal:8100/v1"
	if got != want {
		test.Errorf("omlxContainerAPIBase() = %q, want %q", got, want)
	}
	if strings.Contains(got, "127.0.0.1") {
		test.Errorf("omlxContainerAPIBase() must never be a loopback address (unreachable from the LiteLLM container): %q", got)
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

// The setup offer keys off a "chosen" marker, NOT key existence: the first reconcile always
// auto-mints a key, so gating on existence suppressed the prompt on every later run. Only a
// password the user actually set marks it chosen; the auto-minted one must not.
func TestOmlxPasswordChosenMarker(test *testing.T) {
	_ = os.Unsetenv("OMLX_API_KEY")
	test.Cleanup(func() { _ = os.Unsetenv("OMLX_API_KEY") })
	origStore, origFind := storeCredentialFn, findCredentialFn
	test.Cleanup(func() { storeCredentialFn, findCredentialFn = origStore, origFind })
	vault := map[string]string{}
	storeCredentialFn = func(name, value string) error { vault[name] = value; return nil }
	findCredentialFn = func(name string) (string, error) {
		if value, ok := vault[name]; ok {
			return value, nil
		}
		return "", errors.New("not found")
	}

	ensureOmlxAPIKey() // the reconcile's auto-mint
	if vault["OMLX_API_KEY"] == "" {
		test.Fatal("expected an auto-minted key")
	}
	if OmlxPasswordChosen() {
		test.Error("an auto-minted key must NOT count as chosen — the prompt would never show")
	}
	SetOmlxAPIKey("my-chosen-password")
	if !OmlxPasswordChosen() {
		test.Error("a password set via SetOmlxAPIKey must count as chosen")
	}
}
