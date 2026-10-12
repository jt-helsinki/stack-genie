package colibri

import (
	"errors"
	"testing"
	"time"
)

// fakeCommandRunner records every Run call and returns a scripted response.
type fakeCommandRunner struct {
	calls  [][]string
	output []byte
	err    error
}

func (runner *fakeCommandRunner) Run(binary string, args ...string) ([]byte, error) {
	runner.calls = append(runner.calls, append([]string{binary}, args...))
	return runner.output, runner.err
}

// fakeStatus returns a StatusFn reporting a fixed Configured value — used via
// Config.StatusFn so tests control EnsureServed's configuration check directly,
// without touching the package-level Status/statusCommand (which a CONSUMER
// package, e.g. internal/setup, cannot reach — StatusFn exists on Manager/Config
// precisely so every dependency is controllable from the Config alone).
func fakeStatus(configured bool) func() StatusInfo {
	return func() StatusInfo { return StatusInfo{Configured: configured} }
}

func swapLookPathFound(test *testing.T) {
	test.Helper()
	original := lookPath
	test.Cleanup(func() { lookPath = original })
	lookPath = func(string) (string, error) { return "/usr/local/bin/coli", nil }
}

// EnsureServed adopts an already-healthy server without running anything.
func TestEnsureServedAdoptsHealthy(test *testing.T) {
	runner := &fakeCommandRunner{}
	manager := NewManager(Config{Runner: runner, Probe: func() bool { return true }})
	if err := manager.EnsureServed(); err != nil {
		test.Fatalf("EnsureServed: %v", err)
	}
	if len(runner.calls) != 0 {
		test.Errorf("an already-healthy server must not run any coli subcommand, got %v", runner.calls)
	}
}

// EnsureServed returns NotConfiguredError (not a generic error) when colibri
// reports no model configured — this platform must never try to trigger
// colibri's own first-time model download.
func TestEnsureServedNotConfigured(test *testing.T) {
	swapLookPathFound(test)
	runner := &fakeCommandRunner{}
	manager := NewManager(Config{Runner: runner, Probe: func() bool { return false }, StatusFn: fakeStatus(false)})

	err := manager.EnsureServed()
	if _, ok := err.(NotConfiguredError); !ok { //nolint:errorlint // exact-type check is intentional: this sentinel carries no wrapped cause
		test.Fatalf("EnsureServed = %v (%T), want NotConfiguredError", err, err)
	}
	if len(runner.calls) != 0 {
		test.Errorf("an unconfigured colibri must never run `coli start` (that would fail, but we must not even try triggering setup), got %v", runner.calls)
	}
}

// EnsureServed starts an already-configured server and waits for it to become
// healthy.
func TestEnsureServedStartsConfigured(test *testing.T) {
	swapLookPathFound(test)
	runner := &fakeCommandRunner{output: []byte(`{"ok": true}`)}

	healthy := false
	probeCalls := 0
	probe := func() bool {
		probeCalls++
		if probeCalls >= 2 { // unhealthy on the first check, healthy after "starting"
			healthy = true
		}
		return healthy
	}
	manager := NewManager(Config{Runner: runner, Probe: probe, Sleep: func(time.Duration) {}, StatusFn: fakeStatus(true)})

	if err := manager.EnsureServed(); err != nil {
		test.Fatalf("EnsureServed: %v", err)
	}
	if len(runner.calls) != 1 {
		test.Fatalf("expected exactly one coli subcommand, got %v", runner.calls)
	}
	got := runner.calls[0]
	wantArgs := []string{"start", "--background", "--no-browser", "--json"}
	if len(got) != len(wantArgs)+1 {
		test.Fatalf("coli start args = %v, want binary + %v", got, wantArgs)
	}
	for i, arg := range wantArgs {
		if got[i+1] != arg {
			test.Errorf("coli start arg[%d] = %q, want %q (full: %v)", i, got[i+1], arg, got)
		}
	}
}

// EnsureServed surfaces a `coli start` failure.
func TestEnsureServedStartFailure(test *testing.T) {
	swapLookPathFound(test)
	runner := &fakeCommandRunner{output: []byte("boom"), err: errors.New("exit status 1")}
	manager := NewManager(Config{Runner: runner, Probe: func() bool { return false }, StatusFn: fakeStatus(true)})

	if err := manager.EnsureServed(); err == nil {
		test.Fatal("EnsureServed must surface a coli start failure")
	}
}

// EnsureServed times out if the server never becomes healthy after starting.
func TestEnsureServedTimesOut(test *testing.T) {
	swapLookPathFound(test)
	runner := &fakeCommandRunner{output: []byte(`{"ok": true}`)}
	manager := NewManager(Config{
		Runner:       runner,
		Probe:        func() bool { return false },
		StatusFn:     fakeStatus(true),
		Sleep:        func(time.Duration) {},
		StartTimeout: 5 * time.Millisecond,
		PollInterval: time.Millisecond,
	})
	if err := manager.EnsureServed(); err == nil {
		test.Fatal("EnsureServed must time out when the server never becomes healthy")
	}
}

// Stop runs `coli stop` and is best-effort (never errors).
func TestManagerStop(test *testing.T) {
	swapLookPathFound(test)
	runner := &fakeCommandRunner{err: errors.New("not running")}
	manager := NewManager(Config{Runner: runner})

	if err := manager.Stop(); err != nil {
		test.Fatalf("Stop must be best-effort (never error), got %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0][1] != "stop" {
		test.Errorf("Stop must run `coli stop`, got %v", runner.calls)
	}
}
