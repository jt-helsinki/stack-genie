// Package agentcfg renders the provider configuration the in-workspace agent
// CLIs (opencode, pi) need to talk to the host Headroom proxy through a scoped
// LiteLLM virtual key (arch §15, §17). At workspace start the platform mints a
// per-workspace virtual key and writes one provider file per agent CLI into the
// microVM so the agent reaches the host gateway with that key.
//
// These are pure generators: they marshal stable, indented JSON and never touch
// disk or any external tool, so they are exhaustively unit-tested. The virtual
// key flows host→VM only and is never written to platform disk.
package agentcfg

import "encoding/json"

// ProviderID is the provider handle both agent CLIs use for the host gateway.
const ProviderID = "aip-gateway"

// OpenCodeConfig renders opencode's provider config (`~/.config/opencode/opencode.json`).
//
// opencode talks to the gateway as an OpenAI-compatible provider
// (`@ai-sdk/openai-compatible`). It CAN inject per-request body fields via each
// model's options (AI SDK providerOptions → request body), so the per-project
// Headroom knobs (keepTurns, outputBufferTokens) ride on every model as
// headroom_keep_turns / headroom_output_buffer_tokens. The baseURL carries the
// required /v1 suffix and apiKey carries the scoped virtual key.
func OpenCodeConfig(gatewayURL, apiKey, defaultModel string, models []string, keepTurns, outputBufferTokens int) ([]byte, error) {
	modelEntries := make(map[string]any, len(models))
	for _, model := range models {
		modelEntries[model] = map[string]any{
			"name": model,
			"options": map[string]any{
				"headroom_keep_turns":           keepTurns,
				"headroom_output_buffer_tokens": outputBufferTokens,
			},
		}
	}
	document := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{
			ProviderID: map[string]any{
				"npm":  "@ai-sdk/openai-compatible",
				"name": "AI Platform Gateway",
				"options": map[string]any{
					"baseURL": gatewayURL,
					"apiKey":  apiKey,
				},
				"models": modelEntries,
			},
		},
		"model": ProviderID + "/" + defaultModel,
	}
	return marshalStable(document)
}

// PiConfig renders pi's provider config (`~/.pi/agent/models.json`).
//
// pi talks to the gateway as an OpenAI-completions provider. Unlike opencode, pi
// CANNOT inject per-request body fields, so the Headroom knobs are deliberately
// NOT written here — pi's requests fall back to Headroom's server-side defaults.
// Per-project compression tuning therefore applies to opencode only; pi uses the
// host Headroom defaults. The baseUrl carries the required /v1 suffix and apiKey
// carries the scoped virtual key.
func PiConfig(gatewayURL, apiKey, defaultModel string, models []string) ([]byte, error) {
	modelEntries := make([]map[string]any, 0, len(models))
	for _, model := range models {
		modelEntries = append(modelEntries, map[string]any{
			"id":   model,
			"name": model,
		})
	}
	document := map[string]any{
		"providers": map[string]any{
			ProviderID: map[string]any{
				"baseUrl": gatewayURL,
				"api":     "openai-completions",
				"apiKey":  apiKey,
				"models":  modelEntries,
			},
		},
	}
	return marshalStable(document)
}

// TmuxConfig renders the managed tmux configuration written to
// `/home/workspace/.tmux.conf` at workspace start. The platform's workspace
// session model is "tmux-transparent": persistent, reattachable per-CLI tmux
// sessions back `ai agent`/`ai attach`/`ai sessions`, but the user never types a
// tmux command. To keep tmux invisible to a casual user it hides the status bar;
// it enables the mouse so the wheel scrolls naturally through scrollback, sets
// vi-style copy-mode keys, and keeps a generous scrollback history.
func TmuxConfig() []byte {
	return []byte(`# Managed by the AI Development Platform — tmux-transparent workspace sessions.
# Do not edit by hand; this file is rewritten on every workspace start.
set -g mouse on
set -g status off
setw -g mode-keys vi
set -g history-limit 50000
`)
}

// marshalStable produces deterministic, indented JSON. encoding/json sorts map
// keys, so the output is stable across runs (no spurious workspace-start diffs).
func marshalStable(document any) ([]byte, error) {
	return json.MarshalIndent(document, "", "  ")
}
