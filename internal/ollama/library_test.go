package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const libraryFixture = `[
  {"name": "qwen2.5", "description": "Qwen2.5 models", "tags": ["7b", "72b"]},
  {"name": "llama3.2", "description": "Llama 3.2", "tags": ["1b", "3b"]}
]`

func TestFetchLibraryParsesAndDerivesRepoURL(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(libraryFixture))
	}))
	defer server.Close()

	models, err := FetchLibrary(context.Background(), server.Client(), server.URL)
	if err != nil {
		test.Fatalf("FetchLibrary: %v", err)
	}
	if len(models) != 2 {
		test.Fatalf("got %d models, want 2", len(models))
	}
	// Sorted by name: llama3.2 before qwen2.5.
	if models[0].Name != "llama3.2" || models[1].Name != "qwen2.5" {
		test.Fatalf("unexpected order: %q, %q", models[0].Name, models[1].Name)
	}
	for _, model := range models {
		want := "https://ollama.com/library/" + model.Name
		if model.RepoURL != want {
			test.Errorf("RepoURL = %q, want %q", model.RepoURL, want)
		}
	}
	if len(models[1].Tags) != 2 || models[1].Tags[0] != "7b" {
		test.Errorf("qwen2.5 tags = %v, want [7b 72b]", models[1].Tags)
	}
}

func TestLoadOrFetchLibraryFreshPersistsCache(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(libraryFixture))
	}))
	defer server.Close()

	models, source, err := LoadOrFetchLibrary(context.Background(), server.Client(), server.URL)
	if err != nil {
		test.Fatalf("LoadOrFetchLibrary: %v", err)
	}
	if source != SourceFresh {
		test.Errorf("source = %v, want fresh", source)
	}
	if len(models) != 2 {
		test.Fatalf("got %d models, want 2", len(models))
	}

	path := filepath.Join(home, ".ai-platform", "cache", "ollama-library.json")
	if _, statErr := os.Stat(path); statErr != nil {
		test.Errorf("cache not written at %s: %v", path, statErr)
	}
}

func TestLoadOrFetchLibraryCachedOnLiveFail(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	// Seed a cache.
	if err := SaveLibrary([]LibraryModel{{Name: "qwen2.5", Description: "cached", Tags: []string{"7b"}}}); err != nil {
		test.Fatalf("SaveLibrary: %v", err)
	}

	models, source, err := LoadOrFetchLibrary(context.Background(), http.DefaultClient, "http://127.0.0.1:0/")
	if err == nil {
		test.Fatal("expected a live-fetch error")
	}
	if source != SourceCached {
		test.Errorf("source = %v, want cached", source)
	}
	if len(models) != 1 || models[0].Name != "qwen2.5" {
		test.Fatalf("cached models = %v, want [qwen2.5]", models)
	}
	if models[0].RepoURL != "https://ollama.com/library/qwen2.5" {
		test.Errorf("RepoURL = %q, want derived", models[0].RepoURL)
	}
}

func TestLoadOrFetchLibraryEmptyOnLiveFailNoCache(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	models, source, err := LoadOrFetchLibrary(context.Background(), http.DefaultClient, "http://127.0.0.1:0/")
	if err == nil {
		test.Fatal("expected a live-fetch error")
	}
	if source != SourceCached {
		test.Errorf("source = %v, want cached", source)
	}
	if len(models) != 0 {
		test.Errorf("models = %v, want empty", models)
	}
}
