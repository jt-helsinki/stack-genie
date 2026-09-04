package vllm

import (
	"sync"
	"testing"
	"time"
)

// raceRunner wraps FakeRunner to simulate vLLM's real startup latency: the
// underlying process does not actually bind its port until some time AFTER
// Runner.Start returns to the caller (Python/MLX import + engine setup). onBound
// is invoked once the simulated bind happens.
type raceRunner struct {
	*FakeRunner
	bindDelay time.Duration
	onBound   func()
}

func (runner *raceRunner) Start(alias, model string, port int, storeDir string, opts ServeOptions) (ServerHandle, error) {
	handle, err := runner.FakeRunner.Start(alias, model, port, storeDir, opts)
	if err == nil {
		time.Sleep(runner.bindDelay)
		runner.onBound()
	}
	return handle, err
}

// TestEnsureServedSerializesAcrossManagers proves two SEPARATE Managers (standing
// in for two SEPARATE `ai` process invocations, since `ai` is daemonless) racing
// EnsureServed for the SAME alias no longer both spawn `vllm serve` on the same
// port. Before the lockAlias fix, the second invocation's portInUse() check could
// run in the window before the first's real vLLM process actually bound the
// socket, see the port as free, and start a duplicate — crashing with
// "Address already in use" (the bug hit in production by a double-fired restart).
func TestEnsureServedSerializesAcrossManagers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var boundMu sync.Mutex
	bound := false
	setBound := func() {
		boundMu.Lock()
		bound = true
		boundMu.Unlock()
	}
	probe := func(int) bool {
		boundMu.Lock()
		defer boundMu.Unlock()
		return bound
	}

	original := portInUse
	portInUse = probe
	defer func() { portInUse = original }()

	runnerA := &raceRunner{FakeRunner: &FakeRunner{}, bindDelay: 30 * time.Millisecond, onBound: setBound}
	runnerB := &raceRunner{FakeRunner: &FakeRunner{}, bindDelay: 30 * time.Millisecond, onBound: setBound}

	cfg := Config{Probe: probe, BasePort: 8101, PollInterval: 5 * time.Millisecond}
	managerA := NewManager(mergeConfig(cfg, runnerA))
	managerB := NewManager(mergeConfig(cfg, runnerB))

	var wait sync.WaitGroup
	errs := make([]error, 2)
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, _, errs[0] = managerA.EnsureServed("qwen", "mlx-community/Qwen")
	}()
	go func() {
		defer wait.Done()
		_, _, errs[1] = managerB.EnsureServed("qwen", "mlx-community/Qwen")
	}()
	wait.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("EnsureServed[%d]: %v", i, err)
		}
	}
	total := runnerA.StartCount() + runnerB.StartCount()
	if total != 1 {
		t.Fatalf("total Runner.Start calls = %d, want 1 (the losing manager must adopt the winner's server, not double-start)", total)
	}
}

func mergeConfig(cfg Config, runner Runner) Config {
	cfg.Runner = runner
	return cfg
}
