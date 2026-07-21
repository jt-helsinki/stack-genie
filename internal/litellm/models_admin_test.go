package litellm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRegisterOllamaModelsAddsOnlyMissing verifies the bulk local-model reconcile
// registers installed Ollama models that are NOT already served and skips ones that
// are — the recovery path behind the Local Models `r` refresh.
func TestRegisterOllamaModelsAddsOnlyMissing(test *testing.T) {
	var added []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/model/info":
			// gemma4 already served; qwen3-coder is not.
			_, _ = writer.Write([]byte(`{"data":[{"model_name":"ollama/gemma4","litellm_params":{"model":"ollama/gemma4"},"model_info":{"id":"g"}}]}`))
		case "/model/new":
			payload, _ := io.ReadAll(request.Body)
			var body map[string]any
			_ = json.Unmarshal(payload, &body)
			added = append(added, body["model_name"].(string))
			_, _ = writer.Write([]byte(`{}`))
		default:
			test.Errorf("unexpected %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	got, err := manager.RegisterOllamaModels([]string{"gemma4", "qwen3-coder:30b"})
	if err != nil {
		test.Fatalf("RegisterOllamaModels: %v", err)
	}
	if len(got) != 1 || got[0] != "ollama/qwen3-coder:30b" {
		test.Errorf("registered = %v, want [ollama/qwen3-coder:30b]", got)
	}
	if len(added) != 1 || added[0] != "ollama/qwen3-coder:30b" {
		test.Errorf("gateway saw adds %v, want [ollama/qwen3-coder:30b] (gemma4 already served)", added)
	}
}

// TestSetCredentialRequestShape verifies SetCredential deletes any prior credential
// of the same name then POSTs /credentials with the documented body
// (credential_name, credential_info.custom_llm_provider, credential_values.api_key)
// and the Bearer master key.
func TestSetCredentialRequestShape(test *testing.T) {
	var sawDelete bool
	var postBody map[string]any
	var postAuth string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodDelete:
			// DELETE /credentials/openai-key (the pre-delete on set).
			if request.URL.Path != "/credentials/openai-key" {
				test.Errorf("delete path = %q, want /credentials/openai-key", request.URL.Path)
			}
			sawDelete = true
			_, _ = writer.Write([]byte(`{}`))
		case http.MethodPost:
			if request.URL.Path != "/credentials" {
				test.Errorf("post path = %q, want /credentials", request.URL.Path)
			}
			postAuth = request.Header.Get("Authorization")
			payload, _ := io.ReadAll(request.Body)
			_ = json.Unmarshal(payload, &postBody)
			_, _ = writer.Write([]byte(`{}`))
		default:
			test.Errorf("unexpected %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	if err := manager.SetCredential("openai", "sk-secret-openai"); err != nil {
		test.Fatalf("SetCredential: %v", err)
	}
	if !sawDelete {
		test.Error("SetCredential should pre-delete the existing credential of the same name")
	}
	if postAuth != "Bearer "+testMasterKey {
		test.Errorf("auth = %q, want Bearer master key", postAuth)
	}
	if postBody["credential_name"] != "openai-key" {
		test.Errorf("credential_name = %v, want openai-key", postBody["credential_name"])
	}
	info, _ := postBody["credential_info"].(map[string]any)
	if info["custom_llm_provider"] != "openai" {
		test.Errorf("custom_llm_provider = %v, want openai", info["custom_llm_provider"])
	}
	values, _ := postBody["credential_values"].(map[string]any)
	if values["api_key"] != "sk-secret-openai" {
		test.Errorf("api_key = %v, want the secret", values["api_key"])
	}
}

// TestListCredentialsParses verifies the GET /credentials response (credentials
// array of credential_name + credential_info.custom_llm_provider) is parsed; the
// secret values are never present.
func TestListCredentialsParses(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/credentials" {
			test.Errorf("unexpected %s %s", request.Method, request.URL.Path)
		}
		_, _ = writer.Write([]byte(`{"credentials":[
			{"credential_name":"openai-key","credential_info":{"custom_llm_provider":"openai"}},
			{"credential_name":"gemini-key","credential_info":{"custom_llm_provider":"gemini"}}
		]}`))
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	creds, err := manager.ListCredentials()
	if err != nil {
		test.Fatalf("ListCredentials: %v", err)
	}
	if len(creds) != 2 {
		test.Fatalf("creds = %d, want 2", len(creds))
	}
	if creds[0].Name != "openai-key" || creds[0].Provider != "openai" {
		test.Errorf("creds[0] = %+v", creds[0])
	}
	if creds[1].Name != "gemini-key" || creds[1].Provider != "gemini" {
		test.Errorf("creds[1] = %+v", creds[1])
	}
}

// TestAddModelRequestShape verifies a cloud model add: model_name = the catalog id
// verbatim; litellm_params.model = the LiteLLM-rewritten id; litellm_credential_name
// = the provider credential; model_info carries the catalog metadata.
func TestAddModelRequestShape(test *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/model/new" {
			test.Errorf("unexpected %s %s", request.Method, request.URL.Path)
		}
		payload, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(payload, &body)
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	err := manager.AddModel("google/gemini-3.1-pro",
		ModelParams{Model: "gemini/gemini-3.1-pro", CredentialName: "gemini-key"},
		ModelInfo{Family: "gemini", ContextLen: 1000000})
	if err != nil {
		test.Fatalf("AddModel: %v", err)
	}
	if body["model_name"] != "google/gemini-3.1-pro" {
		test.Errorf("model_name = %v, want the catalog id verbatim", body["model_name"])
	}
	params, _ := body["litellm_params"].(map[string]any)
	if params["model"] != "gemini/gemini-3.1-pro" {
		test.Errorf("litellm_params.model = %v, want the rewritten id", params["model"])
	}
	if params["litellm_credential_name"] != "gemini-key" {
		test.Errorf("litellm_credential_name = %v, want gemini-key", params["litellm_credential_name"])
	}
	// api_base / api_key must be omitted for a credential-referencing cloud model.
	if _, present := params["api_base"]; present {
		test.Errorf("api_base must be omitted for a cloud model, got %v", params["api_base"])
	}
	info, _ := body["model_info"].(map[string]any)
	if info["family"] != "gemini" {
		test.Errorf("model_info.family = %v, want gemini", info["family"])
	}
}

// TestAddModelOllamaShape verifies an Ollama model add: api_base points at the
// in-network Ollama and no credential is referenced.
func TestAddModelOllamaShape(test *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		payload, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(payload, &body)
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	if err := manager.AddModel("ollama/gemma4",
		ModelParams{Model: "ollama/gemma4", APIBase: OllamaAPIBase}, ModelInfo{}); err != nil {
		test.Fatalf("AddModel: %v", err)
	}
	params, _ := body["litellm_params"].(map[string]any)
	if params["api_base"] != OllamaAPIBase {
		test.Errorf("api_base = %v, want %s", params["api_base"], OllamaAPIBase)
	}
	if _, present := params["litellm_credential_name"]; present {
		test.Errorf("an Ollama model must not reference a credential, got %v", params["litellm_credential_name"])
	}
}

// TestDeleteModelRequestShape verifies DeleteModel POSTs /model/delete with the id.
func TestDeleteModelRequestShape(test *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/model/delete" {
			test.Errorf("unexpected %s %s", request.Method, request.URL.Path)
		}
		payload, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(payload, &body)
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	if err := manager.DeleteModel("model-abc-123"); err != nil {
		test.Fatalf("DeleteModel: %v", err)
	}
	if body["id"] != "model-abc-123" {
		test.Errorf("delete body id = %v, want model-abc-123", body["id"])
	}
}

// TestListModelsParses verifies GET /model/info parsing into LiveModel (id, name,
// routed target, derived provider), de-duplicated by name.
func TestListModelsParses(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/model/info" {
			test.Errorf("unexpected %s %s", request.Method, request.URL.Path)
		}
		_, _ = writer.Write([]byte(`{"data":[
			{"model_name":"openai/gpt-5.5","litellm_params":{"model":"openai/gpt-5.5"},"model_info":{"id":"id-1"}},
			{"model_name":"ollama/gemma4","litellm_params":{"model":"ollama/gemma4"},"model_info":{"id":"id-2"}},
			{"model_name":"openai/gpt-5.5","litellm_params":{"model":"openai/gpt-5.5"},"model_info":{"id":"dup"}}
		]}`))
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	models, err := manager.ListModels()
	if err != nil {
		test.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		test.Fatalf("models = %d, want 2 (deduped by name)", len(models))
	}
	byName := map[string]LiveModel{}
	for _, model := range models {
		byName[model.Name] = model
	}
	if got := byName["openai/gpt-5.5"]; got.ID != "id-1" || got.Provider != "openai" {
		test.Errorf("openai model = %+v, want id-1 / openai", got)
	}
	if got := byName["ollama/gemma4"]; got.ID != "id-2" || got.Provider != "ollama" {
		test.Errorf("ollama model = %+v, want id-2 / ollama", got)
	}
}

// TestAddModelEmptyName rejects an empty model_name without a round-trip.
func TestAddModelEmptyName(test *testing.T) {
	manager := NewKeyManager(okProber())
	if err := manager.AddModel("", ModelParams{Model: "x"}, ModelInfo{}); err == nil {
		test.Error("AddModel with empty name should error")
	}
}

// TestRegisterOllamaModelRequestShape verifies an Ollama registration: it first
// lists models (GET /model/info), and — when not already present — POSTs /model/new
// with model_name = "ollama/<name>" verbatim, litellm_params.model = the same, and
// api_base = the in-network Ollama (OllamaAPIBase). No credential is referenced.
func TestRegisterOllamaModelRequestShape(test *testing.T) {
	var addBody map[string]any
	var sawList bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/model/info":
			sawList = true
			_, _ = writer.Write([]byte(`{"data":[]}`)) // nothing registered yet
		case request.Method == http.MethodPost && request.URL.Path == "/model/new":
			payload, _ := io.ReadAll(request.Body)
			_ = json.Unmarshal(payload, &addBody)
			_, _ = writer.Write([]byte(`{}`))
		default:
			test.Errorf("unexpected %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	if err := manager.RegisterOllamaModel("llama3.2:3b"); err != nil {
		test.Fatalf("RegisterOllamaModel: %v", err)
	}
	if !sawList {
		test.Error("RegisterOllamaModel should list existing models before adding")
	}
	if addBody["model_name"] != "ollama/llama3.2:3b" {
		test.Errorf("model_name = %v, want ollama/llama3.2:3b", addBody["model_name"])
	}
	params, _ := addBody["litellm_params"].(map[string]any)
	// PUBLIC handle stays ollama/<name>, but LiteLLM ROUTES on ollama_chat/<name> so it
	// uses Ollama's /api/chat (messages + tools + streaming), not the legacy /api/generate.
	if params["model"] != "ollama_chat/llama3.2:3b" {
		test.Errorf("litellm_params.model = %v, want ollama_chat/llama3.2:3b", params["model"])
	}
	if params["api_base"] != OllamaAPIBase {
		test.Errorf("api_base = %v, want %s", params["api_base"], OllamaAPIBase)
	}
	if _, present := params["litellm_credential_name"]; present {
		test.Errorf("an Ollama model must not reference a credential, got %v", params["litellm_credential_name"])
	}
}

// TestRegisterOllamaModelSkipsWhenPresent verifies registration is a no-op (no
// /model/new) when a model with the same model_name already exists.
func TestRegisterOllamaModelSkipsWhenPresent(test *testing.T) {
	var sawAdd bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/model/info":
			_, _ = writer.Write([]byte(`{"data":[
				{"model_name":"ollama/gemma4","litellm_params":{"model":"ollama/gemma4"},"model_info":{"id":"id-1"}}
			]}`))
		case request.URL.Path == "/model/new":
			sawAdd = true
			_, _ = writer.Write([]byte(`{}`))
		default:
			test.Errorf("unexpected %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	if err := manager.RegisterOllamaModel("gemma4"); err != nil {
		test.Fatalf("RegisterOllamaModel: %v", err)
	}
	if sawAdd {
		test.Error("RegisterOllamaModel should skip /model/new when the model is already registered")
	}
}

// TestUnregisterOllamaModelDeletesByID verifies it finds the entry whose model_name
// == "ollama/<name>" and POSTs /model/delete with that entry's id.
func TestUnregisterOllamaModelDeletesByID(test *testing.T) {
	var deleteBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/model/info":
			_, _ = writer.Write([]byte(`{"data":[
				{"model_name":"ollama/llama3.2:3b","litellm_params":{"model":"ollama/llama3.2:3b"},"model_info":{"id":"id-llama"}},
				{"model_name":"openai/gpt-5.5","litellm_params":{"model":"openai/gpt-5.5"},"model_info":{"id":"id-gpt"}}
			]}`))
		case request.Method == http.MethodPost && request.URL.Path == "/model/delete":
			payload, _ := io.ReadAll(request.Body)
			_ = json.Unmarshal(payload, &deleteBody)
			_, _ = writer.Write([]byte(`{}`))
		default:
			test.Errorf("unexpected %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	if err := manager.UnregisterOllamaModel("llama3.2:3b"); err != nil {
		test.Fatalf("UnregisterOllamaModel: %v", err)
	}
	if deleteBody["id"] != "id-llama" {
		test.Errorf("delete id = %v, want id-llama (the matching ollama/<name> entry)", deleteBody["id"])
	}
}

// TestUnregisterOllamaModelNoOpWhenAbsent verifies it is a no-op (no /model/delete,
// no error) when no model with that model_name is registered.
func TestUnregisterOllamaModelNoOpWhenAbsent(test *testing.T) {
	var sawDelete bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/model/info":
			_, _ = writer.Write([]byte(`{"data":[
				{"model_name":"openai/gpt-5.5","litellm_params":{"model":"openai/gpt-5.5"},"model_info":{"id":"id-gpt"}}
			]}`))
		case request.URL.Path == "/model/delete":
			sawDelete = true
			_, _ = writer.Write([]byte(`{}`))
		default:
			test.Errorf("unexpected %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	if err := manager.UnregisterOllamaModel("ghost"); err != nil {
		test.Fatalf("UnregisterOllamaModel(absent) should be a no-op, got %v", err)
	}
	if sawDelete {
		test.Error("UnregisterOllamaModel should not call /model/delete when the model is absent")
	}
}

// TestOllamaModelName pins the public-handle convention.
func TestOllamaModelName(test *testing.T) {
	if got := OllamaModelName("llama3.2:3b"); got != "ollama/llama3.2:3b" {
		test.Errorf("OllamaModelName = %q, want ollama/llama3.2:3b", got)
	}
}

// TestCredentialName pins the credential naming convention.
func TestCredentialName(test *testing.T) {
	if got := CredentialName("openai"); got != "openai-key" {
		test.Errorf("CredentialName(openai) = %q, want openai-key", got)
	}
}
