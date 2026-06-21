// Package litellm renders the LiteLLM gateway config (arch §14–15) and exposes a
// client for `ai models status|test`. LiteLLM is a thin shared gateway: a single
// default model plus provider aliases, no per-task routing. Rendered config
// references **placeholder** credentials only — real secrets live in ClawPatrol.
package litellm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"gopkg.in/yaml.v3"
)

// OllamaAPIBase is where LiteLLM reaches the platform's Ollama container on the
// shared docker network (aip-net), used as the api_base for ollama/* models.
const OllamaAPIBase = "http://aip-ollama:11434"

// Routing is the platform's thin routing (arch §14): one default + aliases.
type Routing struct {
	Default string            `yaml:"default" json:"default"`
	Aliases map[string]string `yaml:"aliases" json:"aliases"`
}

// DefaultRouting is the built-in default (arch §14–15). It exposes a **full
// catalogue** via per-provider wildcards — the agent may name ANY model from
// Ollama, OpenAI, Anthropic, Google Gemini, or Groq and LiteLLM routes it on
// demand (cloud keys injected by ClawPatrol; Ollama needs none). Registering a
// model does not install it: an Ollama model must still be `ollama pull`ed, and
// a cloud model still needs its key. The named aliases are convenient handles for
// the recommended model per provider; `gemma4` (local Ollama) is the default.
func DefaultRouting() Routing {
	return Routing{
		Default: "gemma4",
		Aliases: map[string]string{
			// Recommended named handles.
			"gemma4":      "ollama/gemma4:31b",
			"gpt-5.5":     "openai/gpt-5.5",
			"claude-opus": "anthropic/claude-opus-4-8",
			"gemini-pro":  "gemini/gemini-3.5-flash",
			// Full per-provider catalogue (any model, routed on demand).
			"ollama/*":    "ollama/*",
			"openai/*":    "openai/*",
			"anthropic/*": "anthropic/*",
			"gemini/*":    "gemini/*",
			"groq/*":      "groq/*",
		},
	}
}

// placeholderKey returns the ClawPatrol placeholder env var for a provider's key
// (arch §17). Ollama needs no credential.
func placeholderKey(provider string) string {
	switch provider {
	case "openai":
		return "os.environ/OPENAI_API_KEY"
	case "anthropic":
		return "os.environ/ANTHROPIC_API_KEY"
	case "gemini":
		return "os.environ/GEMINI_API_KEY"
	case "openrouter":
		return "os.environ/OPENROUTER_API_KEY"
	case "groq":
		return "os.environ/GROQ_API_KEY"
	default:
		return ""
	}
}

// ConfigPath returns config/litellm/config.yaml.
func ConfigPath() (string, error) {
	configDir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "litellm", "config.yaml"), nil
}

// Render writes the LiteLLM config. When providerConfigPath is set (e.g. the
// acceptance harness's mock-provider config, or a real provider config), its
// contents are used verbatim; otherwise a config is generated from routing with
// placeholder credentials only.
func Render(routing Routing, providerConfigPath string) error {
	destination, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}

	var rendered []byte
	if providerConfigPath != "" {
		rendered, err = os.ReadFile(providerConfigPath)
		if err != nil {
			return fmt.Errorf("provider config: %w", err)
		}
	} else {
		rendered, err = yaml.Marshal(build(routing))
		if err != nil {
			return err
		}
	}
	return os.WriteFile(destination, rendered, 0o644)
}

// build maps the platform routing onto LiteLLM's native config shape
// (model_list + litellm_settings), with placeholder keys only.
func build(routing Routing) map[string]any {
	aliasNames := make([]string, 0, len(routing.Aliases))
	for name := range routing.Aliases {
		aliasNames = append(aliasNames, name)
	}
	sort.Strings(aliasNames)

	modelList := make([]map[string]any, 0, len(aliasNames))
	for _, name := range aliasNames {
		target := routing.Aliases[name]
		provider := target
		if slashIndex := strings.IndexByte(target, '/'); slashIndex >= 0 {
			provider = target[:slashIndex]
		}
		params := map[string]any{"model": target}
		if key := placeholderKey(provider); key != "" {
			params["api_key"] = key
		}
		if provider == "ollama" {
			// Ollama runs as a container on the shared network; reach it by name
			// (not localhost, which inside the LiteLLM container is itself).
			params["api_base"] = OllamaAPIBase
		}
		modelList = append(modelList, map[string]any{
			"model_name":     name,
			"litellm_params": params,
		})
	}
	return map[string]any{
		"model_list":       modelList,
		"litellm_settings": map[string]any{"default_model": routing.Default},
	}
}

// StatusInfo is the result of `ai models status` (CLI §8.1).
type StatusInfo struct {
	Healthy   bool     `json:"healthy"`
	Providers []string `json:"providers"`
	Default   string   `json:"default"`
	Ollama    bool     `json:"ollama"`
}

// TestResult is the result of `ai models test` (CLI §8.2). When OK is false,
// Status carries the gateway/provider HTTP status and Error the provider's error
// message (so the user learns *why* — bad model, missing key, provider down).
type TestResult struct {
	Model     string `json:"model"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status,omitempty"`
	LatencyMS int    `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// Human renders the success line for `ai models test` (the failure path is
// rendered by the CLI as an error with an actionable hint).
func (result TestResult) Human() string {
	return fmt.Sprintf("✓ %s reachable via LiteLLM (%dms)", result.Model, result.LatencyMS)
}

// Client talks to the running LiteLLM gateway. The real impl makes HTTP calls;
// tests use a fake.
type Client interface {
	Status() (StatusInfo, error)
	Test(model string) (TestResult, error)
}
