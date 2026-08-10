package vllm

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// ErrNotWired is a legacy sentinel: it USED to be returned by the RealRunner/Pull
// `hardware bring-up` seams before they were wired. Those now spawn/download for real
// (Start execs `vllm serve`, Pull ensures the store), so nothing here returns it —
// but the CLI's actionable-error wrapper still recognises it so a fake surfacing it in
// a test reads as a serving-unavailable condition rather than a bare internal error.
var ErrNotWired = errors.New("vllm: not yet wired (hardware bring-up)")

// RealRunner is the production Runner: it shells out to a DETACHED `vllm serve`
// process, one per served model. The Manager takes a Runner interface, so tests inject
// FakeRunner instead and never touch a real process.
//
// It is still gated UPSTREAM by vllm.Detect (a `vllm` binary on PATH) — a host without
// vLLM never reaches Start — so this file assumes the binary exists and simply reports
// an exec error if it does not.
type RealRunner struct{}

// vllmServeArgs is the pure argv builder for `vllm serve` (everything after the binary
// name), factored out so the exact contract can be unit-tested without spawning a
// process. `--served-model-name <alias>` is the linchpin of the contract: the OpenAI
// endpoint then answers to model=<alias> (NOT the Hugging Face model id), which is what
// LiteLLM routes on. `--download-dir <storeDir>` is a belt-and-braces hint alongside
// the HF_HOME env Start sets, so weights land in / load from the platform store.
func vllmServeArgs(alias, model string, port int, storeDir string) []string {
	return []string{
		"serve", model,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--served-model-name", alias,
		"--download-dir", storeDir,
	}
}

// Start launches a DETACHED `vllm serve <model> --host 127.0.0.1 --port <port>
// --served-model-name <alias>` that OUTLIVES this short-lived CLI: it is put in its own
// process group (Setpgid) so it is not torn down with the `ai` process, HF_HOME points
// at the platform store so weights load from there, and stdout+stderr are redirected to
// <storeDir>/<alias>.log. Start returns immediately (Release, no Wait) with a
// ServerHandle wrapping the PID; the Manager's health probe confirms readiness.
func (RealRunner) Start(alias, model string, port int, storeDir string) (ServerHandle, error) {
	logPath := filepath.Join(storeDir, alias+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return ServerHandle{}, fmt.Errorf("vllm: open log %s: %w", logPath, err)
	}
	defer func() { _ = logFile.Close() }()

	command := exec.Command(BinaryPath(), vllmServeArgs(alias, model, port, storeDir)...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Env = append(os.Environ(), "HF_HOME="+storeDir)
	command.Stdout = logFile
	command.Stderr = logFile
	if startErr := command.Start(); startErr != nil {
		return ServerHandle{}, fmt.Errorf("vllm: start %q (%s) on port %d: %w", alias, model, port, startErr)
	}
	pid := command.Process.Pid
	// Detach: release the OS resources without reaping, so the server keeps running after
	// this CLI exits. It is now owned by its own process group (Stop signals -pid).
	_ = command.Process.Release()
	return ServerHandle{PID: pid}, nil
}

// Stop terminates a server started by Start by signalling its whole process group
// (SIGTERM to -pid), best-effort — a fresh CLI invocation that never started the
// process has no handle and stops it by port instead (StopByPort).
func (RealRunner) Stop(handle ServerHandle) error {
	if handle.PID <= 0 {
		return nil
	}
	_ = syscall.Kill(-handle.PID, syscall.SIGTERM)
	return nil
}

// StopByPort stops the detached `vllm serve` process listening on the given host port,
// discovered by matching its command line — the cross-invocation stop path a fresh,
// daemonless CLI uses on `ai models rm` / uninstall, since it holds no ServerHandle.
// Best-effort: pkill exits non-zero when nothing matched, which is not an error here.
func StopByPort(port int) error {
	if port <= 0 {
		return nil
	}
	pattern := fmt.Sprintf("vllm serve.*--port %d", port)
	_ = exec.Command("pkill", "-f", pattern).Run()
	return nil
}

// RealHealthProbe returns a HealthProbe that GETs http://127.0.0.1:<port>/v1/models
// with a short timeout and reports 200 as healthy. This is a read-only network
// probe (no host mutation).
func RealHealthProbe() HealthProbe {
	client := &http.Client{Timeout: 3 * time.Second}
	return func(port int) bool {
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", port))
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode == http.StatusOK
	}
}

// Pull ensures the platform vLLM store (StoreDir) exists and returns. It does NOT block
// on a download: `vllm serve` fetches weights from the Hugging Face hub LAZILY on first
// launch (Start sets HF_HOME and passes --download-dir at the store), so a multi-GB
// download happens on demand at first serve, not eagerly here. MLX weights
// (mlx-community/*) on darwin, safetensors on Linux/CUDA.
func Pull(model string) error {
	if _, err := StoreDir(); err != nil {
		return fmt.Errorf("vllm: prepare store for %q: %w", model, err)
	}
	return nil
}
