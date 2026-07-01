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

// TestTemplatesAreKeyless verifies the host-side templates render with NO apiKey
// and no model picker — the dynamic, key-bearing values are injected only into the
// in-VM final config (the scoped key never reaches host disk).
func TestTemplatesAreKeyless(test *testing.T) {
	openCode, err := OpenCodeTemplate(testGateway, 5, 8000)
	if err != nil {
		test.Fatal(err)
	}
	pi, err := PiTemplate(testGateway)
	if err != nil {
		test.Fatal(err)
	}
	for name, content := range map[string][]byte{"opencode": openCode, "pi": pi, "codex": CodexConfig(testGateway, "")} {
		text := string(content)
		if strings.Contains(text, testKey) || strings.Contains(text, "sk-") {
			test.Errorf("%s template must be keyless:\n%s", name, text)
		}
		// The gateway base URL (not a secret) IS present so the file is useful.
		if !strings.Contains(text, "host.microsandbox.internal:18787") {
			test.Errorf("%s template missing the gateway base URL:\n%s", name, text)
		}
	}
	// opencode/pi templates carry an empty apiKey field (the shape the user sees).
	var doc map[string]any
	if err := json.Unmarshal(openCode, &doc); err != nil {
		test.Fatal(err)
	}
	provider := nested(test, doc, "provider", ProviderID)
	options := provider["options"].(map[string]any)
	if options["apiKey"] != "" {
		test.Errorf("opencode template apiKey = %v, want empty", options["apiKey"])
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
	} {
		if !strings.Contains(conf, want) {
			test.Errorf("tmux.conf missing %q:\n%s", want, conf)
		}
	}
}
