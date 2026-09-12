// Package agentcfg renders the provider configuration the in-workspace agent
// CLIs need to talk to the host Headroom proxy through a scoped LiteLLM virtual
// key (arch §15, §17). At workspace start the platform mints a per-workspace
// virtual key and routes the gateway-capable agent CLIs through the gateway with
// that key:
//
//   - opencode routes via a JSON config file written into the microVM (OpenCodeConfig).
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
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/crypto/scrypt"
	"gopkg.in/yaml.v3"
)

// HermesDashboardUsername is the fixed basic-auth username the platform configures for
// the hermes web dashboard. Hermes refuses to bind the dashboard to 0.0.0.0 (required so
// the published host port reaches it) unless a dashboard auth provider is registered, so
// the platform auto-configures basic auth under this username with a generated password.
const HermesDashboardUsername = "aip"

// scrypt parameters hermes' dashboard basic-auth expects (from its stdlib hashlib.scrypt):
// N=16384, r=8, p=1, derived-key length 32 bytes, over a random 16-byte salt. The stored
// hash string is `scrypt$<N>$<r>$<p>$<base64(salt)>$<base64(dk)>`.
const (
	hermesScryptN       = 16384
	hermesScryptR       = 8
	hermesScryptP       = 1
	hermesScryptSaltLen = 16
	hermesScryptKeyLen  = 32
)

// HermesDashboardPasswordHash hashes a plaintext dashboard password into the
// `scrypt$16384$8$1$<salt_b64>$<dk_b64>` format hermes stores in dashboard.basic_auth
// (matching its hashlib.scrypt(N=16384, r=8, p=1, dklen=32) over a random 16-byte salt).
// Both the salt and derived key are base64.StdEncoding. A fresh random salt makes two
// hashes of the same password differ.
func HermesDashboardPasswordHash(password string) (string, error) {
	salt := make([]byte, hermesScryptSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate hermes dashboard salt: %w", err)
	}
	derivedKey, err := scrypt.Key([]byte(password), salt, hermesScryptN, hermesScryptR, hermesScryptP, hermesScryptKeyLen)
	if err != nil {
		return "", fmt.Errorf("scrypt hermes dashboard password: %w", err)
	}
	return fmt.Sprintf("scrypt$%d$%d$%d$%s$%s",
		hermesScryptN, hermesScryptR, hermesScryptP,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(derivedKey),
	), nil
}

// ProviderID is the provider handle both agent CLIs use for the host gateway.
const ProviderID = "aip-gateway"

// Model is one served model plus whether it can do tool/function calling. Tools drives
// opencode's per-model `tool_call` flag: a completion-only model
// gets tool_call:false so opencode does not present it as agentic and never sends it a tool
// schema (which the model would reject). Cloud/unknown models default to Tools:true.
type Model struct {
	Name  string
	Tools bool
}

// ModelNames returns just the names of a model list, preserving order.
func ModelNames(models []Model) []string {
	names := make([]string, len(models))
	for index, model := range models {
		names[index] = model.Name
	}
	return names
}

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
// provider key or pulls a local model. defaultModel is therefore optional: when
// empty no top-level `model` is written and opencode falls back to its own
// default-model selection.
func OpenCodeConfig(gatewayURL, apiKey, defaultModel string, models []Model, keepTurns, outputBufferTokens int) ([]byte, error) {
	modelEntries := make(map[string]any, len(models))
	for _, model := range models {
		modelEntries[model.Name] = map[string]any{
			"name": model.Name,
			// tool_call declares whether opencode drives the model agentically (send the
			// tool schema + apply the returned tool calls). These gateway models are not in
			// opencode's models.dev catalog, so opencode would otherwise default them to
			// NON-tool-capable and the agent "does nothing" (only chats). We set it PER
			// MODEL from its advertised capability: a tool-capable model gets true (real
			// work); a completion-only model gets false so
			// opencode never sends it a tool schema — which such a model rejects with a hard
			// "does not support tools" error.
			"tool_call": model.Tools,
			// reasoning + interleaved make opencode SURFACE a thinking model's
			// chain-of-thought. LiteLLM streams the model's thinking as `reasoning_content`
			// deltas (separate from `content`); without this opencode ignores them, so a
			// thinking model like qwen3.6 shows a long blank (hidden reasoning) then a tiny
			// answer — read as "no output". `reasoning: true` marks the model as reasoning
			// and `interleaved.field: reasoning_content` tells opencode which delta field
			// carries it. Set on every gateway model: harmless for non-reasoning models
			// (no reasoning_content arrives, so nothing extra shows), and it can't be
			// per-model catalog-driven here since these models aren't in models.dev.
			"reasoning": true,
			"interleaved": map[string]any{
				"field": "reasoning_content",
			},
			"options": map[string]any{
				"headroom_keep_turns":           keepTurns,
				"headroom_output_buffer_tokens": outputBufferTokens,
			},
		}
	}
	document := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"permission": "allow",
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

// TmuxConfig renders the managed tmux configuration written to
// `/home/workspace/.tmux.conf` at workspace start. The platform's workspace
// session model is "tmux-transparent": persistent, reattachable per-CLI tmux
// sessions back `ai agent`/`ai attach`/`ai sessions`, but the user never types a
// tmux command. To keep tmux invisible to a casual user it hides the status bar;
// it enables the mouse so the wheel/trackpad scrolls the scrollback (wheel-up enters
// copy-mode, since tmux's alternate screen leaves the host terminal's own scrollback
// empty), sets vi-style copy-mode keys, and keeps a generous scrollback history.
// Because mouse-on hands click-drag to tmux, native host text-selection then needs a
// modifier (Shift; Option on macOS) and copy-mode selections are pushed to the host
// clipboard via OSC 52 (set-clipboard on).
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
# mouse ON so the wheel/trackpad scrolls tmux's scrollback. tmux runs on the terminal's
# ALTERNATE screen, so the HOST terminal's own scrollback is empty and wheel-up there does
# nothing — only tmux copy-mode can page back through the 50k-line history, and that needs
# the mouse. Wheel-up enters copy-mode (-e auto-exits at the bottom); a full-screen app that
# has requested the mouse (or a pane already in a mode) still gets the wheel forwarded.
set -g mouse on
bind -n WheelUpPane if-shell -F -t = "#{mouse_any_flag}" "send-keys -M" "if-shell -F -t = '#{pane_in_mode}' 'send-keys -M' 'copy-mode -e'"
bind -n WheelDownPane if-shell -F -t = "#{mouse_any_flag}" "send-keys -M" "if-shell -F -t = '#{pane_in_mode}' 'send-keys -M' 'send-keys Down'"
# Trade-off of mouse ON: tmux owns click-drag, so NATIVE host text-selection now needs a
# modifier (hold Shift; Option on macOS terminals). To keep copy working without it, push
# copy-mode selections straight to the HOST clipboard via OSC 52.
set -g set-clipboard on
set -as terminal-features ",*:clipboard"
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
# extended-keys must be set GLOBAL (-g), not just server (-s): agent TUIs (opencode, omp) probe
# the global value, and setting it server-only leaves that reading off.
set -g extended-keys on
# Emit extended keys in CSI-u form (not the legacy xterm form). Modern agent TUIs
# (opencode, omp) expect csi-u for shift+enter / ctrl-combos; tmux defaults to xterm.
set -g extended-keys-format csi-u
set -sg escape-time 10
`)
}

// marshalStable produces deterministic, indented JSON. encoding/json sorts map
// keys, so the output is stable across runs (no spurious workspace-start diffs).
func marshalStable(document any) ([]byte, error) {
	return json.MarshalIndent(document, "", "  ")
}

// marshalYAML produces deterministic, 2-space-indented YAML (yaml.v3 sorts map keys),
// used for omp's config files (omp reads YAML natively; a written .json would be
// one-shot migrated to .yml).
func marshalYAML(document any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	_ = encoder.Close()
	return buffer.Bytes(), nil
}

// omp ("Oh My Pi", a Pi fork) routes through the gateway like opencode, but reads
// YAML: a GLOBAL ~/.omp/agent/models.yml provider definition (the path omp reads for
// custom providers) + a PROJECT <project>/.omp/config.yml for the default model + provider
// order. NB: the `.yml` extension here is the ONE exception to the platform's `.yaml`-only
// rule — these are the exact filenames the omp tool itself reads (documented as `.yml`),
// NOT files whose name we choose, so they must stay `.yml`. Every YAML file the platform
// OWNS the naming of uses `.yaml`. Both are KEYLESS — the provider apiKey NAMES the AIP_GATEWAY_KEY env var (omp
// resolves a value that names an existing env var as the key), so the real scoped key
// lives only in the in-VM agent env file. The provider uses `openai-models-list`
// DISCOVERY, so omp lists exactly the models the gateway serves each launch (no static
// list to write/refresh — the opencode list-refresh does not apply to omp).
const (
	// OmpAPIKeyRef is written as the provider apiKey: the NAME of the env var omp resolves
	// at runtime (NOT the key), keeping the on-disk config keyless.
	OmpAPIKeyRef = "AIP_GATEWAY_KEY"
	// OmpGlobalModelsGuest is omp's global provider/models config (the path it reads).
	OmpGlobalModelsGuest = "/home/workspace/.omp/agent/models.yml"
	// OmpProjectConfigGuest is omp's project-local settings (default model + provider order).
	OmpProjectConfigGuest = projectDirGuest + "/.omp/config.yml"
)

// OmpModelsConfig renders ~/.omp/agent/models.yml: the aip-gateway provider as an
// OpenAI-completions endpoint with openai-models-list discovery, keyless (apiKey names
// the env var). gatewayURL carries the /v1 suffix.
func OmpModelsConfig(gatewayURL, apiKey string) ([]byte, error) {
	document := map[string]any{
		"providers": map[string]any{
			ProviderID: map[string]any{
				"baseUrl":   gatewayURL,
				"api":       "openai-completions",
				"apiKey":    apiKey,
				"discovery": map[string]any{"type": "openai-models-list"},
			},
		},
	}
	return marshalYAML(document)
}

// OmpConfig renders <project>/.omp/config.yml: the gateway provider first in the order
// and, when a default model is seeded, modelRoles.default = aip-gateway/<model>. Fully
// platform-managed (rewritten each start); an EMPTY defaultModel omits the role so omp's
// persisted last-used selection (agent.db on the /persist overlay) wins — the
// seed-then-remember policy, matching the other CLIs.
func OmpConfig(defaultModel string) ([]byte, error) {
	document := map[string]any{
		"modelProviderOrder": []string{ProviderID},
	}
	if defaultModel != "" {
		document["modelRoles"] = map[string]any{"default": ProviderID + "/" + defaultModel}
	}
	return marshalYAML(document)
}

// hermes is a gateway/api-key agent like opencode/omp: it reaches models ONLY through the
// gateway, keyless. Its config is GLOBAL (one file), written whole via Sandbox.WriteFile
// (off host disk) — mirroring omp's global config. nginx :18787 fronts LiteLLM :4000;
// Headroom input-compression is a LiteLLM pre_call guardrail on that path, so it gets
// compression automatically with no client-side wrap.
const (
	// HermesKeyEnv is written as hermes' provider key_env: the NAME of the env var hermes
	// resolves at runtime (NOT the key), keeping the on-disk config keyless.
	HermesKeyEnv = codexKeyVar
	// HermesConfigGuest is hermes' global config (the path it reads).
	HermesConfigGuest = "/home/workspace/.hermes/config.yaml"
	// HermesSkillsExternalDir is the project-relative skills pool hermes is pointed at via
	// its config's external_dirs (the shared-pool symlink target for hermes; see
	// workspace.sharedResourceLinks). Slash-commands derive from skills, so this is the
	// only resource dir hermes needs.
	HermesSkillsExternalDir = projectDirGuest + "/.hermes/skills"
)

// HermesConfig renders ~/.hermes/config.yaml: an aip-gateway provider (keyless — key_env
// NAMES the AIP_GATEWAY_KEY env var) selected as the model provider, plus external_dirs
// pointing at the shared skills pool so hermes picks up the platform's + Caveman's skills.
// Hermes lists models by endpoint discovery (no static array), so there is no served-list
// to refresh — like omp. A non-empty defaultModel seeds model.default; empty omits it so
// hermes' persisted selection wins. gatewayURL carries the /v1 suffix.
//
// A non-empty dashboardPassword appends the dashboard.basic_auth block (username "aip" +
// the scrypt hash of the password) so hermes will bind its dashboard to 0.0.0.0 — it
// refuses to without a registered dashboard auth provider. An empty dashboardPassword
// omits the block (unchanged behavior).
func HermesConfig(gatewayURL, defaultModel, dashboardPassword string) ([]byte, error) {
	model := map[string]any{"provider": ProviderID}
	if defaultModel != "" {
		model["default"] = defaultModel
	}
	document := map[string]any{
		"providers": map[string]any{
			ProviderID: map[string]any{
				"base_url": gatewayURL,
				"key_env":  HermesKeyEnv,
			},
		},
		"model":         model,
		"external_dirs": []string{HermesSkillsExternalDir},
	}
	if dashboardPassword != "" {
		hash, err := HermesDashboardPasswordHash(dashboardPassword)
		if err != nil {
			return nil, err
		}
		document["dashboard"] = map[string]any{
			"basic_auth": map[string]any{
				"username":      HermesDashboardUsername,
				"password_hash": hash,
			},
		}
	}
	return marshalYAML(document)
}

// MCPServer is a stdio MCP server the platform registers into the agent configs it
// MANAGES WHOLE (codex/hermes/omp — the CLIs whose single config file the
// platform rewrites each start). Their MCP entries must be part of that render, or the
// rewrite would clobber whatever the tool's own installer wrote. The CLIs whose configs
// the platform does NOT own (claude/opencode/gemini) instead get these tools via
// the tools' native `install --platform` / auto-detect, which is not clobbered.
type MCPServer struct {
	Name    string
	Command string
	Args    []string
}

// OmpMcpConfigGuest is omp's project MCP config (the path omp reads stdio MCP servers from,
// distinct from .omp/config.yml; project entries shadow the user file).
const OmpMcpConfigGuest = projectDirGuest + "/.omp/mcp.json"

// GraphifyMCPArgs is the argv (after the venv python) that runs graphify's stdio MCP
// server against the project's built graph. graphifyy[mcp] is installed INTO the project
// venv (.venv-msb) so a plain `python -m graphify.serve` works — the uv-tool install is
// isolated and not importable this way. The graph is built lazily (git hook / `graphify
// update`); until it exists the server yields no tools.
var GraphifyMCPArgs = []string{"-m", "graphify.serve", "graphify-out/graph.json"}

// EnabledMCPServers returns the MCP servers for the enabled AI tools, in a stable order.
// codeReviewGraph/codebaseMemory run their own installed binaries; graphify runs from the
// project venv (venvPython = <project>/.venv-msb/bin/python) where graphifyy[mcp] lives.
// A blank venvPython omits graphify (venv path unknown).
func EnabledMCPServers(codeReviewGraph, codebaseMemory, graphify bool, venvPython string) []MCPServer {
	var servers []MCPServer
	if codeReviewGraph {
		servers = append(servers, MCPServer{Name: "code-review-graph", Command: "code-review-graph", Args: []string{"serve"}})
	}
	if codebaseMemory {
		servers = append(servers, MCPServer{Name: "codebase-memory-mcp", Command: "codebase-memory-mcp"})
	}
	if graphify && venvPython != "" {
		servers = append(servers, MCPServer{Name: "graphify", Command: venvPython, Args: append([]string{}, GraphifyMCPArgs...)})
	}
	return servers
}

// mcpServerMap renders the MCP servers as the {name: {command, args}} object shape shared
// by omp (mcpServers) and (as a nested map) hermes (mcp_servers).
func mcpServerMap(servers []MCPServer) map[string]any {
	out := make(map[string]any, len(servers))
	for _, server := range servers {
		entry := map[string]any{"command": server.Command}
		if len(server.Args) > 0 {
			entry["args"] = server.Args
		}
		out[server.Name] = entry
	}
	return out
}

// OmpMcpConfig renders omp's <project>/.omp/mcp.json: a top-level mcpServers object. omp
// reads stdio MCP servers from here (project entries shadow ~/.omp/agent/mcp.json), so
// this is a STANDALONE file — it does not collide with the platform-managed .omp/config.yml.
func OmpMcpConfig(servers []MCPServer) ([]byte, error) {
	return marshalStable(map[string]any{"mcpServers": mcpServerMap(servers)})
}

// InjectHermesMCP adds the MCP servers to hermes' rendered config under mcp_servers (the
// documented key). It parses HermesConfig's YAML and re-marshals. No servers → unchanged.
func InjectHermesMCP(config []byte, servers []MCPServer) ([]byte, error) {
	if len(servers) == 0 {
		return config, nil
	}
	var document map[string]any
	if err := yaml.Unmarshal(config, &document); err != nil {
		return nil, fmt.Errorf("parse hermes config for MCP injection: %w", err)
	}
	document["mcp_servers"] = mcpServerMap(servers)
	return marshalYAML(document)
}

// AppendCodexMCP appends [mcp_servers.<name>] TOML tables to codex's rendered config so the
// servers ride in the SAME config.toml the platform rewrites each start. Table names may
// contain hyphens (valid TOML bare keys). No servers → the config is returned unchanged.
func AppendCodexMCP(config []byte, servers []MCPServer) []byte {
	if len(servers) == 0 {
		return config
	}
	var buffer bytes.Buffer
	buffer.Write(config)
	for _, server := range servers {
		buffer.WriteString("\n[mcp_servers." + server.Name + "]\n")
		buffer.WriteString("command = " + tomlString(server.Command) + "\n")
		if len(server.Args) > 0 {
			quoted := make([]string, len(server.Args))
			for index, arg := range server.Args {
				quoted[index] = tomlString(arg)
			}
			buffer.WriteString("args = [" + strings.Join(quoted, ", ") + "]\n")
		}
	}
	return buffer.Bytes()
}

// MergeOpenCodeConfig produces the FINAL opencode config from any EXISTING project
// config plus the dynamic values minted at start. It parses the existing JSON and
// deep-merges the generated provider config over it, so the dynamic provider block
// (baseURL, the {env:} key ref, the served-model picker, Headroom knobs) always wins
// while any other user-added top-level keys (themes, MCP servers, …) survive. A
// nil/empty/invalid input falls back to the freshly-generated config, so a brand-new
// project (no file) or a corrupt edit still yields a working config. When called with
// the keyless key ref (OpenCodeAPIKeyRef) the result is keyless — safe on host disk.
func MergeOpenCodeConfig(template []byte, gatewayURL, apiKey, defaultModel string, models []Model, keepTurns, outputBufferTokens int) ([]byte, error) {
	generated, err := OpenCodeConfig(gatewayURL, apiKey, defaultModel, models, keepTurns, outputBufferTokens)
	if err != nil {
		return nil, err
	}
	// When we have a served list, REPLACE the provider's models map wholesale (rather than
	// deep-merging, which would UNION and leave models removed upstream lingering forever)
	// — the list is authoritative. When the list is EMPTY (gateway unreachable), DON'T
	// replace: fall back to the preserving merge so a transient outage never WIPES the
	// existing on-disk list. Every other user key (themes, MCP servers, enabled_providers,
	// the provider's baseURL/apiKey/options) still merges/survives either way.
	var replacePaths []string
	if len(models) > 0 {
		replacePaths = []string{"provider." + ProviderID + ".models"}
	}
	// With no default to seed, actively DROP any previously-seeded top-level "model" so
	// opencode's persisted last-used selection wins (config "model" outranks last-used;
	// a stale one would re-pin it every launch — the "remember" half of seed-then-remember).
	return mergeJSONReplacing(template, generated, replacePaths, dropKeysWhenNoDefault(defaultModel, "model"))
}

// dropKeysWhenNoDefault returns keys to strip from a merged config when no default
// model is being seeded (empty defaultModel), and none when one is.
func dropKeysWhenNoDefault(defaultModel string, keys ...string) []string {
	if defaultModel != "" {
		return nil
	}
	return keys
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
	return mergeJSONReplacing(template, generated, nil, dropKeys)
}

// mergeJSONReplacing is mergeJSONOver with, additionally, a set of dotted key-paths whose
// value is taken WHOLESALE from the generated overlay instead of being deep-merged. This
// is how an authoritative collection (e.g. opencode's `provider.<id>.models`) is REPLACED
// rather than unioned — so entries removed upstream disappear — while every OTHER user key
// still merges/survives.
func mergeJSONReplacing(template, generated []byte, replacePaths, dropKeys []string) ([]byte, error) {
	var overlay map[string]any
	if err := json.Unmarshal(generated, &overlay); err != nil {
		return nil, err
	}
	merged := overlay
	var base map[string]any
	if len(bytes.TrimSpace(template)) > 0 && json.Unmarshal(template, &base) == nil {
		replace := make(map[string]bool, len(replacePaths))
		for _, path := range replacePaths {
			replace[path] = true
		}
		merged = deepMergeJSONPath(base, overlay, "", replace)
	}
	for _, key := range dropKeys {
		delete(merged, key)
	}
	return marshalStable(merged)
}

// deepMergeJSONPath returns base with overlay applied on top; nested objects merge
// recursively and overlay scalars/arrays win (inputs are not mutated). It tracks the
// dotted path to each key; a key whose path is in `replace` is overwritten wholesale from
// overlay (not recursed into), so an authoritative sub-object replaces rather than unions
// with the base's copy.
func deepMergeJSONPath(base, overlay map[string]any, prefix string, replace map[string]bool) map[string]any {
	result := make(map[string]any, len(base))
	for key, value := range base {
		result[key] = value
	}
	for key, overlayValue := range overlay {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if !replace[path] {
			if existing, ok := result[key]; ok {
				if existingMap, isMap := existing.(map[string]any); isMap {
					if overlayMap, isOverlayMap := overlayValue.(map[string]any); isOverlayMap {
						result[key] = deepMergeJSONPath(existingMap, overlayMap, path, replace)
						continue
					}
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

// Env-interpolation reference for the on-disk (keyless) PROJECT configs: the scoped
// virtual key is supplied via the AIP_GATEWAY_KEY env var (exported by
// AgentEnvScript, in-VM only) and NEVER written to disk. opencode uses {env:VAR} — its
// officially documented interpolation form.
const OpenCodeAPIKeyRef = "{env:" + codexKeyVar + "}"

// In-VM paths of the per-CLI PROJECT configs, under the bind-mounted project dir
// (/home/workspace/project — mirrors workspace.workspaceWorkdir; the project dir is
// ONE directory shared host↔guest). The keyless configs live here so the project is
// self-describing; OPENCODE_CONFIG points opencode at its file, refresh-models
// rewrites the opencode file in place, and codex is told to trust this project.
const (
	projectDirGuest            = "/home/workspace/project"
	OpenCodeProjectConfigGuest = projectDirGuest + "/.opencode/opencode.json"
	CodexProjectConfigGuest    = projectDirGuest + "/.codex/config.toml"
)

// gatewayRoot strips a trailing /v1 (and any trailing slash) from the gateway URL,
// for the CLIs whose SDK appends its own version/path segment (claude-code adds
// /v1/messages; gemini-cli's genai SDK adds its own path). opencode/codex keep
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
//
// oauthAgents is the set of installed CLIs configured for OAUTH (subscription) auth —
// those talk DIRECTLY to their provider with their own native login, so their gateway
// env is OMITTED here: an oauth claude-code gets no ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN
// (so its ~/.claude OAuth credentials win) and an oauth gemini gets no
// GOOGLE_GEMINI_BASE_URL/GEMINI_API_KEY. AIP_GATEWAY_KEY is shared with opencode (and
// api-key codex), so it is always exported; an oauth codex simply doesn't reference it
// (its config.toml uses the ChatGPT login — see CodexConfigOAuth). The Graphify OPENAI_*
// env is independent of any agent's auth mode.
func AgentEnvScript(gatewayURL, apiKey, graphifyModel string, oauthAgents map[string]bool) []byte {
	root := gatewayRoot(gatewayURL)
	var buffer bytes.Buffer
	buffer.WriteString("# Managed by the AI Development Platform — gateway env for the env-routed\n")
	buffer.WriteString("# agent CLIs (claude-code, codex, gemini). Written into the microVM at\n")
	buffer.WriteString("# workspace start and sourced by every shell + agent session. Do not edit by\n")
	buffer.WriteString("# hand; this file is rewritten on every workspace start and holds the\n")
	buffer.WriteString("# workspace's scoped virtual key (it never leaves the VM).\n")
	// claude-code (api-key mode only): Anthropic-compatible surface at the gateway root +
	// bearer token. OMITTED for oauth so Claude Code's own subscription login is used and
	// reaches Anthropic directly.
	if !oauthAgents["claude-code"] {
		buffer.WriteString("export " + claudeBaseURLVar + "=" + shellQuote(root) + "\n")
		buffer.WriteString("export " + claudeAuthVar + "=" + shellQuote(apiKey) + "\n")
	}
	// codex: the key its config.toml provider block reads via env_key. Shared with
	// opencode (its {env:} key ref), so always exported — an oauth codex just
	// doesn't reference it.
	buffer.WriteString("export " + codexKeyVar + "=" + shellQuote(apiKey) + "\n")
	// gemini-cli (api-key mode only): the genai SDK's base-URL + key overrides (gateway
	// root). OMITTED for oauth so gemini's own login reaches Google directly.
	if !oauthAgents["gemini"] {
		buffer.WriteString("export " + geminiBaseURLVar + "=" + shellQuote(root) + "\n")
		buffer.WriteString("export " + geminiKeyVar + "=" + shellQuote(apiKey) + "\n")
	}
	// opencode: point it at the per-project config file we write under .opencode/
	// (keyless; the key resolves from AIP_GATEWAY_KEY via {env:} interpolation).
	buffer.WriteString("export " + openCodeConfigVar + "=" + shellQuote(OpenCodeProjectConfigGuest) + "\n")
	// Graphify's headless LLM backend, when a model is configured: route through the
	// gateway's OpenAI-compatible endpoint (nginx → LiteLLM → the shared omlx server).
	// Invoke as `graphify --backend openai`. No real provider key — the scoped
	// virtual key; nothing else in the platform reads OPENAI_*. The configured model is
	// just the NAME omlx already serves it as (no download) — the gateway serves it as
	// omlx/<name> verbatim (see internal/litellm.SyncOmlxModels).
	if graphifyModel != "" {
		buffer.WriteString("export OPENAI_BASE_URL=" + shellQuote(gatewayURL) + "\n") // .../v1
		buffer.WriteString("export OPENAI_API_KEY=" + shellQuote(apiKey) + "\n")
		buffer.WriteString("export OPENAI_MODEL=" + shellQuote(OmlxModelHandle(graphifyModel)) + "\n")
	}
	return buffer.Bytes()
}

// OmlxModelHandle is the gateway model handle for a Graphify model name: "omlx/<name>",
// where name is used VERBATIM — unlike the old per-model vLLM design (a Hugging Face
// repo id whose alias was derived from its last path segment), omlx names its own
// served models directly, so there is nothing to derive.
func OmlxModelHandle(name string) string {
	return "omlx/" + name
}

// terminfoFallbackLine falls back to xterm-256color when $TERM has no in-VM terminfo
// entry (Ghostty's xterm-ghostty, kitty, wezterm, …), so keys/colours render instead of
// "unknown terminal". Inside a session tmux already sets tmux-256color; this covers the
// pre-tmux / non-tmux exec path. It is POSIX, so it is reused verbatim by bash + zsh.
const terminfoFallbackLine = "command -v infocmp >/dev/null 2>&1 && ! infocmp \"$TERM\" >/dev/null 2>&1 && export TERM=xterm-256color\n"

// BashProfile renders the managed ~/.bash_profile written into the microVM at
// workspace start. It sources the standard ~/.bashrc (so an interactive login
// shell behaves normally) and then the agent env file (the gateway env vars for
// the env-routed CLIs) plus the Headroom-wrap alias snippet, so a user running
// claude/codex/opencode from `ai shell` or `ai attach` is routed through the gateway
// with the workspace's scoped key AND gets the `headroom wrap` aliases. It is
// rewritten on every start; the agent env file it sources holds the key (in-VM).
func BashProfile() []byte {
	var buffer bytes.Buffer
	buffer.WriteString("# Managed by the AI Development Platform — sources the agent gateway env for\n")
	buffer.WriteString("# interactive login shells. Do not edit by hand; rewritten on every start.\n")
	// Put the uv-tool bin dir on PATH FIRST, before anything else: `uv tool install`
	// places headroom/graphify (and the curl-installed hermes wrapper) in ~/.local/bin.
	// A `bash -lc` login shell reads THIS file but Debian's ~/.bashrc returns early for
	// non-interactive shells (before its managed PATH line), so without this every
	// programmatic launch (`hermes dashboard`, agent CLIs, detached tool installs) runs
	// with ~/.local/bin absent and fails "command not found". Idempotent guard so nested
	// shells don't stack duplicates.
	buffer.WriteString(`case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) export PATH="$HOME/.local/bin:$PATH" ;; esac` + "\n")
	buffer.WriteString("[ -f \"$HOME/.bashrc\" ] && . \"$HOME/.bashrc\"\n")
	buffer.WriteString("[ -f " + shellQuote(AgentEnvFileGuestPath) + " ] && . " + shellQuote(AgentEnvFileGuestPath) + "\n")
	buffer.WriteString("[ -f " + shellQuote(ShellAliasesFileGuestPath) + " ] && . " + shellQuote(ShellAliasesFileGuestPath) + "\n")
	buffer.WriteString(terminfoFallbackLine)
	return buffer.Bytes()
}

// HeadroomWrapName maps a platform agent CLI to the token Headroom's `wrap` subcommand
// accepts, and reports whether Headroom can wrap it. Headroom `wrap` supports only a
// FIXED set of agent tokens (claude, codex, copilot, cursor, aider, opencode, cline,
// continue, goose, openhands, vibe); of this platform's CLIs claude-code, codex, and
// opencode are wrappable. omp and gemini are NOT — aliasing them would break at
// runtime — so they return ok=false and get no alias. This mirrors the
// graphifyPlatformFlag guard: an unsupported CLI is simply skipped.
func HeadroomWrapName(cli string) (string, bool) {
	switch cli {
	case "claude-code":
		return "claude", true
	case "codex":
		return "codex", true
	case "opencode":
		return "opencode", true
	default:
		return "", false
	}
}

// oauthProviderDomains maps each OAuth-capable CLI to the egress domains its native
// (subscription/OAuth) login talks to DIRECTLY when it bypasses the gateway. They are
// added to the project egress allow-list at create so an oauth agent can reach its
// provider even under `deny` mode (and is explicit under `public`). Best-effort / verify:
// derived from each vendor's documented API + auth hosts; adjust if a vendor moves them.
var oauthProviderDomains = map[string][]string{
	"claude-code": {"api.anthropic.com"},
	"codex":       {"api.openai.com", "chatgpt.com", "auth.openai.com"},
	"gemini":      {"generativelanguage.googleapis.com", "oauth2.googleapis.com", "accounts.google.com", "cloudcode-pa.googleapis.com"},
}

// OAuthProviderDomains returns the egress domains an oauth-mode CLI reaches directly
// (nil for an unknown CLI). Best-effort — see oauthProviderDomains.
func OAuthProviderDomains(cli string) []string {
	return oauthProviderDomains[cli]
}

// ShellAliasesFileGuestPath is the in-VM path of the managed Headroom-wrap alias snippet,
// sourced by every interactive shell (bash + zsh). It is dedicated (not a framework rc), so
// the platform can rewrite it whole on every start without disturbing oh-my-bash/oh-my-zsh.
const ShellAliasesFileGuestPath = "/home/workspace/.config/aip/shell-aliases.sh"

// ShellAliases renders the managed POSIX snippet (written to ShellAliasesFileGuestPath)
// that aliases each Headroom-wrappable agent CLI to `headroom wrap <name>`, so typing e.g.
// `claude` runs `headroom wrap claude` (input compression via the host Headroom CLI, which
// is already installed in-VM). Only the installed CLIs Headroom supports (HeadroomWrapName)
// get an alias; the rest (omp, gemini) are skipped. Tools are iterated in the order
// given and de-duplicated, so the output is deterministic.
func ShellAliases(tools []string) []byte {
	var buffer bytes.Buffer
	buffer.WriteString("# Managed by the AI Development Platform — Headroom wrap aliases for the\n")
	buffer.WriteString("# installed agent CLIs. Sourced by every interactive shell (bash + zsh) so\n")
	buffer.WriteString("# typing e.g. `claude` runs `headroom wrap claude`. Do not edit by hand;\n")
	buffer.WriteString("# this file is rewritten on every workspace start.\n")
	var aliases bytes.Buffer
	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		name, ok := HeadroomWrapName(tool)
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		aliases.WriteString("  alias " + name + "=" + shellQuote("headroom wrap "+name) + "\n")
	}
	if aliases.Len() == 0 {
		return buffer.Bytes()
	}
	// Guard on headroom being present: if it isn't (e.g. an image built before it was
	// installed), define NO alias so the agent CLI still runs natively rather than failing
	// with "headroom: command not found". When headroom is on PATH the aliases apply.
	buffer.WriteString("if command -v headroom >/dev/null 2>&1; then\n")
	buffer.Write(aliases.Bytes())
	buffer.WriteString("fi\n")
	return buffer.Bytes()
}

// ShellRCMarkerBegin / ShellRCMarkerEnd bound the managed block appended to the
// framework-owned interactive rc files (oh-my-bash's ~/.bashrc, oh-my-zsh's ~/.zshrc).
// The platform NEVER overwrites those files — the block is stripped and re-appended
// idempotently on every workspace start, keyed off these markers. They carry no regex
// metacharacters (no slash/dot) so they can be used directly in a sed address.
const (
	ShellRCMarkerBegin = "# >>> AI Development Platform managed >>>"
	ShellRCMarkerEnd   = "# <<< AI Development Platform managed <<<"
)

// ShellRCBlock renders the managed, marker-delimited block appended to the framework rc
// of BOTH interactive shells (bash + zsh). It sources the agent gateway env file (the
// scoped key + gateway env vars) and the Headroom-wrap alias snippet, and applies the
// terminfo fallback — so an interactive shell / tmux pane in either shell routes through
// the gateway AND has the `headroom wrap` aliases defined. The workspace layer strips any
// prior copy between the markers and re-appends this block idempotently on every start.
func ShellRCBlock() []byte {
	var buffer bytes.Buffer
	buffer.WriteString(ShellRCMarkerBegin + "\n")
	buffer.WriteString("# Managed by the AI Development Platform — do not edit between the markers;\n")
	buffer.WriteString("# rewritten on every workspace start. Sources the agent gateway env + the\n")
	buffer.WriteString("# Headroom-wrap aliases for interactive shells (incl. tmux panes).\n")
	// Guarantee the uv-tool bin dir is on PATH: `uv tool install` puts headroom + graphify
	// in ~/.local/bin. The image sets ENV PATH, but an msb-exec'd interactive shell does not
	// reliably inherit it (and oh-my-bash/oh-my-zsh may reset PATH), so without this the
	// `headroom wrap` aliases fail with "headroom: command not found". Idempotent — only
	// prepended if absent, so nested shells don't stack duplicates.
	buffer.WriteString(`case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) export PATH="$HOME/.local/bin:$PATH" ;; esac` + "\n")
	buffer.WriteString("[ -f " + shellQuote(AgentEnvFileGuestPath) + " ] && . " + shellQuote(AgentEnvFileGuestPath) + "\n")
	buffer.WriteString("[ -f " + shellQuote(ShellAliasesFileGuestPath) + " ] && . " + shellQuote(ShellAliasesFileGuestPath) + "\n")
	buffer.WriteString(terminfoFallbackLine)
	buffer.WriteString(ShellRCMarkerEnd + "\n")
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

// CodexConfigOAuth renders codex's ~/.codex/config.toml for OAUTH (subscription) mode:
// codex logs in with its OWN ChatGPT account and talks DIRECTLY to OpenAI, bypassing the
// platform gateway — so NO gateway provider block is written (the platform's tool
// firewall + secret masking do not apply to it). forced_login_method = "chatgpt" pins the
// subscription login and cli_auth_credentials_store = "file" persists it to
// ~/.codex/auth.json (symlinked to /persist so the login survives microVM restarts). The
// project trust entry (CodexTrustConfig) is still written so codex loads this config.
// (developers.openai.com/codex/config-reference.)
func CodexConfigOAuth() []byte {
	var buffer bytes.Buffer
	buffer.WriteString("# Managed by the AI Development Platform — codex in OAuth/subscription mode.\n")
	buffer.WriteString("# codex logs in with its own ChatGPT account and talks DIRECTLY to OpenAI,\n")
	buffer.WriteString("# BYPASSING the platform gateway (its tool firewall + secret masking do not\n")
	buffer.WriteString("# apply). Do not edit by hand; rewritten on every workspace start.\n")
	buffer.WriteString("forced_login_method = " + tomlString("chatgpt") + "\n")
	buffer.WriteString("cli_auth_credentials_store = " + tomlString("file") + "\n")
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

// MergeClaudeSettingsOAuth produces claude-code's per-project settings for OAUTH mode:
// it REPLACES the settings `env` block with an empty one, so any ANTHROPIC_BASE_URL left
// by a prior api-key start is removed and Claude Code's own subscription login
// (~/.claude/.credentials.json) reaches Anthropic directly. The user's OTHER top-level
// settings survive. A nil/corrupt existing file degrades to the empty-env settings.
func MergeClaudeSettingsOAuth(existing []byte) ([]byte, error) {
	generated, err := marshalStable(map[string]any{"env": map[string]any{}})
	if err != nil {
		return nil, err
	}
	// Replace the whole "env" block wholesale (not deep-merge) so a stale gateway base URL
	// cannot linger; every other user key still merges/survives.
	return mergeJSONReplacing(existing, generated, []string{"env"}, nil)
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
// bind-mounted project dir; RefreshScript rewrites the opencode file in place.
// (These alias the exported project-config guest paths declared above.)
const (
	openCodeGuestPath = OpenCodeProjectConfigGuest
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
// catalog models + the registered local models). The script:
//   - requires curl (clear error + exit if absent);
//   - GETs <gatewayBaseURL>/models (the OpenAI-compatible list endpoint) with the
//     scoped virtual key, and extracts the served model ids PORTABLY (grep/sed over
//     the "id":"…" fields — no jq/python);
//   - dedups + sorts them (LC_ALL=C sort -u) exactly as workspace.pickerModels does,
//     so the result matches a fresh workspace start;
//   - rewrites opencode.json BYTE-IDENTICAL to what OpenCodeConfig would produce for that served list, default, gateway, and key —
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
	// byte parity with OpenCodeConfig for the same merged list. The bodies
	// use the KEYLESS key-refs (not apiKey), matching the configs written at start.
	renderConfig := func(models []Model) ([]byte, error) {
		return OpenCodeConfig(gatewayBaseURL, OpenCodeAPIKeyRef, defaultModel, models, keepTurns, outputBufferTokens)
	}
	// One skeleton with a tool-capable item template (tool_call: true); the no-tool item
	// is the same template with the tool_call literal flipped to false. prefix/separator/
	// suffix are shared by both (they contain no tool_call — the item is brace-anchored so
	// the flag stays inside the item), so the shell picks the item per model from the
	// gateway's advertised tool support and reproduces OpenCodeConfig byte-for-byte.
	openCode, err := splitSkeleton(true, renderConfig)
	if err != nil {
		return nil, fmt.Errorf("render opencode skeleton: %w", err)
	}
	openCode.itemNoTool = bytes.Replace(openCode.item, []byte(`"tool_call": true`), []byte(`"tool_call": false`), 1)
	if bytes.Equal(openCode.itemNoTool, openCode.item) {
		return nil, fmt.Errorf("could not locate tool_call in the opencode item template")
	}

	var script bytes.Buffer
	script.WriteString("#!/usr/bin/env bash\n")
	script.WriteString(`# Managed by the AI Development Platform — refresh the in-workspace agent model
# picker. Run this INSIDE the workspace after changing the served models on the host
# (add a provider key with 'ai keys', or pull/remove a local model):
#   refresh-models
# It re-fetches the models the gateway currently SERVES (its DB-backed models) from
# the gateway's /v1/models endpoint and rewrites the agent CLI configs in place.
# Restart your agent CLI afterwards to pick up the new list.
# Do not edit by hand; this file is rewritten on every workspace start.
set -u

`)
	// The baked-in values. The gateway URL keeps its /v1 suffix (it is written into
	// the configs verbatim); the model-info endpoint (<root>/llm/model/info) carries
	// per-model tool support (supports_function_calling) that a plain /v1/models omits.
	script.WriteString("GATEWAY_URL=" + shellQuote(gatewayBaseURL) + "\n")
	script.WriteString("INFO_URL=" + shellQuote(modelInfoURL(gatewayBaseURL)) + "\n")
	script.WriteString("API_KEY=" + shellQuote(apiKey) + "\n")
	script.WriteString("OPENCODE_PATH=" + shellQuote(openCodeGuestPath) + "\n\n")

	// The JSON fragments per config, base64-encoded so arbitrary bytes (newlines,
	// quotes, indentation, the inter-entry separator) survive embedding in the script
	// unchanged. Two per-model item variants — ITEM (tool_call:true) and ITEM_NOTOOL
	// (tool_call:false) — so the shell picks per model from its advertised capability.
	script.WriteString("OPENCODE_PREFIX=" + b64Literal(openCode.prefix) + "\n")
	script.WriteString("OPENCODE_ITEM=" + b64Literal(openCode.item) + "\n")
	script.WriteString("OPENCODE_ITEM_NOTOOL=" + b64Literal(openCode.itemNoTool) + "\n")
	script.WriteString("OPENCODE_SEP=" + b64Literal(openCode.separator) + "\n")
	script.WriteString("OPENCODE_SUFFIX=" + b64Literal(openCode.suffix) + "\n\n")

	script.WriteString(refreshScriptBody)
	return script.Bytes(), nil
}

// refreshScriptBody is the fixed logic of the refresh script. It consumes the
// baked variables RefreshScript prepends. The MODEL_SENTINEL token in the item
// templates is replaced with each (already JSON-escaped) model id; the model ids
// here are simple (alias / "omlx/<name>" / "<provider>/<model>") so a literal
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

# Fetch the models the gateway currently SERVES, WITH per-model tool support, from its
# /model/info endpoint (authenticated with the scoped virtual key). Emit one
# "<model-id><TAB><tools>" line per model — tools=1 unless model_info marks it non-tool
# (supports_function_calling:false). Parsed with awk (portable across the Linux workspace
# and the macOS test host — no jq/python, no GNU-only sed \n); the whole body is slurped
# into one buffer first so it works whether the JSON is compact or pretty-printed. If the
# fetch fails we leave the existing configs UNTOUCHED rather than wiping them.
if ! info_body="$(curl -fsS --max-time 10 -H "Authorization: Bearer $API_KEY" "$INFO_URL" 2>/dev/null)"; then
  echo "refresh-models: could not reach the gateway model info ($INFO_URL) — leaving the current configs unchanged" >&2
  exit 1
fi
pairs="$(printf '%s' "$info_body" | awk '{ buf = buf $0 } END {
  n = split(buf, parts, /"model_name"[[:space:]]*:[[:space:]]*"/)
  for (i = 2; i <= n; i++) {
    seg = parts[i]
    q = index(seg, "\"")
    if (q == 0) continue
    name = substr(seg, 1, q - 1)
    rest = substr(seg, q + 1)
    tools = 1
    if (index(rest, "\"supports_function_calling\":false") > 0) tools = 0
    if (index(rest, "\"supports_function_calling\": false") > 0) tools = 0
    printf "%s\t%s\n", name, tools
  }
}')"

# Dedup + sort EXACTLY as pickerModels does (LC_ALL=C lexical sort, unique). Model names
# are unique, so sorting the "<name><TAB><tools>" lines orders them by name.
merged="$(printf '%s\n' "$pairs" | sed '/^[[:space:]]*$/d' | LC_ALL=C sort -u)"
merged_count="$(printf '%s\n' "$merged" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' ')"

TAB="$(printf '\t')"

# render <prefix-b64> <item-b64> <item-notool-b64> <sep-b64> <suffix-b64> -> full JSON on
# stdout, splicing one decoded item per model (sentinel -> escaped model id) joined by the
# decoded inter-entry separator, exactly reproducing MarshalIndent's layout. The tool or
# no-tool item template is chosen from each model's tools flag.
render() {
  prefix="$(decode "$1")"; item_tmpl="$(decode "$2")"; item_notool_tmpl="$(decode "$3")"; sep="$(decode "$4")"; suffix="$(decode "$5")"
  printf '%s' "$prefix"
  first=1
  while IFS="$TAB" read -r model tools; do
    [ -z "$model" ] && continue
    esc="$(json_escape "$model")"
    if [ "$tools" = "0" ]; then tmpl="$item_notool_tmpl"; else tmpl="$item_tmpl"; fi
    item="${tmpl//$SENTINEL/$esc}"
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

opencode_json="$(render "$OPENCODE_PREFIX" "$OPENCODE_ITEM" "$OPENCODE_ITEM_NOTOOL" "$OPENCODE_SEP" "$OPENCODE_SUFFIX")"

write_config "$OPENCODE_PATH" "$opencode_json" || { echo "refresh-models: failed to write $OPENCODE_PATH" >&2; exit 1; }

echo "refreshed: $merged_count served models — restart your agent CLI to pick them up"
`)

// skeleton is the decomposition of a config's JSON around its model collection:
// the constant prefix, the per-model item template (containing modelSentinel once
// per model-id occurrence), the constant inter-entry separator, and the constant
// suffix. The script emits prefix + item·sep·item·… + suffix, reproducing
// MarshalIndent's exact byte layout.
type skeleton struct {
	prefix []byte
	// item is the per-model template for a TOOL-CAPABLE model (tool_call: true);
	// itemNoTool is the same template for a completion-only model (tool_call: false).
	// The shell picks per model from its advertised capability. prefix/separator/suffix
	// are identical between the two (only the tool_call literal inside the item differs).
	item       []byte
	itemNoTool []byte
	separator  []byte
	suffix     []byte
}

// splitSkeleton derives the decomposition by rendering the config with two distinct
// probe models (sentinelA, sentinelB) and diffing the result against a single-model
// render. The common prefix/suffix are constant; the middle of the two-model render
// is item(A) + separator + item(B), and the single-model item locates where each
// item ends — so the separator is whatever MarshalIndent places BETWEEN entries
// (for a map: "},\n        " style; for an array: "},\n    " style — captured
// verbatim rather than assumed).
// tools is fixed true here: the tool-capable item is the template, and the no-tool
// variant is derived by the caller via a single tool_call byte-replace (so both variants
// share one prefix/separator/suffix). The item is anchored by BRACE-MATCHING its object —
// NOT by a common suffix — because tool_call is the item's LAST field, so a common-suffix
// split would absorb the tool_call literal into the suffix and make it vary per model.
func splitSkeleton(tools bool, render func(models []Model) ([]byte, error)) (skeleton, error) {
	one, err := render([]Model{{Name: modelSentinel, Tools: tools}})
	if err != nil {
		return skeleton{}, err
	}
	// sentinelA sorts before sentinelB, matching the script's sorted model order, so
	// the two-model render places A's entry first.
	two, err := render([]Model{{Name: sentinelA, Tools: tools}, {Name: sentinelB, Tools: tools}})
	if err != nil {
		return skeleton{}, err
	}

	// The prefix is the longest common prefix of the one- and two-model renders — it ends
	// just before the first model entry's map key (where the sentinels first diverge).
	prefix := commonPrefix(one, two)

	// The item is the first model's whole "<key>": { ... } object: from the prefix end
	// through the matching close brace of that object. Everything after is the suffix.
	itemEnd, err := itemObjectEnd(one, len(prefix))
	if err != nil {
		return skeleton{}, fmt.Errorf("locate opencode item object: %w", err)
	}
	item := one[len(prefix):itemEnd]
	suffix := one[itemEnd:]
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

// itemObjectEnd returns the index just past the closing brace of the first JSON object
// ('{' … '}') at or after start, matching braces while skipping any inside string
// literals. Used to bound a model's item object precisely (its tool_call is the last
// field, so a common-suffix bound would slice through it).
func itemObjectEnd(data []byte, start int) (int, error) {
	open := bytes.IndexByte(data[start:], '{')
	if open < 0 {
		return 0, fmt.Errorf("no object open brace after offset %d", start)
	}
	depth := 0
	inString := false
	escaped := false
	for index := start + open; index < len(data); index++ {
		char := data[index]
		if inString {
			switch {
			case escaped:
				escaped = false
			case char == '\\':
				escaped = true
			case char == '"':
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return index + 1, nil
			}
		}
	}
	return 0, fmt.Errorf("unbalanced braces")
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

// modelInfoURL derives the LiteLLM model-info endpoint (which carries per-model
// supports_function_calling) from the gateway base URL the agent configs use. The
// configs carry the /v1 suffix; model-info lives off the nginx /llm route at
// <root>/llm/model/info (e.g. ".../v1" → ".../llm/model/info"). A trailing slash and
// the /v1 suffix are trimmed off the base first. The scoped virtual key can read it.
func modelInfoURL(gatewayBaseURL string) string {
	root := strings.TrimRight(gatewayBaseURL, "/")
	root = strings.TrimSuffix(root, "/v1")
	return strings.TrimRight(root, "/") + "/llm/model/info"
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
