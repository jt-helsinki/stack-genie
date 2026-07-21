package cli

import (
	"bytes"
	"io"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/output"
)

// runCompletion drives a single `ai completion [args...]` invocation against a
// fresh command tree, returning the exit code the command set. A JSON emitter is
// used so interactive() is false — exercising the non-interactive path cleanly.
func runCompletion(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
	exit := output.ExitOK
	cmd := newCompletionCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("completion %v returned error: %v", args, err)
	}
	return exit
}

// No shell arg on a non-TTY (JSON emitter) must exit 2, not block on a prompt.
func TestCompletionMissingShellNonInteractive(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if exit := runCompletion(test, "--print"); exit != output.ExitInvalidInput {
		test.Fatalf("completion with no shell: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// An unsupported shell arg still exits 2.
func TestCompletionUnsupportedShell(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if exit := runCompletion(test, "tcsh", "--print"); exit != output.ExitInvalidInput {
		test.Fatalf("completion tcsh: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// A supported shell arg still works (the positional arg remains the
// non-interactive fallback). --print avoids touching the filesystem.
func TestCompletionShellArgPrints(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := output.ExitOK
	cmd := newCompletionCmd(emitter, &exit)
	var buffer bytes.Buffer
	emitter.Out = &buffer
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"bash", "--print"})
	if err := cmd.Execute(); err != nil {
		test.Fatalf("completion bash --print error: %v", err)
	}
	if exit != output.ExitOK {
		test.Fatalf("completion bash --print: exit = %d, want 0", exit)
	}
	if buffer.Len() == 0 {
		test.Fatal("completion bash --print produced no script")
	}
}
