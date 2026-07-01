package scope

import (
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/state"
)

// seedProject writes a project at <home>/projects/<name> and indexes it, mirroring
// what `ai project create` records. Returns the project root.
func seedProject(test *testing.T, home, name string) string {
	test.Helper()
	root := filepath.Join(home, "projects", name)
	if err := state.OpenStore(root).SaveProject(&state.Project{Name: name, OS: "debian-trixie", Created: "t"}); err != nil {
		test.Fatal(err)
	}
	index, err := state.LoadIndex()
	if err != nil {
		test.Fatal(err)
	}
	index.Projects[name] = state.ProjectIndexEntry{Path: root}
	if err := state.SaveIndex(index); err != nil {
		test.Fatal(err)
	}
	return root
}

func TestResolveOffersCreateOutsideAnyProject(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	resolution, err := Resolve(test.TempDir()) // a directory that is not a project
	if err != nil {
		test.Fatal(err)
	}
	if !resolution.OfferCreate {
		test.Error("cwd outside any project must offer create")
	}
	if resolution.DefaultProject != "" {
		test.Errorf("no default project expected, got %q", resolution.DefaultProject)
	}
}

func TestResolveDefaultsToProjectAtCwd(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	root := seedProject(test, home, "app")

	resolution, err := Resolve(root)
	if err != nil {
		test.Fatal(err)
	}
	if resolution.OfferCreate {
		test.Error("inside a project must NOT offer create")
	}
	if resolution.DefaultProject != "app" {
		test.Errorf("default project = %q, want app", resolution.DefaultProject)
	}
	if len(resolution.Projects) != 1 || resolution.Projects[0].Name != "app" {
		test.Errorf("projects switcher = %+v, want [app]", resolution.Projects)
	}
}

func TestResolveListsAllProjectsAndDefaultsFromAncestor(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	seedProject(test, home, "beta")
	root := seedProject(test, home, "alpha")

	// From a SUBdirectory of alpha, the ancestor project is the default, and both
	// projects appear in the switcher (sorted).
	resolution, err := Resolve(filepath.Join(root, "cmd", "deep"))
	if err != nil {
		test.Fatal(err)
	}
	if resolution.DefaultProject != "alpha" {
		test.Errorf("default from ancestor = %q, want alpha", resolution.DefaultProject)
	}
	if len(resolution.Projects) != 2 || resolution.Projects[0].Name != "alpha" || resolution.Projects[1].Name != "beta" {
		test.Errorf("switcher = %+v, want sorted [alpha beta]", resolution.Projects)
	}
}

func TestValidateCreateTarget(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	root := seedProject(test, home, "app")

	// A directory that is itself a project root is rejected.
	if err := ValidateCreateTarget(root); err == nil {
		test.Error("creating in an existing project root must be rejected")
	}
	// A child (nested inside a project directory) is rejected — no nested workspaces.
	if err := ValidateCreateTarget(filepath.Join(root, "child")); err == nil {
		test.Error("a directory nested inside a workspace must be rejected")
	}
	// A DEEPLY nested, not-yet-existing path inside a project is also rejected
	// (validation walks the existing ancestors).
	if err := ValidateCreateTarget(filepath.Join(root, "a", "b", "c")); err == nil {
		test.Error("a deep non-existent path inside a workspace must be rejected")
	}
	// A parent of a project directory (it merely contains a project deeper in its
	// tree) is allowed.
	if err := ValidateCreateTarget(filepath.Dir(root)); err != nil {
		test.Errorf("parent of a project dir must be allowed: %v", err)
	}
	// Empty target is rejected.
	if err := ValidateCreateTarget(""); err == nil {
		test.Error("empty target must be rejected")
	}
}

func TestIsProjectRoot(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	root := seedProject(test, home, "app")

	if !IsProjectRoot(root) {
		test.Error("seeded project dir must be a project root")
	}
	if IsProjectRoot(filepath.Join(root, "child")) {
		test.Error("a child dir is not a project root")
	}
	if IsProjectRoot(test.TempDir()) {
		test.Error("a plain dir is not a project root")
	}
}
