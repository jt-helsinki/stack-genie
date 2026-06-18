// Package layout creates the global ~/.ai-platform directory tree
// (repo-layout spec §5/§1). It is idempotent: re-running is safe and only
// fills in anything missing.
package layout

import (
	"os"
	"path/filepath"

	"github.com/jt-helsinki/ideal-robot/internal/paths"
)

// dirs are the global subdirectories under ~/.ai-platform (repo-layout §5).
var dirs = []string{
	"agents",
	"audit",
	"cache",
	"config",
	"logs",
	"overlays",
	"prompts",
	"skills",
	"templates/dockerfiles",
	"templates/stacks",
	"tools",
}

// Ensure creates the ~/.ai-platform tree if absent and returns its root. Safe to
// call repeatedly (idempotent).
func Ensure() (string, error) {
	root, err := paths.PlatformDir()
	if err != nil {
		return "", err
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return "", err
		}
	}
	// ~/projects is also part of the platform's host layout (host-backed source).
	projects, err := paths.ProjectsDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(projects, 0o755); err != nil {
		return "", err
	}
	return root, nil
}
