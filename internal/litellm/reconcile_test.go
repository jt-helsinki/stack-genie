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
		{Name: "omlx/gemma4"},           // new → add
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
	if strings.Join(addNames, ",") != "google/gemini-3.1-pro,omlx/gemma4" {
		test.Errorf("add = %v, want the two new models sorted", addNames)
	}
	if len(plan.Delete) != 1 || plan.Delete[0].ID != "id-stale" {
		test.Errorf("delete = %+v, want [id-stale]", plan.Delete)
	}
}

// TestReconcileIdempotent verifies an equal desired/current set yields an empty
// plan (no-op on a re-run).
func TestReconcileIdempotent(test *testing.T) {
	desired := []DesiredModel{{Name: "openai/gpt-5.5"}, {Name: "omlx/gemma4"}}
	current := []LiveModel{
		{ID: "a", Name: "openai/gpt-5.5"},
		{ID: "b", Name: "omlx/gemma4"},
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
// the rewritten litellm_params.model), and that an unkeyed or unroutable provider is
// excluded. Local vLLM models are NOT part of the desired set (managed individually).
func TestDesiredModelsExpandsKeyedProviders(test *testing.T) {
	cat := testCatalog(test) // providers: openai, google (routable); megarouter (not)

	// Key google (→ gemini) only; openai stays unkeyed so its model is excluded.
	desired := DesiredModels(cat, []string{"google", "megarouter"})

	byName := map[string]DesiredModel{}
	names := make([]string, 0, len(desired))
	for _, model := range desired {
		byName[model.Name] = model
		names = append(names, model.Name)
	}
	sort.Strings(names)
	// google/gemini-3.1-pro (keyed) only; openai excluded (unkeyed), megarouter
	// excluded (unroutable — even though "keyed").
	if strings.Join(names, ",") != "google/gemini-3.1-pro" {
		test.Fatalf("desired names = %v, want the keyed-routable model", names)
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
			// Current set: one local keep (shielded), one stale cloud.
			_, _ = writer.Write([]byte(`{"data":[
				{"model_name":"omlx/my-qwen","litellm_params":{"model":"openai/my-qwen"},"model_info":{"id":"keep"}},
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
	// Desired: google keyed (→ google/gemini-3.1-pro). The omlx/my-qwen local model is
	// current but shielded from the resync's delete pass.
	result, err := manager.SyncModels(cat, []string{"google"})
	if err != nil {
		test.Fatalf("SyncModels: %v", err)
	}
	// Added the new cloud model; omlx/my-qwen unchanged (shielded).
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

// TestSyncModelsPreservesOmlx verifies the local-model protection: a cloud-key resync
// must NOT delete "omlx/*" models (owned by SyncOmlxModels — the sole
// local backend). Only stale CLOUD models delete.
// TestSyncOmlxModelsDeletesAndRecreatesAll verifies SyncOmlxModels's refresh
// semantics: unlike SyncModels' pure diff, EVERY currently-registered omlx/*
// model is deleted and re-added fresh on every run — even one whose name is
// unchanged — so a stale registration can never survive a refresh. A model that
// disappeared from omlx's live list is deleted with nothing re-added in its
// place; a cloud registration is untouched throughout.
func TestSyncOmlxModelsDeletesAndRecreatesAll(test *testing.T) {
	var deleted, added []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/model/info":
			_, _ = writer.Write([]byte(`{"data":[
				{"model_name":"omlx/gemma4","litellm_params":{"model":"openai/gemma4"},"model_info":{"id":"omlx-gemma4"}},
				{"model_name":"omlx/removed","litellm_params":{"model":"openai/removed"},"model_info":{"id":"omlx-removed"}},
				{"model_name":"openai/gpt-5.5","litellm_params":{"model":"openai/gpt-5.5"},"model_info":{"id":"cloud-keep"}}
			]}`))
		case "/model/delete":
			payload, _ := io.ReadAll(request.Body)
			var body map[string]any
			_ = json.Unmarshal(payload, &body)
			deleted = append(deleted, body["id"].(string))
			_, _ = writer.Write([]byte(`{}`))
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
	// omlx now only reports "gemma4" (unchanged) — "removed" has disappeared.
	result, err := manager.SyncOmlxModels([]string{"gemma4"}, "http://127.0.0.1:8100/v1", "")
	if err != nil {
		test.Fatalf("SyncOmlxModels: %v", err)
	}

	sort.Strings(deleted)
	if strings.Join(deleted, ",") != "omlx-gemma4,omlx-removed" {
		test.Errorf("deleted = %v, want both prior omlx registrations (unchanged + stale)", deleted)
	}
	if strings.Join(added, ",") != "omlx/gemma4" {
		test.Errorf("added = %v, want only the still-served model re-added", added)
	}
	for _, id := range deleted {
		if id == "cloud-keep" {
			test.Error("a cloud registration must never be deleted by SyncOmlxModels")
		}
	}
	if strings.Join(result.Deleted, ",") != "omlx/gemma4,omlx/removed" {
		test.Errorf("result.Deleted = %v, want both prior omlx model names", result.Deleted)
	}
	if strings.Join(result.Added, ",") != "omlx/gemma4" {
		test.Errorf("result.Added = %v, want the re-added model", result.Added)
	}
}

func TestSyncModelsPreservesOmlx(test *testing.T) {
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/model/info":
			_, _ = writer.Write([]byte(`{"data":[
				{"model_name":"omlx/my-qwen","litellm_params":{"model":"openai/Qwen/Qwen3-8B"},"model_info":{"id":"omlx-keep"}},
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
	// No keyed providers — the resync must still preserve the omlx/* registration and
	// delete only the stale cloud model.
	result, err := manager.SyncModels(testCatalog(test), nil)
	if err != nil {
		test.Fatalf("SyncModels: %v", err)
	}
	if strings.Join(deleted, ",") != "cloud-stale" {
		test.Errorf("gateway saw deletes %v, want only [cloud-stale] — local models must be preserved", deleted)
	}
	for _, name := range result.Deleted {
		if strings.HasPrefix(name, "omlx/") {
			test.Errorf("a resync must not delete vLLM model %q", name)
		}
	}
}
