package views

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/state"
)

// seedProject writes a project root at <parent>/<name> (mirroring what
// `ai project create` records) so it is a real scope.IsProjectRoot directory.
func seedProject(test *testing.T, parent, name string) string {
	test.Helper()
	root := filepath.Join(parent, name)
	if err := state.OpenStore(root).SaveProject(&state.Project{Name: name, OS: "debian-trixie", Created: "t"}); err != nil {
		test.Fatal(err)
	}
	return root
}

// drivePickerInto runs the create view's picker far enough to populate and
// highlight the single directory entry under its starting directory, then returns
// the result of pressing enter on it. The starting directory must contain exactly
// one visible (non-hidden) subdirectory so the highlighted entry is deterministic.
func drivePickerInto(test *testing.T, view *Create) tea.Cmd {
	test.Helper()
	view.SetSize(0, 20)
	loaded := view.Init()() // readDir of the starting directory
	_ = view.Update(loaded) // populate the file list, highlight the first (only) entry
	return view.Update(tea.KeyMsg{Type: tea.KeyEnter})
}

func TestCreateConfirmsPlainDirectory(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	parent := test.TempDir()
	target := filepath.Join(parent, "fresh")
	if err := os.MkdirAll(target, 0o755); err != nil {
		test.Fatal(err)
	}

	view := NewCreate(parent)
	cmd := drivePickerInto(test, view)
	if cmd == nil {
		test.Fatal("selecting a plain directory must emit a command")
	}
	confirmed, ok := cmd().(CreateConfirmedMsg)
	if !ok {
		test.Fatalf("want CreateConfirmedMsg, got %#v", cmd())
	}
	if confirmed.Dir != target {
		test.Fatalf("confirmed dir = %q, want %q", confirmed.Dir, target)
	}
	if view.validationErr != nil {
		test.Fatalf("a plain directory must not set a validation error: %v", view.validationErr)
	}
}

func TestCreateRejectsExistingProjectRoot(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	parent := test.TempDir()
	seedProject(test, parent, "app") // <parent>/app is a project root

	view := NewCreate(parent)
	cmd := drivePickerInto(test, view)

	// The picker still records m.Path/targetDir, but the view must NOT confirm a
	// project-root directory; instead it records the validation error inline.
	if cmd != nil {
		if _, ok := cmd().(CreateConfirmedMsg); ok {
			test.Fatal("an existing project root must not be confirmed")
		}
	}
	if view.validationErr == nil {
		test.Fatal("selecting an existing project root must set a validation error")
	}
	if !strings.Contains(view.View(), "already a workspace") {
		test.Errorf("the validation error should show inline, got view:\n%s", view.View())
	}
}

func TestCreateCancels(test *testing.T) {
	view := NewCreate(test.TempDir())
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		test.Fatal("esc must emit a cancel command")
	}
	if _, ok := cmd().(CreateCancelledMsg); !ok {
		test.Fatalf("want CreateCancelledMsg, got %#v", cmd())
	}
}
