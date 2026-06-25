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
	variants := parseLibraryHTML(string(readFixture(test, "library.html")))
	if len(variants) == 0 {
		test.Fatal("parseLibraryHTML returned no variants")
	}

	// The parser EXPANDS each model into one entry per size variant, in page order
	// (the library page is popularity-sorted): llama3.1's three sizes come first.
	wantFirst := []scrapedModel{
		{Name: "llama3.1:8b", Parameters: "8b", SizeTag: "8b", Model: "llama3.1"},
		{Name: "llama3.1:70b", Parameters: "70b", SizeTag: "70b", Model: "llama3.1"},
		{Name: "llama3.1:405b", Parameters: "405b", SizeTag: "405b", Model: "llama3.1"},
	}
	for index, want := range wantFirst {
		got := variants[index]
		if got.Name != want.Name || got.Parameters != want.Parameters ||
			got.SizeTag != want.SizeTag || got.Model != want.Model {
			test.Fatalf("variants[%d] = %+v, want %+v", index, got, want)
		}
		if got.RepoURL != "https://ollama.com/library/llama3.1" {
			test.Fatalf("variants[%d] RepoURL = %q", index, got.RepoURL)
		}
		if got.PullCount == "" {
			test.Fatalf("variants[%d] PullCount is empty, want a pull-count string", index)
		}
	}

	// A model with no size variants (e.g. an embedding model) yields a single bare
	// entry: Name == model name, empty Parameters, SizeTag "latest".
	var nomic *scrapedModel
	count := 0
	for index := range variants {
		if variants[index].Model == "nomic-embed-text" {
			nomic = &variants[index]
			count++
		}
	}
	if nomic == nil {
		test.Fatal("expected nomic-embed-text in the fixture")
	}
	if count != 1 {
		test.Fatalf("nomic-embed-text expanded into %d entries, want 1 (no variants)", count)
	}
	if nomic.Name != "nomic-embed-text" || nomic.Parameters != "" || nomic.SizeTag != "latest" {
		test.Fatalf("nomic-embed-text entry = %+v, want a bare latest entry", *nomic)
	}

	// Entry names (the pullable refs) are unique.
	seen := map[string]bool{}
	for _, variant := range variants {
		if seen[variant.Name] {
			test.Fatalf("duplicate entry %q", variant.Name)
		}
		seen[variant.Name] = true
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
