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
// `ai create` records) so it is a real scope.IsProjectRoot directory.
func seedProject(test *testing.T, parent, name string) string {
	test.Helper()
	root := filepath.Join(parent, name)
	if err := state.OpenStore(root).SaveProject(&state.Project{Name: name, OS: "debian-trixie", Created: "t"}); err != nil {
		test.Fatal(err)
	}
	return root
}

// enterLocation types path into the location field and presses enter, returning
// the resulting command (nil when the view stays put on a validation error).
func enterLocation(view *Create, path string) tea.Cmd {
	view.input.SetValue(path)
	return view.Update(tea.KeyMsg{Type: tea.KeyEnter})
}

func TestCreateStartsInGivenDirectory(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	start := test.TempDir()

	view := NewCreate(start)
	if got, want := view.input.Value(), ensureTrailingSep(start); got != want {
		test.Fatalf("new-workspace field starts at %q, want the given (current) directory %q", got, want)
	}
}

func TestCreateCreatesMissingDirsAndConfirms(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	base := test.TempDir()
	target := filepath.Join(base, "a", "b", "fresh") // does not exist yet

	view := NewCreate(base)
	cmd := enterLocation(view, target)
	if view.validationErr != nil {
		test.Fatalf("a valid new path must not error: %v", view.validationErr)
	}
	if cmd == nil {
		test.Fatal("confirming a valid location must emit a command")
	}
	confirmed, ok := cmd().(CreateConfirmedMsg)
	if !ok {
		test.Fatalf("want CreateConfirmedMsg, got %#v", cmd())
	}
	if confirmed.Dir != target {
		test.Fatalf("confirmed dir = %q, want %q", confirmed.Dir, target)
	}
	// The path (and its intermediate folders) must have been created.
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		test.Fatalf("the entered path and intermediate folders must be created: %v", err)
	}
}

func TestCreateExpandsTilde(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	view := NewCreate(test.TempDir())
	cmd := enterLocation(view, "~/proj/x")
	if cmd == nil {
		test.Fatalf("expected a confirm command; validation err = %v", view.validationErr)
	}
	confirmed, ok := cmd().(CreateConfirmedMsg)
	if !ok {
		test.Fatalf("want CreateConfirmedMsg, got %#v", cmd())
	}
	if want := filepath.Join(home, "proj", "x"); confirmed.Dir != want {
		test.Fatalf("~ expanded to %q, want %q", confirmed.Dir, want)
	}
}

func TestCreateRejectsExistingWorkspaceRoot(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	base := test.TempDir()
	root := seedProject(test, base, "app")

	view := NewCreate(base)
	cmd := enterLocation(view, root)

	if cmd != nil {
		if _, ok := cmd().(CreateConfirmedMsg); ok {
			test.Fatal("an existing workspace root must not be confirmed")
		}
	}
	if view.validationErr == nil {
		test.Fatal("an existing workspace root must set a validation error")
	}
	if !strings.Contains(view.View(), "already a workspace") {
		test.Errorf("the validation error should show inline, got view:\n%s", view.View())
	}
}

func TestCreateRejectsPathNestedInsideWorkspace(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	base := test.TempDir()
	root := seedProject(test, base, "app")
	nested := filepath.Join(root, "sub", "dir") // inside an existing workspace

	view := NewCreate(base)
	cmd := enterLocation(view, nested)

	if cmd != nil {
		if _, ok := cmd().(CreateConfirmedMsg); ok {
			test.Fatal("a path nested inside a workspace must not be confirmed")
		}
	}
	if view.validationErr == nil {
		test.Fatal("a nested path must set a validation error")
	}
	if !strings.Contains(view.View(), "inside an existing workspace") {
		test.Errorf("the inline error should name the nesting, got view:\n%s", view.View())
	}
	// A rejected path must NOT be created.
	if _, err := os.Stat(nested); !os.IsNotExist(err) {
		test.Errorf("a rejected nested path must not be created (stat err = %v)", err)
	}
}

func TestCreateAutocompletionPrefixMatchesChildDirs(test *testing.T) {
	base := test.TempDir()
	test.Setenv("HOME", test.TempDir())
	for _, name := range []string{"projects", "photos"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o755); err != nil {
			test.Fatal(err)
		}
	}

	suggestions := dirSuggestions(filepath.Join(base, "pro"))
	wantProjects := ensureTrailingSep(filepath.Join(base, "projects"))
	found := false
	for _, suggestion := range suggestions {
		if suggestion == wantProjects {
			found = true
		}
		if strings.Contains(suggestion, "photos") {
			test.Errorf("prefix 'pro' must not suggest photos/: %v", suggestions)
		}
	}
	if !found {
		test.Errorf("prefix 'pro' should suggest projects/, got %v", suggestions)
	}
}

func TestCreateAutocompletionExpandsTilde(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "work"), 0o755); err != nil {
		test.Fatal(err)
	}

	suggestions := dirSuggestions("~/")
	want := ensureTrailingSep(filepath.Join(home, "work"))
	found := false
	for _, suggestion := range suggestions {
		if suggestion == want {
			found = true
		}
	}
	if !found {
		test.Errorf("~/ should list home children as absolute paths, got %v", suggestions)
	}
}

func TestCreateCancels(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	view := NewCreate(test.TempDir())
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		test.Fatal("esc must emit a cancel command")
	}
	if _, ok := cmd().(CreateCancelledMsg); !ok {
		test.Fatalf("want CreateCancelledMsg, got %#v", cmd())
	}
}
