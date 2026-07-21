package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedWorkspace writes a minimal .ai-platform/project.yaml marker at dir so it looks
// like an existing workspace to the location checks (which only os.Stat that file).
func seedWorkspace(test *testing.T, dir string) {
	test.Helper()
	platform := filepath.Join(dir, ".ai-platform")
	if err := os.MkdirAll(platform, 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(platform, "project.yaml"), []byte("schema_version: 1\n"), 0o644); err != nil {
		test.Fatal(err)
	}
}

// A fresh, empty directory is a valid location for a new workspace.
func TestValidateNewLocationFreshDir(test *testing.T) {
	dir := filepath.Join(test.TempDir(), "fresh")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		test.Fatal(err)
	}
	if err := ValidateNewLocation(dir); err != nil {
		test.Fatalf("fresh empty dir should be valid, got %v", err)
	}
}

// A directory that IS itself a workspace is rejected (wraps ErrAlreadyExists).
func TestValidateNewLocationRejectsExistingWorkspace(test *testing.T) {
	dir := test.TempDir()
	seedWorkspace(test, dir)
	err := ValidateNewLocation(dir)
	if !errors.Is(err, ErrAlreadyExists) {
		test.Fatalf("a workspace dir should be rejected with ErrAlreadyExists, got %v", err)
	}
}

// A directory NESTED inside a workspace is rejected, and the message names the ancestor.
func TestValidateNewLocationRejectsNested(test *testing.T) {
	ancestor := test.TempDir()
	seedWorkspace(test, ancestor)
	nested := filepath.Join(ancestor, "sub", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		test.Fatal(err)
	}
	err := ValidateNewLocation(nested)
	if !errors.Is(err, ErrAlreadyExists) {
		test.Fatalf("a dir nested in a workspace should be rejected with ErrAlreadyExists, got %v", err)
	}
	if !strings.Contains(err.Error(), ancestor) {
		test.Errorf("error %q should mention the ancestor workspace %q", err.Error(), ancestor)
	}
}

// Sibling directories under a plain (non-workspace) parent are each valid — no ancestor
// holds a project.yaml.
func TestValidateNewLocationSiblingsPass(test *testing.T) {
	parent := test.TempDir() // parent is NOT a workspace
	for _, name := range []string{"one", "two"} {
		dir := filepath.Join(parent, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			test.Fatal(err)
		}
		if err := ValidateNewLocation(dir); err != nil {
			test.Errorf("sibling dir %q under a non-workspace parent should be valid, got %v", dir, err)
		}
	}
}
