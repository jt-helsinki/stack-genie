package cli

import (
	"io"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/output"
)

// runStateSub drives `ai state <sub>` against a fresh command tree under the test
// HOME, returning the exit code the command set.
func runStateSub(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := output.ExitOK
	cmd := newStateCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("state %v returned error: %v", args, err)
	}
	return exit
}

// `ai state show` reports the projects/config/runtime snapshot from a clean HOME
// (no projects, no runtime.yaml) at exit 0.
func TestStateShowEmptyHome(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Chdir(test.TempDir())
	if exit := runStateSub(test, "show"); exit != output.ExitOK {
		test.Fatalf("state show exit = %d, want 0", exit)
	}
}

// `ai state show` still works from inside a seeded workspace (it resolves the cwd
// project) at exit 0.
func TestStateShowInsideWorkspace(test *testing.T) {
	root := seedProjectAt(test, "app")
	test.Chdir(root)
	if exit := runStateSub(test, "show"); exit != output.ExitOK {
		test.Fatalf("state show (in workspace) exit = %d, want 0", exit)
	}
}

// `ai state repair` rebuilds host-local state from the filesystem at exit 0.
func TestStateRepair(test *testing.T) {
	seedProjectAt(test, "app")
	if exit := runStateSub(test, "repair"); exit != output.ExitOK {
		test.Fatalf("state repair exit = %d, want 0", exit)
	}
}

// `ai state` with no subcommand prints help and leaves the exit code at OK.
func TestStateNoSubcommandPrintsHelp(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if exit := runStateSub(test); exit != output.ExitOK {
		test.Fatalf("bare state exit = %d, want 0", exit)
	}
}
