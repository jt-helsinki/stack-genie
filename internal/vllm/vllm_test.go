package vllm

import (
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// stepClock is a deterministic clock: now() advances one second per call and sleep
// advances by its duration. Strictly-increasing now() gives distinct, ordered
// lastUsed timestamps so LRU is unambiguous without real sleeping.
type stepClock struct{ n int64 }

func (clock *stepClock) now() time.Time        { clock.n++; return time.Unix(0, clock.n*int64(time.Second)) }
func (clock *stepClock) sleep(d time.Duration) { clock.n += int64(d / time.Second) }

// alwaysHealthy is a probe that reports every port healthy.
func alwaysHealthy(int) bool { return true }

// healthySet returns a probe that reports the given ports (and only those) healthy.
func healthySet(ports ...int) HealthProbe {
	set := map[int]bool{}
	for _, port := range ports {
		set[port] = true
	}
	return func(port int) bool { return set[port] }
}

func newTestManager(cfg Config) (*Manager, *FakeRunner, *stepClock) {
	runner := &FakeRunner{}
	clock := &stepClock{}
	cfg.Runner = runner
	if cfg.Probe == nil {
		cfg.Probe = alwaysHealthy
	}
	cfg.Now = clock.now
	cfg.Sleep = clock.sleep
	return NewManager(cfg), runner, clock
}

// newTestManagerWithRunner is newTestManager with a caller-supplied FakeRunner (so a
// test can pre-seed per-alias Start errors).
func newTestManagerWithRunner(cfg Config, runner *FakeRunner) (*Manager, *FakeRunner, *stepClock) {
	clock := &stepClock{}
	cfg.Runner = runner
	if cfg.Probe == nil {
		cfg.Probe = alwaysHealthy
	}
	cfg.Now = clock.now
	cfg.Sleep = clock.sleep
	return NewManager(cfg), runner, clock
}

func TestStoreDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := StoreDir()
	if err != nil {
		t.Fatalf("StoreDir: %v", err)
	}
	want := filepath.Join(home, ".ai-platform", "volumes", "models", "vllm")
	if dir != want {
		t.Fatalf("StoreDir = %q, want %q", dir, want)
	}
}

func TestEndpointHelpers(t *testing.T) {
	if got := Endpoint(8101); got != "http://127.0.0.1:8101/v1" {
		t.Fatalf("Endpoint = %q", got)
	}
	if got := ContainerEndpoint(8101); got != "http://host.docker.internal:8101/v1" {
		t.Fatalf("ContainerEndpoint = %q", got)
	}
}

func TestEnsureServedLazyStart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manager, runner, _ := newTestManager(Config{BasePort: 8101})

	endpoint, err := manager.EnsureServed("llama", "mlx-community/Llama")
	if err != nil {
		t.Fatalf("EnsureServed: %v", err)
	}
	if endpoint != "http://127.0.0.1:8101/v1" {
		t.Fatalf("endpoint = %q", endpoint)
	}
	if runner.StartCount() != 1 {
		t.Fatalf("StartCount = %d, want 1", runner.StartCount())
	}
	call := runner.StartCalls[0]
	if call.Alias != "llama" || call.Model != "mlx-community/Llama" || call.Port != 8101 {
		t.Fatalf("unexpected start call: %+v", call)
	}
	if call.StoreDir == "" {
		t.Fatalf("start call missing store dir")
	}
}

func TestEnsureServedReusesAndTouchesLRU(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manager, runner, _ := newTestManager(Config{BasePort: 8101})

	if _, err := manager.EnsureServed("a", "m-a"); err != nil {
		t.Fatalf("first: %v", err)
	}
	firstLastUsed := manager.servers["a"].lastUsed

	endpoint, err := manager.EnsureServed("a", "m-a")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if endpoint != "http://127.0.0.1:8101/v1" {
		t.Fatalf("endpoint = %q", endpoint)
	}
	if runner.StartCount() != 1 {
		t.Fatalf("StartCount = %d, want 1 (reuse, no restart)", runner.StartCount())
	}
	if !manager.servers["a"].lastUsed.After(firstLastUsed) {
		t.Fatalf("lastUsed not advanced on reuse")
	}
}

func TestPortAllocationUnique(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manager, _, _ := newTestManager(Config{BasePort: 8101, MaxServers: 5})

	for _, alias := range []string{"a", "b", "c"} {
		if _, err := manager.EnsureServed(alias, "m-"+alias); err != nil {
			t.Fatalf("EnsureServed %s: %v", alias, err)
		}
	}
	seen := map[int]bool{}
	for _, status := range manager.Running() {
		if seen[status.Port] {
			t.Fatalf("duplicate port %d", status.Port)
		}
		seen[status.Port] = true
	}
	if len(seen) != 3 {
		t.Fatalf("got %d distinct ports, want 3", len(seen))
	}
	// Contiguous from base.
	for _, want := range []int{8101, 8102, 8103} {
		if !seen[want] {
			t.Fatalf("expected port %d allocated; got %v", want, seen)
		}
	}
}

func TestCapEvictsLRU(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var logs []string
	manager, runner, _ := newTestManager(Config{
		BasePort:   8101,
		MaxServers: 2,
		Log:        func(line string) { logs = append(logs, line) },
	})

	// Fill to the cap, then use "a" so "b" becomes LRU.
	for _, alias := range []string{"a", "b"} {
		if _, err := manager.EnsureServed(alias, "m-"+alias); err != nil {
			t.Fatalf("EnsureServed %s: %v", alias, err)
		}
	}
	if _, err := manager.EnsureServed("a", "m-a"); err != nil { // touch a → b is LRU
		t.Fatalf("touch a: %v", err)
	}

	if _, err := manager.EnsureServed("c", "m-c"); err != nil {
		t.Fatalf("EnsureServed c: %v", err)
	}

	if _, ok := manager.servers["b"]; ok {
		t.Fatalf("expected LRU server b to be evicted")
	}
	if _, ok := manager.servers["a"]; !ok {
		t.Fatalf("expected a to survive")
	}
	if _, ok := manager.servers["c"]; !ok {
		t.Fatalf("expected c to be running")
	}
	if len(manager.servers) != 2 {
		t.Fatalf("running = %d, want 2 (cap)", len(manager.servers))
	}
	if runner.StopCount() != 1 {
		t.Fatalf("StopCount = %d, want 1 (the eviction)", runner.StopCount())
	}
	if len(logs) == 0 {
		t.Fatalf("eviction was not logged")
	}
}

// TestFailedStartAtCapKeepsHealthyServers is the regression for the evict-before-start
// bug: when the running set is at MaxServers and a NEW server fails to start, the
// eviction must NOT have happened — a start that never becomes healthy must never cost
// a working model. Eviction now runs only AFTER the new server is confirmed healthy.
func TestFailedStartAtCapKeepsHealthyServers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &FakeRunner{StartErrs: map[string]error{"c": errors.New("boom")}}
	manager, _, _ := newTestManagerWithRunner(Config{BasePort: 8101, MaxServers: 2}, runner)

	for _, alias := range []string{"a", "b"} {
		if _, err := manager.EnsureServed(alias, "m-"+alias); err != nil {
			t.Fatalf("EnsureServed %s: %v", alias, err)
		}
	}
	// c's start fails: it must error, and NEITHER a nor b may be evicted.
	if _, err := manager.EnsureServed("c", "m-c"); err == nil {
		t.Fatalf("EnsureServed c: expected a start error, got nil")
	}
	if _, ok := manager.servers["a"]; !ok {
		t.Errorf("a was evicted by a failed start of c — must survive")
	}
	if _, ok := manager.servers["b"]; !ok {
		t.Errorf("b was evicted by a failed start of c — must survive")
	}
	if _, ok := manager.servers["c"]; ok {
		t.Errorf("c failed to start but was registered")
	}
	if len(manager.servers) != 2 {
		t.Errorf("running = %d, want 2 (both healthy servers intact)", len(manager.servers))
	}
	if runner.StopCount() != 0 {
		t.Errorf("StopCount = %d, want 0 (a failed start must evict nothing)", runner.StopCount())
	}
}

func TestDeadServerRestarts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &FakeRunner{}
	clock := &stepClock{}
	// Stateful probe: the pre-registered server is dead (first probe false), then the
	// re-started process becomes healthy. This drives the reap-then-restart path.
	calls := 0
	manager := NewManager(Config{
		Runner:   runner,
		Probe:    func(int) bool { calls++; return calls > 1 },
		Now:      clock.now,
		Sleep:    clock.sleep,
		BasePort: 8101,
	})

	// Manually register a "running" server that is now dead.
	manager.servers["a"] = &server{alias: "a", model: "m-a", port: 8101, handle: ServerHandle{PID: 99}, lastUsed: clock.now()}
	manager.usedPorts[8101] = true

	endpoint, err := manager.EnsureServed("a", "m-a")
	if err != nil {
		t.Fatalf("EnsureServed: %v", err)
	}
	if endpoint != "http://127.0.0.1:8101/v1" {
		t.Fatalf("endpoint = %q, want restart on 8101", endpoint)
	}
	if runner.StopCount() != 1 {
		t.Fatalf("dead server not stopped (StopCount=%d)", runner.StopCount())
	}
	if runner.StartCount() != 1 {
		t.Fatalf("restart not started (StartCount=%d)", runner.StartCount())
	}
}

func TestEnsureServedStartError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &FakeRunner{StartErr: errors.New("boom")}
	clock := &stepClock{}
	manager := NewManager(Config{Runner: runner, Probe: alwaysHealthy, Now: clock.now, Sleep: clock.sleep})

	if _, err := manager.EnsureServed("a", "m-a"); err == nil {
		t.Fatalf("expected start error")
	}
	if len(manager.servers) != 0 {
		t.Fatalf("failed start left a registration")
	}
	// Port must be freed for reuse.
	if len(manager.usedPorts) != 0 {
		t.Fatalf("failed start leaked a port: %v", manager.usedPorts)
	}
}

func TestEnsureServedHealthTimeout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &FakeRunner{}
	clock := &stepClock{}
	manager := NewManager(Config{
		Runner:       runner,
		Probe:        func(int) bool { return false }, // never healthy
		Now:          clock.now,
		Sleep:        clock.sleep,
		StartTimeout: 3 * time.Second,
		PollInterval: time.Second,
	})

	if _, err := manager.EnsureServed("a", "m-a"); err == nil {
		t.Fatalf("expected health timeout error")
	}
	if runner.StopCount() != 1 {
		t.Fatalf("timed-out start not cleaned up (StopCount=%d)", runner.StopCount())
	}
	if len(manager.servers) != 0 {
		t.Fatalf("timed-out start left a registration")
	}
}

func TestStopAndStopAll(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manager, runner, _ := newTestManager(Config{BasePort: 8101, MaxServers: 5})

	for _, alias := range []string{"a", "b"} {
		if _, err := manager.EnsureServed(alias, "m-"+alias); err != nil {
			t.Fatalf("EnsureServed %s: %v", alias, err)
		}
	}

	if err := manager.Stop("a"); err != nil {
		t.Fatalf("Stop a: %v", err)
	}
	if _, ok := manager.servers["a"]; ok {
		t.Fatalf("a not removed")
	}
	if err := manager.Stop("missing"); err != nil {
		t.Fatalf("Stop unknown alias should be a no-op success, got %v", err)
	}

	if err := manager.StopAll(); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if len(manager.servers) != 0 {
		t.Fatalf("StopAll left %d servers", len(manager.servers))
	}
	if len(manager.usedPorts) != 0 {
		t.Fatalf("StopAll leaked ports: %v", manager.usedPorts)
	}
	if runner.StopCount() != 2 { // a, then b via StopAll
		t.Fatalf("StopCount = %d, want 2", runner.StopCount())
	}
}

func TestHealth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manager, _, _ := newTestManager(Config{BasePort: 8101, Probe: healthySet(8101)})

	if manager.Health("a") {
		t.Fatalf("unknown alias reported healthy")
	}
	if _, err := manager.EnsureServed("a", "m-a"); err != nil {
		t.Fatalf("EnsureServed: %v", err)
	}
	if !manager.Health("a") {
		t.Fatalf("running server reported unhealthy")
	}
}

func TestRunningSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manager, _, _ := newTestManager(Config{BasePort: 8101, MaxServers: 5})
	for _, alias := range []string{"c", "a", "b"} {
		if _, err := manager.EnsureServed(alias, "m-"+alias); err != nil {
			t.Fatalf("EnsureServed %s: %v", alias, err)
		}
	}
	statuses := manager.Running()
	if len(statuses) != 3 {
		t.Fatalf("Running len = %d", len(statuses))
	}
	// Sorted by alias.
	if statuses[0].Alias != "a" || statuses[1].Alias != "b" || statuses[2].Alias != "c" {
		t.Fatalf("Running not sorted by alias: %+v", statuses)
	}
	for _, status := range statuses {
		if !status.Healthy {
			t.Fatalf("expected healthy: %+v", status)
		}
		if status.Endpoint != Endpoint(status.Port) {
			t.Fatalf("endpoint mismatch: %+v", status)
		}
	}
}

func TestModelFormat(t *testing.T) {
	if ModelFormat("darwin") != FormatMLX {
		t.Fatalf("darwin should be MLX")
	}
	if ModelFormat("linux") != FormatSafetensors {
		t.Fatalf("linux should be safetensors")
	}
}

func TestInstallGuidancePerGOOS(t *testing.T) {
	mac := InstallGuidance("darwin")
	if len(mac) == 0 {
		t.Fatalf("darwin guidance empty")
	}
	joinedMac := joinLines(mac)
	if !contains(joinedMac, "mlx-community") || !contains(joinedMac, "Sonoma") {
		t.Fatalf("darwin guidance missing MLX/Sonoma: %v", mac)
	}
	lin := InstallGuidance("linux")
	if len(lin) == 0 {
		t.Fatalf("linux guidance empty")
	}
	joinedLin := joinLines(lin)
	if !contains(joinedLin, "CUDA") || !contains(joinedLin, "safetensors") {
		t.Fatalf("linux guidance missing CUDA/safetensors: %v", lin)
	}
}

func TestDetect(t *testing.T) {
	original := lookPath
	t.Cleanup(func() { lookPath = original })

	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if installed, _ := Detect(); installed {
		t.Fatalf("Detect should report missing when binary absent")
	}

	lookPath = func(string) (string, error) { return "/usr/local/bin/vllm", nil }
	installed, kind := Detect()
	if !installed {
		t.Fatalf("Detect should report installed when binary present")
	}
	if kind != ModelFormat(runtime.GOOS) {
		t.Fatalf("Detect kind = %q, want %q", kind, ModelFormat(runtime.GOOS))
	}
}

func TestBringUpSeamsNotWired(t *testing.T) {
	if _, err := (RealRunner{}).Start("a", "m", 8101, "/store"); !errors.Is(err, ErrNotWired) {
		t.Fatalf("RealRunner.Start should be ErrNotWired, got %v", err)
	}
	if err := (RealRunner{}).Stop(ServerHandle{PID: 1}); !errors.Is(err, ErrNotWired) {
		t.Fatalf("RealRunner.Stop should be ErrNotWired, got %v", err)
	}
	if err := Pull("mlx-community/x"); !errors.Is(err, ErrNotWired) {
		t.Fatalf("Pull should be ErrNotWired, got %v", err)
	}
}

func joinLines(lines []string) string {
	out := ""
	for _, line := range lines {
		out += line + "\n"
	}
	return out
}

func contains(haystack, needle string) bool {
	for start := 0; start+len(needle) <= len(haystack); start++ {
		if haystack[start:start+len(needle)] == needle {
			return true
		}
	}
	return false
}
