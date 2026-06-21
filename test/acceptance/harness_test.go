// Package acceptance is the Go acceptance-test harness (acceptance-tests spec
// §1.6). It builds the `ai` binary once, runs commands against an ephemeral
// HOME, and asserts the §19 JSON envelope and §18 exit codes — never scraped
// text. Service-dependent tests skip when Docker/Microsandbox/LiteLLM are
// absent (they run on a provisioned Apple Silicon host).
package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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

// TestMain builds the binary once for all tests in the package.
func TestMain(m *testing.M) {
	binary, cleanup, err := buildBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, "acceptance: build failed:", err)
		os.Exit(1)
	}
	sharedBinary = binary
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func buildBinary() (string, func(), error) {
	dir, err := os.MkdirTemp("", "ai-acceptance-*")
	if err != nil {
		return "", nil, err
	}
	binary := filepath.Join(dir, "ai")
	build := exec.Command("go", "build", "-o", binary, "./cmd/ai")
	build.Dir = repoRoot()
	if output, err := build.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return "", nil, fmt.Errorf("go build: %v: %s", err, output)
	}
	return binary, func() { os.RemoveAll(dir) }, nil
}

// repoRoot returns the module root (the test runs in test/acceptance).
func repoRoot() string {
	wd, _ := os.Getwd()
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// Harness runs `ai` commands against an ephemeral HOME and from an isolated
// working directory (so cwd-based behavior — `project create` scaffolds in the
// cwd, project resolution walks up from it — never touches the repo).
type Harness struct {
	Binary string
	Home   string
	Work   string
}

// New returns a Harness with a fresh ephemeral HOME and a separate working dir
// (both removed on test cleanup).
func New(test *testing.T) *Harness {
	return &Harness{Binary: sharedBinary, Home: test.TempDir(), Work: test.TempDir()}
}

// env returns the child environment with HOME repointed and any extra vars.
func (harness *Harness) env(extra ...string) []string {
	return append(append(os.Environ(), "HOME="+harness.Home), extra...)
}

// Run executes `ai <args> --json`, returning the parsed envelope and exit code.
func (harness *Harness) Run(test *testing.T, args ...string) (Envelope, int) {
	test.Helper()
	return harness.runRaw(test, append(args, "--json")...)
}

// Exec runs `ai workspace exec [project] --json -- <argv>` — placing --json
// before the `--` so it is not swallowed as part of the inner command.
func (harness *Harness) Exec(test *testing.T, project string, argv ...string) (Envelope, int) {
	test.Helper()
	args := []string{"workspace", "exec"}
	if project != "" {
		args = append(args, project)
	}
	args = append(args, "--json", "--")
	args = append(args, argv...)
	return harness.runRaw(test, args...)
}

// runRaw runs `ai <args>` verbatim (no automatic --json) and parses the envelope.
// Diagnostics on stderr are discarded; only the stdout envelope is parsed (§19).
func (harness *Harness) runRaw(test *testing.T, args ...string) (Envelope, int) {
	test.Helper()
	command := exec.Command(harness.Binary, args...)
	command.Env = harness.env()
	command.Dir = harness.Work // isolate cwd-based behavior from the repo
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = io.Discard

	exitCode := 0
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if !asExitError(err, &exitErr) {
			test.Fatalf("run `ai %v`: %v", args, err)
		}
		exitCode = exitErr.ExitCode()
	}

	var envelope Envelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		test.Fatalf("`ai %v` did not emit a JSON envelope: %v\n%s", args, err, stdout.String())
	}
	return envelope, exitCode
}

func asExitError(err error, target **exec.ExitError) bool {
	if exitErr, ok := err.(*exec.ExitError); ok {
		*target = exitErr
		return true
	}
	return false
}

// --- assertions -------------------------------------------------------------

// AssertOK fails unless the envelope is ok with the given command and exit 0.
func AssertOK(test *testing.T, envelope Envelope, exitCode int, command string) {
	test.Helper()
	if !envelope.OK || exitCode != 0 {
		test.Fatalf("want ok command=%q exit=0, got ok=%v exit=%d err=%+v", command, envelope.OK, exitCode, envelope.Error)
	}
	if envelope.Command != command {
		test.Fatalf("command = %q, want %q", envelope.Command, command)
	}
}

// AssertError fails unless the envelope failed with the given exit code, and
// checks error.code == exit code (§19).
func AssertError(test *testing.T, envelope Envelope, exitCode, wantCode int) {
	test.Helper()
	if envelope.OK || exitCode != wantCode {
		test.Fatalf("want failure exit=%d, got ok=%v exit=%d", wantCode, envelope.OK, exitCode)
	}
	if envelope.Error == nil || envelope.Error.Code != exitCode {
		test.Fatalf("error.code must equal exit code %d, got %+v", exitCode, envelope.Error)
	}
}

// dataField unmarshals the envelope's data into v.
func (envelope Envelope) dataInto(test *testing.T, v any) {
	test.Helper()
	if err := json.Unmarshal(envelope.Data, v); err != nil {
		test.Fatalf("decode data: %v\n%s", err, string(envelope.Data))
	}
}
