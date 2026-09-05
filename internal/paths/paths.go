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

// VolumesDir is ~/.ai-platform/volumes — the single home for every host-persisted
// SYSTEM data volume (the LiteLLM Postgres data dir, the local model store, …).
// Keeping all system volumes here (rather than scattered Docker named volumes or
// ad-hoc paths under the platform dir) makes them discoverable in one place and
// means `ai uninstall --purge`, which RemoveAll's ~/.ai-platform, removes them
// too. Per-name subdirs (volumes/litellm-db, volumes/models, …) are created on use
// via MkdirAll by their consumer, mirroring how the models dir was created.
func VolumesDir() (string, error) {
	p, err := PlatformDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(p, "volumes"), nil
}

// CacheDir is ~/.ai-platform/cache — the home for fetched caches (the models.dev
// catalog, the model library list, …). Unlike volumes/ (persistent SYSTEM data),
// these are re-fetchable copies; the dir is created on use via MkdirAll by its
// consumer, mirroring VolumesDir, and is removed by `ai uninstall --purge`.
func CacheDir() (string, error) {
	p, err := PlatformDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(p, "cache"), nil
}

// BinDir is ~/.ai-platform/bin — platform-managed host executables the platform
// pins to an exact version (notably the Microsandbox `msb` CLI, kept in lockstep
// with the embedded SDK FFI). Created on use via MkdirAll; removed by
// `ai uninstall --purge`.
func BinDir() (string, error) {
	p, err := PlatformDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(p, "bin"), nil
}

// VenvDir is ~/.ai-platform/venv — the platform-managed, host-side Python virtual
// environment. It is the single home for platform-wide Python tooling that runs on
// the HOST (as opposed to a workspace's in-VM `.venv-msb`): notably the omlx
// local-inference backend. Created on use (by internal/pyenv at `ai setup`) and
// removed by `ai uninstall --purge` (RemoveAll ~/.ai-platform).
func VenvDir() (string, error) {
	platformDir, err := PlatformDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(platformDir, "venv"), nil
}

// OverlaysDir is ~/.ai-platform/overlays — per-workspace persistent overlays
// (arch §26). Host-local persistence, not git material and not a backup.
func OverlaysDir() (string, error) {
	p, err := PlatformDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(p, "overlays"), nil
}

// ProjectsDir is ~/projects — host-backed project source.
func ProjectsDir() (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "projects"), nil
}
