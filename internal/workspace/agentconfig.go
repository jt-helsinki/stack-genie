package workspace

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/agentcfg"
	"github.com/jt-helsinki/stack-genie/internal/config"
)

// writeAgentConfig writes the selected CLI's keyless host-side config (if it
// has one — gemini has none, see agentcfg's package doc). key is the scoped
// gateway key, used ONLY to pick the api-key-vs-oauth content shape (empty =
// oauth); it is never written to any file — the real secret is injected by
// nono at launch (internal/nono.RunOptions.EnvCredentialMap), not here.
func (manager Manager) writeAgentConfig(root, cli string, projectConfig *config.Config, gatewayURL, key string) error {
	switch cli {
	case "opencode":
		return manager.writeOpenCodeConfig(root, projectConfig, gatewayURL)
	case "claude-code":
		return writeClaudeConfig(root, gatewayURL, key != "")
	case "codex":
		return writeCodexConfig(root, projectConfig, gatewayURL, key != "")
	case "omp":
		return manager.writeOmpConfig(root, projectConfig, gatewayURL)
	case "hermes":
		return manager.writeHermesConfig(root, projectConfig, gatewayURL)
	default: // gemini: env-only, no config file (see ensureProfile's set_vars)
		return nil
	}
}

// enabledMCPServers returns the stdio MCP servers for codex/hermes/omp —
// the agent CLIs whose single config file this platform rewrites wholesale
// each launch, so an AI tool's own native per-CLI install (which claude-code/
// opencode/gemini get instead, via internal/workspace/tools.go) would be
// clobbered on the next launch. See agentcfg.MCPServer's doc comment.
func enabledMCPServers(root string, projectConfig *config.Config) []agentcfg.MCPServer {
	venvPython := filepath.Join(root, ".venv", "bin", "python")
	return agentcfg.EnabledMCPServers(
		projectConfig.Context.CodeReviewGraphEnabledOrDefault(),
		projectConfig.Context.CodebaseMemoryEnabledOrDefault(),
		projectConfig.Context.GraphifyEnabledOrDefault(),
		projectConfig.Context.TokenSaveEnabledOrDefault(),
		projectConfig.Context.AgentMemoryEnabledOrDefault(),
		projectConfig.Context.SerenaEnabledOrDefault(),
		venvPython,
	)
}

func (manager Manager) pickerModels() []agentcfg.Model {
	if manager.Served == nil {
		return nil
	}
	served, err := manager.Served.ServedModels()
	if err != nil {
		manager.progress("warning: could not read the served model list from the gateway (" + err.Error() + ") — opencode will list no models until the next launch")
		return nil
	}
	if len(served) == 0 {
		manager.progress("warning: the gateway serves no models yet — run `ai models refresh`; opencode lists none until the next launch")
	}
	seen := make(map[string]struct{}, len(served))
	models := make([]agentcfg.Model, 0, len(served))
	for _, model := range served {
		if model.Name == "" {
			continue
		}
		if _, ok := seen[model.Name]; ok {
			continue
		}
		seen[model.Name] = struct{}{}
		models = append(models, model)
	}
	return models
}

func (manager Manager) writeOpenCodeConfig(root string, projectConfig *config.Config, gatewayURL string) error {
	keepTurns, outputBufferTokens := headroomParams(projectConfig.Context.Strategy)
	path := filepath.Join(root, ".opencode", "opencode.json")
	existing, _ := os.ReadFile(path)
	merged, err := agentcfg.MergeOpenCodeConfig(existing, gatewayURL, agentcfg.OpenCodeAPIKeyRef, "",
		manager.pickerModels(), keepTurns, outputBufferTokens)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, merged)
}

func writeClaudeConfig(root, gatewayURL string, apiKeyMode bool) error {
	path := filepath.Join(root, ".claude", "settings.json")
	existing, _ := os.ReadFile(path)
	var merged []byte
	var err error
	if apiKeyMode {
		merged, err = agentcfg.MergeClaudeSettings(existing, gatewayURL)
	} else {
		merged, err = agentcfg.MergeClaudeSettingsOAuth(existing)
	}
	if err != nil {
		return err
	}
	return writeFileAtomic(path, merged)
}

func writeCodexConfig(root string, projectConfig *config.Config, gatewayURL string, apiKeyMode bool) error {
	path := filepath.Join(root, ".codex", "config.toml")
	var content []byte
	if apiKeyMode {
		content = agentcfg.CodexConfig(gatewayURL, "")
	} else {
		content = agentcfg.CodexConfigOAuth()
	}
	content = agentcfg.AppendCodexMCP(content, enabledMCPServers(root, projectConfig))
	if err := writeFileAtomic(path, content); err != nil {
		return err
	}
	return appendCodexTrust(root)
}

// appendCodexTrust idempotently appends a `[projects."<root>"]` trust entry to
// the GLOBAL ~/.codex/config.toml, which — unlike the old per-project-isolated
// microVM — is now SHARED across every project on this host: a blind overwrite
// would destroy other projects' trust entries, so this only ever appends a
// missing entry, never rewrites the file wholesale.
func appendCodexTrust(root string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(homeDir, ".codex", "config.toml")
	existing, _ := os.ReadFile(path)
	marker := agentcfg.CodexTrustMarker(root)
	if existing != nil && strings.Contains(string(existing), marker) {
		return nil
	}
	updated := append(append([]byte{}, existing...), agentcfg.CodexTrustConfig(root)...)
	return writeFileAtomic(path, updated)
}

func (manager Manager) writeOmpConfig(root string, projectConfig *config.Config, gatewayURL string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	models, err := agentcfg.OmpModelsConfig(gatewayURL, agentcfg.OmpAPIKeyRef)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(homeDir, ".omp", "agent", "models.yml"), models); err != nil {
		return err
	}
	ompConfig, err := agentcfg.OmpConfig("")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(root, ".omp", "config.yml"), ompConfig); err != nil {
		return err
	}
	mcpConfig, err := agentcfg.OmpMcpConfig(enabledMCPServers(root, projectConfig))
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(root, ".omp", "mcp.json"), mcpConfig)
}

func (manager Manager) writeHermesConfig(root string, projectConfig *config.Config, gatewayURL string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	skillsDir := filepath.Join(root, ".ai-platform", "skills")
	content, err := agentcfg.HermesConfig(gatewayURL, "", "", skillsDir)
	if err != nil {
		return err
	}
	content, err = agentcfg.InjectHermesMCP(content, enabledMCPServers(root, projectConfig))
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(homeDir, ".hermes", "config.yaml"), content)
}

// headroomParams mirrors internal/contextopt.HeadroomParams without importing
// it (avoids a dependency edge for three constants).
func headroomParams(strategy string) (keepTurns, outputBufferTokens int) {
	switch strategy {
	case "conservative":
		return 8, 12000
	case "aggressive":
		return 2, 4000
	default:
		return 5, 8000
	}
}

func writeFileAtomic(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o644)
}
