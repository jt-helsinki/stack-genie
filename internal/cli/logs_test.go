package cli

import (
	"io"
	"slices"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/logs"
	"github.com/jt-helsinki/ideal-robot/internal/output"
)

// runLogs drives a single `ai logs [args...]` invocation against a fresh command
// tree with a JSON emitter (so interactive() is false), returning the exit code
// the command set. A JSON emitter keeps the non-interactive path under test.
func runLogs(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
	exit := output.ExitOK
	cmd := newLogsCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("logs %v returned error: %v", args, err)
	}
	return exit
}

// --service must accept every host service in the current tier: the microVM
// runtime plus setup.desiredServices(). With no logs on disk yet, an accepted
// service resolves to an empty result (exit 0), never a crash or an error.
func TestLogsServiceAcceptsFullTier(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	// Run from a non-project directory so the default-workspace resolution (the
	// cwd's project) finds nothing — keeps the test hermetic regardless of where
	// `go test` is invoked (e.g. from inside a project checkout).
	test.Chdir(test.TempDir())

	// Derive the expected set from logs.Services() (the source of truth) rather than
	// a hand-written subset, so dropping an optional log scope (odysseus, chromadb,
	// searxng, ntfy, …) is caught here.
	wantServices := logs.Services()
	for _, service := range wantServices {
		if !slices.Contains(logServices, service) {
			test.Fatalf("logServices missing %q; have %v", service, logServices)
		}
		if exit := runLogs(test, "--service", service); exit != output.ExitOK {
			test.Fatalf("logs --service %s: exit = %d, want 0", service, exit)
		}
	}
}

// An unknown --service value is rejected as invalid input (exit 2).
func TestLogsServiceRejectsUnknown(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Chdir(test.TempDir())

	if exit := runLogs(test, "--service", "bogus"); exit != output.ExitInvalidInput {
		test.Fatalf("logs --service bogus: exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}
