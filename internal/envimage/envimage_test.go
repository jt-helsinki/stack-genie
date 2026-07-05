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
	for _, fragment := range []string{"# stack: rust", "# stack: deno", "# agent CLI: gemini"} {
		if strings.Contains(dockerfile, fragment) {
			test.Errorf("composed Dockerfile unexpectedly contains %q", fragment)
		}
	}
}

func TestComposeMultipleSelections(test *testing.T) {
	installTemplates(test)
	dockerfile, err := Compose("debian-trixie", []string{"go", "rust"}, []string{"opencode", "codex"})
	if err != nil {
		test.Fatal(err)
	}
	for _, fragment := range []string{"# stack: go", "# stack: rust", "# agent CLI: opencode", "# agent CLI: codex"} {
		if !strings.Contains(dockerfile, fragment) {
			test.Errorf("missing %q", fragment)
		}
	}
}

// TestComposeInstallsCopilot verifies the GitHub Copilot CLI snippet composes and installs
// the @github/copilot npm package.
func TestComposeInstallsCopilot(test *testing.T) {
	installTemplates(test)
	dockerfile, err := Compose("debian-trixie", nil, []string{"copilot"})
	if err != nil {
		test.Fatal(err)
	}
	for _, fragment := range []string{"# agent CLI: copilot", "npm install -g @github/copilot"} {
		if !strings.Contains(dockerfile, fragment) {
			test.Errorf("composed Dockerfile missing %q:\n%s", fragment, dockerfile)
		}
	}
}

func TestComposeBakesHeadroom(test *testing.T) {
	installTemplates(test)
	// Headroom is now installed IN each workspace image (via uv tool, headroom-ai[all])
	// so each agent CLI can be wrapped (`headroom wrap <cli>`) to compress provider-API
	// traffic before it leaves the microVM — it is no longer a shared host container.
	dockerfile, err := Compose("debian-trixie", nil, []string{"opencode"})
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(dockerfile, `uv tool install "headroom-ai[all]"`) {
		test.Errorf("composed Dockerfile should install Headroom (headroom-ai) in the workspace image:\n%s", dockerfile)
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
		// Node.js 24 LTS (NodeSource) is baked into every OS base — the distro apt
		// Node is too old for the agent CLIs (pi's undici needs Node >= 22.10).
		"NODE_MAJOR=24",
		"nodesource.com",
		// Python 3, uv, and Graphify are baked into every OS base by default (§12,
		// §25). Graphify installs via `uv tool install` with the bundled extras.
		"python3",
		"astral.sh/uv/install.sh",
		"uv tool install",
		"graphifyy[",
		// Keep-alive so the detached microVM stays up (the image's default shell
		// would exit immediately and msb would stop the sandbox).
		`CMD ["sleep", "infinity"]`,
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
		// The user's selections are appended regardless of OS. (Graphify is no longer
		// registered in the snippet — that moved to runtime.)
		for _, fragment := range []string{"# stack: go", "# agent CLI: opencode", "opencode-ai"} {
			if !strings.Contains(dockerfile, fragment) {
				test.Errorf("%s: missing selection %q", osKey, fragment)
			}
		}
	}
}

func TestWriteProjectDockerfile(test *testing.T) {
	installTemplates(test)
	projectRoot := test.TempDir()
	if err := Write(projectRoot, "debian-trixie", []string{"go"}, []string{"opencode"}); err != nil {
		test.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(projectRoot, ".ai-platform", "Dockerfile"))
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(string(written), "# stack: go") {
		test.Errorf("written Dockerfile missing go stack:\n%s", written)
	}
}
