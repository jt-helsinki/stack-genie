package catalog_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/catalog"
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
	want := filepath.Join(home, ".ai-platform", "cache", "catalog.yaml")
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

	// Save writes YAML (converted from the upstream JSON), not the raw bytes.
	path, _ := catalog.Path()
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved: %v", err)
	}
	if !strings.HasSuffix(path, ".yaml") {
		t.Errorf("cache path = %q, want a .yaml file", path)
	}
	if len(onDisk) == 0 || !strings.Contains(string(onDisk), "models:") {
		t.Errorf("saved catalog is not the expected YAML:\n%.200s", onDisk)
	}

	// Load re-parses to an equivalent catalog.
	loaded, err := catalog.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The JSON→YAML→parse round-trip preserves the model/provider set and the
	// meaningful per-model fields. (A raw DeepEqual is intentionally avoided: YAML
	// omitempty can turn an empty slice into nil, an irrelevant representational
	// difference across the format boundary.)
	if got := len(loaded.Models()); got != len(parsed.Models()) {
		t.Errorf("loaded models = %d, want %d", got, len(parsed.Models()))
	}
	loadedProviders := loaded.Providers()
	parsedProviders := parsed.Providers()
	if len(loadedProviders) != len(parsedProviders) {
		t.Fatalf("loaded providers = %d, want %d", len(loadedProviders), len(parsedProviders))
	}
	for index := range parsedProviders {
		if loadedProviders[index].ID != parsedProviders[index].ID ||
			loadedProviders[index].Name != parsedProviders[index].Name ||
			len(loadedProviders[index].Models) != len(parsedProviders[index].Models) {
			t.Errorf("provider %d mismatch: %+v vs %+v", index, loadedProviders[index], parsedProviders[index])
		}
	}
	// Spot-check a model's fields survive the round-trip.
	byID := func(models []catalog.Model, id string) (catalog.Model, bool) {
		for _, model := range models {
			if model.ID == id {
				return model, true
			}
		}
		return catalog.Model{}, false
	}
	for _, want := range parsed.Models() {
		got, ok := byID(loaded.Models(), want.ID)
		if !ok {
			t.Errorf("model %q missing after round-trip", want.ID)
			continue
		}
		if got.Name != want.Name || got.Limit.Context != want.Limit.Context || got.Reasoning != want.Reasoning {
			t.Errorf("model %q fields changed: got %+v want %+v", want.ID, got, want)
		}
	}
}

func TestMigrateLegacyJSONToYAML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Seed a legacy cache/catalog.json (the pre-YAML format).
	cacheDir := filepath.Join(home, ".ai-platform", "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	legacy := filepath.Join(cacheDir, "catalog.json")
	if err := os.WriteFile(legacy, readFixture(t), 0o644); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	// Load triggers Path()'s lazy migration: the JSON is converted to catalog.yaml
	// and the legacy JSON removed.
	loaded, err := catalog.Load()
	if err != nil {
		t.Fatalf("Load (after seeding legacy json): %v", err)
	}
	if len(loaded.Models()) == 0 {
		t.Fatal("migrated catalog has no models")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "catalog.yaml")); err != nil {
		t.Errorf("catalog.yaml not created by migration: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy catalog.json not removed (stat err = %v)", err)
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

// TestLoadCachedOrFetchStatusUsesCacheWithoutNetwork proves the cache-first path
// returns the cached catalog WITHOUT calling the network (the client always fails;
// if it were used the call would error).
func TestLoadCachedOrFetchStatusUsesCacheWithoutNetwork(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	parsed, err := catalog.Parse(readFixture(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := catalog.Save(parsed); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, source, err := catalog.LoadCachedOrFetchStatus(context.Background(), failingClient(), catalog.DefaultURL)
	if err != nil {
		t.Fatalf("cache-first must not error with a cache present: %v", err)
	}
	if source != catalog.SourceCached {
		t.Errorf("source = %v, want cached", source)
	}
	if len(got.Models()) != len(parsed.Models()) {
		t.Errorf("cached models = %d, want %d", len(got.Models()), len(parsed.Models()))
	}
}

// build() derives a model's ID (and its provider group) from the map key when
// the entry omits an explicit "id" field — the map key is the verbatim
// "<provider>/<model>" id in that case.
func TestParseModelIDFallsBackToMapKey(t *testing.T) {
	// cohere/command-r has NO "id" field; its ID must come from the map key and
	// it must group under the "cohere" provider.
	raw := []byte(`{
		"models": {
			"cohere/command-r": {"name": "Command R", "family": "command"}
		},
		"providers": {
			"cohere": {"id": "cohere", "name": "Cohere", "env": ["COHERE_API_KEY"]}
		}
	}`)
	parsed, err := catalog.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	provider, ok := parsed.Provider("cohere")
	if !ok {
		t.Fatal("cohere provider missing — id was not derived from the map key")
	}
	if len(provider.Models) != 1 {
		t.Fatalf("cohere models = %d, want 1", len(provider.Models))
	}
	if provider.Models[0].ID != "cohere/command-r" {
		t.Errorf("model id = %q, want the map key %q", provider.Models[0].ID, "cohere/command-r")
	}
	if provider.Name != "Cohere" {
		t.Errorf("provider name = %q, want enriched %q", provider.Name, "Cohere")
	}
}

// LoadOrFetchStatus (fetch-first, source-reporting) returns SourceFresh and
// persists the cache on a live fetch, then SourceCached WITH the live-fetch
// error when a later fetch fails but a cache exists — the error is surfaced so
// callers can warn while still using the cache (unlike LoadOrFetch, which
// swallows it).
func TestLoadOrFetchStatusFreshThenCachedWithError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	raw := readFixture(t)

	freshClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return newJSONResponse(http.StatusOK, raw), nil
	})}
	got, source, err := catalog.LoadOrFetchStatus(context.Background(), freshClient, catalog.DefaultURL)
	if err != nil {
		t.Fatalf("live fetch: %v", err)
	}
	if source != catalog.SourceFresh {
		t.Errorf("source = %v, want fresh", source)
	}
	if len(got.Models()) != 4 {
		t.Fatalf("fresh models = %d, want 4", len(got.Models()))
	}
	// The successful fetch must have persisted the cache.
	path, _ := catalog.Path()
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("fresh fetch did not persist cache: %v", statErr)
	}

	// Now the live fetch fails: cached copy is returned WITH the fetch error.
	cached, cachedSource, fetchErr := catalog.LoadOrFetchStatus(context.Background(), failingClient(), catalog.DefaultURL)
	if fetchErr == nil {
		t.Fatal("expected the live-fetch error to be surfaced alongside the cache")
	}
	if cachedSource != catalog.SourceCached {
		t.Errorf("source = %v, want cached", cachedSource)
	}
	if cached == nil || len(cached.Models()) != 4 {
		t.Fatalf("cached catalog not returned on fetch failure: %+v", cached)
	}
}
