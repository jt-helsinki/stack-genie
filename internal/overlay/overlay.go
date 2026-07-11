// Package overlay manages the host-backed per-workspace persistent overlay
// (arch §26). A workspace is a read-only image (built from the project's
// Dockerfile, §25) plus this writable overlay: everything written inside the
// microVM — installed programs, agent state, any file outside the project
// mount — lands here and survives stop/start and `ai destroy`
// recreation. The overlay is keyed by workspace ID and lives under
// ~/.ai-platform/overlays/<workspace-id>/.
//
// This package owns the host directory backing the overlay. Mounting it into
// the microVM as a Microsandbox named volume is done by the workspace Sandbox
// on a provisioned host; the path computed here is what gets mounted.
package overlay

import (
	"os"
	"path/filepath"

	"github.com/jt-helsinki/stack-genie/internal/paths"
)

// Path returns the host directory backing the overlay for a workspace ID. It
// does not create the directory.
func Path(workspaceID string) (string, error) {
	overlaysDir, err := paths.OverlaysDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(overlaysDir, workspaceID), nil
}

// Ensure creates the overlay directory for a workspace if it does not yet
// exist and returns its path. Idempotent — re-ensuring a started workspace's
// overlay re-uses the same directory, which is what makes installs persist
// across restart and recreation.
func Ensure(workspaceID string) (string, error) {
	overlayPath, err := Path(workspaceID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(overlayPath, 0o755); err != nil {
		return "", err
	}
	return overlayPath, nil
}

// Exists reports whether the overlay directory for a workspace is present.
func Exists(workspaceID string) (bool, error) {
	overlayPath, err := Path(workspaceID)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(overlayPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Remove deletes the overlay directory for a workspace. This is permanent
// removal (arch §26): only `ai delete` calls it — `ai destroy`
// keeps the overlay so `start` fully recovers the workspace.
// Idempotent: removing a non-existent overlay is not an error.
func Remove(workspaceID string) error {
	overlayPath, err := Path(workspaceID)
	if err != nil {
		return err
	}
	return os.RemoveAll(overlayPath)
}
