package setup

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/colibri"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
)

func fakeColibriDetect(test *testing.T, installed bool) {
	test.Helper()
	original := colibriDetectFn
	test.Cleanup(func() { colibriDetectFn = original })
	colibriDetectFn = func() bool { return installed }
}

func fakeColibriStatusCommand(test *testing.T, configured bool) {
	test.Helper()
	original := colibriStatusFn
	test.Cleanup(func() { colibriStatusFn = original })
	colibriStatusFn = func() colibri.StatusInfo { return colibri.StatusInfo{Configured: configured} }
}

// startColibriServerHost must NEVER return an error — colibri is optional end to
// end, unlike omlx (see colibri_host.go's package comment).
func TestStartColibriHostNotInstalledNeverErrors(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fakeColibriDetect(test, false)
	if err := startColibriServerHost(); err != nil {
		test.Fatalf("startColibriServerHost must never error when colibri is absent (optional backend), got %v", err)
	}
}

// startColibriServerHost does nothing (no error, no coli subcommand attempted) when
// colibri is installed but not yet configured — this platform must never trigger
// colibri's own first-time model download.
func TestStartColibriHostNotConfiguredNeverErrors(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fakeColibriDetect(test, true)
	fakeColibriStatusCommand(test, false)

	origManagerFn := colibriManagerFn
	test.Cleanup(func() { colibriManagerFn = origManagerFn })
	colibriManagerFn = func() *colibri.Manager {
		return colibri.NewManager(colibri.Config{Probe: func() bool { return false }, StatusFn: colibriStatusFn})
	}

	if err := startColibriServerHost(); err != nil {
		test.Fatalf("startColibriServerHost must never error when colibri is unconfigured, got %v", err)
	}
}

// startColibriServerHost adopts an already-healthy server and syncs LiteLLM.
func TestStartColibriHostStartsAndSyncs(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fakeColibriDetect(test, true)

	origManagerFn, origSyncFn := colibriManagerFn, colibriSyncModelsFn
	test.Cleanup(func() { colibriManagerFn, colibriSyncModelsFn = origManagerFn, origSyncFn })
	colibriManagerFn = func() *colibri.Manager {
		return colibri.NewManager(colibri.Config{Probe: func() bool { return true }})
	}
	var syncCalled bool
	colibriSyncModelsFn = func() (litellm.SyncResult, error) {
		syncCalled = true
		return litellm.SyncResult{Added: []string{"colibri/glm-5.2"}}, nil
	}

	if err := startColibriServerHost(); err != nil {
		test.Fatalf("startColibriServerHost: %v", err)
	}
	if !syncCalled {
		test.Error("startColibriServerHost must sync LiteLLM after the server is healthy")
	}
}

// startColibriServerHost swallows a start failure too (optional backend) —
// UNLIKE startOmlxServerHost, which surfaces one.
func TestStartColibriHostStartFailureNeverErrors(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fakeColibriDetect(test, true)
	fakeColibriStatusCommand(test, true)

	origManagerFn := colibriManagerFn
	test.Cleanup(func() { colibriManagerFn = origManagerFn })
	colibriManagerFn = func() *colibri.Manager {
		return colibri.NewManager(colibri.Config{
			Probe:    func() bool { return false },
			StatusFn: colibriStatusFn,
			Runner:   failingColibriRunner{},
		})
	}

	if err := startColibriServerHost(); err != nil {
		test.Fatalf("startColibriServerHost must never error on a coli start failure (optional backend), got %v", err)
	}
}

type failingColibriRunner struct{}

func (failingColibriRunner) Run(string, ...string) ([]byte, error) {
	return []byte("boom"), errors.New("exit status 1")
}

func TestStopColibriHost(test *testing.T) {
	origManagerFn := colibriManagerFn
	test.Cleanup(func() { colibriManagerFn = origManagerFn })
	var stopped bool
	colibriManagerFn = func() *colibri.Manager {
		return colibri.NewManager(colibri.Config{Runner: stopTrackingRunner{stopped: &stopped}})
	}
	if err := stopColibriServerHost(); err != nil {
		test.Fatalf("stopColibriServerHost: %v", err)
	}
	if !stopped {
		test.Error("stopColibriServerHost must call Manager.Stop")
	}
}

type stopTrackingRunner struct{ stopped *bool }

func (runner stopTrackingRunner) Run(_ string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "stop" {
		*runner.stopped = true
	}
	return nil, nil
}

// ensureColibriAPIKey mirrors TestStartOmlxHostMintsAPIKeyWhenAbsent: it mints and
// stores a fresh key when none exists, and sets it in this process's env.
func TestEnsureColibriAPIKeyMintsWhenAbsent(test *testing.T) {
	_ = os.Unsetenv("COLI_API_KEY")
	test.Cleanup(func() { _ = os.Unsetenv("COLI_API_KEY") })
	origStore, origFind := storeCredentialFn, findCredentialFn
	test.Cleanup(func() { storeCredentialFn, findCredentialFn = origStore, origFind })
	findCredentialFn = func(string) (string, error) { return "", errors.New("not found") }
	var stored string
	storeCredentialFn = func(name, value string) error {
		if name == "COLI_API_KEY" {
			stored = value
		}
		return nil
	}

	ensureColibriAPIKey()

	if stored == "" {
		test.Fatal("ensureColibriAPIKey must mint and store a key when none exists")
	}
	if got := os.Getenv("COLI_API_KEY"); got != stored {
		test.Errorf("COLI_API_KEY env = %q, want the minted+stored key %q", got, stored)
	}
}

func TestLoadColibriAPIKeyFromKeychain(test *testing.T) {
	origFind := findCredentialFn
	test.Cleanup(func() { findCredentialFn = origFind })
	_ = os.Unsetenv("COLI_API_KEY")
	test.Cleanup(func() { _ = os.Unsetenv("COLI_API_KEY") })
	findCredentialFn = func(string) (string, error) { return "colibri-from-keychain", nil }

	LoadColibriAPIKeyFromKeychain()

	if got := os.Getenv("COLI_API_KEY"); got != "colibri-from-keychain" {
		test.Errorf("COLI_API_KEY = %q, want the keychain value", got)
	}
}

// colibriDownDetail reports all three distinct states colibri can be in — unlike
// omlx's two, since colibri also needs the user's own one-time `coli setup`.
func TestColibriDownDetailThreeStates(test *testing.T) {
	fakeColibriDetect(test, false)
	if got := colibriDownDetail(); got == "" {
		test.Error("colibriDownDetail must report something when not installed")
	}

	fakeColibriDetect(test, true)
	fakeColibriStatusCommand(test, false)
	notConfigured := colibriDownDetail()
	if notConfigured == "" {
		test.Error("colibriDownDetail must report something when installed but not configured")
	}

	fakeColibriStatusCommand(test, true)
	notRunning := colibriDownDetail()
	if notRunning == "" || notRunning == notConfigured {
		test.Errorf("colibriDownDetail for configured-but-not-running must differ from the not-configured message, got %q", notRunning)
	}
}

func TestColibriHealthy(test *testing.T) {
	origGet := colibriHTTPGet
	test.Cleanup(func() { colibriHTTPGet = origGet })

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	colibriHTTPGet = func(string) (*http.Response, error) { return http.Get(server.URL) } //nolint:gosec,noctx // test server, fixed local URL

	if !colibriHealthy() {
		test.Error("colibriHealthy() = false, want true against a 200-answering server")
	}

	colibriHTTPGet = func(string) (*http.Response, error) { return nil, errors.New("connection refused") }
	if colibriHealthy() {
		test.Error("colibriHealthy() = true, want false when the probe errors")
	}
}
