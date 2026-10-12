package colibri

import (
	"fmt"
	"os/exec"
	"time"
)

// CommandRunner runs one `coli` subcommand to completion, returning its combined
// output. Injectable so Manager is fully unit-tested with a fake — no real
// process is spawned in tests. Unlike internal/omlx's Runner (which forks and
// tracks a raw detached process this platform owns), colibri manages its own
// backgrounding/PID/log state internally (`coli start --background`/`coli stop`),
// so this is just "run a subcommand and see what it says".
type CommandRunner interface {
	Run(binary string, args ...string) ([]byte, error)
}

// RealCommandRunner shells out via os/exec. It is still gated UPSTREAM by
// Detect (a `coli` binary resolvable) — a host without colibri never reaches it.
type RealCommandRunner struct{}

func (RealCommandRunner) Run(binary string, args ...string) ([]byte, error) {
	return exec.Command(binary, args...).CombinedOutput() //nolint:gosec // binary is BinaryPath()'s own resolved value, args are fixed literals from this package
}

// Manager owns starting/stopping/health-checking the single configured colibri
// server. Runner/Probe/Sleep/StatusFn are injectable so it is fully unit-tested
// with fakes, AND so a consumer package (internal/setup) can control every
// dependency when it builds a Manager for its own tests — not just the ones this
// package's own tests happen to need.
type Manager struct {
	runner       CommandRunner
	probe        HealthProbe
	statusFn     func() StatusInfo
	sleep        func(time.Duration)
	log          func(string)
	startTimeout time.Duration
	pollInterval time.Duration
}

// Config configures a Manager; zero-value fields default to the real
// implementations (RealCommandRunner, DefaultProbe, Status, time.Sleep, a no-op
// logger, DefaultStartTimeout/DefaultPollInterval).
type Config struct {
	Runner       CommandRunner
	Probe        HealthProbe
	StatusFn     func() StatusInfo
	Sleep        func(time.Duration)
	Log          func(string)
	StartTimeout time.Duration
	PollInterval time.Duration
}

// NewManager builds a Manager from cfg, defaulting unset fields to the real
// implementations.
func NewManager(cfg Config) *Manager {
	runner := cfg.Runner
	if runner == nil {
		runner = RealCommandRunner{}
	}
	probe := cfg.Probe
	if probe == nil {
		probe = DefaultProbe
	}
	statusFn := cfg.StatusFn
	if statusFn == nil {
		statusFn = Status
	}
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	logf := cfg.Log
	if logf == nil {
		logf = func(string) {}
	}
	startTimeout := cfg.StartTimeout
	if startTimeout == 0 {
		startTimeout = DefaultStartTimeout
	}
	pollInterval := cfg.PollInterval
	if pollInterval == 0 {
		pollInterval = DefaultPollInterval
	}
	return &Manager{
		runner:       runner,
		probe:        probe,
		statusFn:     statusFn,
		sleep:        sleep,
		log:          logf,
		startTimeout: startTimeout,
		pollInterval: pollInterval,
	}
}

// NotConfiguredError is returned by EnsureServed when colibri has never been
// configured (`coli status --json` reports configured=false) — there is no model
// to start, and this platform deliberately never triggers colibri's own
// (potentially huge) first-time model download on its behalf. Callers treat this
// as a normal, expected, non-fatal state (see this package's doc comment).
type NotConfiguredError struct{}

func (NotConfiguredError) Error() string {
	return "colibri has not been configured yet — run `coli setup` to pick and download a model first"
}

// EnsureServed makes sure the configured colibri model is running and healthy:
// adopts an already-healthy server unchanged; otherwise, if colibri reports
// configured (coli status --json), runs `coli start --background --no-browser
// --json` and polls the health probe until it answers or startTimeout elapses.
// Returns NotConfiguredError (not a generic error) when nothing has been
// configured yet, so callers can treat it as best-effort/skip rather than a
// start failure.
func (manager *Manager) EnsureServed() error {
	if manager.probe() {
		manager.log("colibri: already running and healthy — adopting it")
		return nil
	}
	if !manager.statusFn().Configured {
		return NotConfiguredError{}
	}
	binary, err := BinaryPath()
	if err != nil {
		return fmt.Errorf("colibri: resolve binary: %w", err)
	}
	manager.log("colibri: starting the configured server")
	if output, err := manager.runner.Run(binary, "start", "--background", "--no-browser", "--json"); err != nil {
		return fmt.Errorf("colibri: start failed: %w (output: %s)", err, string(output))
	}
	return manager.waitHealthy()
}

// waitHealthy polls the health probe until it answers or startTimeout elapses.
func (manager *Manager) waitHealthy() error {
	deadline := time.Now().Add(manager.startTimeout)
	for {
		if manager.probe() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("colibri: server did not become healthy within %s", manager.startTimeout)
		}
		manager.sleep(manager.pollInterval)
	}
}

// Stop stops the running colibri server via `coli stop`, best-effort (a
// not-running server is not an error — matches internal/omlx.Manager.Stop's
// contract).
func (manager *Manager) Stop() error {
	binary, err := BinaryPath()
	if err != nil {
		return nil //nolint:nilerr // best-effort: nothing resolvable means nothing to stop
	}
	_, _ = manager.runner.Run(binary, "stop")
	return nil
}
