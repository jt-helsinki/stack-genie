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
	Network   NetworkConfig   `yaml:"network,omitempty" json:"network,omitempty"`
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

// NetworkConfig configures the workspace's two local-dev networking directions
// (arch §29.6). EgressProxy names the internet-egress firewall (ClawPatrol).
// AllowHostServices is the plain-TCP allow-list of host services the workspace
// may reach via the host gateway (e.g. a database). PublishPorts maps guest
// ports to host ports so the host can reach a server inside the workspace.
type NetworkConfig struct {
	EgressProxy       string        `yaml:"egress_proxy,omitempty" json:"egress_proxy,omitempty"`
	AllowHostServices []HostService `yaml:"allow_host_services,omitempty" json:"allow_host_services,omitempty"`
	PublishPorts      []PortMapping `yaml:"publish_ports,omitempty" json:"publish_ports,omitempty"`
}

// HostService is one allow-listed host endpoint. Host defaults to "gateway" (the
// §29.2 host-gateway address) when empty.
type HostService struct {
	Host string `yaml:"host,omitempty" json:"host,omitempty"`
	Port int    `yaml:"port" json:"port"`
}

// PortMapping publishes a guest port to a host port (host → workspace, arch §29.6).
type PortMapping struct {
	Guest int `yaml:"guest" json:"guest"`
	Host  int `yaml:"host" json:"host"`
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
		Network:   NetworkConfig{EgressProxy: "clawpatrol"},
		Agents:    AgentsConfig{ArchiveDays: 14, ReuseAgents: true},
	}
}

// gatewayToken is the placeholder in a HostService.Host that resolves to the
// §29.2 host-gateway address at workspace start.
const gatewayToken = "gateway"

// Validate checks the network block: ports in range and publish-host ports
// unique. It returns the first problem found, or nil.
func (network NetworkConfig) Validate() error {
	for _, service := range network.AllowHostServices {
		if service.Port < 1 || service.Port > 65535 {
			return fmt.Errorf("network.allow_host_services: port %d out of range", service.Port)
		}
	}
	seenHostPorts := map[int]bool{}
	for _, mapping := range network.PublishPorts {
		if mapping.Guest < 1 || mapping.Guest > 65535 || mapping.Host < 1 || mapping.Host > 65535 {
			return fmt.Errorf("network.publish_ports: %d:%d out of range", mapping.Host, mapping.Guest)
		}
		if seenHostPorts[mapping.Host] {
			return fmt.Errorf("network.publish_ports: host port %d mapped twice", mapping.Host)
		}
		seenHostPorts[mapping.Host] = true
	}
	return nil
}

// ResolveHostServices returns the allow-listed host endpoints as host:port
// strings with the "gateway" token (or an empty host) replaced by the resolved
// host-gateway address (arch §29.2, §29.6). This is what `ai workspace start`
// feeds to the Microsandbox network policy.
func (network NetworkConfig) ResolveHostServices(gateway string) []string {
	resolved := make([]string, 0, len(network.AllowHostServices))
	for _, service := range network.AllowHostServices {
		host := service.Host
		if host == "" || host == gatewayToken {
			host = gateway
		}
		resolved = append(resolved, fmt.Sprintf("%s:%d", host, service.Port))
	}
	return resolved
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

// LoadProjectConfig reads only the project-level config.yaml (no global merge),
// returning an empty Config if it does not exist. Use this to edit a single
// project setting in place.
func LoadProjectConfig(projectRoot string) (*Config, error) {
	path := ProjectPath(projectRoot)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, err
	}
	config := &Config{}
	if err := strictUnmarshal(raw, config); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return config, nil
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
