package vllm

import (
	"os/exec"
	"testing"
	"time"
)

// TestDefaultProcessAlive proves the fix for a crashed vllm serve reading as
// "alive" forever: a REAPED zombie must report dead. Process.Release() (what
// RealRunner.Start calls to detach) is pure Go-runtime bookkeeping — it does not
// change the OS parent/child relationship — so a child that exits while this
// process is still its parent and has not been wait()ed sits as a ZOMBIE. A plain
// existence check (kill(pid, 0)) counts a zombie's still-present PID entry as
// "alive"; defaultProcessAlive must instead reap it (WNOHANG) to see it is gone.
func TestDefaultProcessAlive(t *testing.T) {
	running := exec.Command("sh", "-c", "sleep 0.3")
	if err := running.Start(); err != nil {
		t.Fatalf("start running: %v", err)
	}
	defer func() { _, _ = running.Process.Wait() }()
	if !defaultProcessAlive(running.Process.Pid) {
		t.Fatal("a running child reported dead")
	}

	quick := exec.Command("sh", "-c", "exit 1")
	if err := quick.Start(); err != nil {
		t.Fatalf("start quick: %v", err)
	}
	pid := quick.Process.Pid
	// Detach exactly as RealRunner.Start does — NOT Wait(), so the process becomes
	// a zombie once it exits rather than being reaped by us here.
	_ = quick.Process.Release()

	deadline := time.Now().Add(2 * time.Second)
	for defaultProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if defaultProcessAlive(pid) {
		t.Fatal("an exited (zombie) child reported alive — a kill(pid,0)-only check would fail this")
	}
}
