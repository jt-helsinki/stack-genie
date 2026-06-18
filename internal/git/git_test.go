package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func requireGit(test *testing.T) {
	test.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		test.Skip("git not installed")
	}
}

func TestInitCreatesRepo(test *testing.T) {
	requireGit(test)
	dir := filepath.Join(test.TempDir(), "proj")
	if err := RealRunner().Init(dir); err != nil {
		test.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		test.Fatalf("expected .git after init: %v", err)
	}
}

func TestCloneFromLocalRepo(test *testing.T) {
	requireGit(test)
	source := filepath.Join(test.TempDir(), "source")
	if err := RealRunner().Init(source); err != nil {
		test.Fatal(err)
	}
	destination := filepath.Join(test.TempDir(), "clone")
	if err := RealRunner().Clone(source, destination); err != nil {
		test.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, ".git")); err != nil {
		test.Fatalf("expected .git after clone: %v", err)
	}
}
