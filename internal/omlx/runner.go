package omlx

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Runner starts/stops the single omlx server process. RealRunner is the production
// implementation (a detached `omlx serve` process); FakeRunner (below) is the
// in-memory test double the Manager's tests inject instead.
type Runner interface {
	// Start launches the omlx server serving modelDir on port, returning immediately
	// (the Manager's health probe confirms readiness).
	Start(modelDir string, port int) (ServerHandle, error)
	// Stop stops the running server, best-effort (not-running is not an error).
	Stop() error
}

// ServerHandle is the opaque identity of the running omlx process. The Runner owns
// its real meaning (a PID); the Manager only stores it.
type ServerHandle struct {
	PID int
}

// RealRunner is the production Runner: it shells out to a DETACHED `omlx serve
// --model-dir <dir> --port <port>` process. It is still gated UPSTREAM by
// omlx.Detect (an `omlx` binary on PATH) — a host without omlx never reaches
// Start — so this file assumes the binary exists and simply reports an exec error
// if it does not.
type RealRunner struct{}

// Start launches a DETACHED `omlx serve --host 0.0.0.0 --model-dir <dir> --port
// <port> --paged-ssd-cache-dir <dir>` that OUTLIVES this short-lived CLI: it is
// put in its own process group (Setpgid) so it is not torn down with the `ai`
// process, and stdout+stderr are redirected to LogPath() — a SIBLING of the
// model directory (not inside it, so it can never be mistaken for a model
// subdirectory by omlx's own discovery scan). --host 0.0.0.0 overrides omlx's
// own default bind (127.0.0.1 loopback-only): the LiteLLM CONTAINER reaches
// omlx at host.docker.internal:<port>, and on Docker Desktop for Mac that
// route does NOT arrive as literal loopback traffic — a server bound only to
// 127.0.0.1 refuses it outright ("Connection error" from LiteLLM), even though
// the SAME server answers fine to a request from the host's own loopback (e.g.
// omlx's own admin console). Binding all interfaces is still host-only exposure
// (no port is published to a network beyond this host). The process env
// carries OMLX_BASE_PATH pointing at BasePathDir (~/.ai-platform/omlx), so
// every file omlx itself writes (settings.json, its own logs, …) lands under
// the platform's own directory tree instead of the user's home ~/.omlx — model
// DATA stays separate at modelDir (StoreDir) regardless. Start returns
// immediately (Release, no Wait) with a ServerHandle wrapping the PID; the
// Manager's health probe confirms readiness.
func (RealRunner) Start(modelDir string, port int) (ServerHandle, error) {
	logPath := LogPath(modelDir)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return ServerHandle{}, fmt.Errorf("omlx: open log %s: %w", logPath, err)
	}
	defer func() { _ = logFile.Close() }()

	basePath, err := BasePathDir()
	if err != nil {
		return ServerHandle{}, fmt.Errorf("omlx: resolve base path: %w", err)
	}
	pagedSSDCacheDir, err := PagedSSDCacheDir()
	if err != nil {
		return ServerHandle{}, fmt.Errorf("omlx: resolve paged SSD cache dir: %w", err)
	}

	command := exec.Command(BinaryPath(), "serve",
		"--host", "0.0.0.0", "--model-dir", modelDir, "--port", strconv.Itoa(port),
		"--paged-ssd-cache-dir", pagedSSDCacheDir,
	)
	command.Env = append(os.Environ(), "OMLX_BASE_PATH="+basePath)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = logFile
	command.Stderr = logFile
	if startErr := command.Start(); startErr != nil {
		return ServerHandle{}, fmt.Errorf("omlx: start server on port %d: %w", port, startErr)
	}
	pid := command.Process.Pid
	// Detach: release the OS resources without reaping, so the server keeps running
	// after this CLI exits. It is now owned by its own process group.
	_ = command.Process.Release()
	return ServerHandle{PID: pid}, nil
}

// stopGracePeriod bounds how long Stop waits for the process to actually exit
// after a graceful pkill before escalating to SIGKILL.
const stopGracePeriod = 3 * time.Second

// Stop stops the running omlx server, discovered by matching its command line
// (there is exactly ONE such process, so an unqualified pkill is unambiguous —
// unlike the old per-model vLLM design, which needed a port-specific pattern to
// avoid killing sibling servers). omlx renames its own process title to
// "omlx-server" at startup (its `serve` subcommand calls process_title.
// set_process_title, backed by the `setproctitle` package it always depends on)
// — after that rename the original "<path>/omlx serve ..." command line no
// longer appears in `ps`/`pgrep`, so the renamed title is matched too (tried
// first, since it is the steady-state case; the original pattern still covers
// the narrow pre-rename startup window). Best-effort: pkill exits non-zero when
// nothing matched, which is not an error here.
//
// After signalling, Stop WAITS (polling the port, up to stopGracePeriod) for the
// process to actually exit before returning — pkill only SENDS the signal, it
// does not wait for the target to die, and applyHostNativeAction's restart calls
// Start() immediately after Stop() returns: without this wait, a still-shutting-
// down old process can still answer the health probe just long enough to be
// wrongly "adopted" again instead of replaced. A process that ignores the
// graceful term is escalated to SIGKILL after the grace period.
func (RealRunner) Stop() error {
	_ = exec.Command("pkill", "-f", "omlx-server").Run()
	_ = exec.Command("pkill", "-f", "omlx serve").Run()
	deadline := time.Now().Add(stopGracePeriod)
	for portInUse() {
		if time.Now().After(deadline) {
			_ = exec.Command("pkill", "-9", "-f", "omlx-server").Run()
			_ = exec.Command("pkill", "-9", "-f", "omlx serve").Run()
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	return nil
}

// FakeRunner is an in-memory Runner for tests. It records every Start/Stop, hands
// out incrementing PIDs, and can be made to fail via the *Err fields. It never
// touches a real process. Safe for concurrent use.
type FakeRunner struct {
	mu sync.Mutex

	nextPID int

	// StartErr, when non-nil, fails every Start.
	StartErr error
	// StopErr, when non-nil, fails every Stop.
	StopErr error

	// StartCalls / StopCalls record invocations in order.
	StartCalls []FakeStartCall
	StopCalls  int
}

// FakeStartCall captures the arguments of one Runner.Start invocation.
type FakeStartCall struct {
	ModelDir string
	Port     int
	Handle   ServerHandle
}

// Start records the call, returns a fresh handle, and honours StartErr.
func (fake *FakeRunner) Start(modelDir string, port int) (ServerHandle, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.StartErr != nil {
		return ServerHandle{}, fake.StartErr
	}
	fake.nextPID++
	handle := ServerHandle{PID: fake.nextPID}
	fake.StartCalls = append(fake.StartCalls, FakeStartCall{ModelDir: modelDir, Port: port, Handle: handle})
	return handle, nil
}

// Stop records the call and honours StopErr.
func (fake *FakeRunner) Stop() error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.StopCalls++
	return fake.StopErr
}

// StartCount returns how many Start calls were recorded.
func (fake *FakeRunner) StartCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.StartCalls)
}
