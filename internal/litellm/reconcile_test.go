package litellm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// TestReconcileDiff verifies the pure diff: a desired model not currently served
// is added; a served model not desired is deleted (by id); a model in both is a
// no-op.
func TestReconcileDiff(test *testing.T) {
	desired := []DesiredModel{
		{Name: "openai/gpt-5.5"},        // already current → no-op
		{Name: "google/gemini-3.1-pro"}, // new → add
		{Name: "ollama/gemma4"},         // new → add
	}
	current := []LiveModel{
		{ID: "id-keep", Name: "openai/gpt-5.5"},
		{ID: "id-stale", Name: "anthropic/claude-old"}, // not desired → delete
	}

	plan := Reconcile(desired, current)

	addNames := make([]string, 0, len(plan.Add))
	for _, model := range plan.Add {
		addNames = append(addNames, model.Name)
	}
	// Sorted.
	if strings.Join(addNames, ",") != "google/gemini-3.1-pro,ollama/gemma4" {
		test.Errorf("add = %v, want the two new models sorted", addNames)
	}
	if len(plan.Delete) != 1 || plan.Delete[0].ID != "id-stale" {
		test.Errorf("delete = %+v, want [id-stale]", plan.Delete)
	}
}

// TestReconcileIdempotent verifies an equal desired/current set yields an empty
// plan (no-op on a re-run).
func TestReconcileIdempotent(test *testing.T) {
	desired := []DesiredModel{{Name: "openai/gpt-5.5"}, {Name: "ollama/gemma4"}}
	current := []LiveModel{
		{ID: "a", Name: "openai/gpt-5.5"},
		{ID: "b", Name: "ollama/gemma4"},
	}
	plan := Reconcile(desired, current)
	if !plan.Empty() {
		test.Errorf("equal sets should yield an empty plan, got add=%v delete=%v", plan.Add, plan.Delete)
	}
}

// TestReconcileDedupsDesired verifies a duplicated desired name does not produce a
// duplicate add.
func TestReconcileDedupsDesired(test *testing.T) {
	desired := []DesiredModel{{Name: "x/y"}, {Name: "x/y"}}
	plan := Reconcile(desired, nil)
	if len(plan.Add) != 1 {
		test.Errorf("add = %v, want a single deduped entry", plan.Add)
	}
}

// TestDesiredModelsExpandsKeyedProviders verifies DesiredModels expands every
// catalog model of a KEYED, LiteLLM-routable provider (with the credential ref and
// the rewritten litellm_params.model) plus the Ollama models, and that an unkeyed
// or unroutable provider is excluded.
func TestDesiredModelsExpandsKeyedProviders(test *testing.T) {
	cat := testCatalog(test) // providers: openai, google (routable); megarouter (not)

	// Key google (→ gemini) only; openai stays unkeyed so its model is excluded.
	desired := DesiredModels(cat, []string{"google", "megarouter"}, []string{"gemma4"})

	byName := map[string]DesiredModel{}
	names := make([]string, 0, len(desired))
	for _, model := range desired {
		byName[model.Name] = model
		names = append(names, model.Name)
	}
	sort.Strings(names)
	// google/gemini-3.1-pro (keyed) + ollama/gemma4; openai excluded (unkeyed),
	// megarouter excluded (unroutable — even though "keyed").
	if strings.Join(names, ",") != "google/gemini-3.1-pro,ollama/gemma4" {
		test.Fatalf("desired names = %v, want the keyed-routable + ollama models", names)
	}

	cloud := byName["google/gemini-3.1-pro"]
	if cloud.Params.Model != "gemini/gemini-3.1-pro" {
		test.Errorf("cloud litellm_params.model = %q, want gemini/gemini-3.1-pro", cloud.Params.Model)
	}
	if cloud.Params.CredentialName != "gemini-key" {
		test.Errorf("cloud credential = %q, want gemini-key", cloud.Params.CredentialName)
	}
	if cloud.Info.ContextLen != 1000000 {
		test.Errorf("cloud model_info.context = %d, want the catalog limit", cloud.Info.ContextLen)
	}

	local := byName["ollama/gemma4"]
	if local.Params.Model != "ollama/gemma4" || local.Params.APIBase != OllamaAPIBase {
		test.Errorf("ollama params = %+v, want routed ollama/gemma4 with the in-network api_base", local.Params)
	}
	if local.Params.CredentialName != "" {
		test.Errorf("ollama model must not reference a credential, got %q", local.Params.CredentialName)
	}
}

// TestSyncModelsAppliesDiff drives SyncModels against a fake gateway: it lists the
// current models, then adds the desired-but-missing ones and deletes the
// stale-but-present ones via the live API.
func TestSyncModelsAppliesDiff(test *testing.T) {
	var added []string
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/model/info":
			// Current set: one keep, one stale.
			_, _ = writer.Write([]byte(`{"data":[
				{"model_name":"ollama/gemma4","litellm_params":{"model":"ollama/gemma4"},"model_info":{"id":"keep"}},
				{"model_name":"openai/old","litellm_params":{"model":"openai/old"},"model_info":{"id":"stale"}}
			]}`))
		case "/model/new":
			payload, _ := io.ReadAll(request.Body)
			var body map[string]any
			_ = json.Unmarshal(payload, &body)
			added = append(added, body["model_name"].(string))
			_, _ = writer.Write([]byte(`{}`))
		case "/model/delete":
			payload, _ := io.ReadAll(request.Body)
			var body map[string]any
			_ = json.Unmarshal(payload, &body)
			deleted = append(deleted, body["id"].(string))
			_, _ = writer.Write([]byte(`{}`))
		default:
			test.Errorf("unexpected %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	cat := testCatalog(test)
	manager := NewKeyManager(okProber())
	// Desired: google keyed (→ google/gemini-3.1-pro) + ollama/gemma4 (already current).
	result, err := manager.SyncModels(cat, []string{"google"}, []string{"gemma4"})
	if err != nil {
		test.Fatalf("SyncModels: %v", err)
	}
	// Added the new cloud model; ollama/gemma4 unchanged (already current).
	if strings.Join(result.Added, ",") != "google/gemini-3.1-pro" {
		test.Errorf("added = %v, want [google/gemini-3.1-pro]", result.Added)
	}
	if strings.Join(added, ",") != "google/gemini-3.1-pro" {
		test.Errorf("gateway saw adds %v, want [google/gemini-3.1-pro]", added)
	}
	// Deleted the stale openai/old by its id.
	if strings.Join(result.Deleted, ",") != "openai/old" {
		test.Errorf("deleted names = %v, want [openai/old]", result.Deleted)
	}
	if strings.Join(deleted, ",") != "stale" {
		test.Errorf("gateway saw deletes (by id) %v, want [stale]", deleted)
	}
}

// TestSyncModelsPreservesOllamaWhenListEmpty is the regression for the data-loss bug:
// a cloud-key resync called with NO ollama models (e.g. installedOllamaModels() returned
// nil because the Ollama daemon was momentarily unreachable) must NOT delete the
// ollama/* models already registered in LiteLLM — they are still installed in Ollama and
// are owned by `ai models pull`/`rm`, not by this resync. Only stale CLOUD models delete.
func TestSyncModelsPreservesOllamaWhenListEmpty(test *testing.T) {
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/model/info":
			_, _ = writer.Write([]byte(`{"data":[
				{"model_name":"ollama/gemma4","litellm_params":{"model":"ollama/gemma4"},"model_info":{"id":"ollama-keep"}},
				{"model_name":"ollama/qwen3","litellm_params":{"model":"ollama/qwen3"},"model_info":{"id":"ollama-keep-2"}},
				{"model_name":"openai/old","litellm_params":{"model":"openai/old"},"model_info":{"id":"cloud-stale"}}
			]}`))
		case "/model/delete":
			payload, _ := io.ReadAll(request.Body)
			var body map[string]any
			_ = json.Unmarshal(payload, &body)
			deleted = append(deleted, body["id"].(string))
			_, _ = writer.Write([]byte(`{}`))
		case "/model/new":
			_, _ = writer.Write([]byte(`{}`))
		default:
			test.Errorf("unexpected %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	// No keyed providers and — crucially — NO ollama models (the daemon-down case).
	result, err := manager.SyncModels(testCatalog(test), nil, nil)
	if err != nil {
		test.Fatalf("SyncModels: %v", err)
	}
	// Only the stale CLOUD model is deleted; both ollama/* registrations survive.
	if strings.Join(deleted, ",") != "cloud-stale" {
		test.Errorf("gateway saw deletes %v, want only [cloud-stale] — ollama models must be preserved", deleted)
	}
	for _, name := range result.Deleted {
		if strings.HasPrefix(name, "ollama/") {
			test.Errorf("a resync must not delete ollama model %q", name)
		}
	}
}

// TestSyncModelsPreservesDockerModelRunner verifies the local-model protection extends to
// Docker Model Runner: a cloud-key resync must NOT delete "docker-model-runner/*" models,
// which are owned by Register/UnregisterDockerModelRunnerModel — only stale CLOUD models delete.
func TestSyncModelsPreservesDockerModelRunner(test *testing.T) {
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/model/info":
			_, _ = writer.Write([]byte(`{"data":[
				{"model_name":"docker-model-runner/ai/smollm2","litellm_params":{"model":"openai/ai/smollm2"},"model_info":{"id":"dmr-keep"}},
				{"model_name":"openai/old","litellm_params":{"model":"openai/old"},"model_info":{"id":"cloud-stale"}}
			]}`))
		case "/model/delete":
			payload, _ := io.ReadAll(request.Body)
			var body map[string]any
			_ = json.Unmarshal(payload, &body)
			deleted = append(deleted, body["id"].(string))
			_, _ = writer.Write([]byte(`{}`))
		case "/model/new":
			_, _ = writer.Write([]byte(`{}`))
		default:
			test.Errorf("unexpected %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	result, err := manager.SyncModels(testCatalog(test), nil, nil)
	if err != nil {
		test.Fatalf("SyncModels: %v", err)
	}
	if strings.Join(deleted, ",") != "cloud-stale" {
		test.Errorf("gateway saw deletes %v, want only [cloud-stale] — DMR models must be preserved", deleted)
	}
	for _, name := range result.Deleted {
		if strings.HasPrefix(name, "docker-model-runner/") {
			test.Errorf("a resync must not delete DMR model %q", name)
		}
	}
}
