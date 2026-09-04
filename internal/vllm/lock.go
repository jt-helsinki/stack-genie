package vllm

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockAlias acquires an OS-level exclusive file lock scoped to alias, serializing
// EnsureServed across SEPARATE `ai` invocations. `ai` is daemonless — each run
// builds a fresh Manager, so the in-memory `pending` map (which only guards
// concurrent goroutines within ONE process) does nothing across processes.
// Without this, two invocations racing a start/restart for the same alias (e.g. a
// double-fired TUI "restart all") can both see the target port free — vLLM's own
// Python/MLX import + startup takes many seconds before it actually calls
// sock.bind() — and both spawn `vllm serve`, crashing the second one with
// "Address already in use". flock is released by the OS the instant the holding
// process exits for ANY reason (including a crash), so this can never wedge; the
// bounded poll below only guards against an absurdly stuck holder.
func lockAlias(storeDir, alias string) (*os.File, error) {
	path := filepath.Join(storeDir, "."+alias+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("vllm: open start-lock for %q: %w", alias, err)
	}
	deadline := time.Now().Add(DefaultStartTimeout)
	for {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return file, nil
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("vllm: timed out waiting for %q's start lock — another `ai` invocation may be stuck starting it", alias)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// unlockAlias releases a lock acquired by lockAlias.
func unlockAlias(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}
