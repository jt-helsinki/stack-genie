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

	"github.com/jt-helsinki/ideal-robot/internal/conffile"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
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
	Enabled *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// File is config/versions.yaml.
type File struct {
	SchemaVersion int                `json:"schema_version" yaml:"schema_version"`
	Services      map[string]Service `json:"services" yaml:"services"`
}

// Default returns the built-in pins (repo-layout §12.6). Container services are
// pinned by image+tag (currently the `latest` tag for every image); digests are
// intentionally not pinned (platform/arch specific). `ai setup` resolves every
// service-tier image from versions.yaml, falling back to these defaults.
func Default() *File {
	return &File{
		SchemaVersion: SchemaVersion,
		Services: map[string]Service{
			"microsandbox": {Mode: "native", Version: "v0.x", SHA256: "TBD"},
			"litellm":      {Mode: "container", Image: "ghcr.io/berriai/litellm", Tag: "latest"},
			// Postgres backing LiteLLM's admin UI / virtual keys. Pinned to the
			// small Alpine variant (far smaller/faster to pull than postgres:latest).
			"litellm-db": {Mode: "container", Image: "postgres", Tag: "18.4-alpine3.23"},
			// Headroom (input compression) runs as a shared host container in front
			// of LiteLLM; agents send to it at :18787 (arch §8–10, §15). Per-project
			// compression knobs ride per request, so it is no longer baked into the
			// workspace image.
			"headroom": {Mode: "container", Image: "ghcr.io/chopratejas/headroom", Tag: "latest"},
			// Open WebUI is the optional chat UI, routed through LiteLLM as an
			// OpenAI-compatible gateway (published on the host at :18090).
			"open-webui": {Mode: "container", Image: "ghcr.io/open-webui/open-webui", Tag: "latest"},
			// Presidio backs LiteLLM's always-on PII guardrail (arch §17): the
			// analyzer detects PII, the anonymizer masks it. Internal-only containers.
			"presidio-analyzer":   {Mode: "container", Image: "mcr.microsoft.com/presidio-analyzer", Tag: "latest"},
			"presidio-anonymizer": {Mode: "container", Image: "mcr.microsoft.com/presidio-anonymizer", Tag: "latest"},
			// LLM Guard backs LiteLLM's security-scoped legacy callback (arch §17):
			// PromptInjection + Secrets + bearer-token Regex only. Internal-only. The
			// image is an unpinnable rolling tag (ProtectAI/Laiyer legacy).
			"llm-guard": {Mode: "container", Image: "laiyer/llm-guard-api", Tag: "latest"},
			// nginx reverse proxy: the gateway entry on host :18787 in front of
			// Headroom (HTTPS-ready). Internal Headroom is reached by name.
			"proxy": {Mode: "container", Image: "nginx", Tag: "latest"},
			// Ollama is REQUIRED (always on): LiteLLM routes local model traffic to
			// it (arch §14, §16). Cloud models still go LiteLLM → provider; LiteLLM's
			// always-on Presidio guardrails audit both paths (§17).
			"ollama": {Mode: "container", Image: "ollama/ollama", Tag: "latest"},
			// CoreDNS egress-audit resolver: microVMs boot with --dns-nameserver
			// pointed at it so every queried name is logged (arch §29).
			"dns": {Mode: "container", Image: "coredns/coredns", Tag: "latest"},
		},
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
