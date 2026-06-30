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
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/conffile"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"gopkg.in/yaml.v3"
)

// Config mirrors repo-layout §12.4. The same shape applies at every level; each
// level may set any subset.
type Config struct {
	OS        string          `yaml:"os,omitempty" json:"os,omitempty"`
	Agent     AgentConfig     `yaml:"agent,omitempty" json:"agent,omitempty"`
	Context   ContextConfig   `yaml:"context,omitempty" json:"context,omitempty"`
	Workspace WorkspaceConfig `yaml:"workspace,omitempty" json:"workspace,omitempty"`
	// Microsandbox holds per-project options passed to the workspace microVM runtime.
	// It is separate from WorkspaceConfig (resource limits) so msb lifecycle/runtime
	// knobs have a stable namespace in config.yaml.
	Microsandbox MicrosandboxConfig `yaml:"microsandbox,omitempty" json:"microsandbox,omitempty"`
	Network      NetworkConfig      `yaml:"network,omitempty" json:"network,omitempty"`
	// Apps are the opt-in AI applications installed in this workspace, run as
	// in-VM nerdctl containers (arch §7). Each carries the unique host port it is
	// published on so concurrently-running workspaces never collide.
	Apps []AppEntry `yaml:"apps,omitempty" json:"apps,omitempty"`
}

// AppEntry records one installed in-VM app and the unique host port allocated
// for it. The port is reserved at install time and reused on every restart, so a
// workspace's app keeps a stable URL and two running workspaces never publish the
// same host port. Port is only ever published while the app is installed.
type AppEntry struct {
	Key  string `yaml:"key" json:"key"`
	Port int    `yaml:"port" json:"port"`
}

type AgentConfig struct {
	// Tools is the set of agent CLIs installed in the environment (subset of
	// opencode, claude-code, codex, gemini-cli); repo-layout §12.4.
	Tools []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	// DefaultTool is the default agent CLI; must be one of Tools.
	DefaultTool string `yaml:"default_tool,omitempty" json:"default_tool,omitempty"`
}

// ContextConfig holds the per-project context-optimization settings: the
// Headroom input-compression strategy and the Caveman output-compression level
// (arch §8–10). Both are set by `ai context` and read at workspace start.
type ContextConfig struct {
	Strategy     string `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	CavemanLevel string `yaml:"caveman_level,omitempty" json:"caveman_level,omitempty"`
}

// WorkspaceConfig holds the microVM resource limits applied at workspace start
// (arch §7).
type WorkspaceConfig struct {
	CPULimit    int    `yaml:"cpu_limit,omitempty" json:"cpu_limit,omitempty"`
	MemoryLimit string `yaml:"memory_limit,omitempty" json:"memory_limit,omitempty"`
}

// MicrosandboxConfig holds options passed to `msb create` at workspace start.
type MicrosandboxConfig struct {
	// IdleTimeout is the duration passed to `msb create --idle-timeout`. Empty means
	// use DefaultMicrosandboxIdleTimeout. Microsandbox accepts Go-like second/minute/
	// hour strings such as 30s, 5m, 1h, 24h.
	IdleTimeout string `yaml:"idle_timeout,omitempty" json:"idle_timeout,omitempty"`
}

// DefaultMicrosandboxIdleTimeout is the default for new projects and for missing
// config values in older projects. It keeps development workspaces alive across
// normal breaks without a heartbeat loop or host sleep prevention.
const DefaultMicrosandboxIdleTimeout = "24h"

// ResolvedIdleTimeout returns the effective msb idle timeout.
func (microsandbox MicrosandboxConfig) ResolvedIdleTimeout() string {
	if microsandbox.IdleTimeout == "" {
		return DefaultMicrosandboxIdleTimeout
	}
	return microsandbox.IdleTimeout
}

// ValidateIdleTimeout checks a Microsandbox idle-timeout duration. The CLI accepts
// positive Go duration strings (30s, 5m, 1h, 24h). A zero/negative value is rejected
// because `msb --idle-timeout 0` would be an immediate idle stop, not "disabled".
func ValidateIdleTimeout(value string) error {
	if value == "" {
		return nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return fmt.Errorf("microsandbox.idle_timeout: %q (positive duration, e.g. 30s, 5m, 24h)", value)
	}
	return nil
}

// ParseMemoryMiB parses a memory string ("8G", "512M", "2Gi", "2048") into a MiB
// count. A bare number is treated as MiB. G/Gi → ×1024; M/Mi → ×1. It errors on an
// unparseable or non-positive value. Shared by config validation and the workspace
// runtime so the units are interpreted identically everywhere.
func ParseMemoryMiB(value string) (uint64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("empty memory value")
	}
	upper := strings.ToUpper(trimmed)
	multiplier := uint64(1)
	number := upper
	switch {
	case strings.HasSuffix(upper, "GI"):
		multiplier, number = 1024, strings.TrimSuffix(upper, "GI")
	case strings.HasSuffix(upper, "G"):
		multiplier, number = 1024, strings.TrimSuffix(upper, "G")
	case strings.HasSuffix(upper, "MI"):
		multiplier, number = 1, strings.TrimSuffix(upper, "MI")
	case strings.HasSuffix(upper, "M"):
		multiplier, number = 1, strings.TrimSuffix(upper, "M")
	}
	amount, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
	if err != nil || amount <= 0 || math.IsInf(amount, 0) || math.IsNaN(amount) {
		return 0, fmt.Errorf("invalid memory %q (e.g. 512M, 4G, 2048; suffix M/Mi/G/Gi or a bare MiB number)", value)
	}
	mib := amount * float64(multiplier)
	// Guard the float→uint conversion against absurd input (e.g. "9e99G"), whose
	// uint64 cast is implementation-defined. 1<<30 MiB (1 PiB) is far above any real host.
	const maxMiB = 1 << 30
	if mib > maxMiB {
		return 0, fmt.Errorf("memory %q is unreasonably large", value)
	}
	return uint64(mib), nil
}

// ValidateMemory checks a workspace memory_limit. Empty is allowed (the platform
// default applies); otherwise it must parse to a positive MiB count.
func ValidateMemory(value string) error {
	if value == "" {
		return nil
	}
	if _, err := ParseMemoryMiB(value); err != nil {
		return fmt.Errorf("workspace.memory_limit: %s", err)
	}
	return nil
}

// ValidateCPUs checks a workspace cpu_limit. 0 means "use the runtime default"; a
// negative count is invalid.
func ValidateCPUs(cpus int) error {
	if cpus < 0 {
		return fmt.Errorf("workspace.cpu_limit: %d (must be >= 0; 0 uses the default)", cpus)
	}
	return nil
}

// NetworkConfig configures the workspace's two local-dev networking directions
// (arch §29.6). AllowHostServices is the plain-TCP allow-list of host services
// the workspace may reach via the host gateway (e.g. a database). PublishPorts
// maps guest ports to host ports so the host can reach a server inside the
// workspace.
type NetworkConfig struct {
	// Egress is the workspace's default outbound posture, enforced by the
	// Microsandbox network policy (arch §29.6): "public" (DEFAULT — the open
	// internet, private ranges still blocked; lets in-VM nerdctl pull images and
	// AI processes reach the internet, with DNS still audited via `ai network
	// log`), "deny" (only the model gateway + allow_host_services are reachable),
	// or "unrestricted". Empty == "public". This default is a deliberate
	// security-posture choice: the workspace ships allow-outbound and is
	// re-lockable per project with `ai network egress deny`.
	Egress            string        `yaml:"egress,omitempty" json:"egress,omitempty"`
	AllowHostServices []HostService `yaml:"allow_host_services,omitempty" json:"allow_host_services,omitempty"`
	PublishPorts      []PortMapping `yaml:"publish_ports,omitempty" json:"publish_ports,omitempty"`
}

// EgressModes are the valid network.egress values.
var EgressModes = []string{"deny", "public", "unrestricted"}

// ResolvedEgress returns the effective egress mode, defaulting empty to "public"
// (allow-outbound). The default flipped from "deny" to "public" so a freshly
// created workspace can pull in-VM container images and reach the internet out
// of the box; egress is still DNS-audited (`ai network log`) and re-lockable per
// project with `ai network egress deny`.
func (network NetworkConfig) ResolvedEgress() string {
	if network.Egress == "" {
		return "public"
	}
	return network.Egress
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

// Default returns the built-in global defaults (repo-layout §12.4). It omits
// `os` — there is no default OS (it is always chosen per project, arch §25).
func Default() *Config {
	return &Config{
		Agent:     AgentConfig{Tools: []string{"opencode", "pi"}, DefaultTool: "opencode"},
		Context:   ContextConfig{Strategy: "balanced", CavemanLevel: "full"},
		Workspace: WorkspaceConfig{CPULimit: 4, MemoryLimit: "8G"},
		Microsandbox: MicrosandboxConfig{
			IdleTimeout: DefaultMicrosandboxIdleTimeout,
		},
		Network: NetworkConfig{Egress: "public"},
	}
}

// gatewayToken is the placeholder in a HostService.Host that resolves to the
// §29.2 host-gateway address at workspace start.
const gatewayToken = "gateway"

// Validate checks the network block: ports in range and publish-host ports
// unique. It returns the first problem found, or nil.
func (network NetworkConfig) Validate() error {
	switch network.Egress {
	case "", "deny", "public", "unrestricted":
	default:
		return fmt.Errorf("network.egress: %q (one of deny|public|unrestricted)", network.Egress)
	}
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
// host-gateway address (arch §29.2, §29.6). This is what `ai start`
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
	if err := conffile.WriteAtomic(globalPath, Default()); err != nil {
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
	return conffile.WriteAtomic(ProjectPath(projectRoot), config)
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
