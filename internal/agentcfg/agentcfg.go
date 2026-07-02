// Package agentcfg renders the provider configuration the in-workspace agent
// CLIs need to talk to the host Headroom proxy through a scoped LiteLLM virtual
// key (arch §15, §17). At workspace start the platform mints a per-workspace
// virtual key and routes ALL FIVE agent CLIs through the gateway with that key:
//
//   - opencode / pi route via a JSON config file written into the microVM
//     (OpenCodeConfig / PiConfig).
//   - claude-code (`claude`), codex, and gemini route via environment variables
//     (claude-code: ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN; gemini:
//     GOOGLE_GEMINI_BASE_URL + GEMINI_API_KEY) plus, for codex, a TOML provider
//     block in ~/.codex/config.toml that references an env-supplied key — see
//     AgentEnvScript / CodexConfig. The env vars are written into the in-VM agent
//     env file (key in-VM only) and sourced by every shell + agent session.
//
// These are pure generators: they marshal stable, indented JSON/TOML/shell and
// never touch disk or any external tool, so they are exhaustively unit-tested.
// The virtual key flows host→VM only and is never written to platform disk: the
// host-side templates under <project>/.ai-platform/agents/ are KEYLESS, and the
// key is injected solely into the final config/env written INTO the microVM.
package agentcfg

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

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
//
// In the catalog-driven model system there is no built-in default model; the
// served list (from the live gateway) may even be empty until the user adds a
// provider key or pulls an Ollama model. defaultModel is therefore optional: when
// empty no top-level `model` is written and opencode falls back to its own
// default-model selection.
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
		// Restrict opencode to ONLY the platform gateway provider, so the model
		// picker is exactly the models LiteLLM serves — opencode never falls back to
		// an auto-loaded built-in provider or the public models.dev catalog for model
		// selection. (opencode has no officially-supported flag to fully disable the
		// models.dev startup fetch yet — sst/opencode#4959 — but with only this
		// provider enabled its catalog is unused.)
		"enabled_providers": []string{ProviderID},
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
	}
	if defaultModel != "" {
		document["model"] = ProviderID + "/" + defaultModel
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
//
// Terminal capabilities for modern TUI agent CLIs (OpenCode, …): the microVM is
// HEADLESS — there is no terminal emulator inside it. Each agent runs inside tmux
// over a PTY (`msb exec -t`) and its byte stream is rendered by the USER'S HOST
// terminal emulator (WezTerm/Ghostty/iTerm/…). The fix for correct rendering is
// therefore not a VM-side emulator but terminfo + tmux directives so the VM emits
// the right sequences THROUGH tmux:
//   - default-terminal "tmux-256color": the modern terminfo entry tmux advertises
//     to programs inside it (shipped by the image's ncurses-term package).
//   - terminal-features ",*:RGB": pass 24-bit truecolor through for ALL outer
//     terminals (the host emulator decides what it actually renders).
//   - terminal-features ",*:extkeys" + extended-keys on: forward CSI-u / kitty
//     extended key encodings so OpenCode's shift+enter and ctrl-combos survive the
//     tmux layer instead of being collapsed to legacy codes.
//   - escape-time 10: a short escape timeout so single Esc / Alt chords feel snappy
//     in a TUI rather than lagging behind tmux's key-sequence wait.
//
// These directives require tmux >= 3.2 (terminal-features, extended-keys); the
// shipped bases all satisfy this (debian trixie ~3.4, debian bookworm 3.3a,
// ubuntu 24.04 3.4, almalinux 10 ~3.4). Note (a known limitation): tmux does NOT
// proxy GPU/graphics protocols (kitty graphics, sixel), so a TUI's image-rendering
// features will not work inside the tmux session.
func TmuxConfig() []byte {
	return []byte(`# Managed by the AI Development Platform — tmux-transparent workspace sessions.
# Do not edit by hand; this file is rewritten on every workspace start.
set -g mouse on
set -g status off
setw -g mode-keys vi
set -g history-limit 50000

# Terminal capabilities so modern TUI agent CLIs render correctly THROUGH tmux.
# The VM is headless: the user's HOST terminal emulator draws the PTY stream, so
# these only need to make tmux emit/forward the right sequences (terminfo ships
# via the image's ncurses-term). Requires tmux >= 3.2.
set -g default-terminal "tmux-256color"
set -as terminal-features ",*:RGB"
set -as terminal-features ",*:extkeys"
set -s extended-keys on
# Emit extended keys in CSI-u form (not the legacy xterm form). Modern agent TUIs
# (pi, opencode) expect csi-u for shift+enter / ctrl-combos; tmux defaults to xterm.
set -g extended-keys-format csi-u
set -sg escape-time 10
`)
}

// marshalStable produces deterministic, indented JSON. encoding/json sorts map
// keys, so the output is stable across runs (no spurious workspace-start diffs).
func marshalStable(document any) ([]byte, error) {
	return json.MarshalIndent(document, "", "  ")
}

// MergeOpenCodeConfig produces the FINAL opencode config from any EXISTING project
// config plus the dynamic values minted at start. It parses the existing JSON and
// deep-merges the generated provider config over it, so the dynamic provider block
// (baseURL, the {env:} key ref, the served-model picker, Headroom knobs) always wins
// while any other user-added top-level keys (themes, MCP servers, …) survive. A
// nil/empty/invalid input falls back to the freshly-generated config, so a brand-new
// project (no file) or a corrupt edit still yields a working config. When called with
// the keyless key ref (OpenCodeAPIKeyRef) the result is keyless — safe on host disk.
func MergeOpenCodeConfig(template []byte, gatewayURL, apiKey, defaultModel string, models []string, keepTurns, outputBufferTokens int) ([]byte, error) {
	generated, err := OpenCodeConfig(gatewayURL, apiKey, defaultModel, models, keepTurns, outputBufferTokens)
	if err != nil {
		return nil, err
	}
	// With no default to seed, actively DROP any previously-seeded top-level "model" so
	// opencode's persisted last-used selection wins (config "model" outranks last-used;
	// a stale one would re-pin it every launch — this is the "remember" half of the
	// seed-then-remember default).
	return mergeJSONOver(template, generated, dropKeysWhenNoDefault(defaultModel, "model")...)
}

// dropKeysWhenNoDefault returns keys to strip from a merged config when no default
// model is being seeded (empty defaultModel), and none when one is.
func dropKeysWhenNoDefault(defaultModel string, keys ...string) []string {
	if defaultModel != "" {
		return nil
	}
	return keys
}

// PiProjectSettingsGuest is the in-VM path of pi's per-project settings.
const PiProjectSettingsGuest = projectDirGuest + "/.pi/settings.json"

// PiSettings renders pi's per-project .pi/settings.json: it makes the gateway the
// default provider, sets the workspace default model (when one was chosen at setup),
// and points pi's skills/prompts RESOURCE PATHS at the symlinked shared pools (pi
// resolves these paths relative to .pi, where we symlink skills/ and prompts/ into the
// shared .ai-platform pools). Keyless. (pi.dev/docs/latest/settings.)
func PiSettings(defaultModel string) ([]byte, error) {
	document := map[string]any{
		"defaultProvider": ProviderID,
		"skills":          []string{"skills"},
		"prompts":         []string{"prompts"},
	}
	if defaultModel != "" {
		document["defaultModel"] = defaultModel
	}
	return marshalStable(document)
}

// MergePiSettings deep-merges the generated pi settings over an existing project
// .pi/settings.json — the user's other settings survive, the managed keys win.
func MergePiSettings(existing []byte, defaultModel string) ([]byte, error) {
	generated, err := PiSettings(defaultModel)
	if err != nil {
		return nil, err
	}
	// Same seed-then-remember rule as opencode: drop a stale defaultModel when not seeding.
	return mergeJSONOver(existing, generated, dropKeysWhenNoDefault(defaultModel, "defaultModel")...)
}

// mergeJSONOver deep-merges generated over template (generated wins) and renders
// the result with marshalStable. A nil/empty/unparseable template degrades to the
// generated bytes verbatim, so a missing or corrupt host template never breaks the
// workspace start.
// mergeJSONOver deep-merges the generated config over an existing template. dropKeys
// are top-level keys removed from the FINAL result — used to actively unset a
// previously-written key (e.g. a seeded "model") that must not linger, since the merge
// would otherwise preserve the template's copy of it.
func mergeJSONOver(template, generated []byte, dropKeys ...string) ([]byte, error) {
	var overlay map[string]any
	if err := json.Unmarshal(generated, &overlay); err != nil {
		return nil, err
	}
	merged := overlay
	var base map[string]any
	if len(bytes.TrimSpace(template)) > 0 && json.Unmarshal(template, &base) == nil {
		merged = deepMergeJSON(base, overlay)
	}
	for _, key := range dropKeys {
		delete(merged, key)
	}
	return marshalStable(merged)
}

// deepMergeJSON returns base with overlay applied on top; nested objects merge
// recursively and overlay scalars/arrays win. Inputs are not mutated.
func deepMergeJSON(base, overlay map[string]any) map[string]any {
	result := make(map[string]any, len(base))
	for key, value := range base {
		result[key] = value
	}
	for key, overlayValue := range overlay {
		if existing, ok := result[key]; ok {
			if existingMap, isMap := existing.(map[string]any); isMap {
				if overlayMap, isOverlayMap := overlayValue.(map[string]any); isOverlayMap {
					result[key] = deepMergeJSON(existingMap, overlayMap)
					continue
				}
			}
		}
		result[key] = overlayValue
	}
	return result
}

// AgentEnvVarName / AgentEnvFileGuestPath name the env-routing surface for the
// CLIs that take their gateway config from environment variables (claude-code,
// codex, gemini). The env file is written INTO the microVM (key in-VM only) and
// sourced by every shell + agent session.
const (
	// AgentEnvFileGuestPath is the in-VM file the agent env vars are written to.
	AgentEnvFileGuestPath = "/home/workspace/.config/aip/agent-env.sh"

	// claudeBaseURLVar / claudeAuthVar route claude-code (`claude`) through the
	// gateway's Anthropic-compatible surface. The base URL is the gateway WITHOUT
	// the /v1 suffix — claude-code appends /v1/messages itself — and the bearer
	// token goes in ANTHROPIC_AUTH_TOKEN (Authorization: Bearer), which the LiteLLM
	// gateway reads. (code.claude.com/docs/en/llm-gateway-connect.)
	claudeBaseURLVar = "ANTHROPIC_BASE_URL"
	claudeAuthVar    = "ANTHROPIC_AUTH_TOKEN"

	// codexKeyVar is the env var codex's config.toml provider block references via
	// env_key — codex reads the gateway key from the environment, never from the
	// committed TOML. (developers.openai.com/codex/config-reference.)
	codexKeyVar = "AIP_GATEWAY_KEY"

	// geminiBaseURLVar / geminiKeyVar route google gemini-cli through the gateway:
	// the @google/genai SDK honours GOOGLE_GEMINI_BASE_URL (the gateway ROOT, no
	// /v1) and GEMINI_API_KEY. (docs.litellm.ai/docs/tutorials/litellm_gemini_cli.)
	geminiBaseURLVar = "GOOGLE_GEMINI_BASE_URL"
	geminiKeyVar     = "GEMINI_API_KEY"

	// openCodeConfigVar points opencode at its per-project config FILE. opencode's
	// documented project config is <project>/opencode.json at the repo root; setting
	// OPENCODE_CONFIG to the file we write under .opencode/ makes opencode load it
	// regardless of the root-vs-subdir convention (it is a documented precedence
	// entry). (opencode.ai/docs/config.)
	openCodeConfigVar = "OPENCODE_CONFIG"
)

// Env-interpolation references for the on-disk (keyless) PROJECT configs: the scoped
// virtual key is supplied via the AIP_GATEWAY_KEY env var (exported by
// AgentEnvScript, in-VM only) and NEVER written to disk. opencode uses {env:VAR}
// and pi uses $VAR — both officially documented interpolation forms.
const (
	OpenCodeAPIKeyRef = "{env:" + codexKeyVar + "}"
	PiAPIKeyRef       = "$" + codexKeyVar
)

// In-VM paths of the per-CLI PROJECT configs, under the bind-mounted project dir
// (/home/workspace/project — mirrors workspace.workspaceWorkdir; the project dir is
// ONE directory shared host↔guest). The keyless configs live here so the project is
// self-describing; OPENCODE_CONFIG points opencode at its file, refresh-models
// rewrites the opencode/pi files in place, and codex is told to trust this project.
const (
	projectDirGuest            = "/home/workspace/project"
	OpenCodeProjectConfigGuest = projectDirGuest + "/.opencode/opencode.json"
	CodexProjectConfigGuest    = projectDirGuest + "/.codex/config.toml"
)

// PiGlobalModelsGuest is pi's models config at the path pi ACTUALLY reads —
// ~/.pi/agent/models.json (GLOBAL, in-VM home). pi does NOT read a project
// .pi/models.json, so the served-model provider config is written here so pi lists the
// SAME gateway models as opencode. In-VM home (off host disk); keyless via $VAR.
const PiGlobalModelsGuest = "/home/workspace/.pi/agent/models.json"

// gatewayRoot strips a trailing /v1 (and any trailing slash) from the gateway URL,
// for the CLIs whose SDK appends its own version/path segment (claude-code adds
// /v1/messages; gemini-cli's genai SDK adds its own path). opencode/pi/codex keep
// the /v1-suffixed URL verbatim.
func gatewayRoot(gatewayURL string) string {
	trimmed := strings.TrimRight(gatewayURL, "/")
	return strings.TrimSuffix(trimmed, "/v1")
}

// AgentEnvScript renders the POSIX shell snippet that exports the gateway env vars
// the env-routed agent CLIs read (claude-code, codex, gemini). It is written INTO
// the microVM at AgentEnvFileGuestPath (the scoped key flows host→VM only) and
// sourced by every shell + agent session, so all three CLIs reach the gateway with
// the workspace's scoped virtual key by default. gatewayURL carries the /v1 suffix
// (codex keeps it); the claude-code / gemini base URLs are derived as the gateway
// root. apiKey is the scoped virtual key.
func AgentEnvScript(gatewayURL, apiKey, graphifyModel string) []byte {
	root := gatewayRoot(gatewayURL)
	var buffer bytes.Buffer
	buffer.WriteString("# Managed by the AI Development Platform — gateway env for the env-routed\n")
	buffer.WriteString("# agent CLIs (claude-code, codex, gemini). Written into the microVM at\n")
	buffer.WriteString("# workspace start and sourced by every shell + agent session. Do not edit by\n")
	buffer.WriteString("# hand; this file is rewritten on every workspace start and holds the\n")
	buffer.WriteString("# workspace's scoped virtual key (it never leaves the VM).\n")
	// claude-code: Anthropic-compatible surface at the gateway root + bearer token.
	buffer.WriteString("export " + claudeBaseURLVar + "=" + shellQuote(root) + "\n")
	buffer.WriteString("export " + claudeAuthVar + "=" + shellQuote(apiKey) + "\n")
	// codex: the key its config.toml provider block reads via env_key.
	buffer.WriteString("export " + codexKeyVar + "=" + shellQuote(apiKey) + "\n")
	// gemini-cli: the genai SDK's base-URL + key overrides (gateway root).
	buffer.WriteString("export " + geminiBaseURLVar + "=" + shellQuote(root) + "\n")
	buffer.WriteString("export " + geminiKeyVar + "=" + shellQuote(apiKey) + "\n")
	// opencode: point it at the per-project config file we write under .opencode/
	// (keyless; the key resolves from AIP_GATEWAY_KEY via {env:} interpolation).
	buffer.WriteString("export " + openCodeConfigVar + "=" + shellQuote(OpenCodeProjectConfigGuest) + "\n")
	// Graphify's headless LLM backend, when a model is configured: route through the
	// gateway's OpenAI-compatible endpoint (nginx → Headroom → LiteLLM → Ollama).
	// Invoke as `graphify --backend openai`. No real provider key — the scoped
	// virtual key; nothing else in the platform reads OPENAI_*.
	if graphifyModel != "" {
		buffer.WriteString("export OPENAI_BASE_URL=" + shellQuote(gatewayURL) + "\n") // .../v1
		buffer.WriteString("export OPENAI_API_KEY=" + shellQuote(apiKey) + "\n")
		buffer.WriteString("export OPENAI_MODEL=" + shellQuote("ollama/"+graphifyModel) + "\n")
	}
	return buffer.Bytes()
}

// BashProfile renders the managed ~/.bash_profile written into the microVM at
// workspace start. It sources the standard ~/.bashrc (so an interactive login
// shell behaves normally) and then the agent env file (the gateway env vars for
// the env-routed CLIs), so a user running claude/codex/gemini from `ai shell` or
// `ai attach` is routed through the gateway with the workspace's scoped key. It is
// rewritten on every start; the agent env file it sources holds the key (in-VM).
func BashProfile() []byte {
	var buffer bytes.Buffer
	buffer.WriteString("# Managed by the AI Development Platform — sources the agent gateway env for\n")
	buffer.WriteString("# interactive login shells. Do not edit by hand; rewritten on every start.\n")
	buffer.WriteString("[ -f \"$HOME/.bashrc\" ] && . \"$HOME/.bashrc\"\n")
	buffer.WriteString("[ -f " + shellQuote(AgentEnvFileGuestPath) + " ] && . " + shellQuote(AgentEnvFileGuestPath) + "\n")
	return buffer.Bytes()
}

// CodexConfigGuestPath is the in-VM path codex reads its config from.
const CodexConfigGuestPath = "/home/workspace/.codex/config.toml"

// CodexConfig renders codex's ~/.codex/config.toml routing it through the gateway.
// codex requires the OpenAI RESPONSES wire API (chat-completions support was
// removed); the LiteLLM gateway exposes a /responses surface, so the provider's
// base_url keeps the /v1 suffix and wire_api is "responses". The provider key is
// supplied via the env_key env var (codexKeyVar), NEVER written into this file, so
// this config is KEYLESS and safe as a host-side template too.
// (developers.openai.com/codex/config-reference.)
//
// defaultModel is optional: when non-empty it is written as the top-level `model`
// so codex defaults to a gateway-served model; empty omits it (codex falls back to
// its own default selection, matching the catalog-driven no-default policy).
func CodexConfig(gatewayURL, defaultModel string) []byte {
	var buffer bytes.Buffer
	buffer.WriteString("# Managed by the AI Development Platform — route codex through the gateway.\n")
	buffer.WriteString("# Keyless by design: the gateway key is read from the " + codexKeyVar + " env var\n")
	buffer.WriteString("# (set in the in-VM agent env), never written here.\n")
	buffer.WriteString("model_provider = " + tomlString(ProviderID) + "\n")
	if defaultModel != "" {
		buffer.WriteString("model = " + tomlString(defaultModel) + "\n")
	}
	buffer.WriteString("\n[model_providers." + ProviderID + "]\n")
	buffer.WriteString("name = " + tomlString("AI Platform Gateway") + "\n")
	buffer.WriteString("base_url = " + tomlString(strings.TrimRight(gatewayURL, "/")) + "\n")
	buffer.WriteString("env_key = " + tomlString(codexKeyVar) + "\n")
	buffer.WriteString("wire_api = " + tomlString("responses") + "\n")
	return buffer.Bytes()
}

// tomlString renders a Go string as a TOML basic string (escaping backslash and
// double-quote). The values here are simple URLs / identifiers, so this is enough.
func tomlString(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// ClaudeProjectConfigGuest is the in-VM path claude-code reads its per-project
// settings from.
const ClaudeProjectConfigGuest = projectDirGuest + "/.claude/settings.json"

// ClaudeSettings renders claude-code's per-project .claude/settings.json, routing it
// through the gateway. It sets ONLY the base URL (via the settings `env` block); the
// bearer token comes from the exported ANTHROPIC_AUTH_TOKEN env var (settings.json has
// no ${VAR} interpolation), so this file is KEYLESS and safe on host disk. The base URL
// is the gateway ROOT (no /v1 — claude-code appends /v1/messages itself).
// (code.claude.com/docs/en/settings, .../llm-gateway-connect.)
func ClaudeSettings(gatewayURL string) ([]byte, error) {
	document := map[string]any{
		"env": map[string]any{
			claudeBaseURLVar: gatewayRoot(gatewayURL),
		},
	}
	return marshalStable(document)
}

// MergeClaudeSettings deep-merges the generated keyless env block over an existing
// project .claude/settings.json — the user's other settings survive and the gateway
// base URL wins. A nil/corrupt existing file degrades to the generated settings.
func MergeClaudeSettings(existing []byte, gatewayURL string) ([]byte, error) {
	generated, err := ClaudeSettings(gatewayURL)
	if err != nil {
		return nil, err
	}
	return mergeJSONOver(existing, generated)
}

// CodexTrustConfig renders the GLOBAL ~/.codex/config.toml (CodexConfigGuestPath)
// marking the workspace project dir as trusted, so codex loads the keyless
// per-project .codex/config.toml provider block (codex loads a project config only
// for TRUSTED projects). It carries no key.
// (developers.openai.com/codex/config-advanced.)
func CodexTrustConfig() []byte {
	var buffer bytes.Buffer
	buffer.WriteString("# Managed by the AI Development Platform — trust the workspace project\n")
	buffer.WriteString("# so codex loads its keyless per-project .codex/config.toml provider block.\n")
	buffer.WriteString("[projects." + tomlString(projectDirGuest) + "]\n")
	buffer.WriteString("trust_level = " + tomlString("trusted") + "\n")
	return buffer.Bytes()
}

// The agent provider configs live at each CLI's per-project location inside the
// bind-mounted project dir; RefreshScript rewrites the opencode + pi files in place.
// (These alias the exported project-config guest paths declared above.)
const (
	openCodeGuestPath = OpenCodeProjectConfigGuest
	piGuestPath       = PiGlobalModelsGuest
)

// modelSentinel is the per-model token the shell substitutes with each real model
// id. sentinelA/sentinelB are two distinct probe models the host-side splitter
// renders to derive the exact prefix / per-model item / separator / suffix from
// MarshalIndent's output, so the script reproduces Go's layout byte-for-byte. They
// are chosen so they never collide with a real model id or any other JSON token,
// and they sort so A precedes B (the splitter relies on render order).
const (
	modelSentinel = "@@AIP_MODEL@@"
	sentinelA     = "Aaipmodelprobe"
	sentinelB     = "Zaipmodelprobe"
)

// RefreshScript generates a self-contained POSIX shell script that, run INSIDE
// the workspace microVM, re-pulls the in-VM agent model picker WITHOUT restarting
// the microVM. It is installed at /usr/local/bin/refresh-models at workspace start
// (see internal/workspace). Live in-VM execution is a `hardware bring-up`
// verification item; the host-side generation and the script's own logic are
// fully unit-tested (the generated script is executed against a fake curl).
//
// In the catalog-driven model system the in-VM picker is exactly the set of models
// the LiteLLM gateway currently SERVES (its DB-backed models — a keyed provider's
// catalog models + the registered Ollama models). The script:
//   - requires curl (clear error + exit if absent);
//   - GETs <gatewayBaseURL>/models (the OpenAI-compatible list endpoint) with the
//     scoped virtual key, and extracts the served model ids PORTABLY (grep/sed over
//     the "id":"…" fields — no jq/python);
//   - dedups + sorts them (LC_ALL=C sort -u) exactly as workspace.pickerModels does,
//     so the result matches a fresh workspace start;
//   - rewrites opencode.json + pi models.json BYTE-IDENTICAL to what OpenCodeConfig
//     / PiConfig would produce for that served list, default, gateway, and key —
//     by splicing the model fragments into Go-rendered JSON skeletons;
//   - DEGRADES: if the fetch fails it leaves the existing configs untouched and
//     warns (it never wipes them to an empty list);
//   - prints a short human summary.
//
// gatewayBaseURL is the SAME url written into the agent configs (carrying the /v1
// suffix); the script GETs the /models endpoint at that base. apiKey is the scoped
// virtual key (host→VM only) — used ONLY to authenticate the /models fetch, NEVER
// baked into the rewritten configs (those stay keyless via the {env:}/$VAR refs, so
// the on-disk project configs never gain the key). defaultModel is optional.
func RefreshScript(gatewayBaseURL, apiKey, defaultModel string, keepTurns, outputBufferTokens int) ([]byte, error) {
	// Render the two JSON skeletons with a single sentinel model so we can split
	// each into a prefix / per-model template / suffix the shell splices into. The
	// rendered fragments inherit MarshalIndent's exact indentation, guaranteeing
	// byte parity with OpenCodeConfig / PiConfig for the same merged list. The bodies
	// use the KEYLESS key-refs (not apiKey), matching the configs written at start.
	openCode, err := splitSkeleton(func(models []string) ([]byte, error) {
		return OpenCodeConfig(gatewayBaseURL, OpenCodeAPIKeyRef, defaultModel, models, keepTurns, outputBufferTokens)
	})
	if err != nil {
		return nil, fmt.Errorf("render opencode skeleton: %w", err)
	}
	pi, err := splitSkeleton(func(models []string) ([]byte, error) {
		return PiConfig(gatewayBaseURL, PiAPIKeyRef, defaultModel, models)
	})
	if err != nil {
		return nil, fmt.Errorf("render pi skeleton: %w", err)
	}

	var script bytes.Buffer
	script.WriteString("#!/usr/bin/env bash\n")
	script.WriteString(`# Managed by the AI Development Platform — refresh the in-workspace agent model
# picker. Run this INSIDE the workspace after changing the served models on the host
# (add a provider key with 'ai keys', or pull/remove an Ollama model):
#   refresh-models
# It re-fetches the models the gateway currently SERVES (its DB-backed models) from
# the gateway's /v1/models endpoint and rewrites the agent CLI configs in place.
# Restart your agent CLI afterwards to pick up the new list.
# Do not edit by hand; this file is rewritten on every workspace start.
set -u

`)
	// The baked-in values. The gateway URL keeps its /v1 suffix (it is written into
	// the configs verbatim); the served-models list endpoint is <base>/models.
	script.WriteString("GATEWAY_URL=" + shellQuote(gatewayBaseURL) + "\n")
	script.WriteString("MODELS_URL=" + shellQuote(modelsURL(gatewayBaseURL)) + "\n")
	script.WriteString("API_KEY=" + shellQuote(apiKey) + "\n")
	script.WriteString("OPENCODE_PATH=" + shellQuote(openCodeGuestPath) + "\n")
	script.WriteString("PI_PATH=" + shellQuote(piGuestPath) + "\n\n")

	// The four JSON fragments per config, base64-encoded so arbitrary bytes
	// (newlines, quotes, indentation, the inter-entry separator) survive embedding
	// in the script unchanged.
	script.WriteString("OPENCODE_PREFIX=" + b64Literal(openCode.prefix) + "\n")
	script.WriteString("OPENCODE_ITEM=" + b64Literal(openCode.item) + "\n")
	script.WriteString("OPENCODE_SEP=" + b64Literal(openCode.separator) + "\n")
	script.WriteString("OPENCODE_SUFFIX=" + b64Literal(openCode.suffix) + "\n")
	script.WriteString("PI_PREFIX=" + b64Literal(pi.prefix) + "\n")
	script.WriteString("PI_ITEM=" + b64Literal(pi.item) + "\n")
	script.WriteString("PI_SEP=" + b64Literal(pi.separator) + "\n")
	script.WriteString("PI_SUFFIX=" + b64Literal(pi.suffix) + "\n\n")

	script.WriteString(refreshScriptBody)
	return script.Bytes(), nil
}

// refreshScriptBody is the fixed logic of the refresh script. It consumes the
// baked variables RefreshScript prepends. The MODEL_SENTINEL token in the item
// templates is replaced with each (already JSON-escaped) model id; the model ids
// here are simple (alias / "ollama/<name>" / "<provider>/<model>") so a literal
// substitution of the bare value is correct — and the unit test pins parity.
var refreshScriptBody = strings.NewReplacer("@@SENTINEL@@", modelSentinel).Replace(`SENTINEL='@@SENTINEL@@'

if ! command -v curl >/dev/null 2>&1; then
  echo "refresh-models: curl is not installed in this workspace — add 'curl' to the image and recreate the workspace" >&2
  exit 1
fi

# decode <base64> -> stdout (portable: busybox/coreutils/macOS all accept -d).
decode() { printf '%s' "$1" | base64 -d 2>/dev/null || printf '%s' "$1" | base64 --decode; }

# JSON-escape a single line for embedding as a bare value (backslash, quote).
json_escape() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }

# Fetch the models the gateway currently SERVES from its OpenAI-compatible
# /v1/models endpoint (authenticated with the scoped virtual key). Extract each
# "id":"…" PORTABLY (no jq/python): one served model id per line. If the fetch
# fails we leave the existing configs UNTOUCHED rather than wiping them.
if ! served_body="$(curl -fsS --max-time 10 -H "Authorization: Bearer $API_KEY" "$MODELS_URL" 2>/dev/null)"; then
  echo "refresh-models: could not reach the gateway model list ($MODELS_URL) — leaving the current configs unchanged" >&2
  exit 1
fi
served="$(printf '%s' "$served_body" \
  | grep -o '"id"[[:space:]]*:[[:space:]]*"[^"]*"' \
  | sed -e 's/.*:[[:space:]]*"//' -e 's/"$//')"

# Dedup + sort EXACTLY as pickerModels does (LC_ALL=C lexical sort, unique).
# Result: one served model id per line.
merged="$(printf '%s\n' "$served" | sed '/^$/d' | LC_ALL=C sort -u)"
merged_count="$(printf '%s\n' "$merged" | sed '/^$/d' | wc -l | tr -d ' ')"

# render <prefix-b64> <item-b64> <sep-b64> <suffix-b64> -> full JSON on stdout,
# splicing one decoded item per model (sentinel -> escaped model id) joined by the
# decoded inter-entry separator, exactly reproducing MarshalIndent's layout.
render() {
  prefix="$(decode "$1")"; item_tmpl="$(decode "$2")"; sep="$(decode "$3")"; suffix="$(decode "$4")"
  printf '%s' "$prefix"
  first=1
  while IFS= read -r model; do
    [ -z "$model" ] && continue
    esc="$(json_escape "$model")"
    item="${item_tmpl//$SENTINEL/$esc}"
    if [ "$first" -eq 1 ]; then first=0; else printf '%s' "$sep"; fi
    printf '%s' "$item"
  done <<EOF
$merged
EOF
  printf '%s' "$suffix"
}

write_config() {
  path="$1"; content="$2"
  mkdir -p "$(dirname "$path")" || return 1
  tmp="$path.refresh.$$"
  printf '%s' "$content" >"$tmp" || return 1
  mv "$tmp" "$path"
}

opencode_json="$(render "$OPENCODE_PREFIX" "$OPENCODE_ITEM" "$OPENCODE_SEP" "$OPENCODE_SUFFIX")"
pi_json="$(render "$PI_PREFIX" "$PI_ITEM" "$PI_SEP" "$PI_SUFFIX")"

write_config "$OPENCODE_PATH" "$opencode_json" || { echo "refresh-models: failed to write $OPENCODE_PATH" >&2; exit 1; }
write_config "$PI_PATH" "$pi_json" || { echo "refresh-models: failed to write $PI_PATH" >&2; exit 1; }

echo "refreshed: $merged_count served models — restart your agent CLI to pick them up"
`)

// skeleton is the decomposition of a config's JSON around its model collection:
// the constant prefix, the per-model item template (containing modelSentinel once
// per model-id occurrence), the constant inter-entry separator, and the constant
// suffix. The script emits prefix + item·sep·item·… + suffix, reproducing
// MarshalIndent's exact byte layout.
type skeleton struct {
	prefix    []byte
	item      []byte
	separator []byte
	suffix    []byte
}

// splitSkeleton derives the decomposition by rendering the config with two distinct
// probe models (sentinelA, sentinelB) and diffing the result against a single-model
// render. The common prefix/suffix are constant; the middle of the two-model render
// is item(A) + separator + item(B), and the single-model item locates where each
// item ends — so the separator is whatever MarshalIndent places BETWEEN entries
// (for a map: "},\n        " style; for an array: "},\n    " style — captured
// verbatim rather than assumed).
func splitSkeleton(render func(models []string) ([]byte, error)) (skeleton, error) {
	one, err := render([]string{modelSentinel})
	if err != nil {
		return skeleton{}, err
	}
	// sentinelA sorts before sentinelB, matching the script's sorted model order, so
	// the two-model render places A's entry first.
	two, err := render([]string{sentinelA, sentinelB})
	if err != nil {
		return skeleton{}, err
	}

	// The prefix is the longest common prefix of the one- and two-model renders; the
	// suffix is the longest common suffix. Both are constant across model count.
	prefix := commonPrefix(one, two)
	suffix := commonSuffix(one[len(prefix):], two[len(prefix):])

	// The single-model item is exactly the middle of the one-model render.
	item := one[len(prefix) : len(one)-len(suffix)]
	if bytes.Count(item, []byte(modelSentinel)) < 1 {
		return skeleton{}, fmt.Errorf("single-model item does not contain the model sentinel")
	}

	// The two-model middle is itemA + separator + itemB. Derive itemA/itemB by
	// substituting the sentinels into the known item template, then the separator is
	// what remains between them.
	middle := two[len(prefix) : len(two)-len(suffix)]
	itemA := bytes.ReplaceAll(item, []byte(modelSentinel), []byte(sentinelA))
	itemB := bytes.ReplaceAll(item, []byte(modelSentinel), []byte(sentinelB))
	if !bytes.HasPrefix(middle, itemA) || !bytes.HasSuffix(middle, itemB) {
		return skeleton{}, fmt.Errorf("two-model middle does not bracket the derived items")
	}
	separator := middle[len(itemA) : len(middle)-len(itemB)]

	return skeleton{prefix: clone(prefix), item: clone(item), separator: clone(separator), suffix: clone(suffix)}, nil
}

func clone(in []byte) []byte { return append([]byte{}, in...) }

// commonPrefix returns the longest common leading byte slice of a and b.
func commonPrefix(a, b []byte) []byte {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	index := 0
	for index < limit && a[index] == b[index] {
		index++
	}
	return a[:index]
}

// commonSuffix returns the longest common trailing byte slice of a and b.
func commonSuffix(a, b []byte) []byte {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	index := 0
	for index < limit && a[len(a)-1-index] == b[len(b)-1-index] {
		index++
	}
	return a[len(a)-index:]
}

// modelsURL derives the OpenAI-compatible served-models list endpoint from the
// gateway base URL the agent configs use. The configs carry the /v1 suffix, so the
// list endpoint is <base>/models (e.g. ".../v1" → ".../v1/models"). A trailing
// slash on the base is tolerated.
func modelsURL(gatewayBaseURL string) string {
	return strings.TrimRight(gatewayBaseURL, "/") + "/models"
}

// shellQuote single-quotes a value for safe embedding in the script, escaping any
// embedded single quotes the POSIX way ('\”).
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// b64Literal renders bytes as a single-quoted base64 string literal for the script
// (so arbitrary JSON bytes — newlines, quotes, indentation — embed unchanged). The
// in-VM script decodes it with `base64 -d`.
func b64Literal(raw []byte) string {
	return "'" + base64.StdEncoding.EncodeToString(raw) + "'"
}
