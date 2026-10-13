package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Hermetic by default: no test in this package may install multica or shell out to it.
func init() {
	multicaDetectFn = func() bool { return false }
	multicaRunFn = func([]string, ...string) ([]byte, error) { return nil, errors.New("multica must not run in tests") }
	multicaMintKeyFn = func() error { return nil }
}

type multicaHostFixture struct {
	installed, minted bool
	calls             []string
	daemonEnv         []string
	loggedIn          bool
	installErr        error
	progress          []string
}

func newMulticaHostFixture(test *testing.T) *multicaHostFixture {
	test.Helper()
	test.Setenv("HOME", test.TempDir())
	fixture := &multicaHostFixture{}
	origDetect, origInstall, origRun, origMint := multicaDetectFn, multicaInstallFn, multicaRunFn, multicaMintKeyFn
	test.Cleanup(func() {
		multicaDetectFn, multicaInstallFn, multicaRunFn, multicaMintKeyFn = origDetect, origInstall, origRun, origMint
	})
	multicaDetectFn = func() bool { return fixture.installed }
	multicaInstallFn = func() error {
		if fixture.installErr == nil {
			fixture.installed = true
		}
		return fixture.installErr
	}
	multicaMintKeyFn = func() error { fixture.minted = true; return nil }
	multicaRunFn = func(env []string, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		fixture.calls = append(fixture.calls, call)
		switch call {
		case "auth status":
			if !fixture.loggedIn {
				return nil, errors.New("not logged in")
			}
		case "daemon start":
			fixture.daemonEnv = env
		}
		return nil, nil
	}
	return fixture
}

func (fixture *multicaHostFixture) report(line string) {
	fixture.progress = append(fixture.progress, line)
}

func TestEnsureMulticaHostFirstRunInstallsConfiguresShimsAndAsksForLogin(test *testing.T) {
	fixture := newMulticaHostFixture(test)
	ensureMulticaHost(fixture.report, "aip.local")

	if !fixture.installed {
		test.Error("CLI was not installed")
	}
	joined := strings.Join(fixture.calls, "|")
	for _, want := range []string{"config set server_url http://multica.aip.local:18787", "config set app_url http://multica.aip.local:18787"} {
		if !strings.Contains(joined, want) {
			test.Errorf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "daemon start") || fixture.minted {
		test.Errorf("daemon started / key minted before the user signed in: %s", joined)
	}
	if !strings.Contains(strings.Join(fixture.progress, "\n"), "multica login") {
		test.Errorf("no login guidance in %v", fixture.progress)
	}
	// Shims exist for the CLIs Multica supports — and not for gemini, which it does not.
	shimDir := filepath.Join(os.Getenv("HOME"), ".ai-platform", "bin", "multica-shims")
	for _, binary := range []string{"claude", "codex", "opencode", "omp", "hermes"} {
		if _, err := os.Stat(filepath.Join(shimDir, binary)); err != nil {
			test.Errorf("missing shim %s: %v", binary, err)
		}
	}
	if _, err := os.Stat(filepath.Join(shimDir, "gemini")); err == nil {
		test.Error("gemini has no Multica runtime — it must not get a shim")
	}
}

func TestEnsureMulticaHostLoggedInStartsDaemonOnShimPathAfterMintingKey(test *testing.T) {
	fixture := newMulticaHostFixture(test)
	fixture.installed, fixture.loggedIn = true, true
	ensureMulticaHost(fixture.report, "aip.local")

	if !fixture.minted {
		test.Error("shared gateway key not minted before the daemon started")
	}
	joined := strings.Join(fixture.calls, "|")
	if strings.Index(joined, "daemon stop") > strings.Index(joined, "daemon start") || !strings.Contains(joined, "daemon start") {
		test.Errorf("expected stop-then-start, got %s", joined)
	}
	shimDir := filepath.Join(os.Getenv("HOME"), ".ai-platform", "bin", "multica-shims")
	found := false
	for _, entry := range fixture.daemonEnv {
		found = found || strings.HasPrefix(entry, "PATH="+shimDir)
	}
	if !found {
		test.Errorf("daemon not started with the shim dir first on PATH: %v", fixture.daemonEnv)
	}
}

// A failed (e.g. checksum-mismatching) install must stop everything: no config, no shims, no daemon.
func TestEnsureMulticaHostInstallFailureStopsEverything(test *testing.T) {
	fixture := newMulticaHostFixture(test)
	fixture.installErr = errors.New("checksum mismatch")
	ensureMulticaHost(fixture.report, "aip.local")

	if len(fixture.calls) != 0 {
		test.Errorf("multica ran after a failed install: %v", fixture.calls)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".ai-platform", "bin", "multica-shims")); err == nil {
		test.Error("shims written despite the failed install")
	}
	if !strings.Contains(strings.Join(fixture.progress, "\n"), "checksum mismatch") {
		test.Errorf("failure not reported: %v", fixture.progress)
	}
}

func TestMulticaDaemonDetailStates(test *testing.T) {
	fixture := newMulticaHostFixture(test)
	if got := multicaDaemonDetail(); !strings.Contains(got, "not installed") {
		test.Errorf("not installed: %q", got)
	}
	fixture.installed = true
	if got := multicaDaemonDetail(); !strings.Contains(got, "multica login") {
		test.Errorf("not signed in: %q", got)
	}
	fixture.loggedIn = true
	if got := multicaDaemonDetail(); !strings.Contains(got, "daemon running") {
		test.Errorf("running: %q", got)
	}
}
