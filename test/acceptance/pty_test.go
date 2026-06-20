package acceptance

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/creack/pty"
)

// CreateProject drives `ai project create <name>` through a pseudo-terminal,
// accepting every wizard step's default (acceptance-tests §1.4). The wizard's
// TUI is rendered on the pty (stdin+stderr); the final §19 envelope is captured
// cleanly from stdout.
//
// With <name> given as an argument the wizard skips the name step, so the
// remaining steps are OS, agent CLIs, default agent, and software stacks —
// accepted with Enter, which yields the defaults (debian-trixie, opencode, none).
func (harness *Harness) CreateProject(test *testing.T, name string) (Envelope, int) {
	return harness.createProject(test, name, 0)
}

// wizardOSOrder mirrors supportedOSes in internal/cli/project.go — the order the
// OS picker presents, so an index maps to arrow-down presses.
var wizardOSOrder = []string{"debian-trixie", "debian-bookworm", "ubuntu", "alma"}

// CreateProjectWithOS drives the wizard and selects a specific OS by moving the
// single-select cursor down to it before accepting the remaining defaults.
func (harness *Harness) CreateProjectWithOS(test *testing.T, name, osKey string) (Envelope, int) {
	test.Helper()
	osIndex := 0
	for index, candidate := range wizardOSOrder {
		if candidate == osKey {
			osIndex = index
			break
		}
	}
	return harness.createProject(test, name, osIndex)
}

// createProject is the shared PTY driver. osDownPresses arrow-down keystrokes are
// sent on the first (OS) step before its Enter, to move off the default OS.
func (harness *Harness) createProject(test *testing.T, name string, osDownPresses int) (Envelope, int) {
	test.Helper()

	ptmx, tty, err := pty.Open()
	if err != nil {
		test.Fatalf("open pty: %v", err)
	}
	defer ptmx.Close()
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 120})

	command := exec.Command(harness.Binary, "project", "create", name, "--json")
	command.Env = harness.env()
	command.Dir = harness.Work // create scaffolds in the cwd; keep it isolated
	command.Stdin = tty
	command.Stderr = tty // huh renders here and sees an interactive terminal
	var stdout bytes.Buffer
	command.Stdout = &stdout // clean: only the envelope lands here

	if err := command.Start(); err != nil {
		tty.Close()
		test.Fatalf("start create: %v", err)
	}
	tty.Close() // the child holds the slave; we keep the master (ptmx)

	// Continuously drain the TUI output on the master, or the child blocks once
	// the pty buffer fills (and never processes our keystrokes).
	go io.Copy(io.Discard, ptmx)

	// Watchdog: never let a stuck wizard hang the suite.
	watchdog := time.AfterFunc(15*time.Second, func() {
		_ = command.Process.Kill()
	})
	defer watchdog.Stop()

	// Advance each wizard step by sending Enter. On the first (OS) step, move the
	// single-select cursor down osDownPresses times to pick a non-default OS.
	// Extra presses after submission are harmless (the process has exited).
	driveDone := make(chan struct{})
	go func() {
		time.Sleep(250 * time.Millisecond)
		for press := 0; press < osDownPresses; press++ {
			if _, err := ptmx.Write([]byte("\x1b[B")); err != nil { // arrow down
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		for step := 0; step < 8; step++ {
			if _, err := ptmx.Write([]byte("\r")); err != nil {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		close(driveDone)
	}()

	exitCode := 0
	if err := command.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !asExitError(err, &exitErr) {
			test.Fatalf("create wizard: %v", err)
		}
		exitCode = exitErr.ExitCode()
	}
	<-driveDone

	var envelope Envelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		test.Fatalf("create wizard did not emit a clean JSON envelope: %v\n%q", err, stdout.String())
	}
	return envelope, exitCode
}
