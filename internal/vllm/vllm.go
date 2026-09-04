// Package vllm is the host-side manager for per-model vLLM server processes — the
// native-vLLM model backend and sole local-inference runtime.
//
// vLLM serves ONE model per process: the platform runs one
// `vllm serve <model> --port <p>` per served vLLM model, each an OpenAI-compatible
// endpoint at http://127.0.0.1:<p>/v1 on the host loopback. LiteLLM (a container)
// reaches it as http://host.docker.internal:<p>/v1 (the containers already get
// `--add-host=host.docker.internal:host-gateway`).
//
// The served model FORMAT is platform-dependent — vLLM does NOT use GGUF:
//   - Apple Silicon (darwin): MLX weights (mlx-community/*), via the vLLM-Metal plugin.
//   - Linux: Hugging Face safetensors, on CUDA.
//
// The Manager owns lazy start, a max-concurrent cap with LRU eviction, and health
// checks, all over injectable seams (Runner, HealthProbe, a clock) so it is fully
// unit-testable with fakes — no real process or network is touched in tests. The
// ACTUAL `vllm serve` invocation, the model download, and host install/detection
// are `hardware bring-up`: they live behind the Runner seam / in detect.go /
// runner.go and are documented but not wired (they must mutate the host, which the
// platform does not do until validated on a provisioned host). Round B wires this
// package into internal/setup + internal/cli.
package vllm

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/paths"
)

const (
	// DefaultBasePort is the first host port the allocator hands out. vLLM servers
	// occupy DefaultBasePort, DefaultBasePort+1, … (well clear of the platform's
	// nginx gateway on :18787).
	DefaultBasePort = 8101

	// DefaultMaxServers caps how many vLLM processes run at once. vLLM loads a whole
	// model into (V)RAM, so the cap keeps the host from being oversubscribed; a
	// request for an (N+1)-th model evicts the least-recently-used server first.
	DefaultMaxServers = 2

	// DefaultStartTimeout bounds how long EnsureServed waits for a freshly-started
	// server to answer its health probe before giving up. It must be generous: a large
	// MLX/safetensors model (e.g. a 31B, ~18 GB at 4-bit) can take several MINUTES to
	// load into (unified) memory and initialize before it serves — 90s wrongly reported
	// such a model as "failed to install" while it was still warming up.
	DefaultStartTimeout = 15 * time.Minute

	// DefaultPollInterval is the gap between health probes while waiting for start.
	DefaultPollInterval = time.Second

	// storeSubdir is the vLLM model store under VolumesDir.
	storeSubdir = "vllm"
)

// StoreDir is ~/.ai-platform/volumes/models/vllm — the host store for downloaded
// vLLM model weights (MLX on darwin, safetensors on Linux). It is created-on-use
// (MkdirAll), so `ai uninstall --purge`
// (RemoveAll ~/.ai-platform) removes it.
func StoreDir() (string, error) {
	volumesDir, err := paths.VolumesDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(volumesDir, "models", storeSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("vllm: create store dir %s: %w", dir, err)
	}
	return dir, nil
}

// ServerHandle is the opaque identity of a running vLLM process. The Runner owns
// its real meaning (a PID / os.Process wrapper); the Manager only stores it and
// hands it back to Runner.Stop.
type ServerHandle struct {
	PID int
}

// ServeOptions carries the optional `vllm serve` resource/tool-calling knobs a
// caller can pin for a model. The zero value means "use vLLM's own defaults" for
// every field — a caller never has to guess a value it isn't confident about.
type ServeOptions struct {
	// ToolCallParser, when non-empty, enables auto tool choice with that parser
	// (see vllmServeArgs) — the model's CONFIRMED vLLM --tool-call-parser value
	// (hf.CuratedModel.ToolCallParser).
	ToolCallParser string
	// GPUMemoryUtilization, when > 0, is passed as `--gpu-memory-utilization
	// <value>` — the fraction of device memory vLLM may pre-allocate for the
	// KV-cache (0–1). Zero leaves vLLM's own default (0.9) in effect, which
	// pre-allocates a large KV-cache pool sized off TOTAL device memory rather
	// than the model's weight size — the usual reason a small model still shows
	// tens of GB of resident memory.
	GPUMemoryUtilization float64
	// MaxModelLen, when > 0, is passed as `--max-model-len <value>` — caps the
	// context window (tokens) the KV-cache is sized for. Zero leaves the model's
	// own (often very large) default context length in effect.
	MaxModelLen int
	// ReasoningParser, when non-empty, is passed as `--reasoning-parser <value>` —
	// the model's CONFIRMED vLLM reasoning-parser name (hf.CuratedModel.
	// ReasoningParser), e.g. "qwen3" for the Qwen3 family, which ships with
	// thinking enabled by default. Without it, vLLM has no way to split a
	// reasoning model's <think>...</think> block out of the response into
	// reasoning_content — the raw tags land in the visible answer verbatim
	// instead, which is what a client (opencode) then renders as the whole
	// "response". Left unset for a model with no confirmed parser, matching
	// ToolCallParser's philosophy (a WRONG parser can silently corrupt output).
	ReasoningParser string
}

// Runner starts and stops detached `vllm serve` processes. The real implementation
// (runner.go) is a `hardware bring-up` seam; tests use FakeRunner. Start returns a
// handle the Manager later passes to Stop.
type Runner interface {
	// Start launches a detached `vllm serve <model>` bound to 127.0.0.1:<port>,
	// serving it under <alias> as the OpenAI model name, with weights under
	// storeDir, applying the optional opts (see ServeOptions).
	Start(alias, model string, port int, storeDir string, opts ServeOptions) (ServerHandle, error)
	// Stop terminates the process identified by handle.
	Stop(handle ServerHandle) error
}

// HealthProbe reports whether a vLLM OpenAI server on the given host port answers
// (GET http://127.0.0.1:<port>/v1/models → 200). The real probe is a short-timeout
// HTTP GET (see RealHealthProbe); tests inject a fake.
type HealthProbe func(port int) bool

// portInUse reports whether ANYTHING is listening on the loopback port — used to tell a
// prior `vllm serve` that is bound but NOT yet health-serving (still loading weights)
// apart from a free port, so EnsureServed waits on it instead of starting a duplicate
// that would fail with EADDRINUSE. Injectable seam so tests never touch a real socket.
var portInUse = func(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ServerStatus is a read-only snapshot of one running server, returned by Running.
type ServerStatus struct {
	Alias    string    `json:"alias"`
	Model    string    `json:"model"`
	Port     int       `json:"port"`
	Endpoint string    `json:"endpoint"`
	Healthy  bool      `json:"healthy"`
	LastUsed time.Time `json:"last_used"`
}

// server is the Manager's internal record of one running process.
type server struct {
	alias    string
	model    string
	port     int
	handle   ServerHandle
	lastUsed time.Time
}

// Config configures a Manager. Zero-value fields fall back to sensible defaults, so
// callers set only what they need to override (tests inject Runner/Probe/Now/Sleep).
type Config struct {
	// Runner starts/stops processes. REQUIRED — NewManager panics on a nil Runner
	// (there is no meaningful default without host mutation).
	Runner Runner
	// Probe health-checks a server port. REQUIRED for the same reason.
	Probe HealthProbe
	// Now returns the current time; defaults to time.Now. Injectable for tests.
	Now func() time.Time
	// Sleep pauses between health polls; defaults to time.Sleep. Injectable so tests
	// advance a mock clock instead of really sleeping.
	Sleep func(time.Duration)
	// Log receives human-readable lifecycle notes (notably LRU evictions) so they are
	// never silently dropped. Optional; nil discards them.
	Log func(string)
	// ProcessAlive reports whether pid is still running. Defaults to a real
	// (unix signal-0) check. Injectable so waitHealthy's fast-fail-on-death path is
	// deterministic under test without spawning a real process.
	ProcessAlive func(pid int) bool

	// Reserved seeds cross-invocation port truth (alias→loopback port) from the persisted
	// model-runtimes store: `ai` is a daemonless CLI, so a fresh Manager has no memory of
	// the ports earlier invocations recorded. A KNOWN alias REUSES its reserved port on
	// EnsureServed, and allocatePort SKIPS every reserved port so a NEW server never steals
	// a recorded model's port. Optional; nil means no seed. See also SeedReserved.
	Reserved map[string]int

	BasePort     int
	MaxServers   int
	StartTimeout time.Duration
	PollInterval time.Duration
}

// Manager tracks the running vLLM servers and owns lazy start, the max-concurrent
// cap with LRU eviction, and health checks. It is safe for concurrent use.
type Manager struct {
	mu           sync.Mutex
	runner       Runner
	probe        HealthProbe
	now          func() time.Time
	sleep        func(time.Duration)
	logFn        func(string)
	processAlive func(pid int) bool
	basePort     int
	maxServers   int
	startTimeout time.Duration
	pollInterval time.Duration
	servers      map[string]*server // keyed by alias
	usedPorts    map[int]bool
	// reserved is the cross-invocation alias→port seed (see Config.Reserved / SeedReserved):
	// a known alias reuses reserved[alias]; allocatePort skips every reserved port.
	reserved map[string]int
	// pending marks aliases whose server is mid-start (the lock is released during the
	// process launch + health wait), so a concurrent EnsureServed for the SAME alias
	// is rejected rather than starting a duplicate process.
	pending map[string]bool
}

// NewManager builds a Manager from cfg. It panics if Runner or Probe is nil — those
// are the seams that make the package do anything, and a nil one is a programmer
// error, not a runtime condition.
func NewManager(cfg Config) *Manager {
	if cfg.Runner == nil {
		panic("vllm: NewManager requires a non-nil Runner")
	}
	if cfg.Probe == nil {
		panic("vllm: NewManager requires a non-nil Probe")
	}
	manager := &Manager{
		runner:       cfg.Runner,
		probe:        cfg.Probe,
		now:          cfg.Now,
		sleep:        cfg.Sleep,
		logFn:        cfg.Log,
		processAlive: cfg.ProcessAlive,
		basePort:     cfg.BasePort,
		maxServers:   cfg.MaxServers,
		startTimeout: cfg.StartTimeout,
		pollInterval: cfg.PollInterval,
		servers:      make(map[string]*server),
		usedPorts:    make(map[int]bool),
		reserved:     make(map[string]int),
		pending:      make(map[string]bool),
	}
	for alias, port := range cfg.Reserved {
		if port > 0 {
			manager.reserved[alias] = port
		}
	}
	if manager.now == nil {
		manager.now = time.Now
	}
	if manager.sleep == nil {
		manager.sleep = time.Sleep
	}
	if manager.processAlive == nil {
		manager.processAlive = defaultProcessAlive
	}
	if manager.basePort <= 0 {
		manager.basePort = DefaultBasePort
	}
	if manager.maxServers <= 0 {
		manager.maxServers = DefaultMaxServers
	}
	if manager.startTimeout <= 0 {
		manager.startTimeout = DefaultStartTimeout
	}
	if manager.pollInterval <= 0 {
		manager.pollInterval = DefaultPollInterval
	}
	return manager
}

// Endpoint is the host-loopback OpenAI base URL for a vLLM server on port.
func Endpoint(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", port)
}

// ContainerEndpoint is the base URL a CONTAINER (LiteLLM) uses to reach a vLLM
// server on the host: the host-gateway name plus the same port.
func ContainerEndpoint(port int) string {
	return fmt.Sprintf("http://host.docker.internal:%d/v1", port)
}

// PortOf extracts the TCP port from a recorded vLLM endpoint URL (either Endpoint's
// loopback form or ContainerEndpoint's host-gateway form). It is how a fresh,
// daemonless CLI invocation recovers a server's port from the persisted
// model-runtimes store — to seed Reserved (port reuse) or to StopByPort on removal.
// Returns false when endpoint is empty, unparseable, or carries no explicit port.
func PortOf(endpoint string) (int, bool) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return 0, false
	}
	portText := parsed.Port()
	if portText == "" {
		return 0, false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 {
		return 0, false
	}
	return port, true
}

// SeedReserved merges an alias→port map into the Manager's cross-invocation port
// seed (see Config.Reserved). Non-positive ports are ignored. Safe for concurrent
// use; callers typically seed once from config.LoadModelRuntimes before EnsureServed.
func (manager *Manager) SeedReserved(reserved map[string]int) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for alias, port := range reserved {
		if port > 0 {
			manager.reserved[alias] = port
		}
	}
}

// EnsureServed guarantees a healthy vLLM server for alias and returns its host
// loopback port and endpoint. If one is already running and healthy it touches the
// LRU and returns; if it is registered but dead it is stopped and lazily re-started
// (auto-restart on next use — see the package/Health docs). Otherwise it allocates a
// port (REUSING the alias's Reserved port when seeded, so it stays stable across
// invocations), starts the process, and waits until healthy (up to StartTimeout).
//
// The returned port is the loopback port; callers derive both endpoint forms from it
// (Endpoint for host-side probes/records, ContainerEndpoint for the LiteLLM api_base).
//
// If starting would exceed MaxServers the least-recently-used server is EVICTED
// (stopped) first; the eviction is reported via the Log hook, never silently
// dropped. Returns an error if the process fails to start or never becomes healthy;
// in that case the just-started process is stopped and its port freed.
//
// EnsureServed starts with the zero ServeOptions (equivalent to
// EnsureServedWithOptions(alias, model, ServeOptions{})) — use that method directly
// when the caller has a curated tool-call parser or wants to pin the resource knobs
// (see ServeOptions).
func (manager *Manager) EnsureServed(alias, model string) (int, string, error) {
	return manager.EnsureServedWithOptions(alias, model, ServeOptions{})
}

// EnsureServedWithToolParser is EnsureServed with an explicit vLLM tool-call parser
// (equivalent to EnsureServedWithOptions(alias, model, ServeOptions{ToolCallParser:
// toolCallParser})). It has no effect on an ADOPTED already-running server (its
// flags were fixed at its own launch) — restart it to change them.
func (manager *Manager) EnsureServedWithToolParser(alias, model, toolCallParser string) (int, string, error) {
	return manager.EnsureServedWithOptions(alias, model, ServeOptions{ToolCallParser: toolCallParser})
}

// EnsureServedWithOptions is EnsureServed with the full set of optional `vllm serve`
// knobs (see ServeOptions) — the tool-call parser AND/OR the GPU-memory-utilization /
// max-model-len resource caps. They have no effect on an ADOPTED already-running
// server (its flags were fixed at its own launch) — restart it to change them.
func (manager *Manager) EnsureServedWithOptions(alias, model string, opts ServeOptions) (int, string, error) {
	if alias == "" {
		return 0, "", fmt.Errorf("vllm: EnsureServed requires a non-empty alias")
	}
	if model == "" {
		return 0, "", fmt.Errorf("vllm: EnsureServed requires a non-empty model")
	}

	storeDir, err := StoreDir()
	if err != nil {
		return 0, "", err
	}

	manager.mu.Lock()

	restarting := false
	if existing := manager.servers[alias]; existing != nil {
		if manager.probe(existing.port) {
			existing.lastUsed = manager.now()
			port := existing.port
			manager.mu.Unlock()
			return port, Endpoint(port), nil
		}
		// Registered but unhealthy: reap it, then fall through to a fresh start
		// (lazy auto-restart on next use). restarting suppresses the cross-invocation
		// adopt/wait below — we just killed this process, so its port is (becoming) free
		// and we must Start a new one, not adopt/wait on the dying one.
		manager.logf("vllm: server %q on port %d is unhealthy; restarting", alias, existing.port)
		_ = manager.runner.Stop(existing.handle)
		manager.remove(existing)
		restarting = true
	}

	// A concurrent EnsureServed for the SAME alias must not start a duplicate process
	// (the lock is released below during the launch + health wait).
	if manager.pending[alias] {
		manager.mu.Unlock()
		return 0, "", fmt.Errorf("vllm: server %q is already starting", alias)
	}
	port := manager.portFor(alias)
	manager.pending[alias] = true
	manager.mu.Unlock()

	// Cross-PROCESS mutual exclusion: `ai` is daemonless, so the in-memory `pending`
	// map above only protects concurrent goroutines within THIS process — a second
	// `ai` invocation (e.g. a double-fired restart) builds its own fresh Manager and
	// sails right past it. Serialize on an OS file lock instead: it queues a second
	// invocation for the SAME alias behind this one rather than letting both race
	// portInUse() and both spawn `vllm serve` on the same port (see lock.go).
	lockFile, lockErr := lockAlias(storeDir, alias)
	if lockErr != nil {
		manager.mu.Lock()
		delete(manager.pending, alias)
		manager.mu.Unlock()
		return 0, "", lockErr
	}
	defer unlockAlias(lockFile)

	// Start + health-check WITHOUT holding manager.mu, so concurrent Running()/Health()/
	// Stop()/other-alias EnsureServed() are not blocked for up to StartTimeout. A healthy
	// LRU server is evicted ONLY AFTER the new one is confirmed healthy (below), so a
	// start that fails or never becomes healthy never costs a working model.
	//
	// DAEMONLESS REUSE: `ai` is a short-lived CLI, so this Manager has NO in-memory
	// handle to a `vllm serve` started by a PRIOR invocation — it only knows the reserved
	// port (seeded from config/model-runtimes.yaml). If that port is already occupied by
	// a prior process, do NOT start a second server on it (that crashes with EADDRINUSE
	// and is misreported as an install failure): ADOPT it when it is already healthy, or
	// WAIT for it when it is still loading a large model. Only start fresh when the port
	// is genuinely free (or we just reaped a dead server — restarting). The lock above
	// guarantees that by the time we get here, any PRIOR invocation's start attempt for
	// this alias has already finished — so this portInUse read is no longer racy.
	var handle ServerHandle
	var startErr, healthErr error
	switch {
	case !restarting && portInUse(port):
		if manager.probe(port) {
			// Already healthy from a prior invocation — adopt it (zero handle:
			// RealRunner.Stop no-ops on PID<=0, and `ai models rm` stops it by port).
			manager.logf("vllm: adopting the already-running %q server on port %d", alias, port)
		} else {
			manager.logf("vllm: port %d is in use; waiting for the existing %q server to finish loading", port, alias)
			// pid=0: this port's process was NOT started by us (a prior CLI
			// invocation's detached launch, or something else entirely) — we have no
			// PID to liveness-check, only the port.
			healthErr = manager.waitHealthy(port, 0, alias, storeDir)
		}
	default:
		handle, startErr = manager.runner.Start(alias, model, port, storeDir, opts)
		if startErr == nil {
			healthErr = manager.waitHealthy(port, handle.PID, alias, storeDir)
		}
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	delete(manager.pending, alias)
	if startErr != nil {
		delete(manager.usedPorts, port)
		return 0, "", fmt.Errorf("vllm: start %q (%s) on port %d: %w", alias, model, port, startErr)
	}
	if healthErr != nil {
		_ = manager.runner.Stop(handle)
		delete(manager.usedPorts, port)
		return 0, "", healthErr
	}

	manager.servers[alias] = &server{
		alias:    alias,
		model:    model,
		port:     port,
		handle:   handle,
		lastUsed: manager.now(),
	}
	// Bring the running set back within the cap now that the new server is healthy. The
	// new server is the most-recently-used, so it is never the eviction victim.
	manager.evictBeyondCap()
	return port, Endpoint(port), nil
}

// Stop stops the server for alias (if any) and frees its port. Stopping an unknown
// alias is a no-op success.
func (manager *Manager) Stop(alias string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	target := manager.servers[alias]
	if target == nil {
		return nil
	}
	err := manager.runner.Stop(target.handle)
	manager.remove(target)
	if err != nil {
		return fmt.Errorf("vllm: stop %q: %w", alias, err)
	}
	return nil
}

// StopAll stops every running server. It attempts them all and returns the first
// error encountered (all handles are still stopped/removed regardless).
func (manager *Manager) StopAll() error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	var firstErr error
	for _, target := range manager.servers {
		if err := manager.runner.Stop(target.handle); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("vllm: stop %q: %w", target.alias, err)
		}
		delete(manager.usedPorts, target.port)
	}
	manager.servers = make(map[string]*server)
	return firstErr
}

// Running returns a snapshot of every registered server, sorted by alias for
// deterministic output. Healthy is re-probed live per entry.
func (manager *Manager) Running() []ServerStatus {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	statuses := make([]ServerStatus, 0, len(manager.servers))
	for _, target := range manager.servers {
		statuses = append(statuses, ServerStatus{
			Alias:    target.alias,
			Model:    target.model,
			Port:     target.port,
			Endpoint: Endpoint(target.port),
			Healthy:  manager.probe(target.port),
			LastUsed: target.lastUsed,
		})
	}
	sort.Slice(statuses, func(left, right int) bool {
		return statuses[left].Alias < statuses[right].Alias
	})
	return statuses
}

// Health reports whether alias is registered AND its server currently answers the
// health probe.
func (manager *Manager) Health(alias string) bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	target := manager.servers[alias]
	if target == nil {
		return false
	}
	return manager.probe(target.port)
}

// evictBeyondCap stops least-recently-used servers until the running set is within
// MaxServers. It runs AFTER a freshly-started server is registered (the new server is
// the most-recently-used, so it is never the victim) — so a start that fails never
// evicts a healthy server. Caller MUST hold manager.mu.
func (manager *Manager) evictBeyondCap() {
	for len(manager.servers) > manager.maxServers {
		victim := manager.leastRecentlyUsed()
		if victim == nil {
			return
		}
		manager.logf("vllm: evicting LRU server %q (model %s, port %d, last used %s) to stay under MaxServers=%d",
			victim.alias, victim.model, victim.port, victim.lastUsed.Format(time.RFC3339), manager.maxServers)
		_ = manager.runner.Stop(victim.handle)
		manager.remove(victim)
	}
}

// leastRecentlyUsed returns the server with the oldest lastUsed, or nil when none
// are running. Caller MUST hold manager.mu.
func (manager *Manager) leastRecentlyUsed() *server {
	var oldest *server
	for _, candidate := range manager.servers {
		if oldest == nil || candidate.lastUsed.Before(oldest.lastUsed) {
			oldest = candidate
		}
	}
	return oldest
}

// portFor picks the port for a (re)starting alias: its Reserved port when seeded (so a
// known model keeps a stable port across invocations), else a freshly allocated one. The
// chosen port is marked used either way. Caller MUST hold manager.mu.
func (manager *Manager) portFor(alias string) int {
	if reservedPort := manager.reserved[alias]; reservedPort > 0 {
		manager.usedPorts[reservedPort] = true
		return reservedPort
	}
	return manager.allocatePort()
}

// allocatePort returns the lowest free port at/above basePort and marks it used. It
// SKIPS ports reserved for OTHER aliases (Config.Reserved / SeedReserved) so a new
// server never steals a recorded model's port. Caller MUST hold manager.mu.
func (manager *Manager) allocatePort() int {
	port := manager.basePort
	for manager.usedPorts[port] || manager.portReserved(port) {
		port++
	}
	manager.usedPorts[port] = true
	return port
}

// portReserved reports whether any alias has reserved the given port. Caller MUST hold
// manager.mu.
func (manager *Manager) portReserved(port int) bool {
	for _, reservedPort := range manager.reserved {
		if reservedPort == port {
			return true
		}
	}
	return false
}

// remove drops a server from the table and frees its port. Caller MUST hold
// manager.mu.
func (manager *Manager) remove(target *server) {
	delete(manager.servers, target.alias)
	delete(manager.usedPorts, target.port)
}

// waitHealthy polls the probe until the port is healthy, the launched process dies,
// or StartTimeout elapses. Caller MUST hold manager.mu (starts are serialized). The
// clock/sleep seams make this deterministic under test. pid is the PID this Manager
// itself launched (0 when waiting on a port some OTHER process holds — e.g. adopted
// from a prior CLI invocation — where liveness can't be checked). A dead pid fails
// FAST with the tail of the model's own log instead of waiting out the full
// StartTimeout (up to 15m) for a process that already exited — the difference
// between "still loading" and "crashed" reads identically as silence otherwise,
// which was reported as the command "hanging" / "seeming to quit".
func (manager *Manager) waitHealthy(port, pid int, alias, storeDir string) error {
	deadline := manager.now().Add(manager.startTimeout)
	for {
		if manager.probe(port) {
			return nil
		}
		if pid > 0 && !manager.processAlive(pid) {
			return fmt.Errorf("vllm: server %q (pid %d) exited before becoming healthy — check its log: %s",
				alias, pid, filepath.Join(storeDir, alias+".log"))
		}
		if !manager.now().Before(deadline) {
			return fmt.Errorf("vllm: server on port %d did not become healthy within %s", port, manager.startTimeout)
		}
		manager.sleep(manager.pollInterval)
	}
}

func (manager *Manager) logf(format string, args ...any) {
	if manager.logFn != nil {
		manager.logFn(fmt.Sprintf(format, args...))
	}
}
