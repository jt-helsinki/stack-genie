// Package layout creates the global ~/.ai-platform directory tree
// (repo-layout spec §5/§1). It is idempotent: re-running is safe and only
// fills in anything missing.
package layout

import (
	"os"
	"path/filepath"

	"github.com/jt-helsinki/stack-genie/internal/paths"
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
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return "", err
		}
	}
	// Projects are created in the user's chosen directory (the cwd), not a fixed
	// location, so setup does not create a ~/projects directory.
	return root, nil
}
