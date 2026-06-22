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

// Default returns the built-in pins (repo-layout §12.6). Concrete digests are
// filled in at release time; the placeholders keep the schema stable.
func Default() *File {
	return &File{
		SchemaVersion: SchemaVersion,
		Services: map[string]Service{
			"microsandbox": {Mode: "native", Version: "v0.x", SHA256: "TBD"},
			"litellm":      {Mode: "container", Image: "ghcr.io/berriai/litellm", Digest: "sha256:TBD"},
			// Headroom (input compression) runs as a shared host container in front
			// of LiteLLM; agents send to it at :8787 (arch §8–10, §15). Per-project
			// compression knobs ride per request, so it is no longer baked into the
			// workspace image.
			"headroom": {Mode: "container", Image: "ghcr.io/chopratejas/headroom", Digest: "sha256:TBD"},
			// Open WebUI is the optional chat UI, routed through LiteLLM as an
			// OpenAI-compatible gateway (published on the host at :8090).
			"open-webui": {Mode: "container", Image: "ghcr.io/open-webui/open-webui", Digest: "sha256:TBD"},
			// Presidio backs LiteLLM's always-on PII guardrail (arch §17): the
			// analyzer detects PII, the anonymizer masks it. Internal-only containers.
			"presidio-analyzer":   {Mode: "container", Image: "mcr.microsoft.com/presidio-analyzer", Digest: "sha256:TBD"},
			"presidio-anonymizer": {Mode: "container", Image: "mcr.microsoft.com/presidio-anonymizer", Digest: "sha256:TBD"},
			// Ollama is REQUIRED (always on): LiteLLM routes local model traffic to
			// it (arch §14, §16). Cloud models still go LiteLLM → provider; LiteLLM's
			// always-on Presidio guardrails audit both paths (§17).
			"ollama": {Mode: "container", Image: "docker.io/ollama/ollama", Digest: "sha256:TBD"},
		},
	}
}

// Path returns config/versions.json.
func Path() (string, error) {
	configDir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "versions.json"), nil
}

// EnsureDefault writes the default pins if versions.json is absent. Returns true
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
	if err := jsonfile.WriteAtomic(path, Default()); err != nil {
		return false, err
	}
	return true, nil
}

// WriteDefault force-writes the built-in default pins, overwriting any existing
// versions.json. Used by `ai setup --upgrade` to bump to this binary's pins.
func WriteDefault() error {
	path, err := Path()
	if err != nil {
		return err
	}
	return jsonfile.WriteAtomic(path, Default())
}

// Load reads config/versions.json, returning (nil, nil) if absent.
func Load() (*File, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	var file File
	if err := jsonfile.Read(path, &file); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &file, nil
}
