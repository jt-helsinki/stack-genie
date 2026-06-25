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

// TestParseParamSize covers the size-label parser that drives the within-model sort:
// m/b suffixes scale to parameter magnitudes so 270m < 1b < 3b < 70b, decimals work,
// and an unparseable/empty label yields 0 (sorts first).
func TestParseParamSize(test *testing.T) {
	// Ascending order the parser must produce.
	ascending := []string{"", "270m", "0.5b", "1b", "1.5b", "3b", "8b", "70b", "405b", "671b"}
	previous := -1.0
	for _, label := range ascending {
		got := parseParamSize(label)
		if got < previous {
			test.Fatalf("parseParamSize(%q) = %g, not >= previous %g (order broken)", label, got, previous)
		}
		previous = got
	}

	cases := map[string]float64{
		"270m":   270e6,
		"1b":     1e9,
		"3b":     3e9,
		"70b":    70e9,
		"1.5b":   1.5e9,
		"":       0,
		"latest": 0,
	}
	for label, want := range cases {
		if got := parseParamSize(label); got != want {
			test.Fatalf("parseParamSize(%q) = %g, want %g", label, got, want)
		}
	}
	// The canonical ladder the spec calls out.
	if !(parseParamSize("270m") < parseParamSize("1b") &&
		parseParamSize("1b") < parseParamSize("3b") &&
		parseParamSize("3b") < parseParamSize("70b")) {
		test.Fatal("270m < 1b < 3b < 70b must hold")
	}
}

// TestSortVariants asserts the final ordering: alphabetical by model name
// (case-insensitive), then ascending by parameter size within a model, with bare
// (no-variant) entries sorting by name.
func TestSortVariants(test *testing.T) {
	input := []scrapedModel{
		{Name: "qwen2.5:70b", Parameters: "70b", Model: "qwen2.5"},
		{Name: "qwen2.5:7b", Parameters: "7b", Model: "qwen2.5"},
		{Name: "Llama3:8b", Parameters: "8b", Model: "Llama3"}, // uppercase → case-insensitive
		{Name: "llama3:270m", Parameters: "270m", Model: "llama3"},
		{Name: "nomic-embed-text", Parameters: "", Model: "nomic-embed-text"},
		{Name: "qwen2.5:1.5b", Parameters: "1.5b", Model: "qwen2.5"},
	}
	got := sortVariants(input)
	// "llama3" < "nomic-embed-text" < "qwen2.5" (case-insensitive). Within the llama3
	// group, ascending by size: 270m < 8b → [llama3:270m, Llama3:8b]. Within qwen2.5:
	// 1.5b < 7b < 70b.
	want := []string{
		"llama3:270m",
		"Llama3:8b",
		"nomic-embed-text",
		"qwen2.5:1.5b",
		"qwen2.5:7b",
		"qwen2.5:70b",
	}
	if len(got) != len(want) {
		test.Fatalf("sortVariants returned %d entries, want %d", len(got), len(want))
	}
	for index, wantName := range want {
		if got[index].Name != wantName {
			test.Fatalf("sortVariants[%d] = %q, want %q (full: %v)", index, got[index].Name, wantName, names(got))
		}
	}
}

// TestParseLibraryHTMLSorted confirms the parsed fixture, once sorted, is in
// alpha-then-params order end to end.
func TestParseLibraryHTMLSorted(test *testing.T) {
	variants := sortVariants(parseLibraryHTML(string(readFixture(test, "library.html"))))
	for index := 1; index < len(variants); index++ {
		prevModel := strings.ToLower(variants[index-1].Model)
		curModel := strings.ToLower(variants[index].Model)
		if curModel < prevModel {
			test.Fatalf("not alpha-sorted at %d: %q after %q", index, curModel, prevModel)
		}
		if curModel == prevModel {
			prevSize := parseParamSize(variants[index-1].Parameters)
			curSize := parseParamSize(variants[index].Parameters)
			if curSize < prevSize {
				test.Fatalf("within %q not size-ascending at %d: %g after %g",
					curModel, index, curSize, prevSize)
			}
		}
	}
}

func names(variants []scrapedModel) []string {
	out := make([]string, len(variants))
	for index, variant := range variants {
		out[index] = variant.Name
	}
	return out
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
