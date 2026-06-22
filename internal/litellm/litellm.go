// Package litellm renders the LiteLLM gateway config (arch §14–15) and exposes a
// client for `ai models status|test`. LiteLLM is a thin shared gateway: a single
// default model plus provider aliases, no per-task routing. Rendered config
// references **placeholder** credentials only (os.environ/<PROVIDER>_API_KEY) —
// the real keys live in the LiteLLM container's own environment, populated by
// `ai secrets` (keys-in-LiteLLM); they are never written to platform disk.
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
// demand (cloud keys resolved from the LiteLLM container's own environment;
// Ollama needs none). Registering a model does not install it: an Ollama model
// must still be `ollama pull`ed, and
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

// placeholderKey returns the os.environ placeholder LiteLLM resolves for a
// provider's key from the container's own environment (keys-in-LiteLLM, arch
// §17). Ollama needs no credential.
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
		"model_list": modelList,
		// callbacks wires LLM Guard via its LEGACY callback (llmguard_moderations) —
		// a security-scoped guardrail (PromptInjection + Secrets + bearer-token Regex)
		// backed by aip-llm-guard, reached via LLM_GUARD_API_BASE on the LiteLLM
		// container (see setup_real.go). It is NOT a modern guardrails-list entry.
		"litellm_settings": map[string]any{
			"default_model": routing.Default,
			"callbacks":     []string{"llmguard_moderations"},
		},
		"guardrails": buildGuardrails(),
	}
}

// secretEntities is the Presidio entity set the platform masks — deliberately
// scoped to unambiguous financial/identity SECRETS only. General PII (PERSON,
// LOCATION, DATE_TIME, EMAIL_ADDRESS, …) is intentionally NOT masked: a coding
// agent's prompts legitimately contain names, places, paths and emails, and
// masking them corrupts the prompt before the model sees it (e.g. "capital of
// France" → "capital of <LOCATION>"). Credentials/API keys/tokens are handled
// separately by the hide-secrets guardrail (detect-secrets).
var secretEntities = []string{
	"CREDIT_CARD",
	"US_SSN",
	"US_BANK_NUMBER",
	"IBAN_CODE",
	"CRYPTO",
}

// buildGuardrails renders the platform's always-on guardrails (arch §17). All are
// default_on:true, so no client request can opt out — and because every route,
// including cloud providers, passes through the LiteLLM proxy, cloud calls are
// guarded too. The platform ships only fully self-hostable, zero-config-token
// guardrails (no Hub tokens, no cloud APIs), scoped to SECRETS AND CREDENTIALS
// (not general PII):
//   - Presidio (pre_call masks secrets out of the prompt before the model sees
//     them; post_call masks them out of the response), restricted to
//     secretEntities — financial/identity secrets only. Backed by the
//     analyzer/anonymizer containers reached via PRESIDIO_*_API_BASE on the
//     LiteLLM container (see setup_real.go), launched by setup so the config
//     never references a guardrail with no backend.
//   - hide-secrets: LiteLLM's in-process secret detector (bundled detect-secrets,
//     150+ plugins) — strips API keys/tokens/credentials from the prompt. No
//     external server.
//
// LLM Guard is now ENABLED, but via LiteLLM's LEGACY callback
// (litellm_settings.callbacks: ["llmguard_moderations"], see build()) rather than
// this modern guardrails list — that is the only integration LiteLLM offers for
// it. It is SECURITY-SCOPED on purpose: aip-llm-guard runs only the
// PromptInjection, Secrets, and a bearer-token Regex scanners. The FULL LLM Guard
// scanner set (PII/Anonymize, Toxicity, BanTopics, Sentiment, Language, …) is
// DELIBERATELY DEFERRED — it corrupts ordinary coding prompts, the same reason
// general PII masking was removed. Guardrails AI also remains deferred (it needs a
// Guardrails Hub token + manual per-guard install, so it cannot be shipped fully
// automated). Both are documented as future considerations.
func buildGuardrails() []map[string]any {
	// Mask only the scoped secret entities, at a high confidence threshold to
	// avoid false positives on ordinary code/text.
	entityActions := make(map[string]any, len(secretEntities))
	for _, entity := range secretEntities {
		entityActions[entity] = "MASK"
	}
	scoredThresholds := map[string]any{"DEFAULT": 0.6}
	return []map[string]any{
		{
			"guardrail_name": "presidio-secrets-input",
			"litellm_params": map[string]any{
				"guardrail":                 "presidio",
				"mode":                      "pre_call",
				"default_on":                true,
				"presidio_filter_scope":     "input",
				"pii_entities_config":       entityActions,
				"presidio_score_thresholds": scoredThresholds,
			},
		},
		{
			"guardrail_name": "presidio-secrets-output",
			"litellm_params": map[string]any{
				"guardrail":                 "presidio",
				"mode":                      "post_call",
				"default_on":                true,
				"presidio_filter_scope":     "output",
				"pii_entities_config":       entityActions,
				"presidio_score_thresholds": scoredThresholds,
			},
		},
		{
			"guardrail_name": "hide-secrets",
			"litellm_params": map[string]any{
				"guardrail":  "hide-secrets",
				"mode":       "pre_call",
				"default_on": true,
			},
		},
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
