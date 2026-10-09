package views

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestCreateStartDir: a non-empty argument is returned verbatim; an empty one falls
// back to the working directory.
func TestCreateStartDir(test *testing.T) {
	if got := createStartDir("/some/where"); got != "/some/where" {
		test.Errorf("createStartDir(%q) = %q, want it returned verbatim", "/some/where", got)
	}
	wd, err := os.Getwd()
	if err != nil {
		test.Fatalf("Getwd: %v", err)
	}
	if got := createStartDir(""); got != wd {
		test.Errorf("createStartDir(\"\") = %q, want the working directory %q", got, wd)
	}
}

// TestCreateWizardBackNavigation walks the early static steps forward, then uses
// shift+tab (prev) to step back one at a time, verifying the transitions and that
// back at the first step is a no-op. Complements the forward-flow wizard tests.
func TestCreateWizardBackNavigation(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	base := test.TempDir()
	target := filepath.Join(base, "demo-ws")

	wizard := NewCreate(base, nil)
	wizard.SetSize(80, 24)
	enter := func() { wizard.Update(tea.KeyMsg{Type: tea.KeyEnter}) }
	back := func() { wizard.Update(tea.KeyMsg{Type: tea.KeyShiftTab}) }

	wizard.location.input.SetValue(target)
	enter() // location -> name
	if wizard.step != stepName {
		test.Fatalf("after location enter, step = %d, want stepName (%d)", wizard.step, stepName)
	}
	wizard.name.input.SetValue("demo-ws")
	enter() // name -> shell
	if wizard.step != stepShell {
		test.Fatalf("after name enter, step = %d, want stepShell (%d)", wizard.step, stepShell)
	}
	enter() // shell -> agents
	if wizard.step != stepAgents {
		test.Fatalf("after shell enter, step = %d, want stepAgents (%d)", wizard.step, stepAgents)
	}

	back() // agents -> shell
	if wizard.step != stepShell {
		test.Fatalf("after back, step = %d, want stepShell (%d)", wizard.step, stepShell)
	}
	back() // shell -> name
	if wizard.step != stepName {
		test.Fatalf("after back, step = %d, want stepName (%d)", wizard.step, stepName)
	}
	back() // name -> location
	if wizard.step != stepLocation {
		test.Fatalf("after back, step = %d, want stepLocation (%d)", wizard.step, stepLocation)
	}
	back() // location -> location (no-op at the first step)
	if wizard.step != stepLocation {
		test.Fatalf("back at the first step must be a no-op, step = %d", wizard.step)
	}
}
