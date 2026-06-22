package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectRoundTrip(test *testing.T) {
	root := test.TempDir()
	st := OpenStore(root)
	in := &Project{Name: "my-app", OS: "debian-trixie", Created: "2026-06-18T10:00:00Z"}
	if err := st.SaveProject(in); err != nil {
		test.Fatal(err)
	}
	got, err := st.LoadProject()
	if err != nil {
		test.Fatal(err)
	}
	if got.Name != "my-app" || got.OS != "debian-trixie" || got.SchemaVersion != SchemaVersion {
		test.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestUnknownFieldRejected(test *testing.T) {
	root := test.TempDir()
	dir := filepath.Join(root, ".ai-platform")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		test.Fatal(err)
	}
	bad := "schema_version: 1\nname: x\nos: alma\ncreated: test\nbogus: true\n"
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte(bad), 0o644); err != nil {
		test.Fatal(err)
	}
	if _, err := OpenStore(root).LoadProject(); err == nil {
		test.Fatal("expected unknown-field rejection, got nil")
	}
}

func TestBadSchemaVersionRejected(test *testing.T) {
	root := test.TempDir()
	dir := filepath.Join(root, ".ai-platform")
	_ = os.MkdirAll(dir, 0o755)
	bad := "schema_version: 99\nname: x\nos: alma\ncreated: test\n"
	_ = os.WriteFile(filepath.Join(dir, "project.yaml"), []byte(bad), 0o644)
	if _, err := OpenStore(root).LoadProject(); err == nil {
		test.Fatal("expected schema_version rejection")
	}
}

func TestAtomicWriteLeavesNoTempFile(test *testing.T) {
	root := test.TempDir()
	st := OpenStore(root)
	if err := st.SaveProject(&Project{Name: "a", OS: "ubuntu", Created: "test"}); err != nil {
		test.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".ai-platform"))
	if err != nil {
		test.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			test.Fatalf("leftover temp file: %s", entry.Name())
		}
	}
}

func TestWorkspaceListing(test *testing.T) {
	root := test.TempDir()
	st := OpenStore(root)
	if err := st.SaveWorkspace(&Workspace{ID: "aip-app", Project: "app", Status: StatusStarted}); err != nil {
		test.Fatal(err)
	}
	ws, err := st.ListWorkspaces()
	if err != nil || len(ws) != 1 || ws[0].ID != "aip-app" {
		test.Fatalf("workspaces=%+v err=%v", ws, err)
	}
}

func TestListMissingDirsIsEmpty(test *testing.T) {
	st := OpenStore(test.TempDir())
	ws, err := st.ListWorkspaces()
	if err != nil || len(ws) != 0 {
		test.Fatalf("want empty, got %+v err=%v", ws, err)
	}
}

func TestIndexMissingReturnsEmpty(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	idx, err := LoadIndex()
	if err != nil {
		test.Fatal(err)
	}
	if idx.SchemaVersion != SchemaVersion || len(idx.Projects) != 0 {
		test.Fatalf("want empty index, got %+v", idx)
	}
}

func TestIndexRoundTrip(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	idx := NewProjectsIndex()
	idx.Projects["my-app"] = ProjectIndexEntry{Path: "/somewhere/my-app"}
	if err := SaveIndex(idx); err != nil {
		test.Fatal(err)
	}
	got, err := LoadIndex()
	if err != nil {
		test.Fatal(err)
	}
	if got.Projects["my-app"].Path != "/somewhere/my-app" {
		test.Fatalf("index round-trip mismatch: %+v", got)
	}
}

func TestRepairDropsStaleAndKeepsValid(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	// A valid project on disk.
	validRoot := filepath.Join(home, "projects", "good")
	if err := OpenStore(validRoot).SaveProject(&Project{Name: "good", OS: "alma", Created: "test"}); err != nil {
		test.Fatal(err)
	}

	idx := NewProjectsIndex()
	idx.Projects["good"] = ProjectIndexEntry{Path: validRoot}
	idx.Projects["ghost"] = ProjectIndexEntry{Path: filepath.Join(home, "projects", "ghost")} // no project.yaml
	if err := SaveIndex(idx); err != nil {
		test.Fatal(err)
	}

	rep, err := Repair()
	if err != nil {
		test.Fatal(err)
	}
	if len(rep.RemovedProjects) != 1 || rep.RemovedProjects[0] != "ghost" {
		test.Fatalf("expected ghost removed, got %+v", rep.RemovedProjects)
	}

	after, err := LoadIndex()
	if err != nil {
		test.Fatal(err)
	}
	if _, ok := after.Projects["ghost"]; ok {
		test.Fatal("ghost should have been dropped from index")
	}
	if _, ok := after.Projects["good"]; !ok {
		test.Fatal("good should have been kept")
	}
	// run/ shard dirs ensured for the valid project.
	if _, err := os.Stat(filepath.Join(validRoot, ".ai-platform", "run", "workspaces")); err != nil {
		test.Fatalf("run/workspaces not ensured: %v", err)
	}
}
