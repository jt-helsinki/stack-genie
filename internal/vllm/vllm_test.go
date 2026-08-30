package vllm

import (
	"errors"
	"os"
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

// TestMain stubs the portInUse seam to false for the whole package so no test dials a
// real socket (a developer may have a live `vllm serve` on the default port). Tests that
// exercise the adopt/wait path override portInUse themselves and restore it.
func TestMain(m *testing.M) {
	original := portInUse
	portInUse = func(int) bool { return false }
	code := m.Run()
	portInUse = original
	os.Exit(code)
}

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

	_, endpoint, err := manager.EnsureServed("llama", "mlx-community/Llama")
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
	if call.Opts.ToolCallParser != "" {
		t.Fatalf("ToolCallParser = %q, want empty (EnsureServed passes none)", call.Opts.ToolCallParser)
	}
}

// TestEnsureServedWithToolParserPassesThrough proves the tool-call parser reaches the
// Runner.Start call on a fresh launch (the fix for tool_choice="auto" failing against
// curated models with a confirmed vLLM --tool-call-parser).
func TestEnsureServedWithToolParserPassesThrough(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manager, runner, _ := newTestManager(Config{BasePort: 8101})

	_, _, err := manager.EnsureServedWithToolParser("my-qwen", "mlx-community/Qwen3-8B", "hermes")
	if err != nil {
		t.Fatalf("EnsureServedWithToolParser: %v", err)
	}
	if runner.StartCount() != 1 {
		t.Fatalf("StartCount = %d, want 1", runner.StartCount())
	}
	if got := runner.StartCalls[0].Opts.ToolCallParser; got != "hermes" {
		t.Fatalf("ToolCallParser = %q, want hermes", got)
	}
}

// TestEnsureServedWithOptionsPassesResourceKnobsThrough proves GPUMemoryUtilization
// and MaxModelLen reach the Runner.Start call on a fresh launch.
func TestEnsureServedWithOptionsPassesResourceKnobsThrough(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manager, runner, _ := newTestManager(Config{BasePort: 8101})

	_, _, err := manager.EnsureServedWithOptions("my-qwen", "mlx-community/Qwen3-8B", ServeOptions{
		ToolCallParser:       "hermes",
		GPUMemoryUtilization: 0.5,
		MaxModelLen:          8192,
	})
	if err != nil {
		t.Fatalf("EnsureServedWithOptions: %v", err)
	}
	if runner.StartCount() != 1 {
		t.Fatalf("StartCount = %d, want 1", runner.StartCount())
	}
	got := runner.StartCalls[0].Opts
	if got.ToolCallParser != "hermes" || got.GPUMemoryUtilization != 0.5 || got.MaxModelLen != 8192 {
		t.Fatalf("Opts = %+v, want {hermes 0.5 8192}", got)
	}
}

func TestEnsureServedReusesAndTouchesLRU(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manager, runner, _ := newTestManager(Config{BasePort: 8101})

	if _, _, err := manager.EnsureServed("a", "m-a"); err != nil {
		t.Fatalf("first: %v", err)
	}
	firstLastUsed := manager.servers["a"].lastUsed

	_, endpoint, err := manager.EnsureServed("a", "m-a")
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
		if _, _, err := manager.EnsureServed(alias, "m-"+alias); err != nil {
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
		if _, _, err := manager.EnsureServed(alias, "m-"+alias); err != nil {
			t.Fatalf("EnsureServed %s: %v", alias, err)
		}
	}
	if _, _, err := manager.EnsureServed("a", "m-a"); err != nil { // touch a → b is LRU
		t.Fatalf("touch a: %v", err)
	}

	if _, _, err := manager.EnsureServed("c", "m-c"); err != nil {
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
		if _, _, err := manager.EnsureServed(alias, "m-"+alias); err != nil {
			t.Fatalf("EnsureServed %s: %v", alias, err)
		}
	}
	// c's start fails: it must error, and NEITHER a nor b may be evicted.
	if _, _, err := manager.EnsureServed("c", "m-c"); err == nil {
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

	_, endpoint, err := manager.EnsureServed("a", "m-a")
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

// A prior invocation's server already owns the reserved port: EnsureServed must ADOPT
// it (no new process) when it is healthy, and WAIT on it (no new process) when it is
// still loading — never start a duplicate that would hit EADDRINUSE.
func TestEnsureServedAdoptsPortInUse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	original := portInUse
	portInUse = func(int) bool { return true } // a prior process holds the port
	t.Cleanup(func() { portInUse = original })

	// Healthy prior server → adopt, do not start.
	adopt, runner, _ := newTestManager(Config{BasePort: 8101})
	if _, endpoint, err := adopt.EnsureServed("a", "m-a"); err != nil || endpoint != "http://127.0.0.1:8101/v1" {
		t.Fatalf("adopt EnsureServed = %q, %v", endpoint, err)
	}
	if runner.StartCount() != 0 {
		t.Fatalf("must NOT start a duplicate when the port is in use + healthy (StartCount=%d)", runner.StartCount())
	}

	// Occupied but not yet healthy (loading), then becomes healthy → wait, do not start.
	calls := 0
	clock := &stepClock{}
	waiting := NewManager(Config{
		Runner:   &FakeRunner{},
		Probe:    func(int) bool { calls++; return calls > 2 }, // unhealthy at first, then healthy
		Now:      clock.now,
		Sleep:    clock.sleep,
		BasePort: 8101,
	})
	if _, _, err := waiting.EnsureServed("b", "m-b"); err != nil {
		t.Fatalf("wait EnsureServed: %v", err)
	}
	// FakeRunner in `waiting` was never asked to start (it waited on the existing server).
}

func TestEnsureServedStartError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &FakeRunner{StartErr: errors.New("boom")}
	clock := &stepClock{}
	manager := NewManager(Config{Runner: runner, Probe: alwaysHealthy, Now: clock.now, Sleep: clock.sleep})

	if _, _, err := manager.EnsureServed("a", "m-a"); err == nil {
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

	if _, _, err := manager.EnsureServed("a", "m-a"); err == nil {
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
		if _, _, err := manager.EnsureServed(alias, "m-"+alias); err != nil {
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
	if _, _, err := manager.EnsureServed("a", "m-a"); err != nil {
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
		if _, _, err := manager.EnsureServed(alias, "m-"+alias); err != nil {
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

// TestVLLMServeArgs pins the `vllm serve` argv contract: it serves the model under the
// ALIAS (--served-model-name), binds the loopback port, and points the download dir at the
// platform store. Routing on the alias is what makes the OpenAI endpoint answer to
// model=<alias>. The real exec (RealRunner.Start forking the process) is a `hardware
// bring-up` path and is NOT exercised here.
func TestVLLMServeArgs(t *testing.T) {
	args := vllmServeArgs("my-qwen", "mlx-community/Qwen3-8B", 8101, "/store/vllm", ServeOptions{})
	want := []string{
		"serve", "mlx-community/Qwen3-8B",
		"--host", "127.0.0.1",
		"--port", "8101",
		"--served-model-name", "my-qwen",
		"--download-dir", "/store/vllm",
	}
	if len(args) != len(want) {
		t.Fatalf("vllmServeArgs = %v, want %v", args, want)
	}
	for index := range want {
		if args[index] != want[index] {
			t.Fatalf("vllmServeArgs[%d] = %q, want %q (full %v)", index, args[index], want[index], args)
		}
	}
}

// TestVLLMServeArgsToolCallParser proves a non-empty toolCallParser appends
// --enable-auto-tool-choice --tool-call-parser <value> — the fix for LiteLLM's
// "auto tool choice requires --enable-auto-tool-choice and --tool-call-parser to be
// set" error on every agentic tool_choice="auto" request to a curated model with a
// confirmed parser (hf.CuratedModel.ToolCallParser).
func TestVLLMServeArgsToolCallParser(t *testing.T) {
	args := vllmServeArgs("my-qwen", "mlx-community/Qwen3-8B", 8101, "/store/vllm", ServeOptions{ToolCallParser: "hermes"})
	want := []string{
		"serve", "mlx-community/Qwen3-8B",
		"--host", "127.0.0.1",
		"--port", "8101",
		"--served-model-name", "my-qwen",
		"--download-dir", "/store/vllm",
		"--enable-auto-tool-choice", "--tool-call-parser", "hermes",
	}
	if len(args) != len(want) {
		t.Fatalf("vllmServeArgs = %v, want %v", args, want)
	}
	for index := range want {
		if args[index] != want[index] {
			t.Fatalf("vllmServeArgs[%d] = %q, want %q (full %v)", index, args[index], want[index], args)
		}
	}
}

// TestVLLMServeArgsResourceKnobs proves GPUMemoryUtilization/MaxModelLen append
// --gpu-memory-utilization/--max-model-len — the fix for a small model resident at
// tens of GB because vLLM's own default (--gpu-memory-utilization 0.9) pre-allocates
// the KV-cache off TOTAL device memory, not the model's weight size. Zero values
// (the default ServeOptions) must NOT append either flag — see TestVLLMServeArgs.
func TestVLLMServeArgsResourceKnobs(t *testing.T) {
	args := vllmServeArgs("my-qwen", "mlx-community/Qwen3-8B", 8101, "/store/vllm", ServeOptions{
		GPUMemoryUtilization: 0.5,
		MaxModelLen:          8192,
	})
	want := []string{
		"serve", "mlx-community/Qwen3-8B",
		"--host", "127.0.0.1",
		"--port", "8101",
		"--served-model-name", "my-qwen",
		"--download-dir", "/store/vllm",
		"--gpu-memory-utilization", "0.5",
		"--max-model-len", "8192",
	}
	if len(args) != len(want) {
		t.Fatalf("vllmServeArgs = %v, want %v", args, want)
	}
	for index := range want {
		if args[index] != want[index] {
			t.Fatalf("vllmServeArgs[%d] = %q, want %q (full %v)", index, args[index], want[index], args)
		}
	}
}

// TestPullEnsuresStore proves Pull is wired (no ErrNotWired): it creates the store dir and
// returns nil — weights are fetched lazily by `vllm serve` on first launch, not here.
func TestPullEnsuresStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := Pull("mlx-community/x"); err != nil {
		t.Fatalf("Pull should be wired (nil), got %v", err)
	}
	store := filepath.Join(home, ".ai-platform", "volumes", "models", "vllm")
	if info, err := os.Stat(store); err != nil || !info.IsDir() {
		t.Fatalf("Pull should create the store dir %s (err=%v)", store, err)
	}
}

// TestReservedPortReusedForKnownAlias proves a Reserved-seeded alias REUSES its recorded
// port across invocations (a fresh Manager has no memory otherwise), so a model's port is
// stable.
func TestReservedPortReusedForKnownAlias(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manager, _, _ := newTestManager(Config{BasePort: 8101, Reserved: map[string]int{"known": 8109}})
	port, endpoint, err := manager.EnsureServed("known", "m-known")
	if err != nil {
		t.Fatalf("EnsureServed: %v", err)
	}
	if port != 8109 || endpoint != "http://127.0.0.1:8109/v1" {
		t.Fatalf("reserved alias got port %d / %q, want 8109", port, endpoint)
	}
}

// TestAllocateSkipsReservedPort proves allocatePort never hands a NEW alias a port that is
// reserved for a DIFFERENT (not-yet-started) alias.
func TestAllocateSkipsReservedPort(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// base port 8101 is reserved for "other"; a fresh alias must skip it.
	manager, _, _ := newTestManager(Config{BasePort: 8101, Reserved: map[string]int{"other": 8101}})
	port, _, err := manager.EnsureServed("fresh", "m-fresh")
	if err != nil {
		t.Fatalf("EnsureServed: %v", err)
	}
	if port == 8101 {
		t.Fatalf("fresh alias got the reserved port 8101; must skip it")
	}
	if port != 8102 {
		t.Fatalf("fresh alias port = %d, want 8102 (lowest free skipping the reserved 8101)", port)
	}
}

// TestSeedReservedMerges proves SeedReserved feeds the same alias→port seed post-construction.
func TestSeedReservedMerges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manager, _, _ := newTestManager(Config{BasePort: 8101})
	manager.SeedReserved(map[string]int{"seeded": 8120})
	port, _, err := manager.EnsureServed("seeded", "m-seeded")
	if err != nil {
		t.Fatalf("EnsureServed: %v", err)
	}
	if port != 8120 {
		t.Fatalf("seeded alias port = %d, want 8120", port)
	}
}

// TestPortOf parses the port from both endpoint forms and rejects portless/garbage input.
func TestPortOf(t *testing.T) {
	cases := []struct {
		endpoint string
		want     int
		ok       bool
	}{
		{"http://127.0.0.1:8101/v1", 8101, true},
		{"http://host.docker.internal:8102/v1", 8102, true},
		{"http://127.0.0.1/v1", 0, false},
		{"", 0, false},
		{"::::", 0, false},
	}
	for _, testCase := range cases {
		port, ok := PortOf(testCase.endpoint)
		if ok != testCase.ok || port != testCase.want {
			t.Errorf("PortOf(%q) = (%d, %v), want (%d, %v)", testCase.endpoint, port, ok, testCase.want, testCase.ok)
		}
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
