package cli

import (
	"bytes"
	"io"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
	"github.com/spf13/cobra"
)

// `ai agent`/`ai attach` (and their workspace.* forms) are interactive-only (they
// own the terminal and emit no envelope), so under --json they are rejected at
// exit 2 before ever reaching the Manager — like `ai shell`.
func TestSessionCommandsGatedUnderJSON(test *testing.T) {
	cases := map[string]struct {
		builder func(*output.Emitter, *int) *cobra.Command
		args    []string
	}{
		"top-level agent":  {newAgentCmd, []string{"opencode"}},
		"workspace agent":  {newWorkspaceAgentCmd, []string{"opencode"}},
		"top-level attach": {newAttachCmd, nil},
		"workspace attach": {newWorkspaceAttachCmd, nil},
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
// must include the defaults `ai project create` installs.
func TestAgentCLINamesIncludesDefaults(test *testing.T) {
	names := workspace.AgentCLINames()
	want := map[string]bool{"opencode": false, "pi": false, "claude-code": false, "codex": false, "gemini": false}
	for _, name := range names {
		want[name] = true
	}
	for name, seen := range want {
		if !seen {
			test.Errorf("AgentCLINames missing %q (got %v)", name, names)
		}
	}
}

// sessionsResult renders a human table with the session names + attached flag.
func TestSessionsResultHumanRendersNames(test *testing.T) {
	result := sessionsResult{
		Project: "app",
		Sessions: []workspace.Session{
			{Name: "shell", Attached: true, Activity: "1700000000"},
			{Name: "opencode", Attached: false, Activity: ""},
		},
	}
	rendered := result.Human()
	for _, want := range []string{"NAME", "ATTACHED", "shell", "opencode"} {
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
