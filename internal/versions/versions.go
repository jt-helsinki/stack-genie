// Package versions manages config/versions.json — the pinned versions/digests of
// the platform's host services and the Microsandbox runtime (repo-layout §12.6).
// `ai setup` installs to these pins; `ai setup --upgrade` bumps them.
package versions

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/jt-helsinki/ideal-robot/internal/jsonfile"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
)

// SchemaVersion is stamped on config/versions.json.
const SchemaVersion = 1

// Service is a single pinned service entry. Container-tier services pin an
// image+digest; native-tier pin a version+sha256.
type Service struct {
	Mode    string `json:"mode"` // container | native
	Version string `json:"version,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Image   string `json:"image,omitempty"`
	Digest  string `json:"digest,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

// File is config/versions.json.
type File struct {
	SchemaVersion int                `json:"schema_version"`
	Services      map[string]Service `json:"services"`
}

func boolPtr(b bool) *bool { return &b }

// Default returns the built-in pins (repo-layout §12.6). Concrete digests are
// filled in at release time; the placeholders keep the schema stable.
func Default() *File {
	return &File{
		SchemaVersion: SchemaVersion,
		Services: map[string]Service{
			"microsandbox": {Mode: "native", Version: "v0.x", SHA256: "TBD"},
			"clawpatrol":   {Mode: "native", Version: "v0.x", SHA256: "TBD"},
			"litellm":      {Mode: "container", Image: "ghcr.io/berriai/litellm", Digest: "sha256:TBD"},
			"headroom":     {Mode: "container", Image: "ghcr.io/chopratejas/headroom", Digest: "sha256:TBD"},
			"ollama":       {Mode: "container", Image: "docker.io/ollama/ollama", Digest: "sha256:TBD", Enabled: boolPtr(false)},
		},
	}
}

// Path returns config/versions.json.
func Path() (string, error) {
	c, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(c, "versions.json"), nil
}

// EnsureDefault writes the default pins if versions.json is absent. Returns true
// when it created the file. Idempotent.
func EnsureDefault() (created bool, err error) {
	p, err := Path()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(p); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := jsonfile.WriteAtomic(p, Default()); err != nil {
		return false, err
	}
	return true, nil
}

// Load reads config/versions.json, returning (nil, nil) if absent.
func Load() (*File, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	var f File
	if err := jsonfile.Read(p, &f); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &f, nil
}
