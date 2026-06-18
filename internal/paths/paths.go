// Package paths resolves the platform's on-disk layout (repo-layout spec §1).
//
// Every location derives from the user's home directory, so the acceptance
// harness can repoint HOME at a throwaway dir (acceptance-tests spec §1.6) and
// have ~/.ai-platform and ~/projects resolve under it.
package paths

import (
	"os"
	"path/filepath"
)

// Home returns the user's home directory.
func Home() (string, error) {
	return os.UserHomeDir()
}

// PlatformDir is ~/.ai-platform — global platform state, config, and resources.
func PlatformDir() (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".ai-platform"), nil
}

// ConfigDir is ~/.ai-platform/config — global config and indexes.
func ConfigDir() (string, error) {
	p, err := PlatformDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(p, "config"), nil
}

// LogsDir is ~/.ai-platform/logs.
func LogsDir() (string, error) {
	p, err := PlatformDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(p, "logs"), nil
}

// ProjectsDir is ~/projects — host-backed project source.
func ProjectsDir() (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "projects"), nil
}
