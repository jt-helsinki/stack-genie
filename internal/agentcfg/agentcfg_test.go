package agentcfg

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const (
	testGateway = "http://host.microsandbox.internal:18787/v1"
	testKey     = "sk-workspace-scoped-1234"
)

// testModels mixes tool-capable and completion-only models so tests exercise both
// tool_call:true and tool_call:false. gemini-pro is the completion-only one.
var testModels = []Model{
	{Name: "gemma4", Tools: true},
	{Name: "gpt-5.5", Tools: true},
	{Name: "claude-opus", Tools: true},
	{Name: "gemini-pro", Tools: false},
}

// toolModels wraps names as tool-capable Models for the merge/existence tests.
func toolModels(names ...string) []Model {
	out := make([]Model, len(names))
	for index, name := range names {
		out[index] = Model{Name: name, Tools: true}
	}
	return out
}

func TestOpenCodeConfigStructure(test *testing.T) {
	raw, err := OpenCodeConfig(testGateway, testKey, "gemma4", testModels, 5, 8000)
	if err != nil {
		test.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		test.Fatalf("output is not valid JSON: %v", err)
	}

	if document["$schema"] != "https://opencode.ai/config.json" {
		test.Errorf("missing/wrong $schema: %v", document["$schema"])
	}
	if document["model"] != "aip-gateway/gemma4" {
		test.Errorf("default model = %v, want aip-gateway/gemma4", document["model"])
	}

	// opencode is restricted to ONLY the gateway provider, so the model picker is
	// exactly the LiteLLM-served set (no auto-loaded built-in providers / models.dev).
	enabled, ok := document["enabled_providers"].([]any)
	if !ok || len(enabled) != 1 || enabled[0] != ProviderID {
		test.Errorf("enabled_providers = %v, want [%s]", document["enabled_providers"], ProviderID)
	}

	provider := nested(test, document, "provider", ProviderID)
	if provider["npm"] != "@ai-sdk/openai-compatible" {
		test.Errorf("npm = %v", provider["npm"])
	}
	options, ok := provider["options"].(map[string]any)
	if !ok {
		test.Fatalf("provider.options not an object: %v", provider["options"])
	}
	if options["baseURL"] != testGateway {
		test.Errorf("baseURL = %v, want %v", options["baseURL"], testGateway)
	}
	if !strings.HasSuffix(testGateway, "/v1") {
		test.Fatal("test gateway must carry the required /v1 suffix")
	}
	if options["apiKey"] != testKey {
		test.Errorf("apiKey = %v, want %v", options["apiKey"], testKey)
	}

	modelsNode, ok := provider["models"].(map[string]any)
	if !ok {
		test.Fatalf("provider.models not an object: %v", provider["models"])
	}
	if len(modelsNode) != len(testModels) {
		test.Fatalf("models count = %d, want %d", len(modelsNode), len(testModels))
	}
	for _, model := range testModels {
		entry, ok := modelsNode[model.Name].(map[string]any)
		if !ok {
			test.Fatalf("model %q missing: %v", model.Name, modelsNode[model.Name])
		}
		// tool_call must MATCH the model's advertised capability: a tool-capable model
		// gets true (opencode drives it agentically); a completion-only model gets false
		// (opencode never sends it a tool schema, which such a model would reject).
		if entry["tool_call"] != model.Tools {
			test.Errorf("model %q tool_call = %v, want %v", model.Name, entry["tool_call"], model.Tools)
		}
		// Every model marks reasoning + points opencode at the reasoning_content delta so
		// a thinking model's chain-of-thought is surfaced instead of hidden.
		if entry["reasoning"] != true {
			test.Errorf("model %q must set reasoning:true, got %v", model.Name, entry["reasoning"])
		}
		interleaved, ok := entry["interleaved"].(map[string]any)
		if !ok || interleaved["field"] != "reasoning_content" {
			test.Errorf("model %q interleaved must be {field: reasoning_content}, got %v", model.Name, entry["interleaved"])
		}
		modelOptions, ok := entry["options"].(map[string]any)
		if !ok {
			test.Fatalf("model %q options missing: %v", model.Name, entry["options"])
		}
		// opencode CAN inject per-request body fields: the Headroom knobs ride on
		// every model's options.
		if modelOptions["headroom_keep_turns"] != float64(5) {
			test.Errorf("model %q headroom_keep_turns = %v, want 5", model.Name, modelOptions["headroom_keep_turns"])
		}
		if modelOptions["headroom_output_buffer_tokens"] != float64(8000) {
			test.Errorf("model %q headroom_output_buffer_tokens = %v, want 8000", model.Name, modelOptions["headroom_output_buffer_tokens"])
		}
	}
}

func TestHermesConfigStructure(test *testing.T) {
	raw, err := HermesConfig(testGateway, "gemma4", "")
	if err != nil {
		test.Fatal(err)
	}
	text := string(raw)
	// KEYLESS: key_env NAMES the env var; the base_url is the gateway; skills pool wired.
	// Tokens checked independently so YAML quoting of the URL never makes this brittle.
	for _, want := range []string{
		"base_url", testGateway,
		"key_env", HermesKeyEnv,
		"provider", ProviderID,
		"default: gemma4",
		"external_dirs", HermesSkillsExternalDir,
	} {
		if !strings.Contains(text, want) {
			test.Errorf("hermes config missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, testKey) || strings.Contains(text, "sk-") {
		test.Errorf("hermes config must be keyless:\n%s", text)
	}
	// A blank default omits the default line (seed-then-remember).
	blank, err := HermesConfig(testGateway, "", "")
	if err != nil {
		test.Fatal(err)
	}
	if strings.Contains(string(blank), "default:") {
		test.Errorf("a blank default must omit model.default:\n%s", blank)
	}
	// A blank dashboard password omits the dashboard.basic_auth block.
	if strings.Contains(string(blank), "basic_auth") {
		test.Errorf("a blank dashboard password must omit the dashboard block:\n%s", blank)
	}
	// A non-empty dashboard password appends dashboard.basic_auth with the fixed
	// username + a scrypt hash — keyless still holds.
	withDash, err := HermesConfig(testGateway, "gemma4", "s3cret-pw")
	if err != nil {
		test.Fatal(err)
	}
	dashText := string(withDash)
	for _, want := range []string{"dashboard", "basic_auth", "username: " + HermesDashboardUsername, "password_hash", "scrypt$16384$8$1$"} {
		if !strings.Contains(dashText, want) {
			test.Errorf("hermes dashboard config missing %q:\n%s", want, dashText)
		}
	}
	if strings.Contains(dashText, "s3cret-pw") {
		test.Errorf("the plaintext dashboard password must not appear in the config:\n%s", dashText)
	}
}

func TestHermesDashboardPasswordHash(test *testing.T) {
	hash, err := HermesDashboardPasswordHash("hunter2")
	if err != nil {
		test.Fatal(err)
	}
	if !strings.HasPrefix(hash, "scrypt$16384$8$1$") {
		test.Errorf("hash missing scrypt prefix: %q", hash)
	}
	fields := strings.Split(hash, "$")
	if len(fields) != 6 {
		test.Fatalf("hash must have 6 $-separated fields, got %d: %q", len(fields), hash)
	}
	salt, err := base64.StdEncoding.DecodeString(fields[4])
	if err != nil || len(salt) != 16 {
		test.Errorf("salt field must be valid base64 of 16 bytes: len=%d err=%v", len(salt), err)
	}
	derivedKey, err := base64.StdEncoding.DecodeString(fields[5])
	if err != nil || len(derivedKey) != 32 {
		test.Errorf("dk field must be valid base64 of 32 bytes: len=%d err=%v", len(derivedKey), err)
	}
	// A fresh random salt makes two hashes of the same password differ.
	other, err := HermesDashboardPasswordHash("hunter2")
	if err != nil {
		test.Fatal(err)
	}
	if other == hash {
		test.Error("two hashes of the same password must differ (random salt)")
	}
}

func TestConfigsAreIndented(test *testing.T) {
	openCode, err := OpenCodeConfig(testGateway, testKey, "gemma4", testModels, 2, 4000)
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(string(openCode), "\n  ") {
		test.Error("opencode config is not indented")
	}
}

// TestProjectConfigsAreKeyless verifies the on-disk (host, project) per-CLI configs
// render KEYLESS: the scoped key is referenced via env interpolation ({env:}/$VAR) or
// env_key, never written literally — so the key never reaches host disk. The gateway
// base URL (not a secret) IS present so the config is useful.
func TestProjectConfigsAreKeyless(test *testing.T) {
	openCode, err := OpenCodeConfig(testGateway, OpenCodeAPIKeyRef, "", testModels, 5, 8000)
	if err != nil {
		test.Fatal(err)
	}
	claude, err := ClaudeSettings(testGateway)
	if err != nil {
		test.Fatal(err)
	}
	configs := map[string][]byte{
		"opencode": openCode, "codex": CodexConfig(testGateway, ""),
		"claude": claude, "codex-trust": CodexTrustConfig(),
	}
	for name, content := range configs {
		if text := string(content); strings.Contains(text, testKey) || strings.Contains(text, "sk-") {
			test.Errorf("%s project config must be keyless:\n%s", name, text)
		}
	}
	// opencode references the key via env interpolation, not a literal value.
	if !strings.Contains(string(openCode), OpenCodeAPIKeyRef) {
		test.Errorf("opencode must reference the key via %s:\n%s", OpenCodeAPIKeyRef, openCode)
	}
	// The gateway base URL is present in every config-file CLI (opencode/codex keep
	// /v1; claude carries the gateway ROOT in its env block).
	for name, content := range configs {
		if name == "codex-trust" {
			continue // trust file carries no base URL
		}
		if !strings.Contains(string(content), "host.microsandbox.internal:18787") {
			test.Errorf("%s config missing the gateway base URL:\n%s", name, content)
		}
	}
}

// TestMergeOpenCodeConfigInjectsDynamic verifies the merge keeps a user's
// template key (a custom top-level field) AND overlays the dynamic provider block
// (baseURL, apiKey, models) on top.
func TestMergeOpenCodeConfigInjectsDynamic(test *testing.T) {
	template := []byte(`{"theme":"dracula","provider":{}}`)
	merged, err := MergeOpenCodeConfig(template, testGateway, testKey, "", toolModels("gemma4"), 5, 8000)
	if err != nil {
		test.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(merged, &doc); err != nil {
		test.Fatalf("merged config not valid JSON: %v", err)
	}
	if doc["theme"] != "dracula" {
		test.Errorf("user theme not preserved: %v", doc["theme"])
	}
	provider := nested(test, doc, "provider", ProviderID)
	options := provider["options"].(map[string]any)
	if options["apiKey"] != testKey {
		test.Errorf("dynamic apiKey not injected: %v", options["apiKey"])
	}
	if options["baseURL"] != testGateway {
		test.Errorf("dynamic baseURL not injected: %v", options["baseURL"])
	}
}

// TestMergeOpenCodeConfigReplacesModelList verifies the served-model list is REPLACED
// wholesale (not unioned) so a model removed upstream disappears, while a user's other
// keys survive — and that an EMPTY served list (gateway down) PRESERVES the existing list
// rather than wiping it.
func TestMergeOpenCodeConfigReplacesModelList(test *testing.T) {
	// Existing config: an OLD model under the gateway provider + a user's custom top-level key.
	template := []byte(`{"theme":"dracula","provider":{"aip-gateway":{"models":{"vllm/old:latest":{"name":"vllm/old:latest"}}}}}`)

	merged, err := MergeOpenCodeConfig(template, testGateway, testKey, "", toolModels("vllm/new:latest"), 5, 8000)
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(string(merged), "vllm/new:latest") {
		test.Errorf("new served model missing:\n%s", merged)
	}
	if strings.Contains(string(merged), "vllm/old:latest") {
		test.Errorf("removed model must be dropped (list REPLACED, not unioned):\n%s", merged)
	}
	if !strings.Contains(string(merged), "dracula") {
		test.Errorf("user top-level key must survive the list replace:\n%s", merged)
	}

	// Empty served list (gateway down) must PRESERVE the existing list — never wipe it.
	preserved, err := MergeOpenCodeConfig(template, testGateway, testKey, "", nil, 5, 8000)
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(string(preserved), "vllm/old:latest") {
		test.Errorf("empty served list must not wipe the existing models:\n%s", preserved)
	}
}

// TestMergeNilTemplateFallsBack verifies a nil/empty/corrupt template degrades to
// the freshly-generated config (older projects, or a broken edit, still work).
func TestMergeNilTemplateFallsBack(test *testing.T) {
	merged, err := MergeOpenCodeConfig(nil, testGateway, testKey, "", toolModels("gemma4"), 5, 8000)
	if err != nil {
		test.Fatal(err)
	}
	generated, err := OpenCodeConfig(testGateway, testKey, "", toolModels("gemma4"), 5, 8000)
	if err != nil {
		test.Fatal(err)
	}
	if string(merged) != string(generated) {
		test.Errorf("nil template must yield the generated config verbatim")
	}
	// A corrupt template also falls back rather than erroring.
	corrupt, err := MergeOpenCodeConfig([]byte("{not json"), testGateway, testKey, "", toolModels("gemma4"), 5, 8000)
	if err != nil {
		test.Fatalf("corrupt template must not error: %v", err)
	}
	if string(corrupt) != string(generated) {
		test.Error("corrupt template must fall back to the generated config")
	}
}

// TestCodexConfig verifies codex's config.toml routes through the gateway via the
// RESPONSES wire API with a keyless, env-supplied key (env_key).
func TestCodexConfig(test *testing.T) {
	toml := string(CodexConfig(testGateway, "gemma4"))
	for _, want := range []string{
		`model_provider = "` + ProviderID + `"`,
		`model = "gemma4"`,
		`[model_providers.` + ProviderID + `]`,
		`base_url = "` + testGateway + `"`,
		`env_key = "AIP_GATEWAY_KEY"`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(toml, want) {
			test.Errorf("codex config.toml missing %q:\n%s", want, toml)
		}
	}
	// No default model → no top-level model line.
	if strings.Contains(string(CodexConfig(testGateway, "")), "\nmodel = ") {
		test.Error("codex config must omit the model line when no default is set")
	}
}

// TestAgentEnvScript verifies the env-routed CLIs get the right gateway env vars,
// with claude-code/gemini base URLs stripped of the /v1 suffix (they append their
// own path) and the scoped key present.
func TestAgentEnvScript(test *testing.T) {
	env := string(AgentEnvScript(testGateway, testKey, "", nil))
	root := "http://host.microsandbox.internal:18787" // testGateway minus /v1
	for _, want := range []string{
		`export ANTHROPIC_BASE_URL='` + root + `'`,
		`export ANTHROPIC_AUTH_TOKEN='` + testKey + `'`,
		`export AIP_GATEWAY_KEY='` + testKey + `'`,
		`export GOOGLE_GEMINI_BASE_URL='` + root + `'`,
		`export GEMINI_API_KEY='` + testKey + `'`,
	} {
		if !strings.Contains(env, want) {
			test.Errorf("agent env missing %q:\n%s", want, env)
		}
	}
	// The claude-code/gemini base URLs must NOT carry the /v1 suffix.
	if strings.Contains(env, root+"/v1") {
		test.Errorf("claude-code/gemini base URL must be the gateway root (no /v1):\n%s", env)
	}
	// With no graphify model configured, no OPENAI_* vars are exported.
	if strings.Contains(env, "OPENAI_") {
		test.Errorf("no graphify model → no OPENAI_* vars expected:\n%s", env)
	}
}

// TestAgentEnvScriptGraphify verifies that when a graphify model IS configured,
// its OpenAI-compatible backend is routed through the gateway's /v1 endpoint with
// the scoped virtual key and the vllm/<alias> public name.
func TestAgentEnvScriptGraphify(test *testing.T) {
	env := string(AgentEnvScript(testGateway, testKey, "llama3.1:8b", nil))
	for _, want := range []string{
		`export OPENAI_BASE_URL='` + testGateway + `'`, // keeps /v1 (OpenAI SDK appends /chat/completions)
		`export OPENAI_API_KEY='` + testKey + `'`,
		`export OPENAI_MODEL='vllm/llama3.1:8b'`,
	} {
		if !strings.Contains(env, want) {
			test.Errorf("graphify env missing %q:\n%s", want, env)
		}
	}
}

// TestAgentEnvScriptOAuth verifies that an OAuTH-mode agent's gateway env is OMITTED so
// its native login wins, while api-key agents keep theirs. AIP_GATEWAY_KEY is shared with
// opencode, so it is always exported.
func TestAgentEnvScriptOAuth(test *testing.T) {
	// claude-code + gemini in oauth: their gateway env must be gone.
	oauth := string(AgentEnvScript(testGateway, testKey, "", map[string]bool{"claude-code": true, "gemini": true}))
	for _, absent := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "GOOGLE_GEMINI_BASE_URL", "GEMINI_API_KEY", "ANTHROPIC_API_KEY"} {
		if strings.Contains(oauth, absent) {
			test.Errorf("oauth agent env must NOT export %q:\n%s", absent, oauth)
		}
	}
	// The shared gateway key (used by opencode) is still exported.
	if !strings.Contains(oauth, "export AIP_GATEWAY_KEY='"+testKey+"'") {
		test.Errorf("AIP_GATEWAY_KEY (shared with opencode) must still be exported:\n%s", oauth)
	}

	// api-key claude-code (empty/absent oauth set) keeps the ANTHROPIC_* exports.
	apiKey := string(AgentEnvScript(testGateway, testKey, "", map[string]bool{"gemini": true}))
	if !strings.Contains(apiKey, "ANTHROPIC_BASE_URL") || !strings.Contains(apiKey, "ANTHROPIC_AUTH_TOKEN") {
		test.Errorf("api-key claude-code must keep its ANTHROPIC_* env:\n%s", apiKey)
	}
	if strings.Contains(apiKey, "GEMINI_API_KEY") {
		test.Errorf("oauth gemini must NOT export GEMINI_API_KEY:\n%s", apiKey)
	}

	// copilot (forced-oauth, gateway-incapable) is never referenced by the env script — it
	// authenticates natively to GitHub. Passing it in the oauth set adds no gateway env for
	// it and no copilot/github references at all.
	withCopilot := string(AgentEnvScript(testGateway, testKey, "", map[string]bool{"copilot": true}))
	for _, absent := range []string{"copilot", "COPILOT", "githubcopilot", "GH_TOKEN", "GITHUB_TOKEN"} {
		if strings.Contains(withCopilot, absent) {
			test.Errorf("copilot must have no gateway env in the agent env script (found %q):\n%s", absent, withCopilot)
		}
	}
}

// TestCodexConfigOAuth verifies the OAuth codex config pins the ChatGPT login and file
// credential store, and carries NO gateway provider block.
func TestCodexConfigOAuth(test *testing.T) {
	toml := string(CodexConfigOAuth())
	for _, want := range []string{`forced_login_method = "chatgpt"`, `cli_auth_credentials_store = "file"`} {
		if !strings.Contains(toml, want) {
			test.Errorf("codex oauth config missing %q:\n%s", want, toml)
		}
	}
	if strings.Contains(toml, "model_providers") || strings.Contains(toml, "env_key") {
		test.Errorf("codex oauth config must NOT write a gateway provider block:\n%s", toml)
	}
}

// TestMergeClaudeSettingsOAuth verifies the oauth merge strips a stale gateway base URL
// (empty env block) while keeping the user's other top-level settings.
func TestMergeClaudeSettingsOAuth(test *testing.T) {
	existing := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://gw/","OTHER":"x"},"theme":"dark"}`)
	merged, err := MergeClaudeSettingsOAuth(existing)
	if err != nil {
		test.Fatal(err)
	}
	text := string(merged)
	if strings.Contains(text, "ANTHROPIC_BASE_URL") {
		test.Errorf("oauth claude settings must drop the gateway base URL:\n%s", text)
	}
	if !strings.Contains(text, `"theme": "dark"`) {
		test.Errorf("oauth claude settings must keep the user's other keys:\n%s", text)
	}
}

// TestOAuthProviderDomains checks the per-CLI egress domain lists.
func TestOAuthProviderDomains(test *testing.T) {
	cases := map[string]string{
		"claude-code": "api.anthropic.com",
		"codex":       "chatgpt.com",
		"gemini":      "generativelanguage.googleapis.com",
		"copilot":     "api.githubcopilot.com",
	}
	for cli, want := range cases {
		domains := OAuthProviderDomains(cli)
		if !contains(domains, want) {
			test.Errorf("OAuthProviderDomains(%q) = %v, want to contain %q", cli, domains, want)
		}
	}
	if domains := OAuthProviderDomains("opencode"); domains != nil {
		test.Errorf("opencode is not OAuth-capable; OAuthProviderDomains should be nil, got %v", domains)
	}
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// nested walks document[key1][key2] asserting each level is an object.
func nested(test *testing.T, document map[string]any, key1, key2 string) map[string]any {
	test.Helper()
	level1, ok := document[key1].(map[string]any)
	if !ok {
		test.Fatalf("%q not an object: %v", key1, document[key1])
	}
	level2, ok := level1[key2].(map[string]any)
	if !ok {
		test.Fatalf("%q.%q not an object: %v", key1, key2, level1[key2])
	}
	return level2
}

// TmuxConfig renders the managed transparent tmux config: mouse ON with a wheel-up→
// copy-mode binding (so the wheel scrolls tmux's scrollback past the alternate screen)
// plus OSC 52 clipboard so copy still reaches the host, status off (invisible), vi
// copy-mode keys, and a history limit, PLUS the terminal-capability directives modern
// TUI agent CLIs need to render correctly through tmux (truecolor + extended keys + a
// modern terminfo entry).
func TestTmuxConfig(test *testing.T) {
	conf := string(TmuxConfig())
	for _, want := range []string{
		// Mouse-scroll: wheel-up must reach tmux's scrollback via copy-mode, and copy
		// must still land on the host clipboard now that tmux owns click-drag.
		"set -g mouse on",
		"WheelUpPane",
		"copy-mode -e",
		"set -g set-clipboard on",
		"set -g status off",
		"setw -g mode-keys vi",
		"history-limit",
		// Modern-TUI capabilities (see the task background):
		// a modern terminfo entry, 24-bit truecolor passthrough, and CSI-u /
		// kitty extended-key forwarding so OpenCode's key combos survive tmux.
		`set -g default-terminal "tmux-256color"`,
		`set -as terminal-features ",*:RGB"`,
		`set -as terminal-features ",*:extkeys"`,
		"set -g extended-keys on",
		"set -g extended-keys-format csi-u",
	} {
		if !strings.Contains(conf, want) {
			test.Errorf("tmux.conf missing %q:\n%s", want, conf)
		}
	}
}

// TestOmpModelsConfig verifies omp's global models.yml is keyless YAML with the
// aip-gateway provider using openai-models-list discovery (no static model list).
func TestOmpModelsConfig(test *testing.T) {
	raw, err := OmpModelsConfig(testGateway, OmpAPIKeyRef)
	if err != nil {
		test.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{"providers:", ProviderID + ":", "baseUrl: " + testGateway, "api: openai-completions", "openai-models-list", "apiKey: " + OmpAPIKeyRef} {
		if !strings.Contains(out, want) {
			test.Errorf("omp models.yml missing %q:\n%s", want, out)
		}
	}
	// KEYLESS: the apiKey names the env var, the real scoped key is never written.
	if strings.Contains(out, testKey) {
		test.Errorf("omp models.yml must be keyless (env-var name, not the key):\n%s", out)
	}
	// Discovery-based: no explicit models list.
	if strings.Contains(out, "models:") {
		test.Errorf("omp uses discovery, not a static models list:\n%s", out)
	}
}

// TestOmpConfigSeedThenRemember verifies the project config.yml carries the provider
// order always and modelRoles.default ONLY when a default is seeded.
func TestOmpConfigSeedThenRemember(test *testing.T) {
	seeded, err := OmpConfig("vllm/qwen3-coder:30b")
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(string(seeded), "modelProviderOrder:") || !strings.Contains(string(seeded), ProviderID) {
		test.Errorf("omp config.yml missing provider order:\n%s", seeded)
	}
	if !strings.Contains(string(seeded), "default: "+ProviderID+"/vllm/qwen3-coder:30b") {
		test.Errorf("seeded omp config.yml missing modelRoles.default:\n%s", seeded)
	}
	empty, err := OmpConfig("")
	if err != nil {
		test.Fatal(err)
	}
	if strings.Contains(string(empty), "modelRoles") {
		test.Errorf("un-seeded omp config.yml must omit modelRoles (last-used wins):\n%s", empty)
	}
}

// TestHeadroomWrapName pins the mapping from this platform's agent CLIs to Headroom's
// fixed `wrap` tokens: claude-code→claude, codex→codex, opencode→opencode are wrappable;
// omp and gemini are NOT (aliasing them would break at runtime).
func TestHeadroomWrapName(test *testing.T) {
	wrappable := map[string]string{
		"claude-code": "claude",
		"codex":       "codex",
		"opencode":    "opencode",
		"copilot":     "copilot",
	}
	for cli, want := range wrappable {
		got, ok := HeadroomWrapName(cli)
		if !ok || got != want {
			test.Errorf("HeadroomWrapName(%q) = (%q, %v), want (%q, true)", cli, got, ok, want)
		}
	}
	for _, cli := range []string{"omp", "gemini", "unknown", ""} {
		if got, ok := HeadroomWrapName(cli); ok {
			test.Errorf("HeadroomWrapName(%q) = (%q, true), want ok=false (not Headroom-wrappable)", cli, got)
		}
	}
}

// TestShellAliases verifies the snippet aliases each wrappable installed CLI to
// `headroom wrap <name>` and OMITS the non-wrappable ones (omp, gemini).
func TestShellAliases(test *testing.T) {
	snippet := string(ShellAliases([]string{"opencode", "omp", "claude-code", "codex", "gemini"}))
	for _, want := range []string{
		`alias claude='headroom wrap claude'`,
		`alias codex='headroom wrap codex'`,
		`alias opencode='headroom wrap opencode'`,
	} {
		if !strings.Contains(snippet, want) {
			test.Errorf("ShellAliases missing %q:\n%s", want, snippet)
		}
	}
	for _, absent := range []string{"alias omp=", "alias gemini="} {
		if strings.Contains(snippet, absent) {
			test.Errorf("ShellAliases must not alias a non-wrappable CLI (%q):\n%s", absent, snippet)
		}
	}
	// Deterministic + de-duplicated: a repeated tool yields exactly one alias line.
	repeated := string(ShellAliases([]string{"claude-code", "claude-code"}))
	if strings.Count(repeated, "alias claude=") != 1 {
		test.Errorf("ShellAliases must de-duplicate; got:\n%s", repeated)
	}
	// No tools → no alias lines (just the managed-by header comments).
	if strings.Contains(string(ShellAliases(nil)), "alias ") {
		test.Errorf("ShellAliases(nil) should define no aliases")
	}
}

// TestShellRCBlock verifies the managed rc block is marker-delimited and sources BOTH the
// agent gateway env file and the Headroom-wrap alias snippet, so bash + zsh interactive
// shells route through the gateway with the aliases defined.
func TestShellRCBlock(test *testing.T) {
	block := string(ShellRCBlock())
	for _, want := range []string{
		ShellRCMarkerBegin,
		ShellRCMarkerEnd,
		AgentEnvFileGuestPath,
		ShellAliasesFileGuestPath,
	} {
		if !strings.Contains(block, want) {
			test.Errorf("ShellRCBlock missing %q:\n%s", want, block)
		}
	}
	if !strings.HasPrefix(block, ShellRCMarkerBegin) {
		test.Errorf("ShellRCBlock must start with the begin marker:\n%s", block)
	}
	if !strings.Contains(block, "&& . '"+AgentEnvFileGuestPath+"'") {
		test.Errorf("ShellRCBlock must SOURCE the agent env file:\n%s", block)
	}
	if !strings.Contains(block, "&& . '"+ShellAliasesFileGuestPath+"'") {
		test.Errorf("ShellRCBlock must SOURCE the shell-aliases file:\n%s", block)
	}
}

// TestBashProfileSourcesAliases verifies the managed ~/.bash_profile also sources the
// Headroom-wrap alias snippet (alongside the agent env), keeping login shells consistent.
func TestBashProfileSourcesAliases(test *testing.T) {
	profile := string(BashProfile())
	if !strings.Contains(profile, ShellAliasesFileGuestPath) {
		test.Errorf("BashProfile must source the shell-aliases file:\n%s", profile)
	}
	if !strings.Contains(profile, AgentEnvFileGuestPath) {
		test.Errorf("BashProfile must still source the agent env file:\n%s", profile)
	}
}
