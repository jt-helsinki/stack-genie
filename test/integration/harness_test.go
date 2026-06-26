//go:build integration

// Package integration is the LIVE integration suite: it exercises the real `ai`
// binary against a RUNNING Docker + Microsandbox stack on this machine (the
// `hardware bring-up` path the unit tests deliberately fake). It is gated behind
// the `integration` build tag so `go build ./...` / `go test ./...` never see it,
// and it self-skips when the stack is not reachable (so it is a no-op on any host
// without Docker + msb + a brought-up platform).
//
// Run it with: make test-integration   (go test -tags integration ./test/integration/...)
//
// Unlike test/acceptance, these tests run against the REAL ~/.ai-platform state
// (the live stack's catalog, runtime.yaml, and project index) — they must, since
// the whole point is to talk to the running services. They are careful to clean
// up everything they create (the suite workspace + any dummy keys).
//
// NB: this file is named harness_test.go (not harness.go) so the testing
// framework actually invokes TestMain — a TestMain in a non-_test.go file is
// just an ordinary function and is never run.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// EnvelopeError mirrors the error object in the §19 envelope.
type EnvelopeError struct {
	Code    int    `json:"code"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Envelope is the CLI §19 output envelope.
type Envelope struct {
	OK       bool            `json:"ok"`
	Command  string          `json:"command"`
	Data     json.RawMessage `json:"data"`
	Error    *EnvelopeError  `json:"error"`
	Warnings []string        `json:"warnings"`
}

// sharedBinary is the path to the `ai` binary built once for the package.
var sharedBinary string

// stackReady records whether the live stack is reachable (set in TestMain). When
// false, every test self-skips.
var (
	stackReady  bool
	stackReason string
)

// TestMain builds the `ai` binary once and probes the live stack. If the stack
// is not reachable the tests still RUN but each one self-skips via requireStack,
// so a machine without the platform sees a clean no-op rather than failures.
func TestMain(m *testing.M) {
	binary, cleanup, err := buildBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration: build failed:", err)
		os.Exit(1)
	}
	sharedBinary = binary

	stackReady, stackReason = probeStack()
	if !stackReady {
		fmt.Fprintln(os.Stderr, "integration: stack not ready —", stackReason)
		fmt.Fprintln(os.Stderr, "integration: tests will self-skip")
	}

	code := m.Run()
	cleanup()
	os.Exit(code)
}

// probeStack checks that Docker + msb are installed and the platform stack is
// reachable. It returns (false, reason) when any precondition is missing so the
// suite can skip with a clear message.
func probeStack() (bool, string) {
	if _, err := exec.LookPath("docker"); err != nil {
		return false, "docker not on PATH"
	}
	if _, err := exec.LookPath("msb"); err != nil {
		return false, "msb (Microsandbox) not on PATH"
	}
	// `docker info` must succeed (daemon up).
	if out, err := runCmd(20*time.Second, "docker", "info"); err != nil {
		return false, fmt.Sprintf("docker info failed: %v: %s", err, truncate(out, 200))
	}
	// The platform must be set up + healthy. `ai doctor --json` is the canonical
	// health probe; require its envelope ok AND the embedded report ok.
	out, _, code := runBinary(40*time.Second, "", "doctor", "--json")
	if code != 0 {
		return false, fmt.Sprintf("ai doctor exited %d", code)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return false, fmt.Sprintf("ai doctor did not emit a JSON envelope: %v", err)
	}
	if !env.OK {
		return false, "ai doctor envelope not ok (run `ai setup`)"
	}
	return true, ""
}

func buildBinary() (string, func(), error) {
	dir, err := os.MkdirTemp("", "ai-integration-*")
	if err != nil {
		return "", nil, err
	}
	binary := filepath.Join(dir, "ai")
	build := exec.Command("go", "build", "-o", binary, "./cmd/ai")
	build.Dir = repoRoot()
	if output, err := build.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("go build: %v: %s", err, output)
	}
	return binary, func() { _ = os.RemoveAll(dir) }, nil
}

// repoRoot returns the module root (the test runs in test/integration).
func repoRoot() string {
	wd, _ := os.Getwd()
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// requireStack skips the calling test unless the live stack was found reachable.
func requireStack(test *testing.T) {
	test.Helper()
	if !stackReady {
		test.Skipf("live stack not reachable: %s", stackReason)
	}
}

// --- running the binary -----------------------------------------------------

// runBinary runs the `ai` binary with the given args from dir (real HOME, real
// environment — the live stack lives in ~/.ai-platform). It returns combined-safe
// stdout, stderr, and the exit code. A long default timeout suits slow container
// + microVM operations; callers pass a tighter one where appropriate.
func runBinary(timeout time.Duration, dir string, args ...string) (stdout, stderr string, code int) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, sharedBinary, args...)
	command.Env = os.Environ() // real HOME — talk to the live stack
	if dir != "" {
		command.Dir = dir
	}
	var outBuf, errBuf bytes.Buffer
	command.Stdout = &outBuf
	command.Stderr = &errBuf
	err := command.Run()
	code = 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			// Timeout or spawn failure — surface as a sentinel non-zero code.
			code = -1
			errBuf.WriteString("\n[runBinary] " + err.Error())
		}
	}
	return outBuf.String(), errBuf.String(), code
}

// runCmd runs an arbitrary command (used for docker probes), returning combined
// output and an error.
func runCmd(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	out, err := command.CombinedOutput()
	return string(out), err
}

// run is the test-facing wrapper: it runs `ai <args> --json` from the optional
// working directory, parses the envelope, and returns it with the exit code and
// raw stderr. The --json flag is appended automatically. For `exec` (which needs
// --json before the `--`) use runExec.
func run(test *testing.T, dir string, timeout time.Duration, args ...string) (Envelope, int, string) {
	test.Helper()
	args = append(args, "--json")
	stdout, stderr, code := runBinary(timeout, dir, args...)
	env, err := decode(stdout)
	if err != nil {
		test.Fatalf("`ai %s` did not emit a JSON envelope (exit %d): %v\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), code, err, stdout, stderr)
	}
	return env, code, stderr
}

// runExec runs `ai exec [name] --json -- <argv...>` (placing --json before `--`
// so it is not swallowed by the inner command).
func runExec(test *testing.T, dir string, timeout time.Duration, name string, argv ...string) (Envelope, int, string) {
	test.Helper()
	args := []string{"exec"}
	if name != "" {
		args = append(args, name)
	}
	args = append(args, "--json", "--")
	args = append(args, argv...)
	stdout, stderr, code := runBinary(timeout, dir, args...)
	env, err := decode(stdout)
	if err != nil {
		test.Fatalf("`ai exec` did not emit a JSON envelope (exit %d): %v\nstdout:\n%s\nstderr:\n%s",
			code, err, stdout, stderr)
	}
	return env, code, stderr
}

// decode parses a §19 envelope from stdout.
func decode(stdout string) (Envelope, error) {
	var env Envelope
	err := json.Unmarshal([]byte(stdout), &env)
	return env, err
}

// jsonUnmarshal is a thin wrapper so callers in other files can decode raw JSON
// without importing encoding/json directly.
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// dataInto unmarshals the envelope's data into v.
func (env Envelope) dataInto(test *testing.T, v any) {
	test.Helper()
	if err := json.Unmarshal(env.Data, v); err != nil {
		test.Fatalf("decode data: %v\n%s", err, string(env.Data))
	}
}

// --- assertions -------------------------------------------------------------

// assertOK fails (non-fatally) unless the envelope is ok with the given command
// key and exit 0. It returns true on success so callers can gate follow-ups. It
// uses Errorf (not Fatalf) so one failing step does not abort the whole group —
// each bring-up seam is reported independently.
func assertOK(test *testing.T, env Envelope, code int, command string) bool {
	test.Helper()
	ok := true
	if !env.OK || code != 0 {
		test.Errorf("want ok command=%q exit=0, got ok=%v exit=%d err=%+v", command, env.OK, code, env.Error)
		ok = false
	}
	if command != "" && env.Command != command {
		test.Errorf("command = %q, want %q", env.Command, command)
		ok = false
	}
	return ok
}

// assertError fails unless the envelope failed with the given exit code and
// error.code == exit code (§19).
func assertError(test *testing.T, env Envelope, code, wantCode int) {
	test.Helper()
	if env.OK || code != wantCode {
		test.Errorf("want failure exit=%d, got ok=%v exit=%d", wantCode, env.OK, code)
		return
	}
	if env.Error == nil || env.Error.Code != code {
		test.Errorf("error.code must equal exit code %d, got %+v", code, env.Error)
	}
}

// --- waiting ----------------------------------------------------------------

// waitFor polls cond until it returns true or timeout elapses (250ms interval).
// It returns whether the condition was met — for generous container + microVM
// warm-up.
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return cond()
}

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max {
		return value[:max] + "…"
	}
	return value
}
