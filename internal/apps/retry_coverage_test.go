package apps

import (
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
)

// retryExec models an in-VM runtime that reports "unreachable" on the FIRST
// `nerdctl run` and succeeds on the second, so runContainer's re-ensure-and-retry
// branch can be exercised. It also fails `nerdctl start`/`restart` so Start/Restart
// fall through to a fresh run.
type retryExec struct {
	calls        [][]string
	runCount     int
	firstStderr  string // stderr returned on the first run (unreachable vs ordinary)
	failStartCmd bool   // when true, nerdctl start/restart return non-zero (force fallback)
}

func (exec *retryExec) run(argv []string) (ExecResult, error) {
	exec.calls = append(exec.calls, argv)
	if len(argv) >= 2 && argv[0] == "nerdctl" {
		switch argv[1] {
		case "start", "restart":
			if exec.failStartCmd {
				return ExecResult{ExitCode: 1, Stderr: "no such container"}, nil
			}
		case "run":
			exec.runCount++
			if exec.runCount == 1 {
				return ExecResult{ExitCode: 1, Stderr: exec.firstStderr}, nil
			}
			return ExecResult{ExitCode: 0}, nil
		}
	}
	return ExecResult{}, nil
}

func runCalls(calls [][]string) int {
	count := 0
	for _, call := range calls {
		if len(call) >= 2 && call[0] == "nerdctl" && call[1] == "run" {
			count++
		}
	}
	return count
}

// TestStartRetriesWhenRuntimeUnreachable: Start falls through to a fresh run; the
// first run reports the runtime unreachable, so EnsureRuntime is re-invoked and the
// run is retried once, then succeeds.
func TestStartRetriesWhenRuntimeUnreachable(test *testing.T) {
	exec := &retryExec{firstStderr: "failed to dial: connection refused", failStartCmd: true}
	deps, _ := newTestDeps(&config.Config{Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}}}, exec.run)
	ensureCalls := 0
	deps.EnsureRuntime = func() error { ensureCalls++; return nil }
	manager := NewManager(deps)

	if err := manager.Start("openwebui"); err != nil {
		test.Fatalf("Start with a recoverable unreachable runtime should succeed: %v", err)
	}
	if got := runCalls(exec.calls); got != 2 {
		test.Fatalf("nerdctl run attempted %d times, want 2 (initial + one retry)", got)
	}
	// EnsureRuntime: once before the initial run, once on the retry.
	if ensureCalls != 2 {
		test.Fatalf("EnsureRuntime called %d times, want 2 (initial + retry)", ensureCalls)
	}
}

// TestRestartRetriesWhenRuntimeUnreachable mirrors the Start case via Restart's
// fall-through to a fresh run.
func TestRestartRetriesWhenRuntimeUnreachable(test *testing.T) {
	exec := &retryExec{firstStderr: "cannot access containerd.sock", failStartCmd: true}
	deps, _ := newTestDeps(&config.Config{Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}}}, exec.run)
	ensureCalls := 0
	deps.EnsureRuntime = func() error { ensureCalls++; return nil }
	manager := NewManager(deps)

	if err := manager.Restart("openwebui"); err != nil {
		test.Fatalf("Restart with a recoverable unreachable runtime should succeed: %v", err)
	}
	if got := runCalls(exec.calls); got != 2 {
		test.Fatalf("nerdctl run attempted %d times, want 2 (initial + one retry)", got)
	}
	if ensureCalls != 2 {
		test.Fatalf("EnsureRuntime called %d times, want 2 (initial + retry)", ensureCalls)
	}
}

// TestRunContainerNoRetryOnOrdinaryError: an ordinary (non-unreachable) run failure
// is NOT retried — the error is returned after a single attempt.
func TestRunContainerNoRetryOnOrdinaryError(test *testing.T) {
	exec := &retryExec{firstStderr: "invalid image reference", failStartCmd: true}
	deps, _ := newTestDeps(&config.Config{Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}}}, exec.run)
	ensureCalls := 0
	deps.EnsureRuntime = func() error { ensureCalls++; return nil }
	manager := NewManager(deps)

	if err := manager.Start("openwebui"); err == nil {
		test.Fatal("Start should return the run error when the failure is not runtime-unreachable")
	}
	if got := runCalls(exec.calls); got != 1 {
		test.Fatalf("nerdctl run attempted %d times, want 1 (no retry on an ordinary error)", got)
	}
	// EnsureRuntime called only once (before the initial run); no retry re-ensure.
	if ensureCalls != 1 {
		test.Fatalf("EnsureRuntime called %d times, want 1 (no retry)", ensureCalls)
	}
}
