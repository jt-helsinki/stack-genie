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

func TestComposeDoesNotBakeHeadroom(test *testing.T) {
	installTemplates(test)
	// Headroom now runs as a shared host container in front of LiteLLM, so it must
	// NOT be installed into the workspace image (arch §8–10, §15). Agents reach the
	// host Headroom at AI_PLATFORM_HOST:18787 instead.
	dockerfile, err := Compose("debian-trixie", nil, []string{"opencode"})
	if err != nil {
		test.Fatal(err)
	}
	for _, fragment := range []string{"headroom", "headroom-ai", "18787"} {
		if strings.Contains(dockerfile, fragment) {
			test.Errorf("composed Dockerfile unexpectedly bakes in Headroom (%q):\n%s", fragment, dockerfile)
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

// TestAllOSTemplatesExposeIdenticalBaseSurface is the [S5] OS-equivalence check:
// every shipped OS template composes and exposes the same base tooling surface
// (git, gh, ca-certificates, sudo, a passwordless-sudo `workspace` user), and the
// selected stack/agent CLI are appended regardless of OS.
func TestAllOSTemplatesExposeIdenticalBaseSurface(test *testing.T) {
	installTemplates(test)

	// FROM line per OS — proves the user's OS choice is the one applied.
	osBaseImage := map[string]string{
		"debian-trixie":   "FROM debian:trixie-slim",
		"debian-bookworm": "FROM debian:bookworm-slim",
		"ubuntu":          "FROM ubuntu:24.04",
		"alma":            "FROM almalinux:10",
	}
	// Identical base tooling surface across every OS (arch §12, §25).
	baseSurface := []string{
		"ca-certificates",
		"curl",
		"git",
		"gh",
		"sudo",
		"useradd --create-home --shell /bin/bash workspace",
		"workspace ALL=(ALL) NOPASSWD:ALL",
		"USER workspace",
		// In-VM OCI container runtime (arch §7): the pinned nerdctl-full tarball
		// (containerd + nerdctl + runc + CNI + buildkit) is installed on every OS.
		"NERDCTL_VERSION=2.3.4",
		"nerdctl-full-",
		// Python 3 + Graphify are baked into every OS base by default (§12, §25).
		"python3",
		"graphifyy",
	}

	for osKey, fromLine := range osBaseImage {
		dockerfile, err := Compose(osKey, []string{"go"}, []string{"opencode"})
		if err != nil {
			test.Fatalf("compose %s: %v", osKey, err)
		}
		if !strings.Contains(dockerfile, fromLine) {
			test.Errorf("%s: missing base image line %q", osKey, fromLine)
		}
		for _, fragment := range baseSurface {
			if !strings.Contains(dockerfile, fragment) {
				test.Errorf("%s: base surface missing %q:\n%s", osKey, fragment, dockerfile)
			}
		}
		// The user's selections are appended regardless of OS.
		for _, fragment := range []string{"# stack: go", "# agent CLI: opencode"} {
			if !strings.Contains(dockerfile, fragment) {
				test.Errorf("%s: missing selection %q", osKey, fragment)
			}
		}
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
