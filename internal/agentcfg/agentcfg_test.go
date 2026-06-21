package agentcfg

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	testGateway = "http://host.microsandbox.internal:8787/v1"
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
