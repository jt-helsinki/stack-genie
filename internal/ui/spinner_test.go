package ui

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestRunWithSpinnerEmbeddedRunsDirectly verifies that inside the `ai ui` terminal
// pane (EmbeddedTerminalEnv set) RunWithSpinner runs the work directly — no nested
// bubbletea program (whose renderer would emit cursor-control/hide-cursor sequences
// and fight the child's streaming progress) — while still running the work, letting
// the child's output through, and printing the title + a final status line.
func TestRunWithSpinnerEmbeddedRunsDirectly(test *testing.T) {
	test.Setenv(EmbeddedTerminalEnv, "1")
	var out bytes.Buffer
	ran := false
	err := RunWithSpinner(&out, "starting workspace temp", func() error {
		ran = true
		_, _ = out.WriteString("layer 1/9 downloading\n")
		return nil
	})
	if err != nil {
		test.Fatalf("unexpected error: %v", err)
	}
	if !ran {
		test.Fatal("work was not run")
	}
	rendered := out.String()
	if !strings.Contains(rendered, "starting workspace temp") {
		test.Errorf("title missing from output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "layer 1/9 downloading") {
		test.Errorf("child progress missing from output:\n%s", rendered)
	}
	// The bubbletea renderer hides the cursor (\x1b[?25l) when it runs; embedded mode
	// must NOT start it, so that sequence must be absent.
	if strings.Contains(rendered, "\x1b[?25l") {
		test.Errorf("a bubbletea spinner ran inside the embedded pane:\n%q", rendered)
	}
}

// TestRunWithSpinnerEmbeddedReportsFailure verifies the work's error is returned and
// the failure line is printed in embedded mode.
func TestRunWithSpinnerEmbeddedReportsFailure(test *testing.T) {
	test.Setenv(EmbeddedTerminalEnv, "1")
	var out bytes.Buffer
	wantErr := errors.New("boom")
	err := RunWithSpinner(&out, "starting workspace temp", func() error { return wantErr })
	if !errors.Is(err, wantErr) {
		test.Fatalf("got %v, want %v", err, wantErr)
	}
	if !strings.Contains(out.String(), "starting workspace temp") {
		test.Errorf("title missing from failure output:\n%s", out.String())
	}
}

// TestSpinnerModelQuitsOnCtrlC verifies ctrl+c quits the bubbletea program with
// ErrInterrupted — without this, the raw-mode terminal swallows the keystroke as a
// meaningless key event and the spinner never stops.
func TestSpinnerModelQuitsOnCtrlC(test *testing.T) {
	model := spinnerModel{spinner: newSpinner(), title: "t", work: func() error { return nil }}
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		test.Fatal("ctrl+c must return a command (tea.Quit)")
	}
	result := updated.(spinnerModel)
	if !result.done || !errors.Is(result.err, ErrInterrupted) {
		test.Errorf("ctrl+c must mark done with ErrInterrupted, got done=%v err=%v", result.done, result.err)
	}
}

// A non-ctrl+c key must NOT quit or set an error — it just falls through to the
// spinner's own tick handling, harmlessly ignored.
func TestSpinnerModelIgnoresOtherKeys(test *testing.T) {
	model := spinnerModel{spinner: newSpinner(), title: "t", work: func() error { return nil }}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	result := updated.(spinnerModel)
	if result.done || result.err != nil {
		test.Errorf("a plain key must not quit or set an error, got done=%v err=%v", result.done, result.err)
	}
}
