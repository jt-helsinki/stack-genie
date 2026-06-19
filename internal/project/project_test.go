package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/overlay"
	"github.com/jt-helsinki/ideal-robot/internal/state"
	"github.com/jt-helsinki/ideal-robot/internal/templates"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
)

func withTemplates(test *testing.T) {
	test.Helper()
	test.Setenv("HOME", test.TempDir())
	if err := templates.Install(); err != nil {
		test.Fatal(err)
	}
}

func sampleSpec() Spec {
	return Spec{
		Name:        "my-app",
		OS:          "debian-trixie",
		Stacks:      []string{"go"},
		AgentCLIs:   []string{"opencode", "codex"},
		DefaultTool: "opencode",
	}
}

func TestScaffoldWritesArtifactsAndIndex(test *testing.T) {
	withTemplates(test)

	root, err := Scaffold(sampleSpec(), "2026-06-18T00:00:00Z")
	if err != nil {
		test.Fatal(err)
	}

	dockerfile, err := os.ReadFile(filepath.Join(root, ".ai-platform", "Dockerfile"))
	if err != nil {
		test.Fatal(err)
	}
	for _, fragment := range []string{"FROM debian:trixie-slim", "# stack: go", "# agent CLI: opencode", "# agent CLI: codex"} {
		if !strings.Contains(string(dockerfile), fragment) {
			test.Errorf("Dockerfile missing %q", fragment)
		}
	}

	for _, file := range []string{"config.yaml", "profile.yaml", "project.json", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(root, ".ai-platform", file)); err != nil {
			test.Errorf("missing %s: %v", file, err)
		}
	}

	profile, _ := os.ReadFile(filepath.Join(root, ".ai-platform", "profile.yaml"))
	if !strings.Contains(string(profile), "go") {
		test.Errorf("profile.yaml missing stack: %s", profile)
	}

	index, err := state.LoadIndex()
	if err != nil {
		test.Fatal(err)
	}
	if _, ok := index.Projects["my-app"]; !ok {
		test.Fatal("project not registered in index")
	}
}

func TestScaffoldInvalidName(test *testing.T) {
	withTemplates(test)
	spec := sampleSpec()
	spec.Name = "-bad-"
	if _, err := Scaffold(spec, "t"); !errors.Is(err, ErrInvalidName) {
		test.Fatalf("want ErrInvalidName, got %v", err)
	}
}

func TestScaffoldDuplicate(test *testing.T) {
	withTemplates(test)
	if _, err := Scaffold(sampleSpec(), "t"); err != nil {
		test.Fatal(err)
	}
	if _, err := Scaffold(sampleSpec(), "t"); !errors.Is(err, ErrAlreadyExists) {
		test.Fatalf("want ErrAlreadyExists, got %v", err)
	}
}

func TestListReturnsProjects(test *testing.T) {
	withTemplates(test)
	if _, err := Scaffold(sampleSpec(), "t"); err != nil {
		test.Fatal(err)
	}
	entries, err := List()
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "my-app" || entries[0].OS != "debian-trixie" {
		test.Fatalf("list = %+v", entries)
	}
}

func TestDeleteKeepsSourceClearsRun(test *testing.T) {
	withTemplates(test)
	root, err := Scaffold(sampleSpec(), "t")
	if err != nil {
		test.Fatal(err)
	}
	// Simulate run/ state then delete (no purge).
	_ = os.MkdirAll(filepath.Join(root, ".ai-platform", "run", "workspaces"), 0o755)
	if err := Delete("my-app", false); err != nil {
		test.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".ai-platform", "Dockerfile")); err != nil {
		test.Errorf("tracked source should be kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".ai-platform", "run")); !os.IsNotExist(err) {
		test.Errorf("run/ should be cleared, got %v", err)
	}
	index, _ := state.LoadIndex()
	if _, ok := index.Projects["my-app"]; ok {
		test.Error("project should be removed from index")
	}
}

func TestDeletePurgeRemovesSource(test *testing.T) {
	withTemplates(test)
	root, err := Scaffold(sampleSpec(), "t")
	if err != nil {
		test.Fatal(err)
	}
	if err := Delete("my-app", true); err != nil {
		test.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		test.Errorf("--purge should remove the source tree, got %v", err)
	}
}

func TestDeleteRemovesOverlay(test *testing.T) {
	withTemplates(test)
	if _, err := Scaffold(sampleSpec(), "t"); err != nil {
		test.Fatal(err)
	}
	// Simulate a provisioned workspace's persistent overlay.
	workspaceID := workspace.Name("my-app")
	if _, err := overlay.Ensure(workspaceID); err != nil {
		test.Fatal(err)
	}
	if err := Delete("my-app", false); err != nil {
		test.Fatal(err)
	}
	// Deleting the project is permanent removal (arch §26): the overlay goes too.
	present, err := overlay.Exists(workspaceID)
	if err != nil || present {
		test.Fatalf("delete should remove the overlay: present=%v err=%v", present, err)
	}
}

func TestDeleteUnknown(test *testing.T) {
	withTemplates(test)
	if err := Delete("ghost", false); !errors.Is(err, ErrUnknownProject) {
		test.Fatalf("want ErrUnknownProject, got %v", err)
	}
}
