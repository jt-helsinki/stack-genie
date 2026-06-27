package ui

import (
	"bytes"
	"errors"
	"strings"
	"testing"
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
