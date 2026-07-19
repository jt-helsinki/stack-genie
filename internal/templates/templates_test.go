package templates_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/templates"
)

// osKeys, stacks, and agentCLIs mirror the directories embedded under
// internal/templates/files. Keep these in sync with that tree.
var (
	osKeys    = []string{"alma", "debian-bookworm", "debian-trixie", "ubuntu"}
	stacks    = []string{"deno", "go", "java", "maven", "rust"}
	agentCLIs = []string{"claude-code", "codex", "copilot", "gemini", "omp", "opencode"}
)

// redirectHome points HOME (and USERPROFILE for portability) at a temp dir so
// InstalledRoot resolves under an isolated ~/.ai-platform.
func redirectHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func TestInstalledRoot(t *testing.T) {
	home := redirectHome(t)
	root, err := templates.InstalledRoot()
	if err != nil {
		t.Fatalf("InstalledRoot: %v", err)
	}
	want := filepath.Join(home, ".ai-platform", "templates")
	if root != want {
		t.Fatalf("InstalledRoot = %q, want %q", root, want)
	}
}

func TestInstallCopiesTreeToDisk(t *testing.T) {
	redirectHome(t)
	if err := templates.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	root, err := templates.InstalledRoot()
	if err != nil {
		t.Fatalf("InstalledRoot: %v", err)
	}

	representative := []string{
		filepath.Join("dockerfiles", "debian-trixie", "Dockerfile"),
		filepath.Join("dockerfiles", "alma", "Dockerfile"),
		filepath.Join("stacks", "go", "Dockerfile.snippet"),
		filepath.Join("stacks", "rust", "Dockerfile.snippet"),
		filepath.Join("agentclis", "opencode", "Dockerfile.snippet"),
		filepath.Join("agentclis", "claude-code", "Dockerfile.snippet"),
		filepath.Join("agentclis", "openclaw", "Dockerfile.snippet"),
		filepath.Join("agentclis", "hermes", "Dockerfile.snippet"),
	}
	for _, relative := range representative {
		path := filepath.Join(root, relative)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("expected installed file %q: %v", relative, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("installed file %q is empty", relative)
		}
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	redirectHome(t)
	if err := templates.Install(); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	first, err := templates.BaseDockerfile("ubuntu")
	if err != nil {
		t.Fatalf("BaseDockerfile after first install: %v", err)
	}
	// A second Install overwrites in place and must not error.
	if err := templates.Install(); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	second, err := templates.BaseDockerfile("ubuntu")
	if err != nil {
		t.Fatalf("BaseDockerfile after second install: %v", err)
	}
	if first != second {
		t.Fatalf("content changed across reinstall:\nfirst:  %q\nsecond: %q", first, second)
	}
}

func TestBaseDockerfileKnownKeys(t *testing.T) {
	redirectHome(t)
	if err := templates.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, osKey := range osKeys {
		t.Run(osKey, func(t *testing.T) {
			got, err := templates.BaseDockerfile(osKey)
			if err != nil {
				t.Fatalf("BaseDockerfile(%q): %v", osKey, err)
			}
			if strings.TrimSpace(got) == "" {
				t.Fatalf("BaseDockerfile(%q) returned blank content", osKey)
			}
		})
	}
}

// TestBaseDockerfileShipsContainerRuntime asserts every OS base Dockerfile
// installs the in-VM OCI container runtime (arch §7): the pinned nerdctl-full
// tarball (containerd + nerdctl + runc + CNI + buildkit) plus the runtime OS deps
// CNI needs (iptables, iproute). The runtime is the foundation Phase 1 builds on.
func TestBaseDockerfileShipsContainerRuntime(t *testing.T) {
	redirectHome(t)
	if err := templates.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, osKey := range osKeys {
		t.Run(osKey, func(t *testing.T) {
			got, err := templates.BaseDockerfile(osKey)
			if err != nil {
				t.Fatalf("BaseDockerfile(%q): %v", osKey, err)
			}
			for _, fragment := range []string{
				"NERDCTL_VERSION=2.3.4",
				"nerdctl-full-",
				"tar -C /usr/local",
				// Node.js 24 LTS (NodeSource) baked into every OS base.
				"NODE_MAJOR=24",
				"nodesource.com",
				"python3",
				// uv (Astral) is baked in — used by headroom + the opt-in graphify /
				// code-review-graph tool snippets. Graphify itself is NO LONGER baked
				// (it is a conditional tools/ snippet — see
				// TestOptInToolsAreConditionalSnippetsNotBaked).
				"astral.sh/uv/install.sh",
				"uv tool install",
				// rtk ("Rust Token Killer") installed via its official install.sh so
				// Claude Code's rtk PreToolUse hook finds the binary on PATH.
				"rtk-ai/rtk/master/install.sh",
				// Keep-alive so the detached microVM stays running.
				`CMD ["sleep", "infinity"]`,
			} {
				if !strings.Contains(got, fragment) {
					t.Errorf("%s: base Dockerfile missing container-runtime install %q:\n%s", osKey, fragment, got)
				}
			}
			// CNI's bridge plugin needs iptables + iproute at runtime.
			if !strings.Contains(got, "iptables") {
				t.Errorf("%s: base Dockerfile missing iptables (CNI bridge dep)", osKey)
			}
		})
	}
}

// TestOptInToolsAreConditionalSnippetsNotBaked asserts the opt-in code-graph tools live
// as conditional Dockerfile snippets (appended only when selected) and are NOT baked into
// any OS base — so an unselected tool's installer never runs at build.
func TestOptInToolsAreConditionalSnippetsNotBaked(t *testing.T) {
	redirectHome(t)
	if err := templates.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// The base must NOT contain either tool's installer.
	for _, osKey := range osKeys {
		got, err := templates.BaseDockerfile(osKey)
		if err != nil {
			t.Fatalf("BaseDockerfile(%q): %v", osKey, err)
		}
		for _, absent := range []string{"code-review-graph", "codebase-memory-mcp"} {
			if strings.Contains(got, "install --no-cache "+absent) || strings.Contains(got, absent+"/main/install.sh") {
				t.Errorf("%s: opt-in tool %q must NOT be baked into the base Dockerfile", osKey, absent)
			}
		}
		// Graphify is now an opt-in snippet too — its `graphifyy` install must not be baked.
		if strings.Contains(got, "graphifyy[") {
			t.Errorf("%s: graphify must NOT be baked into the base Dockerfile (it is a conditional tools/ snippet)", osKey)
		}
	}
	// The snippets must exist and carry the install command.
	graphify, err := templates.ToolSnippet("graphify")
	if err != nil {
		t.Fatalf("ToolSnippet(graphify): %v", err)
	}
	if !strings.Contains(graphify, "uv tool install --no-cache \"graphifyy[") {
		t.Errorf("graphify snippet missing its install command:\n%s", graphify)
	}
	crg, err := templates.ToolSnippet("code-review-graph")
	if err != nil {
		t.Fatalf("ToolSnippet(code-review-graph): %v", err)
	}
	if !strings.Contains(crg, "uv tool install --no-cache code-review-graph") {
		t.Errorf("code-review-graph snippet missing its install command:\n%s", crg)
	}
	cmm, err := templates.ToolSnippet("codebase-memory-mcp")
	if err != nil {
		t.Fatalf("ToolSnippet(codebase-memory-mcp): %v", err)
	}
	if !strings.Contains(cmm, "DeusData/codebase-memory-mcp/main/install.sh") || !strings.Contains(cmm, "--ui --skip-config") {
		t.Errorf("codebase-memory-mcp snippet missing its install command:\n%s", cmm)
	}
}

// TestBaseDockerfileShipsTerminfo asserts every OS base Dockerfile installs
// ncurses-term, which provides the modern terminfo entries (notably
// tmux-256color) that in-VM TUI agent CLIs need to render correctly through the
// per-session tmux. The VM is headless — the host terminal emulator draws the
// PTY stream — so this terminfo + the managed tmux config (agentcfg.TmuxConfig)
// is what makes truecolor + extended keys work. The package name is the same
// (ncurses-term) on apt (debian/ubuntu) and dnf (almalinux).
func TestBaseDockerfileShipsTerminfo(t *testing.T) {
	redirectHome(t)
	if err := templates.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, osKey := range osKeys {
		t.Run(osKey, func(t *testing.T) {
			got, err := templates.BaseDockerfile(osKey)
			if err != nil {
				t.Fatalf("BaseDockerfile(%q): %v", osKey, err)
			}
			if !strings.Contains(got, "ncurses-term") {
				t.Errorf("%s: base Dockerfile missing ncurses-term (modern terminfo for tmux TUIs):\n%s", osKey, got)
			}
		})
	}
}

// TestBaseDockerfileShipsShellsAndHeadroom asserts every OS base ships the
// interactive-shell frameworks (zsh + oh-my-bash + oh-my-zsh) and the Headroom CLI
// (installed via uv tool as headroom-ai[proxy]). The shell frameworks back the per-
// workspace bash/zsh choice; Headroom is installed in-VM so each agent CLI can be
// wrapped (`headroom wrap <cli>`) to compress provider-API traffic before it leaves
// the microVM. All are installed for the workspace user; the package/command names
// are the same across apt (debian/ubuntu) and dnf (almalinux).
func TestBaseDockerfileShipsShellsAndHeadroom(t *testing.T) {
	redirectHome(t)
	if err := templates.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, osKey := range osKeys {
		t.Run(osKey, func(t *testing.T) {
			got, err := templates.BaseDockerfile(osKey)
			if err != nil {
				t.Fatalf("BaseDockerfile(%q): %v", osKey, err)
			}
			for _, fragment := range []string{
				// zsh package in the base layer (bash ships with every distro).
				"    zsh \\",
				// oh-my-bash + oh-my-zsh, installed unattended for the workspace user.
				"ohmybash/oh-my-bash",
				"ohmyzsh/ohmyzsh",
				"--unattended",
				// Headroom CLI, installed via uv tool alongside Graphify.
				`uv tool install --no-cache "headroom-ai[proxy]"`,
			} {
				if !strings.Contains(got, fragment) {
					t.Errorf("%s: base Dockerfile missing %q:\n%s", osKey, fragment, got)
				}
			}
		})
	}
}

func TestBaseDockerfileUnknownKey(t *testing.T) {
	redirectHome(t)
	if err := templates.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := templates.BaseDockerfile("no-such-os"); err == nil {
		t.Fatal("BaseDockerfile(unknown) = nil error, want error")
	}
}

func TestStackSnippetKnownStacks(t *testing.T) {
	redirectHome(t)
	if err := templates.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, stack := range stacks {
		t.Run(stack, func(t *testing.T) {
			got, err := templates.StackSnippet(stack)
			if err != nil {
				t.Fatalf("StackSnippet(%q): %v", stack, err)
			}
			if strings.TrimSpace(got) == "" {
				t.Fatalf("StackSnippet(%q) returned blank content", stack)
			}
		})
	}
}

func TestStackSnippetUnknown(t *testing.T) {
	redirectHome(t)
	if err := templates.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := templates.StackSnippet("cobol"); err == nil {
		t.Fatal("StackSnippet(unknown) = nil error, want error")
	}
}

func TestAgentCLISnippetKnownCLIs(t *testing.T) {
	redirectHome(t)
	if err := templates.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, cli := range agentCLIs {
		t.Run(cli, func(t *testing.T) {
			got, err := templates.AgentCLISnippet(cli)
			if err != nil {
				t.Fatalf("AgentCLISnippet(%q): %v", cli, err)
			}
			if strings.TrimSpace(got) == "" {
				t.Fatalf("AgentCLISnippet(%q) returned blank content", cli)
			}
			// Graphify registration is NO LONGER in the snippets — it moved to
			// RUNTIME (workspace start: `graphify install --project` in ~/project,
			// where the project is bind-mounted). A snippet only installs its CLI, so
			// it must not run graphify at image-build time.
			if strings.Contains(got, "graphify") {
				t.Errorf("AgentCLISnippet(%q) should not run graphify (registration moved to runtime):\n%s", cli, got)
			}
		})
	}
}

func TestAgentCLISnippetUnknown(t *testing.T) {
	redirectHome(t)
	if err := templates.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := templates.AgentCLISnippet("emacs-doctor"); err == nil {
		t.Fatal("AgentCLISnippet(unknown) = nil error, want error")
	}
}

// Reading a snippet before Install must error because nothing is on disk yet.
func TestReadBeforeInstallErrors(t *testing.T) {
	redirectHome(t)
	if _, err := templates.BaseDockerfile("ubuntu"); err == nil {
		t.Error("BaseDockerfile before Install = nil error, want error")
	}
	if _, err := templates.StackSnippet("go"); err == nil {
		t.Error("StackSnippet before Install = nil error, want error")
	}
	if _, err := templates.AgentCLISnippet("opencode"); err == nil {
		t.Error("AgentCLISnippet before Install = nil error, want error")
	}
}
