package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/output"
)

func TestServiceStateLabel(test *testing.T) {
	if serviceStateLabel("") != "" {
		test.Fatal("empty state should render empty")
	}
	// Whatever the styling, the state word itself must survive.
	for _, state := range []string{"running", "stopped", "disabled", "degraded"} {
		if !strings.Contains(serviceStateLabel(state), state) {
			test.Fatalf("serviceStateLabel(%q) dropped the word: %q", state, serviceStateLabel(state))
		}
	}
}

func TestValueOrEmpty(test *testing.T) {
	if valueOrEmpty("") != "" {
		test.Fatal("empty value should render empty")
	}
	if !strings.Contains(valueOrEmpty("http://x"), "http://x") {
		test.Fatalf("non-empty value should be rendered: %q", valueOrEmpty("http://x"))
	}
}

func TestComposeResultHuman(test *testing.T) {
	human := composeResult{Path: "/tmp/compose.yaml"}.Human()
	for _, want := range []string{"/tmp/compose.yaml", "docker compose"} {
		if !strings.Contains(human, want) {
			test.Fatalf("composeResult Human missing %q:\n%s", want, human)
		}
	}
}

// `ai services compose` writes the debug docker-compose artifact under HOME (exit 0).
func TestServicesComposeWritesFile(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := output.ExitOK
	cmd := newServicesComposeCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("services compose error: %v", err)
	}
	if exit != output.ExitOK {
		test.Fatalf("services compose exit = %d, want 0", exit)
	}
	path := os.Getenv("HOME") + "/.ai-platform/docker-compose.yaml"
	if _, statErr := os.Stat(path); statErr != nil {
		test.Fatalf("compose file not written at %q: %v", path, statErr)
	}
}

// `ai services` with no subcommand prints help and leaves the exit code at OK.
func TestServicesNoSubcommandPrintsHelp(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := output.ExitOK
	cmd := newServicesCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("bare services error: %v", err)
	}
	if exit != output.ExitOK {
		test.Fatalf("bare services exit = %d, want 0", exit)
	}
}
