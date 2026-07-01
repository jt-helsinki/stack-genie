package templates_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/templates"
)

// osKeys, stacks, and agentCLIs mirror the directories embedded under
// internal/templates/files. Keep these in sync with that tree.
var (
	osKeys    = []string{"alma", "debian-bookworm", "debian-trixie", "ubuntu"}
	stacks    = []string{"deno", "go", "java", "maven", "node", "python", "rust"}
	agentCLIs = []string{"claude-code", "codex", "gemini", "opencode", "pi"}
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
		filepath.Join("stacks", "python", "Dockerfile.snippet"),
		filepath.Join("agentclis", "opencode", "Dockerfile.snippet"),
		filepath.Join("agentclis", "claude-code", "Dockerfile.snippet"),
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
				// Graphify installs via uv (Astral) with the bundled extras.
				"astral.sh/uv/install.sh",
				"uv tool install",
				"graphifyy[",
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
			// Every agent-CLI snippet registers Graphify with itself: `graphify
			// install` for claude-code (the default platform) and `graphify install
			// … --platform <cli>` for the others. Assert it registers Graphify and,
			// where applicable, targets its own platform — tolerant of extra flags
			// (e.g. --project).
			graphifyFlag := map[string]string{
				"claude-code": "",
				"codex":       "--platform codex",
				"gemini":      "--platform gemini",
				"opencode":    "--platform opencode",
				"pi":          "--platform pi",
			}
			if flag, ok := graphifyFlag[cli]; ok {
				if !strings.Contains(got, "graphify install") {
					t.Errorf("AgentCLISnippet(%q) missing `graphify install`:\n%s", cli, got)
				}
				if flag != "" && !strings.Contains(got, flag) {
					t.Errorf("AgentCLISnippet(%q) missing %q:\n%s", cli, flag, got)
				}
			} else if strings.Contains(got, "graphify install") {
				t.Errorf("AgentCLISnippet(%q) unexpectedly registers Graphify (unknown platform)", cli)
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
