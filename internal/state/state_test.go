package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectRoundTrip(t *testing.T) {
	root := t.TempDir()
	st := OpenStore(root)
	in := &Project{Name: "my-app", OS: "debian-trixie", Created: "2026-06-18T10:00:00Z"}
	if err := st.SaveProject(in); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadProject()
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "my-app" || got.OS != "debian-trixie" || got.SchemaVersion != SchemaVersion {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".ai-platform")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := `{"schema_version":1,"name":"x","os":"alma","created":"t","bogus":true}`
	if err := os.WriteFile(filepath.Join(dir, "project.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(root).LoadProject(); err == nil {
		t.Fatal("expected unknown-field rejection, got nil")
	}
}

func TestBadSchemaVersionRejected(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".ai-platform")
	_ = os.MkdirAll(dir, 0o755)
	bad := `{"schema_version":99,"name":"x","os":"alma","created":"t"}`
	_ = os.WriteFile(filepath.Join(dir, "project.json"), []byte(bad), 0o644)
	if _, err := OpenStore(root).LoadProject(); err == nil {
		t.Fatal("expected schema_version rejection")
	}
}

func TestAtomicWriteLeavesNoTempFile(t *testing.T) {
	root := t.TempDir()
	st := OpenStore(root)
	if err := st.SaveProject(&Project{Name: "a", OS: "ubuntu", Created: "t"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".ai-platform"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

func TestWorkspaceAndAgentListing(t *testing.T) {
	root := t.TempDir()
	st := OpenStore(root)
	agentName := "review-agent"
	if err := st.SaveWorkspace(&Workspace{ID: "aip-app", Project: "app", Type: WorkspaceProject, Status: StatusStarted}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAgent(&Agent{Name: agentName, Project: "app", State: "active"}); err != nil {
		t.Fatal(err)
	}
	ws, err := st.ListWorkspaces()
	if err != nil || len(ws) != 1 || ws[0].ID != "aip-app" {
		t.Fatalf("workspaces=%+v err=%v", ws, err)
	}
	ag, err := st.ListAgents()
	if err != nil || len(ag) != 1 || ag[0].Name != agentName {
		t.Fatalf("agents=%+v err=%v", ag, err)
	}
}

func TestListMissingDirsIsEmpty(t *testing.T) {
	st := OpenStore(t.TempDir())
	ws, err := st.ListWorkspaces()
	if err != nil || len(ws) != 0 {
		t.Fatalf("want empty, got %+v err=%v", ws, err)
	}
}

func TestIndexMissingReturnsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	idx, err := LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if idx.SchemaVersion != SchemaVersion || len(idx.Projects) != 0 {
		t.Fatalf("want empty index, got %+v", idx)
	}
}

func TestIndexRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	idx := NewProjectsIndex()
	idx.Projects["my-app"] = ProjectIndexEntry{Path: "/somewhere/my-app"}
	if err := SaveIndex(idx); err != nil {
		t.Fatal(err)
	}
	got, err := LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if got.Projects["my-app"].Path != "/somewhere/my-app" {
		t.Fatalf("index round-trip mismatch: %+v", got)
	}
}

func TestRepairDropsStaleAndKeepsValid(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A valid project on disk.
	validRoot := filepath.Join(home, "projects", "good")
	if err := OpenStore(validRoot).SaveProject(&Project{Name: "good", OS: "alma", Created: "t"}); err != nil {
		t.Fatal(err)
	}

	idx := NewProjectsIndex()
	idx.Projects["good"] = ProjectIndexEntry{Path: validRoot}
	idx.Projects["ghost"] = ProjectIndexEntry{Path: filepath.Join(home, "projects", "ghost")} // no project.json
	if err := SaveIndex(idx); err != nil {
		t.Fatal(err)
	}

	rep, err := Repair()
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.RemovedProjects) != 1 || rep.RemovedProjects[0] != "ghost" {
		t.Fatalf("expected ghost removed, got %+v", rep.RemovedProjects)
	}

	after, err := LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Projects["ghost"]; ok {
		t.Fatal("ghost should have been dropped from index")
	}
	if _, ok := after.Projects["good"]; !ok {
		t.Fatal("good should have been kept")
	}
	// run/ shard dirs ensured for the valid project.
	if _, err := os.Stat(filepath.Join(validRoot, ".ai-platform", "run", "workspaces")); err != nil {
		t.Fatalf("run/workspaces not ensured: %v", err)
	}
}
