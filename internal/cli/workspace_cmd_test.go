package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
	"github.com/spf13/cobra"
)

// execCmd builds an `ai exec` command bound to a fresh emitter/exit and runs it
// with args, returning the exit code. Using Execute (not a direct RunE call) so
// cobra populates ArgsLenAtDash for the `--` handling.
func runWorkspaceCmd(test *testing.T, build func(*output.Emitter, *int) *cobra.Command, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := output.ExitOK
	cmd := build(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("%v returned error: %v", args, err)
	}
	return exit
}

// `ai exec` without a `--` separator is a usage error (exit 2).
func TestExecMissingDashIsExit2(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Chdir(test.TempDir())
	if exit := runWorkspaceCmd(test, newExecCmd, "foo"); exit != output.ExitInvalidInput {
		test.Fatalf("exec without -- exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// `ai exec -- <cmd>` outside a workspace fails to resolve the workspace (exit 2).
func TestExecOutsideWorkspaceIsExit2(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Chdir(test.TempDir())
	if exit := runWorkspaceCmd(test, newExecCmd, "--", "echo", "hi"); exit != output.ExitInvalidInput {
		test.Fatalf("exec outside workspace exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// `ai sessions` outside a workspace is an invalid-input error (exit 2).
func TestSessionsOutsideWorkspaceIsExit2(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Chdir(test.TempDir())
	if exit := runWorkspaceCmd(test, newSessionsCmd); exit != output.ExitInvalidInput {
		test.Fatalf("sessions outside workspace exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// `ai sessions kill <session>` outside a workspace is invalid input (exit 2).
func TestSessionsKillOutsideWorkspaceIsExit2(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Chdir(test.TempDir())
	if exit := runWorkspaceCmd(test, newSessionsCmd, "kill", "mysession"); exit != output.ExitInvalidInput {
		test.Fatalf("sessions kill outside workspace exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// The interactive-only verbs (agent/shell/attach) are rejected when there is no
// terminal (as in `go test`) with exit 2, before touching any backend.
func TestInteractiveVerbsRejectedWithoutTTY(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Chdir(test.TempDir())
	if exit := runWorkspaceCmd(test, newAgentCmd, "opencode"); exit != output.ExitInvalidInput {
		test.Fatalf("agent without TTY exit = %d, want %d", exit, output.ExitInvalidInput)
	}
	if exit := runWorkspaceCmd(test, newShellCmd); exit != output.ExitInvalidInput {
		test.Fatalf("shell without TTY exit = %d, want %d", exit, output.ExitInvalidInput)
	}
	if exit := runWorkspaceCmd(test, newAttachCmd); exit != output.ExitInvalidInput {
		test.Fatalf("attach without TTY exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

func TestSessionsResultHuman(test *testing.T) {
	empty := sessionsResult{Project: "app"}
	if !strings.Contains(empty.Human(), "No sessions yet") {
		test.Fatalf("empty sessions Human missing hint:\n%s", empty.Human())
	}

	recent := fmt.Sprintf("%d", time.Now().Add(-90*time.Second).Unix())
	full := sessionsResult{Project: "app", Sessions: []workspace.Session{
		{Name: "shell", Attached: true, Activity: recent},
		{Name: "opencode", Attached: false, Activity: ""},
	}}
	human := full.Human()
	for _, want := range []string{"NAME", "ATTACHED", "IDLE", "shell", "opencode"} {
		if !strings.Contains(human, want) {
			test.Fatalf("sessions Human missing %q:\n%s", want, human)
		}
	}
}

func TestSessionKillResultHuman(test *testing.T) {
	human := sessionKillResult{Project: "app", Session: "shell"}.Human()
	if !strings.Contains(human, "shell") || !strings.Contains(human, "app") {
		test.Fatalf("sessionKillResult Human missing fields:\n%s", human)
	}
}

func TestAttachNoSessionsResultHuman(test *testing.T) {
	human := attachNoSessionsResult{Project: "app"}.Human()
	if !strings.Contains(human, "app") || !strings.Contains(human, "ai shell") {
		test.Fatalf("attachNoSessionsResult Human missing guidance:\n%s", human)
	}
}

func TestSessionIdle(test *testing.T) {
	if got := sessionIdle(""); got != "-" {
		test.Fatalf("blank activity = %q, want -", got)
	}
	if got := sessionIdle("not-a-number"); got != "-" {
		test.Fatalf("unparseable activity = %q, want -", got)
	}
	if got := sessionIdle(fmt.Sprintf("%d", time.Now().Add(-30*time.Second).Unix())); !strings.HasSuffix(got, "s") {
		test.Fatalf("30s ago should render seconds, got %q", got)
	}
	if got := sessionIdle(fmt.Sprintf("%d", time.Now().Add(-5*time.Minute).Unix())); !strings.HasSuffix(got, "m") {
		test.Fatalf("5m ago should render minutes, got %q", got)
	}
	if got := sessionIdle(fmt.Sprintf("%d", time.Now().Add(-3*time.Hour).Unix())); !strings.HasSuffix(got, "h") {
		test.Fatalf("3h ago should render hours, got %q", got)
	}
}

func TestSecondArg(test *testing.T) {
	if secondArg(nil) != "" || secondArg([]string{"a"}) != "" {
		test.Fatal("secondArg should be empty with <2 args")
	}
	if secondArg([]string{"a", "b"}) != "b" {
		test.Fatal("secondArg should return args[1]")
	}
}

func TestSessionNamesAndOptions(test *testing.T) {
	sessions := []workspace.Session{
		{Name: "shell", Attached: true},
		{Name: "opencode", Attached: false, Activity: "0"},
	}
	names := sessionNames(sessions)
	if len(names) != 2 || names[0] != "shell" || names[1] != "opencode" {
		test.Fatalf("sessionNames = %v", names)
	}
	options := sessionOptions(sessions)
	if len(options) != 2 {
		test.Fatalf("sessionOptions len = %d, want 2", len(options))
	}
	if !strings.Contains(options[0].Key, "attached") {
		test.Fatalf("attached session option should be labelled: %q", options[0].Key)
	}
}

func TestMapWorkspaceErr(test *testing.T) {
	var platformErr *output.Error

	// Pre-coded errors pass through.
	coded := output.Errorf(output.ExitPermission, "no")
	if !errors.As(mapWorkspaceErr(coded), &platformErr) || platformErr.Code != output.ExitPermission {
		test.Fatalf("coded error should pass through: %v", mapWorkspaceErr(coded))
	}

	// Unknown-project / not-started / unknown-agent → invalid input (exit 2).
	for _, sentinel := range []error{workspace.ErrUnknownProject, workspace.ErrNotStarted, workspace.ErrUnknownAgentCLI} {
		if !errors.As(mapWorkspaceErr(sentinel), &platformErr) || platformErr.Code != output.ExitInvalidInput {
			test.Fatalf("%v should map to invalid input, got %v", sentinel, mapWorkspaceErr(sentinel))
		}
	}

	// Missing runtime deps → missing dependency (exit 3).
	for _, sentinel := range []error{workspace.ErrContainerRuntimeMissing, workspace.ErrMsbMissing, workspace.ErrTmuxMissing} {
		if !errors.As(mapWorkspaceErr(sentinel), &platformErr) || platformErr.Code != output.ExitMissingDep {
			test.Fatalf("%v should map to missing dep, got %v", sentinel, mapWorkspaceErr(sentinel))
		}
	}

	// Anything else → runtime failure (exit 4).
	if !errors.As(mapWorkspaceErr(errors.New("boom")), &platformErr) || platformErr.Code != output.ExitRuntimeFailure {
		test.Fatalf("generic error should map to runtime failure")
	}
}
