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

	"github.com/jt-helsinki/ideal-robot/internal/paths"
)

//go:embed files
var embedded embed.FS

const (
	dockerfilesDir = "dockerfiles" // one Dockerfile per OS key
	stacksDir      = "stacks"      // one Dockerfile.snippet per software stack
	agentCLIsDir   = "agentclis"   // one Dockerfile.snippet per agent CLI
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

// StackSnippet returns the installed Dockerfile snippet for a software stack.
func StackSnippet(stack string) (string, error) {
	return readInstalled(filepath.Join(stacksDir, stack, "Dockerfile.snippet"))
}

// AgentCLISnippet returns the installed Dockerfile snippet for an agent CLI.
func AgentCLISnippet(cli string) (string, error) {
	return readInstalled(filepath.Join(agentCLIsDir, cli, "Dockerfile.snippet"))
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
