package cli

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
	"github.com/spf13/cobra"
)

// `ai agent`/`ai attach`/`ai shell`/`ai start`/`ai restart` are interactive-only
// (they own the terminal and emit no envelope), so under --json they are
// rejected at exit 2 before ever reaching the Manager.
func TestInteractiveCommandsGatedUnderJSON(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Chdir(test.TempDir())
	cases := map[string]struct {
		builder func(*output.Emitter, *int) *cobra.Command
		args    []string
	}{
		"top-level agent":   {newAgentCmd, []string{"opencode"}},
		"top-level attach":  {newAttachCmd, nil},
		"top-level shell":   {newShellCmd, nil},
		"top-level start":   {newStartCmd, nil},
		"top-level restart": {newRestartCmd, nil},
	}
	for name, testCase := range cases {
		test.Run(name, func(test *testing.T) {
			var stderr bytes.Buffer
			emitter := &output.Emitter{Out: io.Discard, Err: &stderr, JSON: true}
			exit := output.ExitOK
			cmd := testCase.builder(emitter, &exit)
			if err := cmd.RunE(cmd, testCase.args); err != nil {
				test.Fatalf("RunE returned a non-nil error: %v", err)
			}
			if exit != output.ExitInvalidInput {
				test.Fatalf("under --json exit = %d, want %d (invalid input)", exit, output.ExitInvalidInput)
			}
		})
	}
}

// AgentCLINames exposes the launchable agent CLIs (used by completion); the set
// must include the defaults `ai create` installs.
func TestAgentCLINamesIncludesDefaults(test *testing.T) {
	names := workspace.AgentCLINames()
	want := map[string]bool{"opencode": false, "omp": false, "claude-code": false, "codex": false, "gemini": false, "hermes": false}
	for _, name := range names {
		want[name] = true
	}
	for name, seen := range want {
		if !seen {
			test.Errorf("AgentCLINames missing %q (got %v)", name, names)
		}
	}
}

// sessionsResult renders a human table with the session id/name/status/attached.
func TestSessionsResultHumanRendersNames(test *testing.T) {
	result := sessionsResult{
		Project: "app",
		Sessions: []workspace.Session{
			{SessionID: "abc12345", Name: "shell", Status: "running", Attachment: "attached"},
			{SessionID: "def67890", Name: "opencode", Status: "running", Attachment: "detached"},
		},
	}
	rendered := result.Human()
	for _, want := range []string{"NAME", "STATUS", "ATTACHED", "shell", "opencode"} {
		if !bytes.Contains([]byte(rendered), []byte(want)) {
			test.Errorf("sessions table missing %q:\n%s", want, rendered)
		}
	}
}

// An empty session list renders a friendly hint, not a bare table.
func TestSessionsResultHumanEmptyHint(test *testing.T) {
	result := sessionsResult{Project: "app"}
	if !bytes.Contains([]byte(result.Human()), []byte("No sessions yet")) {
		test.Errorf("empty sessions should hint how to start one: %q", result.Human())
	}
}

// `ai attach` with no sessions tells the user to use `ai agent` to create one.
func TestAttachNoSessionsResultHumanPointsAtAgent(test *testing.T) {
	rendered := attachNoSessionsResult{Project: "app"}.Human()
	for _, want := range []string{"No sessions", "app", "ai agent"} {
		if !bytes.Contains([]byte(rendered), []byte(want)) {
			test.Errorf("no-sessions hint missing %q:\n%s", want, rendered)
		}
	}
}

// sessionOptions labels each attach choice with its name + short id but keeps
// the option VALUE the full SessionID (what gets attached).
func TestSessionOptionsLabelsAndValues(test *testing.T) {
	options := sessionOptions([]workspace.Session{
		{SessionID: "abc12345xyz", Name: "shell"},
		{SessionID: "def67890xyz", Name: ""},
	})
	if len(options) != 2 {
		test.Fatalf("expected 2 options, got %d", len(options))
	}
	if options[0].Value != "abc12345xyz" || !bytes.Contains([]byte(options[0].Key), []byte("shell")) {
		test.Errorf("first option = %+v, want value abc12345xyz + 'shell' label", options[0])
	}
	if options[1].Value != "def67890xyz" || !bytes.Contains([]byte(options[1].Key), []byte("(shell)")) {
		test.Errorf("unnamed session option = %+v, want value def67890xyz + '(shell)' label", options[1])
	}
}

// runningSessionByName finds only a RUNNING session with a matching name.
func TestRunningSessionByName(test *testing.T) {
	sessions := []workspace.Session{
		{SessionID: "a", Name: "codex", Status: "exited"},
		{SessionID: "b", Name: "codex", Status: "running"},
	}
	id, found := runningSessionByName(sessions, "codex")
	if !found || id != "b" {
		test.Fatalf("runningSessionByName = (%q, %v), want (b, true)", id, found)
	}
	if _, found := runningSessionByName(sessions, "claude-code"); found {
		test.Fatal("runningSessionByName should not match an absent name")
	}
}

func TestMapWorkspaceErr(test *testing.T) {
	var platformErr *output.Error

	// Pre-coded errors pass through.
	coded := output.Errorf(output.ExitPermission, "no")
	if !errors.As(mapWorkspaceErr(coded), &platformErr) || platformErr.Code != output.ExitPermission {
		test.Fatalf("coded error should pass through: %v", mapWorkspaceErr(coded))
	}

	// Unknown-project / unknown-agent / no-active-session → invalid input (exit 2).
	for _, sentinel := range []error{workspace.ErrUnknownProject, workspace.ErrUnknownAgentCLI, workspace.ErrNoActiveSession} {
		if !errors.As(mapWorkspaceErr(sentinel), &platformErr) || platformErr.Code != output.ExitInvalidInput {
			test.Fatalf("%v should map to invalid input, got %v", sentinel, mapWorkspaceErr(sentinel))
		}
	}

	// Missing nono → exit 3.
	if !errors.As(mapWorkspaceErr(workspace.ErrNonoMissing), &platformErr) || platformErr.Code != output.ExitMissingDep {
		test.Fatalf("ErrNonoMissing should map to missing dependency, got %v", mapWorkspaceErr(workspace.ErrNonoMissing))
	}

	// Anything else is a runtime failure (exit 4).
	mapped := mapWorkspaceErr(errors.New("boom"))
	if !errors.As(mapped, &platformErr) || platformErr.Code != output.ExitRuntimeFailure {
		test.Fatalf("generic error should map to runtime failure, got %v", mapped)
	}
}
