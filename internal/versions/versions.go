// Package versions manages config/versions.yaml — the pinned versions/tags of
// the platform's host services and the Microsandbox runtime (repo-layout §12.6).
// `ai setup` installs to these pins; `ai setup --upgrade` bumps them. Container
// services are pinned by image+tag (no digest — digests are platform/arch
// specific, so pinning one breaks cross-platform pulls).
package versions

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/jt-helsinki/stack-genie/internal/conffile"
	"github.com/jt-helsinki/stack-genie/internal/paths"
	"github.com/jt-helsinki/stack-genie/internal/services"
)

// SchemaVersion is stamped on config/versions.yaml.
const SchemaVersion = 1

// Service is a single pinned service entry. Container-tier services pin an
// image+tag; native-tier pin a version+sha256. Digests are intentionally not
// pinned — they differ per platform/arch, so a digest pin breaks cross-platform
// pulls.
type Service struct {
	Mode    string `json:"mode" yaml:"mode"` // container | native
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
	SHA256  string `json:"sha256,omitempty" yaml:"sha256,omitempty"`
	Image   string `json:"image,omitempty" yaml:"image,omitempty"`
	Tag     string `json:"tag,omitempty" yaml:"tag,omitempty"`
}

// File is config/versions.yaml.
type File struct {
	SchemaVersion int                `json:"schema_version" yaml:"schema_version"`
	Services      map[string]Service `json:"services" yaml:"services"`
}

// Default returns the built-in pins (repo-layout §12.6). Container services are
// pinned by image+tag — most use `latest`, but some are intentionally more
// specific (e.g. postgres `18.4-alpine3.23`, nginx `stable-alpine3.23-slim`);
// digests are intentionally not pinned (platform/arch specific). `ai setup`
// resolves every service-tier image from versions.yaml, falling back to these
// defaults.
//
// The pins are DERIVED from the internal/services registry (the single source of
// truth for the platform's service topology), so they cannot drift from the log
// scopes / console endpoints / setup reconcile. The registry carries the split
// keys (presidio-analyzer/anonymizer), the standalone litellm-db, and the native
// microsandbox runtime; this projects each pin into a versions.Service. (The host
// service tier no longer pins Open WebUI or Odysseus — Open WebUI moved to a
// per-workspace in-VM app and Odysseus was removed.) A fresh map is built each
// call, so callers cannot mutate shared state.
func Default() *File {
	pinned := make(map[string]Service, len(services.VersionPins()))
	for key, pin := range services.VersionPins() {
		pinned[key] = Service{
			Mode:    pin.Mode,
			Image:   pin.Image,
			Tag:     pin.Tag,
			Version: pin.Version,
			SHA256:  pin.SHA256,
		}
	}
	return &File{
		SchemaVersion: SchemaVersion,
		Services:      pinned,
	}
}

// Path returns config/versions.yaml.
func Path() (string, error) {
	configDir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "versions.yaml"), nil
}

// EnsureDefault writes the default pins if versions.yaml is absent. Returns true
// when it created the file. Idempotent.
func EnsureDefault() (created bool, err error) {
	path, err := Path()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := conffile.WriteAtomic(path, Default()); err != nil {
		return false, err
	}
	return true, nil
}

// WriteDefault force-writes the built-in default pins, overwriting any existing
// versions.yaml. Used by `ai setup --upgrade` to bump to this binary's pins.
func WriteDefault() error {
	path, err := Path()
	if err != nil {
		return err
	}
	return conffile.WriteAtomic(path, Default())
}

// Load reads config/versions.yaml, returning (nil, nil) if absent.
func Load() (*File, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	var file File
	if err := conffile.Read(path, &file); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &file, nil
}
