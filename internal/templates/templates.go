// Package templates embeds the source environment templates (OS base
// Dockerfiles, software-stack snippets, agent-CLI snippets) and installs them
// into ~/.ai-platform/templates (repo-layout §1.5). `ai create` composes
// a project's .ai-platform/Dockerfile from the installed copies (internal/envimage),
// so a user can customize them after install.
package templates

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jt-helsinki/stack-genie/internal/paths"
)

//go:embed files
var embedded embed.FS

const (
	dockerfilesDir = "dockerfiles" // one Dockerfile per OS key
	stacksDir      = "stacks"      // one Dockerfile.snippet per software stack
	agentCLIsDir   = "agentclis"   // one Dockerfile.snippet per agent CLI
	toolsDir       = "tools"       // one Dockerfile.snippet per opt-in dev tool
)

// InstalledRoot returns ~/.ai-platform/templates.
func InstalledRoot() (string, error) {
	platformDir, err := paths.PlatformDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(platformDir, "templates"), nil
}

// Install copies the embedded templates into ~/.ai-platform/templates,
// overwriting so `ai setup --upgrade` refreshes them. Idempotent.
func Install() error {
	root, err := InstalledRoot()
	if err != nil {
		return err
	}
	return fs.WalkDir(embedded, "files", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel("files", path)
		if err != nil {
			return err
		}
		destination := filepath.Join(root, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		contents, err := embedded.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		return os.WriteFile(destination, contents, 0o644)
	})
}

// BaseDockerfile returns the installed OS base Dockerfile for an OS key.
func BaseDockerfile(osKey string) (string, error) {
	return readInstalled(filepath.Join(dockerfilesDir, osKey, "Dockerfile"))
}

// dnfOSKeys are the OS keys whose base image uses dnf (AlmaLinux/EL) rather than apt
// (Debian/Ubuntu) — see StackSnippet's dnf-variant selection.
var dnfOSKeys = map[string]bool{"alma": true}

// StackSnippet returns the installed Dockerfile snippet for a software stack, for the
// given OS key. A stack whose install step depends on the OS's package manager (e.g.
// go/deno/maven/java, which `apt-get`/`dnf install` a distro package) ships a
// "Dockerfile.dnf.snippet" sibling for dnf-based OSes (see dnfOSKeys); that variant is
// used when present, else the plain "Dockerfile.snippet" (apt, or package-manager-free
// like rust's rustup installer) covers every OS.
func StackSnippet(stack, osKey string) (string, error) {
	if dnfOSKeys[osKey] {
		if content, err := readInstalled(filepath.Join(stacksDir, stack, "Dockerfile.dnf.snippet")); err == nil {
			return content, nil
		}
	}
	return readInstalled(filepath.Join(stacksDir, stack, "Dockerfile.snippet"))
}

// AgentCLISnippet returns the installed Dockerfile snippet for an agent CLI.
func AgentCLISnippet(cli string) (string, error) {
	return readInstalled(filepath.Join(agentCLIsDir, cli, "Dockerfile.snippet"))
}

// ToolSnippet returns the installed Dockerfile snippet for an opt-in dev tool
// (e.g. code-review-graph, codebase-memory-mcp) — appended to the project
// Dockerfile only when that tool is selected at `ai create`.
func ToolSnippet(tool string) (string, error) {
	return readInstalled(filepath.Join(toolsDir, tool, "Dockerfile.snippet"))
}

func readInstalled(relative string) (string, error) {
	root, err := InstalledRoot()
	if err != nil {
		return "", err
	}
	contents, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		return "", err
	}
	return string(contents), nil
}
