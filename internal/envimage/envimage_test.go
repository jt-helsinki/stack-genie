package envimage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/templates"
)

// installTemplates lays the embedded templates under a throwaway HOME so Compose
// can read them.
func installTemplates(test *testing.T) {
	test.Helper()
	test.Setenv("HOME", test.TempDir())
	if err := templates.Install(); err != nil {
		test.Fatal(err)
	}
}

func TestComposeSelectedOnly(test *testing.T) {
	installTemplates(test)

	dockerfile, err := Compose("debian-trixie", []string{"go"}, []string{"opencode"})
	if err != nil {
		test.Fatal(err)
	}
	mustContain := []string{
		"FROM debian:trixie-slim", // base
		"gh",                      // base tooling
		"# stack: go",
		"golang-go",
		"# agent CLI: opencode",
		"opencode-ai",
	}
	for _, fragment := range mustContain {
		if !strings.Contains(dockerfile, fragment) {
			test.Errorf("composed Dockerfile missing %q:\n%s", fragment, dockerfile)
		}
	}
	// Unselected stacks/CLIs must not leak in.
	for _, fragment := range []string{"# stack: node", "# stack: python", "# agent CLI: gemini"} {
		if strings.Contains(dockerfile, fragment) {
			test.Errorf("composed Dockerfile unexpectedly contains %q", fragment)
		}
	}
}

func TestComposeMultipleSelections(test *testing.T) {
	installTemplates(test)
	dockerfile, err := Compose("debian-trixie", []string{"go", "node"}, []string{"opencode", "codex"})
	if err != nil {
		test.Fatal(err)
	}
	for _, fragment := range []string{"# stack: go", "# stack: node", "# agent CLI: opencode", "# agent CLI: codex"} {
		if !strings.Contains(dockerfile, fragment) {
			test.Errorf("missing %q", fragment)
		}
	}
}

func TestComposeNoStacks(test *testing.T) {
	installTemplates(test)
	dockerfile, err := Compose("debian-trixie", nil, []string{"opencode"})
	if err != nil {
		test.Fatal(err)
	}
	if strings.Contains(dockerfile, "# stack:") {
		test.Errorf("expected no stacks, got:\n%s", dockerfile)
	}
}

func TestComposeUnknownInputs(test *testing.T) {
	installTemplates(test)
	if _, err := Compose("no-such-os", nil, []string{"opencode"}); err == nil {
		test.Error("expected error for unknown OS")
	}
	if _, err := Compose("debian-trixie", []string{"cobol"}, nil); err == nil {
		test.Error("expected error for unknown stack")
	}
	if _, err := Compose("debian-trixie", nil, []string{"emacs"}); err == nil {
		test.Error("expected error for unknown agent CLI")
	}
}

func TestWriteProjectDockerfile(test *testing.T) {
	installTemplates(test)
	projectRoot := test.TempDir()
	if err := Write(projectRoot, "debian-trixie", []string{"python"}, []string{"opencode"}); err != nil {
		test.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(projectRoot, ".ai-platform", "Dockerfile"))
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(string(written), "# stack: python") {
		test.Errorf("written Dockerfile missing python stack:\n%s", written)
	}
}
