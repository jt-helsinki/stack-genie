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
	Runtime   string          `yaml:"runtime" json:"runtime,omitempty"`
	OS        string          `yaml:"os" json:"os,omitempty"`
	Git       GitConfig       `yaml:"git" json:"git,omitempty"`
	Agent     AgentConfig     `yaml:"agent" json:"agent,omitempty"`
	Context   ContextConfig   `yaml:"context" json:"context,omitempty"`
	Workspace WorkspaceConfig `yaml:"workspace" json:"workspace,omitempty"`
	Agents    AgentsConfig    `yaml:"agents" json:"agents,omitempty"`
}

type GitConfig struct {
	MergeStrategy string `yaml:"merge_strategy" json:"merge_strategy,omitempty"`
}

type AgentConfig struct {
	// Tools is the set of agent CLIs installed in the environment (subset of
	// opencode, claude-code, codex, gemini-cli); repo-layout §12.4.
	Tools []string `yaml:"tools" json:"tools,omitempty"`
	// DefaultTool is the default agent CLI; must be one of Tools.
	DefaultTool string `yaml:"default_tool" json:"default_tool,omitempty"`
}

type ContextConfig struct {
	MaxTokens            int     `yaml:"max_tokens" json:"max_tokens,omitempty"`
	CompressionThreshold float64 `yaml:"compression_threshold" json:"compression_threshold,omitempty"`
	Strategy             string  `yaml:"strategy" json:"strategy,omitempty"`
	CavemanLevel         string  `yaml:"caveman_level" json:"caveman_level,omitempty"`
}

type WorkspaceConfig struct {
	CPULimit    int    `yaml:"cpu_limit" json:"cpu_limit,omitempty"`
	MemoryLimit string `yaml:"memory_limit" json:"memory_limit,omitempty"`
}

type AgentsConfig struct {
	ArchiveDays int  `yaml:"archive_days" json:"archive_days,omitempty"`
	ReuseAgents bool `yaml:"reuse_agents" json:"reuse_agents,omitempty"`
}

// GlobalPath returns ~/.ai-platform/config/config.yaml.
func GlobalPath() (string, error) {
	c, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(c, "config.yaml"), nil
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
	layers := []string{globalPath}
	if projectRoot != "" {
		layers = append(layers, ProjectPath(projectRoot))
	}

	merged := map[string]any{}
	for _, path := range layers {
		m, ok, err := loadLayer(path)
		if err != nil {
			return nil, err
		}
		if ok {
			merged = deepMerge(merged, m)
		}
	}

	out := &Config{}
	if len(merged) > 0 {
		b, err := yaml.Marshal(merged)
		if err != nil {
			return nil, err
		}
		if err := strictUnmarshal(b, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// loadLayer reads one config file into a map, validating it has no unknown
// fields. ok is false when the file does not exist.
func loadLayer(path string) (m map[string]any, ok bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	// Validate against the known schema (rejects unknown fields, with filename).
	if err := strictUnmarshal(b, &Config{}); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	return m, true, nil
}

func strictUnmarshal(b []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty document
		}
		return err
	}
	return nil
}

// deepMerge returns base with over applied on top; nested maps merge recursively
// and scalar/over values win. Inputs are not mutated.
func deepMerge(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		if bv, ok := out[k]; ok {
			if bm, ok1 := bv.(map[string]any); ok1 {
				if om, ok2 := v.(map[string]any); ok2 {
					out[k] = deepMerge(bm, om)
					continue
				}
			}
		}
		out[k] = v
	}
	return out
}
