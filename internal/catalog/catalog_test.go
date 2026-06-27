package catalog_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/catalog"
)

func readFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "catalog.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func TestParseProvidersAndModelFields(t *testing.T) {
	parsed, err := catalog.Parse(readFixture(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Providers are grouped by id prefix and sorted ascending.
	providers := parsed.Providers()
	gotIDs := make([]string, 0, len(providers))
	for _, provider := range providers {
		gotIDs = append(gotIDs, provider.ID)
	}
	wantIDs := []string{"anthropic", "google", "meta", "openai"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("provider ids = %v, want %v", gotIDs, wantIDs)
	}

	// Provider name + env enriched from the providers map.
	openai, ok := parsed.Provider("openai")
	if !ok {
		t.Fatal("openai provider missing")
	}
	if openai.Name != "OpenAI" {
		t.Errorf("openai.Name = %q, want %q", openai.Name, "OpenAI")
	}
	if !reflect.DeepEqual(openai.Env, []string{"OPENAI_API_KEY"}) {
		t.Errorf("openai.Env = %v, want [OPENAI_API_KEY]", openai.Env)
	}

	// meta is absent from the providers map → Name falls back to id, Env empty.
	meta, ok := parsed.Provider("meta")
	if !ok {
		t.Fatal("meta provider missing")
	}
	if meta.Name != "meta" {
		t.Errorf("meta.Name = %q, want fallback %q", meta.Name, "meta")
	}
	if len(meta.Env) != 0 {
		t.Errorf("meta.Env = %v, want empty", meta.Env)
	}

	// Known model: verify fields verbatim.
	var codex catalog.Model
	found := false
	for _, model := range openai.Models {
		if model.ID == "openai/gpt-5.2-codex" {
			codex = model
			found = true
		}
	}
	if !found {
		t.Fatal("openai/gpt-5.2-codex not found")
	}
	if codex.ID != "openai/gpt-5.2-codex" {
		t.Errorf("id = %q, want verbatim openai/gpt-5.2-codex", codex.ID)
	}
	if codex.Name != "GPT-5.2 Codex" {
		t.Errorf("name = %q", codex.Name)
	}
	if codex.Family != "gpt-codex" {
		t.Errorf("family = %q", codex.Family)
	}
	if codex.ReleaseDate != "2025-12-11" || codex.LastUpdated != "2025-12-11" {
		t.Errorf("dates = %q / %q", codex.ReleaseDate, codex.LastUpdated)
	}
	if codex.Knowledge != "2025-08-31" {
		t.Errorf("knowledge = %q", codex.Knowledge)
	}
	if codex.Limit.Context != 400000 || codex.Limit.Output != 128000 || codex.Limit.Input != 272000 {
		t.Errorf("limits = %+v", codex.Limit)
	}
	if !reflect.DeepEqual(codex.Modalities.Input, []string{"text", "image", "pdf"}) {
		t.Errorf("modalities.input = %v", codex.Modalities.Input)
	}
	if !reflect.DeepEqual(codex.Modalities.Output, []string{"text"}) {
		t.Errorf("modalities.output = %v", codex.Modalities.Output)
	}
	if !codex.Reasoning || !codex.ToolCall || !codex.Attachment || !codex.StructuredOutput {
		t.Errorf("bool flags wrong: reasoning=%v tool=%v attach=%v structured=%v",
			codex.Reasoning, codex.ToolCall, codex.Attachment, codex.StructuredOutput)
	}
	if codex.Temperature {
		t.Errorf("temperature = true, want false")
	}
	if codex.OpenWeights {
		t.Errorf("open_weights = true, want false")
	}
}

func TestGroupByProviderCorrectness(t *testing.T) {
	parsed, err := catalog.Parse(readFixture(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, provider := range parsed.Providers() {
		if len(provider.Models) == 0 {
			t.Errorf("provider %q has no models", provider.ID)
		}
		for _, model := range provider.Models {
			prefix := provider.ID + "/"
			if len(model.ID) <= len(prefix) || model.ID[:len(prefix)] != prefix {
				t.Errorf("model %q grouped under wrong provider %q", model.ID, provider.ID)
			}
		}
	}
	// Flat Models() returns all four, sorted by id.
	models := parsed.Models()
	if len(models) != 4 {
		t.Fatalf("Models() len = %d, want 4", len(models))
	}
	want := []string{
		"anthropic/claude-opus-4-5",
		"google/gemini-2.5-pro-tts",
		"meta/llama-4-scout-17b-instruct",
		"openai/gpt-5.2-codex",
	}
	for i, model := range models {
		if model.ID != want[i] {
			t.Errorf("Models()[%d].ID = %q, want %q", i, model.ID, want[i])
		}
	}
}

func TestPathUnderCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := catalog.Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	want := filepath.Join(home, ".ai-platform", "cache", "catalog.json")
	if path != want {
		t.Errorf("Path() = %q, want %q", path, want)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	raw := readFixture(t)
	parsed, err := catalog.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := catalog.Save(parsed); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Save writes the RAW bytes byte-for-byte.
	path, _ := catalog.Path()
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved: %v", err)
	}
	if !reflect.DeepEqual(onDisk, raw) {
		t.Error("saved bytes != raw fixture bytes")
	}

	// Load re-parses to an equivalent catalog.
	loaded, err := catalog.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(loaded.Models()); got != len(parsed.Models()) {
		t.Errorf("loaded models = %d, want %d", got, len(parsed.Models()))
	}
	if !reflect.DeepEqual(loaded.Providers(), parsed.Providers()) {
		t.Error("loaded providers != parsed providers")
	}
}

// roundTripErr is an http.RoundTripper that always fails — simulates offline.
type roundTripErr struct{}

func (roundTripErr) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network down")
}

func failingClient() *http.Client {
	return &http.Client{Transport: roundTripErr{}}
}

func TestLoadOrFetchOfflineFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	raw := readFixture(t)

	// Seed a saved copy on disk.
	parsed, err := catalog.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := catalog.Save(parsed); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Failing client + present saved copy → returns saved catalog, no error.
	got, err := catalog.LoadOrFetch(context.Background(), failingClient(), catalog.DefaultURL)
	if err != nil {
		t.Fatalf("LoadOrFetch (offline w/ copy): %v", err)
	}
	if len(got.Models()) != len(parsed.Models()) {
		t.Errorf("fallback models = %d, want %d", len(got.Models()), len(parsed.Models()))
	}
}

func TestLoadOrFetchNoNetworkNoCopy(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no saved copy

	_, err := catalog.LoadOrFetch(context.Background(), failingClient(), catalog.DefaultURL)
	if err == nil {
		t.Fatal("expected error when neither fetch nor saved copy works")
	}
}

func TestFetchParsesServedBody(t *testing.T) {
	raw := readFixture(t)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return newJSONResponse(http.StatusOK, raw), nil
	})}
	got, err := catalog.Fetch(context.Background(), client, catalog.DefaultURL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got.Models()) != 4 {
		t.Errorf("fetched models = %d, want 4", len(got.Models()))
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	if _, err := catalog.Parse([]byte(`{"providers":{}}`)); err == nil {
		t.Error("expected error for catalog with no models")
	}
	if _, err := catalog.Parse([]byte(`not json`)); err == nil {
		t.Error("expected error for invalid json")
	}
}
