package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/state"
	"github.com/spf13/cobra"
)

// seedProjectAt creates a project on disk under a fresh HOME and returns its root.
func seedProjectAt(test *testing.T, name string) string {
	test.Helper()
	home := test.TempDir()
	test.Setenv("HOME", home)
	root := filepath.Join(home, "projects", name)
	if err := state.OpenStore(root).SaveProject(&state.Project{Name: name, OS: "debian-trixie", Created: "t"}); err != nil {
		test.Fatal(err)
	}
	index := state.NewProjectsIndex()
	index.Projects[name] = state.ProjectIndexEntry{Path: root}
	if err := state.SaveIndex(index); err != nil {
		test.Fatal(err)
	}
	return root
}

func cmdWithProjectFlag(value string) *cobra.Command {
	cmd := &cobra.Command{Use: "x"}
	cmd.Flags().String("project", value, "")
	return cmd
}

func TestCurrentProjectNameBubblesUp(test *testing.T) {
	root := seedProjectAt(test, "app")
	sub := filepath.Join(root, "src", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		test.Fatal(err)
	}
	test.Chdir(sub)

	name, found, err := currentProjectName()
	if err != nil || !found || name != "app" {
		test.Fatalf("currentProjectName = (%q,%v,%v), want (app,true,nil)", name, found, err)
	}
}

func TestCurrentProjectNameOutsideProject(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Chdir(test.TempDir())

	_, found, err := currentProjectName()
	if err != nil || found {
		test.Fatalf("outside a project want (_,false,nil), got found=%v err=%v", found, err)
	}
}

func TestResolveProjectNamePrecedence(test *testing.T) {
	root := seedProjectAt(test, "app")
	test.Chdir(root)

	// Explicit positional beats both the flag and the CWD.
	if name, err := resolveProjectName(cmdWithProjectFlag("flagproj"), "explicit"); err != nil || name != "explicit" {
		test.Fatalf("explicit should win: %q %v", name, err)
	}
	// --project beats the CWD when there is no positional.
	if name, err := resolveProjectName(cmdWithProjectFlag("flagproj"), ""); err != nil || name != "flagproj" {
		test.Fatalf("flag should win over cwd: %q %v", name, err)
	}
	// CWD project is the default when neither is given.
	if name, err := resolveProjectName(cmdWithProjectFlag(""), ""); err != nil || name != "app" {
		test.Fatalf("cwd default: %q %v", name, err)
	}
}

func TestResolveProjectNameNoneIsExit2(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Chdir(test.TempDir())

	if _, err := resolveProjectName(cmdWithProjectFlag(""), ""); err == nil {
		test.Fatal("expected an error when no project is resolvable")
	}
}
