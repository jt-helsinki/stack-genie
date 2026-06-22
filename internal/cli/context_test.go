package cli

import (
	"io"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/contextopt"
	"github.com/jt-helsinki/ideal-robot/internal/output"
)

// runContext drives a single `ai context <sub> [args...]` invocation against a
// fresh command tree with a JSON emitter (so interactive() is false), returning
// the exit code the command set. A JSON emitter keeps the non-interactive path
// under test — no prompt is ever attempted.
func runContext(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
	exit := output.ExitOK
	cmd := newContextCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("context %v returned error: %v", args, err)
	}
	return exit
}

// With the relaxed Args, a missing value on a non-TTY (JSON emitter) is exit 2,
// for both strategy and caveman, with project resolved from the cwd.
func TestContextValueRequiredNonInteractive(test *testing.T) {
	root := seedProjectAt(test, "app")
	test.Chdir(root)

	if exit := runContext(test, "strategy"); exit != output.ExitInvalidInput {
		test.Fatalf("strategy with no value: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
	if exit := runContext(test, "caveman"); exit != output.ExitInvalidInput {
		test.Fatalf("caveman with no value: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// A value passed on the command line is used as-is (no prompt) and applied.
func TestContextValueFromArg(test *testing.T) {
	root := seedProjectAt(test, "app")
	test.Chdir(root)

	if exit := runContext(test, "strategy", "aggressive"); exit != output.ExitOK {
		test.Fatalf("strategy aggressive: exit = %d, want 0", exit)
	}
	status, err := contextopt.GetStatus(root)
	if err != nil {
		test.Fatalf("GetStatus: %v", err)
	}
	if status.Strategy != "aggressive" {
		test.Fatalf("strategy = %q, want aggressive", status.Strategy)
	}

	if exit := runContext(test, "caveman", "ultra"); exit != output.ExitOK {
		test.Fatalf("caveman ultra: exit = %d, want 0", exit)
	}
	status, err = contextopt.GetStatus(root)
	if err != nil {
		test.Fatalf("GetStatus: %v", err)
	}
	if status.CavemanLevel != "ultra" {
		test.Fatalf("caveman level = %q, want ultra", status.CavemanLevel)
	}
}

// With two args the first is the project and the second the value (unchanged
// disambiguation); the lone arg in the one-arg form is the VALUE, not a project.
func TestContextProjectAndValueArg(test *testing.T) {
	root := seedProjectAt(test, "app")

	if exit := runContext(test, "strategy", "app", "balanced"); exit != output.ExitOK {
		test.Fatalf("strategy app balanced: exit = %d, want 0", exit)
	}
	status, err := contextopt.GetStatus(root)
	if err != nil {
		test.Fatalf("GetStatus: %v", err)
	}
	if status.Strategy != "balanced" {
		test.Fatalf("strategy = %q, want balanced", status.Strategy)
	}
}

// An invalid value (one arg, treated as the value) is rejected as exit 2.
func TestContextInvalidValueArg(test *testing.T) {
	root := seedProjectAt(test, "app")
	test.Chdir(root)

	if exit := runContext(test, "strategy", "bogus"); exit != output.ExitInvalidInput {
		test.Fatalf("strategy bogus: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}
