package state

import (
	"os"
	"path/filepath"
	"testing"
)

// markProject writes the .ai-platform/project.yaml marker FindProjectRoot walks for.
func markProject(test *testing.T, dir string) {
	test.Helper()
	platform := filepath.Join(dir, ".ai-platform")
	if err := os.MkdirAll(platform, 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(platform, "project.yaml"), []byte("schema_version: 1\n"), 0o644); err != nil {
		test.Fatal(err)
	}
}

// FindProjectRoot returns the project root when started at the root itself and when
// started from a nested subdirectory, walking up to the nearest ancestor with the marker.
func TestFindProjectRootWalksUp(test *testing.T) {
	root := test.TempDir()
	markProject(test, root)

	// Started at the root itself.
	got, ok := FindProjectRoot(root)
	if !ok || got != root {
		test.Fatalf("FindProjectRoot(root) = (%q, %v), want (%q, true)", got, ok, root)
	}

	// Started from a nested subdirectory: it climbs to the marked ancestor.
	nested := filepath.Join(root, "src", "pkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		test.Fatal(err)
	}
	got, ok = FindProjectRoot(nested)
	if !ok || got != root {
		test.Fatalf("FindProjectRoot(nested) = (%q, %v), want (%q, true)", got, ok, root)
	}
}

// With no ancestor holding the marker, FindProjectRoot reports not-found (ok=false, "").
func TestFindProjectRootNotFound(test *testing.T) {
	dir := filepath.Join(test.TempDir(), "plain", "child")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		test.Fatal(err)
	}
	got, ok := FindProjectRoot(dir)
	if ok || got != "" {
		test.Fatalf("FindProjectRoot(unmarked) = (%q, %v), want (\"\", false)", got, ok)
	}
}
