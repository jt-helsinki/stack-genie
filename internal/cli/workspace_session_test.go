package cli

import (
	"bytes"
	"io"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
	"github.com/spf13/cobra"
)

// `ai agent`/`ai attach` are interactive-only (they own the terminal and emit no
// envelope), so under --json they are rejected at exit 2 before ever reaching the
// Manager — like `ai shell`.
func TestSessionCommandsGatedUnderJSON(test *testing.T) {
	cases := map[string]struct {
		builder func(*output.Emitter, *int) *cobra.Command
		args    []string
	}{
		"top-level agent":  {newAgentCmd, []string{"opencode"}},
		"top-level attach": {newAttachCmd, nil},
		"top-level shell":  {newShellCmd, nil},
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
	want := map[string]bool{"opencode": false, "omp": false, "claude-code": false, "codex": false, "gemini": false}
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

// `ai attach` with no sessions tells the user to use `ai shell` to create one.
func TestAttachNoSessionsResultHumanPointsAtShell(test *testing.T) {
	rendered := attachNoSessionsResult{Project: "app"}.Human()
	for _, want := range []string{"No sessions", "app", "ai shell"} {
		if !bytes.Contains([]byte(rendered), []byte(want)) {
			test.Errorf("no-sessions hint missing %q:\n%s", want, rendered)
		}
	}
}

// validateSessionName accepts tmux-safe names and rejects empty / punctuated ones
// (tmux forbids '.'/':' and whitespace in session names).
func TestValidateSessionName(test *testing.T) {
	valid := []string{"shell", "build-2", "my_session", "OpenCode"}
	for _, name := range valid {
		if err := validateSessionName(name); err != nil {
			test.Errorf("validateSessionName(%q) = %v, want nil", name, err)
		}
	}
	invalid := []string{"", "   ", "has space", "dot.name", "colon:name", "slash/name"}
	for _, name := range invalid {
		if err := validateSessionName(name); err == nil {
			test.Errorf("validateSessionName(%q) = nil, want an error", name)
		}
	}
}

// sessionNames projects the session list to plain names for the pickers.
func TestSessionNames(test *testing.T) {
	got := sessionNames([]workspace.Session{{Name: "shell"}, {Name: "opencode"}})
	if len(got) != 2 || got[0] != "shell" || got[1] != "opencode" {
		test.Fatalf("sessionNames = %v, want [shell opencode]", got)
	}
}

// sessionOptions labels each attach choice with its attached/idle state but keeps
// the option VALUE the bare session name (what gets attached).
func TestSessionOptionsLabelsAndValues(test *testing.T) {
	options := sessionOptions([]workspace.Session{
		{Name: "shell", Attached: true},
		{Name: "opencode", Attached: false, Activity: "1"},
	})
	if len(options) != 2 {
		test.Fatalf("expected 2 options, got %d", len(options))
	}
	if options[0].Value != "shell" || !bytes.Contains([]byte(options[0].Key), []byte("attached")) {
		test.Errorf("attached session option = %+v, want value shell + 'attached' label", options[0])
	}
	if options[1].Value != "opencode" {
		test.Errorf("second option value = %q, want opencode", options[1].Value)
	}
}
