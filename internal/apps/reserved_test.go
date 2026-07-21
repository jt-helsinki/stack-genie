package apps

import (
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/state"
)

func TestReservedPortsAcrossWorkspaces(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	rootA := filepath.Join(home, "a")
	rootB := filepath.Join(home, "b")
	if err := config.WriteProject(rootA, &config.Config{Apps: []config.AppEntry{{Key: "openwebui", Port: 21000}}}); err != nil {
		test.Fatal(err)
	}
	if err := config.WriteProject(rootB, &config.Config{Apps: []config.AppEntry{{Key: "anythingllm", Port: 21001}}}); err != nil {
		test.Fatal(err)
	}
	index := state.NewProjectsIndex()
	index.Projects["a"] = state.ProjectIndexEntry{Path: rootA}
	index.Projects["b"] = state.ProjectIndexEntry{Path: rootB}
	if err := state.SaveIndex(index); err != nil {
		test.Fatal(err)
	}

	reserved, err := ReservedPortsAcrossWorkspaces()
	if err != nil {
		test.Fatal(err)
	}
	if !reserved[21000] || !reserved[21001] {
		test.Fatalf("reserved = %v, want both 21000 and 21001", reserved)
	}
	if reserved[21002] {
		test.Fatal("21002 should not be reserved")
	}
}

func TestReservedPortsEmptyWhenNoProjects(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	reserved, err := ReservedPortsAcrossWorkspaces()
	if err != nil {
		test.Fatal(err)
	}
	if len(reserved) != 0 {
		test.Fatalf("reserved = %v, want empty", reserved)
	}
}
