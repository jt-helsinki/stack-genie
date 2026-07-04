package agentcfg

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	testGateway = "http://host.microsandbox.internal:18787/v1"
	testKey     = "sk-workspace-scoped-1234"
)

var testModels = []string{"gemma4", "gpt-5.5", "claude-opus", "gemini-pro"}

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
		entry, ok := modelsNode[model].(map[string]any)
		if !ok {
			test.Fatalf("model %q missing: %v", model, modelsNode[model])
		}
		modelOptions, ok := entry["options"].(map[string]any)
		if !ok {
			test.Fatalf("model %q options missing: %v", model, entry["options"])
		}
		// opencode CAN inject per-request body fields: the Headroom knobs ride on
		// every model's options.
		if modelOptions["headroom_keep_turns"] != float64(5) {
			test.Errorf("model %q headroom_keep_turns = %v, want 5", model, modelOptions["headroom_keep_turns"])
		}
		if modelOptions["headroom_output_buffer_tokens"] != float64(8000) {
			test.Errorf("model %q headroom_output_buffer_tokens = %v, want 8000", model, modelOptions["headroom_output_buffer_tokens"])
		}
	}
}

func TestPiConfigStructure(test *testing.T) {
	raw, err := PiConfig(testGateway, testKey, "gemma4", testModels)
	if err != nil {
		test.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		test.Fatalf("output is not valid JSON: %v", err)
	}

	provider := nested(test, document, "providers", ProviderID)
	if provider["baseUrl"] != testGateway {
		test.Errorf("baseUrl = %v, want %v", provider["baseUrl"], testGateway)
	}
	if provider["api"] != "openai-completions" {
		test.Errorf("api = %v, want openai-completions", provider["api"])
	}
	if provider["apiKey"] != testKey {
		test.Errorf("apiKey = %v, want %v", provider["apiKey"], testKey)
	}

	rawModels, ok := provider["models"].([]any)
	if !ok {
		test.Fatalf("models not an array: %v", provider["models"])
	}
	if len(rawModels) != len(testModels) {
		test.Fatalf("models count = %d, want %d", len(rawModels), len(testModels))
	}
	for index, item := range rawModels {
		entry, ok := item.(map[string]any)
		if !ok {
			test.Fatalf("model[%d] not an object: %v", index, item)
		}
		if entry["id"] != testModels[index] {
			test.Errorf("model[%d] id = %v, want %v", index, entry["id"], testModels[index])
		}
		// pi CANNOT inject per-request body fields, so the Headroom knobs must be
		// absent: pi falls back to Headroom's server-side defaults.
		if _, present := entry["options"]; present {
			test.Errorf("model[%d] must not carry per-request options: %v", index, entry["options"])
		}
	}

	// The whole document must be free of the Headroom knobs for pi.
	if strings.Contains(string(raw), "headroom_keep_turns") || strings.Contains(string(raw), "headroom_output_buffer_tokens") {
		test.Errorf("pi config must not contain Headroom knobs:\n%s", raw)
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
	pi, err := PiConfig(testGateway, testKey, "gemma4", testModels)
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(string(pi), "\n  ") {
		test.Error("pi config is not indented")
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
	pi, err := PiConfig(testGateway, PiAPIKeyRef, "", testModels)
	if err != nil {
		test.Fatal(err)
	}
	claude, err := ClaudeSettings(testGateway)
	if err != nil {
		test.Fatal(err)
	}
	configs := map[string][]byte{
		"opencode": openCode, "pi": pi, "codex": CodexConfig(testGateway, ""),
		"claude": claude, "codex-trust": CodexTrustConfig(),
	}
	for name, content := range configs {
		if text := string(content); strings.Contains(text, testKey) || strings.Contains(text, "sk-") {
			test.Errorf("%s project config must be keyless:\n%s", name, text)
		}
	}
	// opencode/pi reference the key via env interpolation, not a literal value.
	if !strings.Contains(string(openCode), OpenCodeAPIKeyRef) {
		test.Errorf("opencode must reference the key via %s:\n%s", OpenCodeAPIKeyRef, openCode)
	}
	if !strings.Contains(string(pi), PiAPIKeyRef) {
		test.Errorf("pi must reference the key via %s:\n%s", PiAPIKeyRef, pi)
	}
	// The gateway base URL is present in every config-file CLI (opencode/pi/codex keep
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

// TestPiSettings verifies pi's per-project settings set the gateway default provider
// and point the skills/prompts resource paths at the symlinked shared pools.
func TestPiSettings(test *testing.T) {
	settings, err := PiSettings("ollama/gemma4")
	if err != nil {
		test.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(settings, &doc); err != nil {
		test.Fatalf("pi settings not valid JSON: %v", err)
	}
	if doc["defaultProvider"] != ProviderID {
		test.Errorf("defaultProvider = %v, want %s", doc["defaultProvider"], ProviderID)
	}
	if doc["defaultModel"] != "ollama/gemma4" {
		test.Errorf("defaultModel = %v, want ollama/gemma4", doc["defaultModel"])
	}
	for _, key := range []string{"skills", "prompts"} {
		paths, ok := doc[key].([]any)
		if !ok || len(paths) == 0 {
			test.Errorf("pi settings %q resource path missing: %v", key, doc[key])
		}
	}
	// An empty default omits the key (no forced model — CLIs fall back to their own).
	empty, err := PiSettings("")
	if err != nil {
		test.Fatal(err)
	}
	var emptyDoc map[string]any
	if err := json.Unmarshal(empty, &emptyDoc); err != nil {
		test.Fatal(err)
	}
	if _, present := emptyDoc["defaultModel"]; present {
		test.Errorf("empty default should omit defaultModel, got %v", emptyDoc["defaultModel"])
	}
}

// TestMergeOpenCodeConfigInjectsDynamic verifies the merge keeps a user's
// template key (a custom top-level field) AND overlays the dynamic provider block
// (baseURL, apiKey, models) on top.
func TestMergeOpenCodeConfigInjectsDynamic(test *testing.T) {
	template := []byte(`{"theme":"dracula","provider":{}}`)
	merged, err := MergeOpenCodeConfig(template, testGateway, testKey, "", []string{"gemma4"}, 5, 8000)
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
	template := []byte(`{"theme":"dracula","provider":{"aip-gateway":{"models":{"ollama/old:latest":{"name":"ollama/old:latest"}}}}}`)

	merged, err := MergeOpenCodeConfig(template, testGateway, testKey, "", []string{"ollama/new:latest"}, 5, 8000)
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(string(merged), "ollama/new:latest") {
		test.Errorf("new served model missing:\n%s", merged)
	}
	if strings.Contains(string(merged), "ollama/old:latest") {
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
	if !strings.Contains(string(preserved), "ollama/old:latest") {
		test.Errorf("empty served list must not wipe the existing models:\n%s", preserved)
	}
}

// TestMergeNilTemplateFallsBack verifies a nil/empty/corrupt template degrades to
// the freshly-generated config (older projects, or a broken edit, still work).
func TestMergeNilTemplateFallsBack(test *testing.T) {
	merged, err := MergeOpenCodeConfig(nil, testGateway, testKey, "", []string{"gemma4"}, 5, 8000)
	if err != nil {
		test.Fatal(err)
	}
	generated, err := OpenCodeConfig(testGateway, testKey, "", []string{"gemma4"}, 5, 8000)
	if err != nil {
		test.Fatal(err)
	}
	if string(merged) != string(generated) {
		test.Errorf("nil template must yield the generated config verbatim")
	}
	// A corrupt template also falls back rather than erroring.
	corrupt, err := MergeOpenCodeConfig([]byte("{not json"), testGateway, testKey, "", []string{"gemma4"}, 5, 8000)
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
	env := string(AgentEnvScript(testGateway, testKey, ""))
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
// the scoped virtual key and the ollama/<model> public name.
func TestAgentEnvScriptGraphify(test *testing.T) {
	env := string(AgentEnvScript(testGateway, testKey, "llama3.1:8b"))
	for _, want := range []string{
		`export OPENAI_BASE_URL='` + testGateway + `'`, // keeps /v1 (OpenAI SDK appends /chat/completions)
		`export OPENAI_API_KEY='` + testKey + `'`,
		`export OPENAI_MODEL='ollama/llama3.1:8b'`,
	} {
		if !strings.Contains(env, want) {
			test.Errorf("graphify env missing %q:\n%s", want, env)
		}
	}
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

// TmuxConfig renders the managed transparent tmux config: mouse on (wheel
// scrollback), status off (invisible), vi copy-mode keys, and a history limit,
// PLUS the terminal-capability directives modern TUI agent CLIs need to render
// correctly through tmux (truecolor + extended keys + a modern terminfo entry).
func TestTmuxConfig(test *testing.T) {
	conf := string(TmuxConfig())
	for _, want := range []string{
		// Existing transparency settings.
		"set -g mouse on",
		"set -g status off",
		"setw -g mode-keys vi",
		"history-limit",
		// Modern-TUI capabilities (see the task background):
		// a modern terminfo entry, 24-bit truecolor passthrough, and CSI-u /
		// kitty extended-key forwarding so OpenCode's key combos survive tmux.
		`set -g default-terminal "tmux-256color"`,
		`set -as terminal-features ",*:RGB"`,
		`set -as terminal-features ",*:extkeys"`,
		"set -s extended-keys on",
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
	seeded, err := OmpConfig("ollama/qwen3-coder:30b")
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(string(seeded), "modelProviderOrder:") || !strings.Contains(string(seeded), ProviderID) {
		test.Errorf("omp config.yml missing provider order:\n%s", seeded)
	}
	if !strings.Contains(string(seeded), "default: "+ProviderID+"/ollama/qwen3-coder:30b") {
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
