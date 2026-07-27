// Package vllm is the host-side manager for per-model vLLM server processes — the
// native-vLLM model backend that sits alongside Ollama.
//
// vLLM serves ONE model per process: the platform runs one
// `vllm serve <model> --port <p>` per served vLLM model, each an OpenAI-compatible
// endpoint at http://127.0.0.1:<p>/v1 on the host loopback. LiteLLM (a container)
// reaches it as http://host.docker.internal:<p>/v1 (the containers already get
// `--add-host=host.docker.internal:host-gateway`).
//
// The served model FORMAT is platform-dependent — vLLM does NOT use GGUF and there
// is NO shared store with Ollama:
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
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/paths"
)

const (
	// DefaultBasePort is the first host port the allocator hands out. vLLM servers
	// occupy DefaultBasePort, DefaultBasePort+1, … (well clear of the platform's
	// nginx gateway on :18787 and Ollama's :11434).
	DefaultBasePort = 8101

	// DefaultMaxServers caps how many vLLM processes run at once. vLLM loads a whole
	// model into (V)RAM, so the cap keeps the host from being oversubscribed; a
	// request for an (N+1)-th model evicts the least-recently-used server first.
	DefaultMaxServers = 2

	// DefaultStartTimeout bounds how long EnsureServed waits for a freshly-started
	// server to answer its health probe before giving up.
	DefaultStartTimeout = 90 * time.Second

	// DefaultPollInterval is the gap between health probes while waiting for start.
	DefaultPollInterval = time.Second

	// storeSubdir is the vLLM model store, a sibling of the Ollama store under
	// VolumesDir. The two backends never share weights.
	storeSubdir = "vllm"
)

// StoreDir is ~/.ai-platform/volumes/models/vllm — the host store for downloaded
// vLLM model weights (MLX on darwin, safetensors on Linux). It is created-on-use
// (MkdirAll), mirroring the Ollama model store, so `ai uninstall --purge`
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

// Runner starts and stops detached `vllm serve` processes. The real implementation
// (runner.go) is a `hardware bring-up` seam; tests use FakeRunner. Start returns a
// handle the Manager later passes to Stop.
type Runner interface {
	// Start launches a detached `vllm serve <model>` bound to 127.0.0.1:<port>,
	// serving it under <alias> as the OpenAI model name, with weights under storeDir.
	Start(alias, model string, port int, storeDir string) (ServerHandle, error)
	// Stop terminates the process identified by handle.
	Stop(handle ServerHandle) error
}

// HealthProbe reports whether a vLLM OpenAI server on the given host port answers
// (GET http://127.0.0.1:<port>/v1/models → 200). The real probe is a short-timeout
// HTTP GET (see RealHealthProbe); tests inject a fake.
type HealthProbe func(port int) bool

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
	basePort     int
	maxServers   int
	startTimeout time.Duration
	pollInterval time.Duration
	servers      map[string]*server // keyed by alias
	usedPorts    map[int]bool
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
		basePort:     cfg.BasePort,
		maxServers:   cfg.MaxServers,
		startTimeout: cfg.StartTimeout,
		pollInterval: cfg.PollInterval,
		servers:      make(map[string]*server),
		usedPorts:    make(map[int]bool),
	}
	if manager.now == nil {
		manager.now = time.Now
	}
	if manager.sleep == nil {
		manager.sleep = time.Sleep
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

// EnsureServed guarantees a healthy vLLM server for alias and returns its host
// endpoint. If one is already running and healthy it touches the LRU and returns;
// if it is registered but dead it is stopped and lazily re-started (auto-restart on
// next use — see the package/Health docs). Otherwise it allocates a port, starts
// the process, and waits until healthy (up to StartTimeout).
//
// If starting would exceed MaxServers the least-recently-used server is EVICTED
// (stopped) first; the eviction is reported via the Log hook, never silently
// dropped. Returns an error if the process fails to start or never becomes healthy;
// in that case the just-started process is stopped and its port freed.
func (manager *Manager) EnsureServed(alias, model string) (string, error) {
	if alias == "" {
		return "", fmt.Errorf("vllm: EnsureServed requires a non-empty alias")
	}
	if model == "" {
		return "", fmt.Errorf("vllm: EnsureServed requires a non-empty model")
	}

	storeDir, err := StoreDir()
	if err != nil {
		return "", err
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()

	if existing := manager.servers[alias]; existing != nil {
		if manager.probe(existing.port) {
			existing.lastUsed = manager.now()
			return Endpoint(existing.port), nil
		}
		// Registered but unhealthy: reap it, then fall through to a fresh start
		// (lazy auto-restart on next use).
		manager.logf("vllm: server %q on port %d is unhealthy; restarting", alias, existing.port)
		_ = manager.runner.Stop(existing.handle)
		manager.remove(existing)
	}

	manager.evictToFit()

	port := manager.allocatePort()
	handle, err := manager.runner.Start(alias, model, port, storeDir)
	if err != nil {
		delete(manager.usedPorts, port)
		return "", fmt.Errorf("vllm: start %q (%s) on port %d: %w", alias, model, port, err)
	}

	if err := manager.waitHealthy(port); err != nil {
		_ = manager.runner.Stop(handle)
		delete(manager.usedPorts, port)
		return "", err
	}

	manager.servers[alias] = &server{
		alias:    alias,
		model:    model,
		port:     port,
		handle:   handle,
		lastUsed: manager.now(),
	}
	return Endpoint(port), nil
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

// evictToFit stops least-recently-used servers until there is room for one more
// under MaxServers. Caller MUST hold manager.mu.
func (manager *Manager) evictToFit() {
	for len(manager.servers) >= manager.maxServers {
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

// allocatePort returns the lowest free port at/above basePort and marks it used.
// Caller MUST hold manager.mu.
func (manager *Manager) allocatePort() int {
	port := manager.basePort
	for manager.usedPorts[port] {
		port++
	}
	manager.usedPorts[port] = true
	return port
}

// remove drops a server from the table and frees its port. Caller MUST hold
// manager.mu.
func (manager *Manager) remove(target *server) {
	delete(manager.servers, target.alias)
	delete(manager.usedPorts, target.port)
}

// waitHealthy polls the probe until the port is healthy or StartTimeout elapses.
// Caller MUST hold manager.mu (starts are serialized). The clock/sleep seams make
// this deterministic under test.
func (manager *Manager) waitHealthy(port int) error {
	deadline := manager.now().Add(manager.startTimeout)
	for {
		if manager.probe(port) {
			return nil
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
