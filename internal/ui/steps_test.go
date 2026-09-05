package ui

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestStepsModelQuitsOnCtrlC verifies ctrl+c quits the bubbletea program with
// ErrInterrupted — without this, the raw-mode terminal swallows the keystroke as a
// meaningless key event and the checklist never stops (see spinnerModel's twin).
func TestStepsModelQuitsOnCtrlC(test *testing.T) {
	model := stepsModel{spinner: newSpinner(), title: "t"}
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		test.Fatal("ctrl+c must return a command (tea.Quit)")
	}
	result := updated.(stepsModel)
	if !result.done || !errors.Is(result.err, ErrInterrupted) {
		test.Errorf("ctrl+c must mark done with ErrInterrupted, got done=%v err=%v", result.done, result.err)
	}
}

// A non-ctrl+c key must NOT quit or set an error — it just falls through to the
// spinner's own tick handling, harmlessly ignored.
func TestStepsModelIgnoresOtherKeys(test *testing.T) {
	model := stepsModel{spinner: newSpinner(), title: "t"}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	result := updated.(stepsModel)
	if result.done || result.err != nil {
		test.Errorf("a plain key must not quit or set an error, got done=%v err=%v", result.done, result.err)
	}
}
