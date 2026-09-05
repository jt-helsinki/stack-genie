package omlx

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockServer acquires an OS-level exclusive file lock serializing EnsureServed
// across SEPARATE `ai` invocations. `ai` is daemonless — each run builds a fresh
// Manager, so the in-process startMu guard (omlx.go) only protects concurrent
// goroutines within ONE process. Without this, two invocations racing a start
// (e.g. a double-fired restart) can both see the port free — the server process's
// own startup (FastAPI + MLX import) takes time before it actually binds the
// socket — and both spawn `omlx serve`, crashing the second one with "Address
// already in use" (the exact bug this pattern already fixed once for vLLM's
// per-model start path; the same daemonless race applies here). flock is released
// by the OS the instant the holding process exits for ANY reason (including a
// crash), so it can never wedge; the bounded poll below only guards against an
// absurdly stuck holder.
func lockServer() (*os.File, error) {
	dir, err := StoreDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(dir), ".omlx-start.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("omlx: open start-lock: %w", err)
	}
	deadline := time.Now().Add(DefaultStartTimeout)
	for {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return file, nil
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("omlx: timed out waiting for the start lock — another `ai` invocation may be stuck starting it")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// unlockServer releases a lock acquired by lockServer.
func unlockServer(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}
