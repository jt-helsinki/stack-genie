// Package config loads and merges the platform configuration hierarchy
// (arch §27, repo-layout §12.4). Precedence is project > global: a value set in
// the project's config.yaml overrides the global default. Unknown fields are
// rejected.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"gopkg.in/yaml.v3"
)

// Config mirrors repo-layout §12.4. The same shape applies at every level; each
// level may set any subset.
type Config struct {
	Runtime   string          `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	OS        string          `yaml:"os,omitempty" json:"os,omitempty"`
	Git       GitConfig       `yaml:"git,omitempty" json:"git,omitempty"`
	Agent     AgentConfig     `yaml:"agent,omitempty" json:"agent,omitempty"`
	Context   ContextConfig   `yaml:"context,omitempty" json:"context,omitempty"`
	Workspace WorkspaceConfig `yaml:"workspace,omitempty" json:"workspace,omitempty"`
	Agents    AgentsConfig    `yaml:"agents,omitempty" json:"agents,omitempty"`
}

type GitConfig struct {
	MergeStrategy string `yaml:"merge_strategy,omitempty" json:"merge_strategy,omitempty"`
}

type AgentConfig struct {
	// Tools is the set of agent CLIs installed in the environment (subset of
	// opencode, claude-code, codex, gemini-cli); repo-layout §12.4.
	Tools []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	// DefaultTool is the default agent CLI; must be one of Tools.
	DefaultTool string `yaml:"default_tool,omitempty" json:"default_tool,omitempty"`
}

type ContextConfig struct {
	MaxTokens            int     `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`
	CompressionThreshold float64 `yaml:"compression_threshold,omitempty" json:"compression_threshold,omitempty"`
	Strategy             string  `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	CavemanLevel         string  `yaml:"caveman_level,omitempty" json:"caveman_level,omitempty"`
}

type WorkspaceConfig struct {
	CPULimit    int    `yaml:"cpu_limit,omitempty" json:"cpu_limit,omitempty"`
	MemoryLimit string `yaml:"memory_limit,omitempty" json:"memory_limit,omitempty"`
}

type AgentsConfig struct {
	ArchiveDays int  `yaml:"archive_days,omitempty" json:"archive_days,omitempty"`
	ReuseAgents bool `yaml:"reuse_agents,omitempty" json:"reuse_agents,omitempty"`
}

// Default returns the built-in global defaults (repo-layout §12.4). It omits
// `os` — there is no default OS (it is always chosen per project, arch §25).
func Default() *Config {
	return &Config{
		Runtime:   "docker",
		Git:       GitConfig{MergeStrategy: "squash"},
		Agent:     AgentConfig{Tools: []string{"opencode"}, DefaultTool: "opencode"},
		Context:   ContextConfig{MaxTokens: 64000, CompressionThreshold: 0.75, Strategy: "balanced", CavemanLevel: "full"},
		Workspace: WorkspaceConfig{CPULimit: 4, MemoryLimit: "8G"},
		Agents:    AgentsConfig{ArchiveDays: 14, ReuseAgents: true},
	}
}

// EnsureGlobalDefault writes the default global config.yaml if none exists yet.
// It returns true when it created the file. Idempotent.
func EnsureGlobalDefault() (created bool, err error) {
	globalPath, err := GlobalPath()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(globalPath); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o755); err != nil {
		return false, err
	}
	encoded, err := yaml.Marshal(Default())
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(globalPath, encoded, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// WriteProject writes a project's <projectRoot>/.ai-platform/config.yaml.
func WriteProject(projectRoot string, config *Config) error {
	path := ProjectPath(projectRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	encoded, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o644)
}

// GlobalPath returns ~/.ai-platform/config/config.yaml.
func GlobalPath() (string, error) {
	configDir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "config.yaml"), nil
}

// ProjectPath returns <projectRoot>/.ai-platform/config.yaml.
func ProjectPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".ai-platform", "config.yaml")
}

// Load reads the global config and, when projectRoot is non-empty, the project
// config, then merges them with project > global precedence. A missing layer is
// skipped (not an error).
func Load(projectRoot string) (*Config, error) {
	globalPath, err := GlobalPath()
	if err != nil {
		return nil, err
	}
	layerPaths := []string{globalPath}
	if projectRoot != "" {
		layerPaths = append(layerPaths, ProjectPath(projectRoot))
	}

	merged := map[string]any{}
	for _, layerPath := range layerPaths {
		layer, present, err := loadLayer(layerPath)
		if err != nil {
			return nil, err
		}
		if present {
			merged = deepMerge(merged, layer)
		}
	}

	config := &Config{}
	if len(merged) > 0 {
		encoded, err := yaml.Marshal(merged)
		if err != nil {
			return nil, err
		}
		if err := strictUnmarshal(encoded, config); err != nil {
			return nil, err
		}
	}
	return config, nil
}

// loadLayer reads one config file into a map, validating it has no unknown
// fields. present is false when the file does not exist.
func loadLayer(path string) (layer map[string]any, present bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	// Validate against the known schema (rejects unknown fields, with filename).
	if err := strictUnmarshal(raw, &Config{}); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &layer); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	return layer, true, nil
}

func strictUnmarshal(raw []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty document
		}
		return err
	}
	return nil
}

// deepMerge returns base with overlay applied on top; nested maps merge
// recursively and scalar/overlay values win. Inputs are not mutated.
func deepMerge(base, overlay map[string]any) map[string]any {
	result := make(map[string]any, len(base))
	for key, value := range base {
		result[key] = value
	}
	for key, overlayValue := range overlay {
		if existing, ok := result[key]; ok {
			if existingMap, isMap := existing.(map[string]any); isMap {
				if overlayMap, isMap := overlayValue.(map[string]any); isMap {
					result[key] = deepMerge(existingMap, overlayMap)
					continue
				}
			}
		}
		result[key] = overlayValue
	}
	return result
}
