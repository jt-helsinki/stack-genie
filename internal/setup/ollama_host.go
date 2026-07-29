package setup

// ollama_host.go wires HOST-NATIVE Ollama lifecycle (start/stop/restart) into
// `ai services` (and the TUI Services tab) via ControlService's host-native path.
// Ollama is NOT an aip-* container — it is a host `ollama serve` process on
// 127.0.0.1:11434 — so its control is a host-exec path, not `docker start/stop`.
//
// hardware bring-up: the real process spawn / pkill run only on a provisioned host
// (gated on the `ollama` binary being present); the logic is unit-tested through
// injectable seams so no test ever forks a process.

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/paths"
)

// ollamaStartAttempts / ollamaStartPollInterval bound the post-spawn readiness poll
// so a just-launched `ollama serve` is confirmed reachable (or given up on) without
// blocking indefinitely. The poll checks reachability BEFORE sleeping, so an
// already-warm server returns on the first iteration with no delay.
const (
	ollamaStartAttempts     = 20
	ollamaStartPollInterval = 250 * time.Millisecond
)

// ollamaBinaryLookup resolves the host `ollama` binary path (ok=false when it is not
// on PATH). Injectable seam so tests exercise the present/absent branches without a
// real binary.
var ollamaBinaryLookup = func() (string, bool) {
	path, err := exec.LookPath("ollama")
	return path, err == nil
}

// spawnHostOllama launches a DETACHED `ollama serve` (its own process group, released
// so it outlives this short-lived CLI) with the given OLLAMA_* env, teeing output to a
// log file under ~/.ai-platform/logs. Injectable seam — tests substitute a recorder so
// no process is forked. hardware bring-up: the real spawn runs only on a provisioned host.
var spawnHostOllama = realSpawnHostOllama

// pkillHostOllama best-effort terminates the host `ollama serve` process. Injectable
// seam (tests record the call); the default mirrors internal/uninstall's stopHostOllama.
var pkillHostOllama = func() error {
	return exec.Command("pkill", "-f", "ollama serve").Run()
}

// realSpawnHostOllama is the production spawn: it starts `ollama serve` detached with
// the platform's OLLAMA_* env (env is KEY=VALUE pairs appended to the current env) and
// redirects stdout+stderr to ~/.ai-platform/logs/ollama-host.log (best-effort — a log
// that cannot be opened is dropped, the server still starts).
func realSpawnHostOllama(binPath string, env []string) error {
	command := exec.Command(binPath, "serve")
	command.Env = append(os.Environ(), env...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if logsDir, err := paths.LogsDir(); err == nil {
		if logFile, err := os.OpenFile(filepath.Join(logsDir, "ollama-host.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			command.Stdout = logFile
			command.Stderr = logFile
			defer func() { _ = logFile.Close() }()
		}
	}
	if err := command.Start(); err != nil {
		return err
	}
	// Detach: we never wait on it (it is the long-lived host service), so release the
	// process handle so this CLI can exit without reaping it.
	return command.Process.Release()
}

// startHostOllamaService brings up the host-native Ollama:
//   - already reachable → no-op success (idempotent).
//   - `ollama` on PATH → spawn a detached `ollama serve` with the platform env, then
//     poll briefly for reachability (returning success once it answers, else after the
//     spawn — best-effort, the server may still be warming).
//   - no `ollama` binary → an actionable missing-dependency error (exit 3).
func startHostOllamaService() error {
	if hostOllamaReachable() {
		return nil
	}
	binPath, ok := ollamaBinaryLookup()
	if !ok {
		return output.Errorf(output.ExitMissingDep,
			"host-native Ollama is not installed — install it "+
				"(macOS: Ollama.app or `brew install ollama`; "+
				"Linux: `curl -fsSL https://ollama.com/install.sh | sh`), "+
				"set OLLAMA_MODELS=%s, then run `ai services start ollama`",
			hostOllamaModelsDir())
	}
	if err := spawnHostOllama(binPath, hostOllamaEnvPairs()); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "start host-native Ollama: %s", err)
	}
	for attempt := 0; attempt < ollamaStartAttempts; attempt++ {
		if hostOllamaReachable() {
			return nil
		}
		time.Sleep(ollamaStartPollInterval)
	}
	// Launched but not yet answering: not an error (it may still be loading). The
	// status read that follows will report it as "starting"/"stopped" accordingly.
	return nil
}

// stopHostOllamaService stops the host-native Ollama, best-effort (a not-running
// server is not an error). It mirrors internal/uninstall's stopHostOllama pkill.
func stopHostOllamaService() error {
	_ = pkillHostOllama()
	return nil
}
