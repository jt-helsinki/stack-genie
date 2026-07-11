package state

import (
	"path/filepath"
	"testing"
)

// TestShowOutsideProject verifies `ai state show` from a directory that is not
// inside any project: the global index is reported and Project stays nil.
func TestShowOutsideProject(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	idx := NewProjectsIndex()
	idx.Projects["my-app"] = ProjectIndexEntry{Path: filepath.Join(home, "projects", "my-app")}
	if err := SaveIndex(idx); err != nil {
		test.Fatal(err)
	}

	snap, err := Show(test.TempDir()) // an unrelated cwd
	if err != nil {
		test.Fatalf("Show: %v", err)
	}
	if len(snap.Projects) != 1 {
		test.Errorf("snapshot projects = %d, want the indexed one", len(snap.Projects))
	}
	if snap.Project != nil {
		test.Errorf("Project should be nil outside a project, got %+v", snap.Project)
	}
}

// TestShowInsideProject verifies the per-project portion: cwd (or an ancestor walk
// from a subdirectory) resolves the project root and reports its runtime state.
func TestShowInsideProject(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	root := filepath.Join(home, "projects", "my-app")
	store := OpenStore(root)
	if err := store.SaveProject(&Project{Name: "my-app", OS: "debian-trixie", Created: "t"}); err != nil {
		test.Fatal(err)
	}
	if err := store.SaveWorkspace(&Workspace{ID: "aip-my-app", Project: "my-app", Status: StatusStarted}); err != nil {
		test.Fatal(err)
	}
	if err := SaveIndex(NewProjectsIndex()); err != nil {
		test.Fatal(err)
	}

	// From a nested dir inside the project: the ancestor walk must find the root.
	snap, err := Show(filepath.Join(root, "src", "deep"))
	if err != nil {
		test.Fatalf("Show: %v", err)
	}
	if snap.Project == nil {
		test.Fatal("Project should be populated inside a project dir")
	}
	if snap.Project.Root != root || snap.Project.Project.Name != "my-app" {
		test.Errorf("project state = %+v, want root %q / name my-app", snap.Project, root)
	}
	if len(snap.Project.Workspaces) != 1 || snap.Project.Workspaces[0].ID != "aip-my-app" {
		test.Errorf("workspaces = %+v, want the saved handle", snap.Project.Workspaces)
	}
	if store.Root() != root {
		test.Errorf("Store.Root = %q, want %q", store.Root(), root)
	}
}
