package layout_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/layout"
)

// expectedDirs mirrors the documented global subdirectories under ~/.ai-platform
// (repo-layout §5). Kept in the test so a change to the production list is a
// deliberate, reviewed edit on both sides.
var expectedDirs = []string{
	"agents",
	"audit",
	"cache",
	"config",
	"logs",
	"overlays",
	"prompts",
	"skills",
	"templates/dockerfiles",
	"templates/stacks",
	"tools",
}

func TestEnsureCreatesTreeAndReturnsRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root, err := layout.Ensure()
	if err != nil {
		t.Fatalf("Ensure() error: %v", err)
	}

	wantRoot := filepath.Join(home, ".ai-platform")
	if root != wantRoot {
		t.Errorf("Ensure() root = %q, want %q", root, wantRoot)
	}

	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("platform root not created: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("platform root %q is not a directory", root)
	}

	for _, dir := range expectedDirs {
		full := filepath.Join(root, dir)
		dirInfo, statErr := os.Stat(full)
		if statErr != nil {
			t.Errorf("expected subdir %q missing: %v", dir, statErr)
			continue
		}
		if !dirInfo.IsDir() {
			t.Errorf("expected subdir %q is not a directory", dir)
		}
	}
}

func TestEnsureRootedAtRedirectedHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root, err := layout.Ensure()
	if err != nil {
		t.Fatalf("Ensure() error: %v", err)
	}
	relative, err := filepath.Rel(home, root)
	if err != nil {
		t.Fatalf("Rel(%q, %q) error: %v", home, root, err)
	}
	if relative != ".ai-platform" {
		t.Errorf("Ensure() root not directly under HOME: rel = %q", relative)
	}
}

func TestEnsureDoesNotCreateProjectsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if _, err := layout.Ensure(); err != nil {
		t.Fatalf("Ensure() error: %v", err)
	}
	// Projects live in the user's cwd, so setup must not create ~/projects.
	if _, err := os.Stat(filepath.Join(home, "projects")); !os.IsNotExist(err) {
		t.Errorf("Ensure() unexpectedly created ~/projects (stat err = %v)", err)
	}
}

func TestEnsureIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	firstRoot, err := layout.Ensure()
	if err != nil {
		t.Fatalf("first Ensure() error: %v", err)
	}

	// Drop a sentinel file inside an existing subdir; a re-run must not clobber it.
	sentinel := filepath.Join(firstRoot, "config", "sentinel.txt")
	if writeErr := os.WriteFile(sentinel, []byte("keep me"), 0o644); writeErr != nil {
		t.Fatalf("writing sentinel: %v", writeErr)
	}

	secondRoot, err := layout.Ensure()
	if err != nil {
		t.Fatalf("second Ensure() error: %v", err)
	}
	if secondRoot != firstRoot {
		t.Errorf("Ensure() root changed across calls: %q != %q", secondRoot, firstRoot)
	}

	contents, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("sentinel disappeared after second Ensure(): %v", err)
	}
	if string(contents) != "keep me" {
		t.Errorf("sentinel clobbered: got %q", string(contents))
	}
}
