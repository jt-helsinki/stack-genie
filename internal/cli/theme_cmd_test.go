package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/ui"
)

// runTheme drives a single `ai theme [args...]` invocation against a fresh
// command tree (no TTY in tests, so the non-interactive branches run), returning
// the exit code the command set.
func runTheme(test *testing.T, args ...string) int {
	test.Helper()
	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard}
	exit := output.ExitOK
	cmd := newThemeCmd(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		test.Fatalf("theme %v returned error: %v", args, err)
	}
	return exit
}

// distinctTheme returns a selectable theme name other than the active one, so a
// set operation actually changes state.
func distinctTheme(active string) string {
	for _, name := range ui.ThemeNames() {
		if name != active {
			return name
		}
	}
	return active
}

// No-arg, non-interactive `ai theme` reports the active theme and changes nothing.
func TestThemeShowNonInteractive(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if exit := runTheme(test); exit != output.ExitOK {
		test.Fatalf("theme show exit = %d, want 0", exit)
	}
}

// `ai theme <valid>` persists the choice per host (exit 0), reflected by LoadThemeName.
func TestThemeSetValidPersists(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	target := distinctTheme(ui.CurrentTheme())
	if exit := runTheme(test, target); exit != output.ExitOK {
		test.Fatalf("theme set %q exit = %d, want 0", target, exit)
	}
	if got := ui.LoadThemeName(); got != target {
		test.Fatalf("persisted theme = %q, want %q", got, target)
	}
}

// An unknown theme name is invalid input (exit 2) and persists nothing.
func TestThemeSetUnknownIsExit2(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if exit := runTheme(test, "no-such-theme-xyz"); exit != output.ExitInvalidInput {
		test.Fatalf("unknown theme exit = %d, want %d", exit, output.ExitInvalidInput)
	}
}

// themeResult.Human lists all themes and marks the active one.
func TestThemeResultHuman(test *testing.T) {
	names := ui.ThemeNames()
	result := themeResult{Theme: names[0], Available: names}
	human := result.Human()
	for _, name := range names {
		if !strings.Contains(human, name) {
			test.Fatalf("Human() missing theme %q:\n%s", name, human)
		}
	}
	if !strings.Contains(human, "active") {
		test.Fatalf("Human() should mark the active theme:\n%s", human)
	}
}
