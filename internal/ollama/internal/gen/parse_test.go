package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readFixture loads a testdata file or fails the test.
func readFixture(test *testing.T, name string) []byte {
	test.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		test.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func TestParseLibraryHTML(test *testing.T) {
	models := parseLibraryHTML(string(readFixture(test, "library.html")))
	if len(models) == 0 {
		test.Fatal("parseLibraryHTML returned no models")
	}

	// Page order is preserved (the library page is popularity-sorted): llama3.1 first.
	first := models[0]
	if first.Name != "llama3.1" {
		test.Fatalf("models[0].Name = %q, want llama3.1", first.Name)
	}
	wantParams := []string{"8b", "70b", "405b"}
	if len(first.Parameters) != len(wantParams) {
		test.Fatalf("llama3.1 params = %v, want %v", first.Parameters, wantParams)
	}
	for index, want := range wantParams {
		if first.Parameters[index] != want {
			test.Fatalf("llama3.1 params[%d] = %q, want %q", index, first.Parameters[index], want)
		}
	}
	if first.RepoURL != "https://ollama.com/library/llama3.1" {
		test.Fatalf("llama3.1 RepoURL = %q", first.RepoURL)
	}
	if first.PullCount == "" {
		test.Fatal("llama3.1 PullCount is empty, want a pull-count string")
	}

	// A model with no size variants (e.g. an embedding model) still parses, with
	// empty Parameters.
	var nomic *scrapedModel
	for index := range models {
		if models[index].Name == "nomic-embed-text" {
			nomic = &models[index]
		}
	}
	if nomic == nil {
		test.Fatal("expected nomic-embed-text in the fixture")
	}
	if len(nomic.Parameters) != 0 {
		test.Fatalf("nomic-embed-text params = %v, want none", nomic.Parameters)
	}

	// Names are unique.
	seen := map[string]bool{}
	for _, model := range models {
		if seen[model.Name] {
			test.Fatalf("duplicate model %q", model.Name)
		}
		seen[model.Name] = true
	}
}

func TestParseLibraryHTMLEmptyOnGarbage(test *testing.T) {
	if got := parseLibraryHTML("<html><body>nothing here</body></html>"); len(got) != 0 {
		test.Fatalf("parseLibraryHTML on garbage returned %d models, want 0", len(got))
	}
}

func TestSumManifestLayers(test *testing.T) {
	total, err := sumManifestLayers(strings.NewReader(string(readFixture(test, "manifest.json"))))
	if err != nil {
		test.Fatalf("sumManifestLayers: %v", err)
	}
	const want = int64(9608338848 + 11355 + 42)
	if total != want {
		test.Fatalf("sumManifestLayers = %d, want %d", total, want)
	}
}

func TestSumManifestLayersBadJSON(test *testing.T) {
	if _, err := sumManifestLayers(strings.NewReader("not json")); err == nil {
		test.Fatal("expected an error on malformed manifest JSON")
	}
}
