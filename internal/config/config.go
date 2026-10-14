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
	"slices"

	"github.com/jt-helsinki/stack-genie/internal/conffile"
	"github.com/jt-helsinki/stack-genie/internal/paths"
	"gopkg.in/yaml.v3"
)

// Config mirrors repo-layout §12.4. The same shape applies at every level; each
// level may set any subset.
type Config struct {
	OS        string          `yaml:"os,omitempty" json:"os,omitempty"`
	Agent     AgentConfig     `yaml:"agent,omitempty" json:"agent,omitempty"`
	Context   ContextConfig   `yaml:"context,omitempty" json:"context,omitempty"`
	Workspace WorkspaceConfig `yaml:"workspace,omitempty" json:"workspace,omitempty"`
	Network   NetworkConfig   `yaml:"network,omitempty" json:"network,omitempty"`
}

type AgentConfig struct {
	// Tools is the set of agent CLIs installed in the environment (subset of
	// opencode, claude-code, codex, gemini-cli); repo-layout §12.4.
	Tools []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	// DefaultTool is the default agent CLI; must be one of Tools.
	DefaultTool string `yaml:"default_tool,omitempty" json:"default_tool,omitempty"`
	// GraphifyModel is the local omlx-served model Graphify uses for its (headless)
	// LLM backend — the model NAME as omlx currently serves it (no download; model
	// management lives entirely in omlx's own admin panel), reached THROUGH the
	// gateway as `omlx/<name>` (proxy → LiteLLM → the shared omlx server). Empty
	// leaves Graphify's backend unconfigured (the in-session /graphify skill still
	// uses the agent session's own model).
	GraphifyModel string `yaml:"graphify_model,omitempty" json:"graphify_model,omitempty"`
	// AuthModes records the per-CLI authentication mode for the CLIs that have a
	// first-party subscription login (claude-code, codex, gemini): "api-key" (the
	// DEFAULT — route through the platform gateway with the scoped virtual key, so the
	// tool firewall + secret masking apply) or "oauth" (the CLI's OWN subscription/OAuth
	// login, talking DIRECTLY to the provider, bypassing the gateway and its guardrails).
	// Keyed by CLI name; a CLI absent from the map defaults to "api-key" (see AuthMode).
	// opencode/omp have no subscription and are never recorded here — they are always
	// gateway/api-key. A FORCED-oauth CLI (ForcedOAuthCLIs) is recorded as "oauth" and
	// can be nothing else — see AuthMode.
	AuthModes map[string]string `yaml:"auth_modes,omitempty" json:"auth_modes,omitempty"`
	// HermesDashboardPassword is the plaintext basic-auth password for the hermes web
	// dashboard. Hermes REFUSES to bind its dashboard to 0.0.0.0 (required so the
	// published host port reaches it) unless an auth provider is configured, so the
	// platform auto-configures basic auth: it generates this password on first start,
	// stores the SCRYPT hash in hermes' ~/.hermes/config.yaml, and shows the login in
	// `ai apps`. This is a LOW-sensitivity LOCAL dashboard credential — NOT a provider
	// key or the scoped LiteLLM virtual key — so persisting the plaintext in the project
	// config.yaml is acceptable; do NOT confuse it with the gateway/provider secrets that
	// must never touch host disk.
	HermesDashboardPassword string `yaml:"hermes_dashboard_password,omitempty" json:"hermes_dashboard_password,omitempty"`
}

// OAuthCapableCLIs are the agent CLIs with a first-party subscription/OAuth login that
// the workspace can CHOOSE to use INSTEAD of the gateway (the single source of truth for
// which CLIs are OFFERED an api-key-vs-oauth choice). opencode/pi/omp have no subscription
// and are always gateway/api-key, so they are never in this set. Forced-oauth CLIs
// (ForcedOAuthCLIs) are NOT here — they have no api-key mode, so there is no choice to
// offer.
func OAuthCapableCLIs() []string {
	return []string{"claude-code", "codex", "gemini"}
}

// ForcedOAuthCLIs are agent CLIs that can ONLY authenticate via their own native (OAuth/
// subscription) login — they have NO api-key/gateway mode at all, so they are ALWAYS
// "oauth" and are never offered an auth-mode choice. Currently none of the supported
// CLIs are forced-oauth (the mechanism is retained for a future CLI that, like GitHub
// Copilot CLI before it was removed, cannot accept an API key or be pointed at the
// platform gateway).
func ForcedOAuthCLIs() []string {
	return nil
}

// IsOAuthEligible reports whether a CLI can run in oauth mode — either it may CHOOSE oauth
// (OAuthCapableCLIs) or it is oauth-only (ForcedOAuthCLIs). Everything else is always
// gateway/api-key.
func IsOAuthEligible(cli string) bool {
	return slices.Contains(OAuthCapableCLIs(), cli) || slices.Contains(ForcedOAuthCLIs(), cli)
}

// AuthMode returns the recorded auth mode for a CLI. A forced-oauth CLI (ForcedOAuthCLIs)
// is ALWAYS "oauth" regardless of what the map says (it has no api-key mode). Otherwise it
// defaults to "api-key" (the gateway-routed default) when the CLI is absent from AuthModes
// or the map is nil.
func (agent AgentConfig) AuthMode(cli string) string {
	if slices.Contains(ForcedOAuthCLIs(), cli) {
		return "oauth"
	}
	if mode, ok := agent.AuthModes[cli]; ok && mode != "" {
		return mode
	}
	return "api-key"
}

// ContextConfig holds the per-project context-optimization settings: the
// Headroom input-compression strategy and the Caveman output-compression level
// (arch §8–10). Both are set by `ai context` and read at workspace start.
type ContextConfig struct {
	Strategy     string `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	CavemanLevel string `yaml:"caveman_level,omitempty" json:"caveman_level,omitempty"`
	// CavemanEnabled records whether the Caveman output-compression toolkit is
	// installed into the workspace at start (chosen at `ai create`). A pointer so
	// three states are distinct: unset (nil), explicit true, explicit false. Unset
	// defaults to enabled for backward compatibility with projects created before
	// the toggle existed — see CavemanEnabledOrDefault.
	CavemanEnabled *bool `yaml:"caveman_enabled,omitempty" json:"caveman_enabled,omitempty"`
	// CodeReviewGraphEnabled records whether the code-review-graph toolkit
	// (https://code-review-graph.com) is installed into the workspace at start and
	// registered with each installed agent CLI as an MCP server (chosen at `ai
	// create`). A pointer so three states are distinct: unset (nil), explicit true,
	// explicit false. It is OPT-IN — unset (nil) defaults to DISABLED (unlike Caveman)
	// so existing projects are unaffected — see CodeReviewGraphEnabledOrDefault.
	CodeReviewGraphEnabled *bool `yaml:"code_review_graph_enabled,omitempty" json:"code_review_graph_enabled,omitempty"`
	// CodebaseMemoryEnabled records whether the codebase-memory-mcp server
	// (https://github.com/DeusData/codebase-memory-mcp) is installed into the
	// workspace at start and registered with each installed agent CLI as an MCP
	// server (chosen at `ai create`). A pointer so three states are distinct: unset
	// (nil), explicit true, explicit false. It is OPT-IN — unset (nil) defaults to
	// DISABLED (unlike Caveman) — see CodebaseMemoryEnabledOrDefault.
	CodebaseMemoryEnabled *bool `yaml:"codebase_memory_enabled,omitempty" json:"codebase_memory_enabled,omitempty"`
	// GraphifyEnabled records whether the Graphify knowledge-graph toolkit (PyPI
	// `graphifyy`) is installed into the workspace image and registered with each
	// installed agent CLI at start (chosen at `ai create` from the unified AI-tools
	// list). A pointer so three states are distinct: unset (nil), explicit true,
	// explicit false. Unset defaults to ENABLED for backward compatibility with
	// projects created before Graphify was selectable (it was always baked in then) —
	// see GraphifyEnabledOrDefault.
	GraphifyEnabled *bool `yaml:"graphify_enabled,omitempty" json:"graphify_enabled,omitempty"`
	// OpenVikingEnabled records whether OpenViking (https://docs.openviking.ai — "the
	// context database for AI agents") is installed into the workspace at start and
	// registered with each supported installed agent CLI (chosen at `ai create`). A
	// pointer so three states are distinct: unset (nil), explicit true, explicit false.
	// It is OPT-IN — unset (nil) defaults to DISABLED (unlike Caveman) — see
	// OpenVikingEnabledOrDefault.
	OpenVikingEnabled *bool `yaml:"openviking_enabled,omitempty" json:"openviking_enabled,omitempty"`
	// TokenSaveEnabled records whether tokensave (https://github.com/aovestdipaperino/tokensave
	// — a code-graph MCP server for 34 languages) is installed into the workspace at
	// start and registered with each installed agent CLI (chosen at `ai create`). A
	// pointer so three states are distinct: unset (nil), explicit true, explicit false.
	// It is OPT-IN — unset (nil) defaults to DISABLED — see TokenSaveEnabledOrDefault.
	TokenSaveEnabled *bool `yaml:"tokensave_enabled,omitempty" json:"tokensave_enabled,omitempty"`
	// AgentMemoryEnabled records whether agentmemory (https://github.com/rohitg00/agentmemory
	// — persistent cross-session memory for agents) is installed into the workspace at
	// start and registered with each installed agent CLI (chosen at `ai create`). A
	// pointer so three states are distinct: unset (nil), explicit true, explicit false.
	// It is OPT-IN — unset (nil) defaults to DISABLED — see AgentMemoryEnabledOrDefault.
	AgentMemoryEnabled *bool `yaml:"agentmemory_enabled,omitempty" json:"agentmemory_enabled,omitempty"`
	// PonytailEnabled records whether the Ponytail skill (https://github.com/DietrichGebert/ponytail
	// — nags the agent to check YAGNI/stdlib/existing code before writing anything new)
	// is installed into the workspace at start (chosen at `ai create`). A pointer so
	// three states are distinct: unset (nil), explicit true, explicit false. It is
	// OPT-IN — unset (nil) defaults to DISABLED — see PonytailEnabledOrDefault.
	PonytailEnabled *bool `yaml:"ponytail_enabled,omitempty" json:"ponytail_enabled,omitempty"`
	// AgentSkillsEnabled records whether addyosmani/agent-skills (a curated library of
	// senior-engineer skills — spec-writing, TDD, review, security, shipping) is mirrored
	// into the shared skill pool and, for gemini, additionally installed natively (chosen
	// at `ai create`). A pointer so three states are distinct: unset (nil), explicit true,
	// explicit false. It is OPT-IN — unset (nil) defaults to DISABLED — see
	// AgentSkillsEnabledOrDefault.
	AgentSkillsEnabled *bool `yaml:"agent_skills_enabled,omitempty" json:"agent_skills_enabled,omitempty"`
	// SerenaEnabled records whether Serena (https://github.com/oraios/serena — an LSP-backed
	// semantic code-navigation/editing MCP server) is installed into the project's Python
	// environment (<project>/.venv) and registered with each installed agent CLI (chosen
	// at `ai create`). A pointer so three states are distinct: unset (nil), explicit true,
	// explicit false. It is OPT-IN — unset (nil) defaults to DISABLED — see SerenaEnabledOrDefault.
	SerenaEnabled *bool `yaml:"serena_enabled,omitempty" json:"serena_enabled,omitempty"`
}

// CavemanEnabledOrDefault reports whether Caveman should be installed at workspace
// start. An unset (nil) value defaults to true so pre-toggle projects keep today's
// auto-install behavior; an explicit false disables it.
func (settings ContextConfig) CavemanEnabledOrDefault() bool {
	return settings.CavemanEnabled == nil || *settings.CavemanEnabled
}

// CodeReviewGraphEnabledOrDefault reports whether code-review-graph should be
// installed at workspace start. It is OPT-IN: an unset (nil) value defaults to FALSE,
// so it is only ever installed when explicitly chosen at `ai create`.
func (settings ContextConfig) CodeReviewGraphEnabledOrDefault() bool {
	return settings.CodeReviewGraphEnabled != nil && *settings.CodeReviewGraphEnabled
}

// CodebaseMemoryEnabledOrDefault reports whether codebase-memory-mcp should be
// installed at workspace start. It is OPT-IN: an unset (nil) value defaults to FALSE,
// so it is only ever installed when explicitly chosen at `ai create`.
func (settings ContextConfig) CodebaseMemoryEnabledOrDefault() bool {
	return settings.CodebaseMemoryEnabled != nil && *settings.CodebaseMemoryEnabled
}

// GraphifyEnabledOrDefault reports whether Graphify should be baked into the image and
// registered at workspace start. An unset (nil) value defaults to true so pre-toggle
// projects (where Graphify was always baked in) keep today's behavior; an explicit false
// disables it.
func (settings ContextConfig) GraphifyEnabledOrDefault() bool {
	return settings.GraphifyEnabled == nil || *settings.GraphifyEnabled
}

// OpenVikingEnabledOrDefault reports whether OpenViking should be installed at
// workspace start. It is OPT-IN: an unset (nil) value defaults to FALSE, so it is only
// ever installed when explicitly chosen at `ai create`.
func (settings ContextConfig) OpenVikingEnabledOrDefault() bool {
	return settings.OpenVikingEnabled != nil && *settings.OpenVikingEnabled
}

// TokenSaveEnabledOrDefault reports whether tokensave should be installed at
// workspace start. It is OPT-IN: an unset (nil) value defaults FALSE, so it is
// only ever installed when explicitly chosen at `ai create`.
func (settings ContextConfig) TokenSaveEnabledOrDefault() bool {
	return settings.TokenSaveEnabled != nil && *settings.TokenSaveEnabled
}

// AgentMemoryEnabledOrDefault reports whether agentmemory should be installed at
// workspace start. It is OPT-IN: an unset (nil) value defaults FALSE, so it is
// only ever installed when explicitly chosen at `ai create`.
func (settings ContextConfig) AgentMemoryEnabledOrDefault() bool {
	return settings.AgentMemoryEnabled != nil && *settings.AgentMemoryEnabled
}

// PonytailEnabledOrDefault reports whether the Ponytail skill should be installed
// at workspace start. It is OPT-IN: an unset (nil) value defaults FALSE, so it is
// only ever installed when explicitly chosen at `ai create`.
func (settings ContextConfig) PonytailEnabledOrDefault() bool {
	return settings.PonytailEnabled != nil && *settings.PonytailEnabled
}

// AgentSkillsEnabledOrDefault reports whether addyosmani/agent-skills should be mirrored
// into the shared skill pool at workspace start. It is OPT-IN: an unset (nil) value
// defaults FALSE, so it is only ever installed when explicitly chosen at `ai create`.
func (settings ContextConfig) AgentSkillsEnabledOrDefault() bool {
	return settings.AgentSkillsEnabled != nil && *settings.AgentSkillsEnabled
}

// SerenaEnabledOrDefault reports whether Serena should be installed at workspace start. It
// is OPT-IN: an unset (nil) value defaults FALSE, so it is only ever installed when
// explicitly chosen at `ai create`.
func (settings ContextConfig) SerenaEnabledOrDefault() bool {
	return settings.SerenaEnabled != nil && *settings.SerenaEnabled
}

// ToolEnabled reports whether the given AI-tool key (one of create.SupportedAITools —
// not imported here to avoid a dependency cycle, so the keys are matched as plain
// strings) is enabled, via the matching *EnabledOrDefault method. An unknown key
// reports false. Shared by `ai tools list` and the TUI Apps tab so both render the
// exact same enabled state from one definition.
func (settings ContextConfig) ToolEnabled(tool string) bool {
	switch tool {
	case "caveman":
		return settings.CavemanEnabledOrDefault()
	case "graphify":
		return settings.GraphifyEnabledOrDefault()
	case "code-review-graph":
		return settings.CodeReviewGraphEnabledOrDefault()
	case "codebase-memory-mcp":
		return settings.CodebaseMemoryEnabledOrDefault()
	case "openviking":
		return settings.OpenVikingEnabledOrDefault()
	case "tokensave":
		return settings.TokenSaveEnabledOrDefault()
	case "agentmemory":
		return settings.AgentMemoryEnabledOrDefault()
	case "ponytail":
		return settings.PonytailEnabledOrDefault()
	case "agent-skills":
		return settings.AgentSkillsEnabledOrDefault()
	case "serena":
		return settings.SerenaEnabledOrDefault()
	default:
		return false
	}
}

// WorkspaceConfig holds the microVM resource limits applied at workspace start
// (arch §7).
// WorkspaceConfig holds workspace-level preferences that survive the move to
// nono (host-process sandboxing, no VM): there is no vCPU/memory/disk to size
// and no idle-timeout concept (a nono session simply runs until it exits or is
// stopped), so those fields are gone along with IsolatedDirs/SharedMounts (nono
// doesn't virtualize the filesystem — there's no second "guest view" to mount
// into). Shell survives: it's just which interactive shell `ai shell` launches.
type WorkspaceConfig struct {
	// Shell is the interactive shell `ai shell` launches inside the nono sandbox:
	// "bash" (the platform default) or "zsh". Empty is treated as bash.
	Shell string `yaml:"shell,omitempty" json:"shell,omitempty"`
}

// ValidateShell checks a workspace shell choice. Empty is allowed (callers treat it as
// bash — the platform default, today's behavior); otherwise it must be "bash" or "zsh".
func ValidateShell(value string) error {
	switch value {
	case "", "bash", "zsh":
		return nil
	default:
		return fmt.Errorf("workspace.shell: %q (one of bash|zsh)", value)
	}
}

// ValidateAuthMode checks a per-agent auth mode. Empty is allowed (callers treat it as
// "api-key" — the default, gateway-routed); otherwise it must be "api-key" or "oauth".
func ValidateAuthMode(value string) error {
	switch value {
	case "", "api-key", "oauth":
		return nil
	default:
		return fmt.Errorf("agent.auth_modes: %q (one of api-key|oauth)", value)
	}
}

// ValidateCPUs checks a workspace cpu_limit. 0 means "use the runtime default"; a
// negative count is invalid.
// NetworkConfig declares a project's egress policy, mirroring the shape of a
// nono profile's own `network` section directly (internal/nono.Profile) —
// internal/egress compiles this straight into that profile at `ai create`/
// `ai network` time, rather than into Microsandbox net-rules.
type NetworkConfig struct {
	// Egress is "public" (DEFAULT — unrestricted outbound, matching nono's own
	// default) or "deny" (no network access at all, nono's --block-net). Empty ==
	// "public". Re-lockable per project with `ai network egress deny`.
	Egress string `yaml:"egress,omitempty" json:"egress,omitempty"`
	// AllowDomain / DenyDomain are proxy allow/deny-list entries (nono
	// --allow-domain/--deny-domain), only meaningful when Egress is "deny" (they
	// carve exceptions into an otherwise-blocked default).
	AllowDomain []string `yaml:"allow_domain,omitempty" json:"allow_domain,omitempty"`
	DenyDomain  []string `yaml:"deny_domain,omitempty" json:"deny_domain,omitempty"`
}

// EgressModes are the valid network.egress values.
var EgressModes = []string{"deny", "public"}

// ResolvedEgress returns the effective egress mode, defaulting empty to "public"
// (nono's own default: unrestricted outbound). Re-lockable per project with
// `ai network egress deny`.
func (network NetworkConfig) ResolvedEgress() string {
	if network.Egress == "" {
		return "public"
	}
	return network.Egress
}

// Default returns the built-in global defaults (repo-layout §12.4). It omits
// `os` — there is no default OS (it is always chosen per project, arch §25).
func Default() *Config {
	return &Config{
		Agent:     AgentConfig{Tools: []string{"opencode"}, DefaultTool: "opencode"},
		Context:   ContextConfig{Strategy: "balanced", CavemanLevel: "full"},
		Workspace: WorkspaceConfig{Shell: "bash"},
		Network:   NetworkConfig{Egress: "public"},
	}
}

// Validate checks the network block.
func (network NetworkConfig) Validate() error {
	switch network.Egress {
	case "", "deny", "public":
	default:
		return fmt.Errorf("network.egress: %q (one of deny|public)", network.Egress)
	}
	return nil
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
