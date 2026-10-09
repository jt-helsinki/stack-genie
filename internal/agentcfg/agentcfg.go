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
		"$schema":    "https://opencode.ai/config.json",
		"permission": "allow",
		// lsp enables opencode's built-in language-server integration (diagnostics,
		// go-to-definition, etc. surfaced to the agent). Without it opencode logs "all
		// LSPs are disabled" and the agent loses that context.
		"lsp": true,
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
// OmpAPIKeyRef is written as the provider apiKey: the NAME of the env var omp
// resolves at runtime (NOT the key), keeping the on-disk config keyless. nono
// injects the real value under this name via --env-credential-map at launch
// (see CodexKeyEnvVar, the same name codex/opencode/hermes share).
const OmpAPIKeyRef = CodexKeyEnvVar

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
// HermesKeyEnv is written as hermes' provider key_env: the NAME of the env var hermes
// resolves at runtime (NOT the key), keeping the on-disk config keyless.
const HermesKeyEnv = CodexKeyEnvVar

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
func HermesConfig(gatewayURL, defaultModel, dashboardPassword, skillsDir string) ([]byte, error) {
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
		"external_dirs": []string{skillsDir},
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

// GraphifyMCPArgs is the argv (after the venv python) that runs graphify's stdio MCP
// server against the project's built graph. graphifyy[mcp] is installed INTO the project
// venv (.venv-msb) so a plain `python -m graphify.serve` works — the uv-tool install is
// isolated and not importable this way. The graph is built lazily (git hook / `graphify
// update`); until it exists the server yields no tools.
var GraphifyMCPArgs = []string{"-m", "graphify.serve", "graphify-out/graph.json"}

// EnabledMCPServers returns the MCP servers for the enabled AI tools, in a stable order.
// codeReviewGraph/codebaseMemory/tokensave run their own installed binaries; agentMemory
// runs via npx (the agentmemory npm package is globally installed, but its MCP entrypoint
// is published as the separate @agentmemory/mcp package, fetched on demand); graphify runs
// from the project venv (venvPython = <project>/.venv-msb/bin/python) where graphifyy[mcp]
// lives. A blank venvPython omits graphify (venv path unknown).
func EnabledMCPServers(codeReviewGraph, codebaseMemory, graphify, tokensave, agentMemory bool, venvPython string) []MCPServer {
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
	if tokensave {
		servers = append(servers, MCPServer{Name: "tokensave", Command: "tokensave", Args: []string{"serve"}})
	}
	if agentMemory {
		servers = append(servers, MCPServer{Name: "agentmemory", Command: "npx", Args: []string{"-y", "@agentmemory/mcp"}})
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

// Credential env-var names the host-installed agent CLIs read their gateway
// key from. nono injects the real value under these names at launch, via
// `--env-credential-map <credential-ref> <NAME>` resolving a credential the
// platform stored in the OS keystore (internal/nono.StoreCredential) — never
// written to a config file, profile, or plain host env file. ClaudeBaseURLEnvVar
// / GeminiBaseURLEnvVar are NOT secret (they're the gateway's public local
// address) and are instead set directly — claude's into its settings.json
// (ClaudeSettings), gemini's as a plain nono profile environment.set_vars entry
// (gemini has no config-file base-URL setting at all).
const (
	// claudeBaseURLVar is non-secret; written into claude's settings.json (ClaudeSettings).
	claudeBaseURLVar = "ANTHROPIC_BASE_URL"
	// ClaudeAuthEnvVar is secret; injected by nono at launch (api-key mode only).
	ClaudeAuthEnvVar = "ANTHROPIC_AUTH_TOKEN"
	// CodexKeyEnvVar is secret; shared by codex/opencode/omp/hermes, all of which
	// reference this SAME name in their (keyless) on-disk configs.
	CodexKeyEnvVar = "AIP_GATEWAY_KEY"
	// GeminiBaseURLEnvVar is non-secret; gemini has no config-file base-URL setting,
	// so this is set directly via the project's nono profile environment.set_vars.
	GeminiBaseURLEnvVar = "GOOGLE_GEMINI_BASE_URL"
	// GeminiKeyEnvVar is secret; injected by nono at launch (api-key mode only).
	GeminiKeyEnvVar = "GEMINI_API_KEY"
	// OpenCodeConfigEnvVar is non-secret; points opencode at the project config file
	// written under .opencode/ (set via the project's nono profile environment.set_vars).
	OpenCodeConfigEnvVar = "OPENCODE_CONFIG"
)

// OpenCodeAPIKeyRef is the env-interpolation reference written into the keyless
// on-disk opencode.json: the scoped virtual key is supplied via the
// CodexKeyEnvVar env var (injected by nono at launch, never written to disk).
// opencode uses {env:VAR} — its officially documented interpolation form.
const OpenCodeAPIKeyRef = "{env:" + CodexKeyEnvVar + "}"

// gatewayRoot strips a trailing /v1 (and any trailing slash) from the gateway URL,
// for the CLIs whose SDK appends its own version/path segment (claude-code adds
// /v1/messages; gemini-cli's genai SDK adds its own path). opencode/codex keep
// the /v1-suffixed URL verbatim.
func gatewayRoot(gatewayURL string) string {
	trimmed := strings.TrimRight(gatewayURL, "/")
	return strings.TrimSuffix(trimmed, "/v1")
}

// OmlxModelHandle is the gateway model handle for a Graphify model name: "omlx/<name>",
// where name is used VERBATIM — unlike the old per-model vLLM design (a Hugging Face
// repo id whose alias was derived from its last path segment), omlx names its own
// served models directly, so there is nothing to derive.
func OmlxModelHandle(name string) string {
	return "omlx/" + name
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

// CodexConfig renders codex's ~/.codex/config.toml routing it through the gateway.
// codex requires the OpenAI RESPONSES wire API (chat-completions support was
// removed); the LiteLLM gateway exposes a /responses surface, so the provider's
// base_url keeps the /v1 suffix and wire_api is "responses". The provider key is
// supplied via the env_key env var (CodexKeyEnvVar), injected by nono at launch —
// NEVER written into this file, so this config is KEYLESS and safe on host disk.
// (developers.openai.com/codex/config-reference.)
//
// defaultModel is optional: when non-empty it is written as the top-level `model`
// so codex defaults to a gateway-served model; empty omits it (codex falls back to
// its own default selection, matching the catalog-driven no-default policy).
func CodexConfig(gatewayURL, defaultModel string) []byte {
	var buffer bytes.Buffer
	buffer.WriteString("# Managed by the AI Development Platform — route codex through the gateway.\n")
	buffer.WriteString("# Keyless by design: the gateway key is read from the " + CodexKeyEnvVar + " env var\n")
	buffer.WriteString("# (injected by nono at launch), never written here.\n")
	buffer.WriteString("model_provider = " + tomlString(ProviderID) + "\n")
	if defaultModel != "" {
		buffer.WriteString("model = " + tomlString(defaultModel) + "\n")
	}
	buffer.WriteString("\n[model_providers." + ProviderID + "]\n")
	buffer.WriteString("name = " + tomlString("AI Platform Gateway") + "\n")
	buffer.WriteString("base_url = " + tomlString(strings.TrimRight(gatewayURL, "/")) + "\n")
	buffer.WriteString("env_key = " + tomlString(CodexKeyEnvVar) + "\n")
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

// CodexTrustConfig renders ONE `[projects."<path>"]` TOML table entry marking
// projectPath as trusted, so codex loads that project's keyless per-project
// .codex/config.toml provider block (codex loads a project config only for
// TRUSTED projects). On the host this is ONE entry among potentially many —
// ~/.codex/config.toml is SHARED across every project on the machine, unlike
// the old per-project-isolated microVM — so callers must APPEND this (after
// checking it isn't already present) rather than overwrite the file wholesale.
// It carries no key. (developers.openai.com/codex/config-advanced.)
func CodexTrustConfig(projectPath string) []byte {
	var buffer bytes.Buffer
	buffer.WriteString("\n[projects." + tomlString(projectPath) + "]\n")
	buffer.WriteString("trust_level = " + tomlString("trusted") + "\n")
	return buffer.Bytes()
}

// CodexTrustMarker is the exact TOML table header CodexTrustConfig writes for
// projectPath, used to check idempotently whether an existing
// ~/.codex/config.toml already trusts this project before appending.
func CodexTrustMarker(projectPath string) string {
	return "[projects." + tomlString(projectPath) + "]"
}
