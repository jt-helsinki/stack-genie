package hf

import (
	"errors"
	"strings"
	"testing"
)

// fakeAPIBody is a small Hub /api/models response: two servable text-generation
// repos (one gated) plus an embeddings repo the filter must drop.
const fakeAPIBody = `[
  {"id":"mlx-community/Qwen3-8B-4bit","downloads":1500000,"gated":false,"pipeline_tag":"text-generation"},
  {"id":"mlx-community/bge-small-en-embed","downloads":900000,"pipeline_tag":"feature-extraction"},
  {"id":"mlx-community/Llama-3-8B-Instruct-4bit","downloads":2000000,"gated":"manual","pipeline_tag":"text-generation"}
]`

// swapAPIGet installs a fake HTTP seam and returns a restore func.
func swapAPIGet(fake func(string) ([]byte, error)) func() {
	prev := hfAPIGet
	hfAPIGet = fake
	return func() { hfAPIGet = prev }
}

// The fetch hits the per-OS URL and parses+filters the response into CuratedModels.
func TestFetchCuratedPerOSURLAndParse(test *testing.T) {
	cases := []struct {
		goos    string
		wantURL string
	}{
		{"darwin", "author=mlx-community"},
		{"linux", "sort=trendingScore"},
	}
	for _, testCase := range cases {
		var gotURL string
		restore := swapAPIGet(func(url string) ([]byte, error) {
			gotURL = url
			return []byte(fakeAPIBody), nil
		})
		models, err := fetchCurated(testCase.goos)
		restore()
		if err != nil {
			test.Fatalf("[%s] fetchCurated: %v", testCase.goos, err)
		}
		if !strings.Contains(gotURL, testCase.wantURL) {
			test.Fatalf("[%s] URL %q missing %q", testCase.goos, gotURL, testCase.wantURL)
		}
		// The embeddings repo is filtered out; the two text-gen repos remain.
		if len(models) != 2 {
			test.Fatalf("[%s] models = %d, want 2 (embeddings dropped)", testCase.goos, len(models))
		}
		var gated CuratedModel
		for _, model := range models {
			if model.Repo == "mlx-community/Llama-3-8B-Instruct-4bit" {
				gated = model
			}
		}
		if gated.Name != "Llama-3-8B-Instruct-4bit" {
			test.Fatalf("[%s] name = %q, want base repo name", testCase.goos, gated.Name)
		}
		if !strings.Contains(gated.Description, "gated") {
			test.Fatalf("[%s] gated repo description = %q, want a gated note", testCase.goos, gated.Description)
		}
	}
}

// A live refresh writes the cache; a subsequent no-network read returns it.
func TestLoadOrFetchCuratedFreshWritesCache(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	restore := swapAPIGet(func(string) ([]byte, error) { return []byte(fakeAPIBody), nil })
	models, source, err := LoadOrFetchCurated("darwin", true)
	restore()
	if err != nil {
		test.Fatalf("refresh: %v", err)
	}
	if source != SourceFresh {
		test.Fatalf("source = %v, want SourceFresh", source)
	}
	if len(models) != 2 {
		test.Fatalf("models = %d, want 2", len(models))
	}
	if _, ok := CachedCuratedInfo("darwin"); !ok {
		test.Fatal("expected a cache stamp after a fresh fetch")
	}
	// A plain read with the network down now returns the cached copy.
	restoreDown := swapAPIGet(func(string) ([]byte, error) { return nil, errors.New("offline") })
	defer restoreDown()
	cached, cachedSource, err := LoadOrFetchCurated("darwin", false)
	if err != nil {
		test.Fatalf("cached read: %v", err)
	}
	if cachedSource != SourceCached {
		test.Fatalf("source = %v, want SourceCached", cachedSource)
	}
	if len(cached) != 2 {
		test.Fatalf("cached models = %d, want 2", len(cached))
	}
}

// A read with no cache and no refresh returns the compiled-in built-in list.
func TestLoadOrFetchCuratedNoCacheReturnsBuiltin(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	models, source, err := LoadOrFetchCurated("linux", false)
	if err != nil {
		test.Fatalf("read: %v", err)
	}
	if source != SourceBuiltin {
		test.Fatalf("source = %v, want SourceBuiltin", source)
	}
	if len(models) != len(CuratedModels("linux")) {
		test.Fatalf("models = %d, want the built-in list (%d)", len(models), len(CuratedModels("linux")))
	}
}

// A refresh whose fetch fails degrades to the cache when present, else the
// built-in list.
func TestLoadOrFetchCuratedRefreshFailureDegrades(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	// No cache yet + a failing fetch → built-in.
	restore := swapAPIGet(func(string) ([]byte, error) { return nil, errors.New("boom") })
	_, source, err := LoadOrFetchCurated("darwin", true)
	restore()
	if err != nil {
		test.Fatalf("failed refresh should not error: %v", err)
	}
	if source != SourceBuiltin {
		test.Fatalf("source = %v, want SourceBuiltin (no cache)", source)
	}

	// Seed the cache with a good refresh.
	restore = swapAPIGet(func(string) ([]byte, error) { return []byte(fakeAPIBody), nil })
	_, seeded, _ := LoadOrFetchCurated("darwin", true)
	restore()
	if seeded != SourceFresh {
		test.Fatalf("seed source = %v, want SourceFresh", seeded)
	}

	// A failing refresh now degrades to the cached copy, not the built-in list.
	restore = swapAPIGet(func(string) ([]byte, error) { return nil, errors.New("boom") })
	defer restore()
	cached, degraded, _ := LoadOrFetchCurated("darwin", true)
	if degraded != SourceCached {
		test.Fatalf("degraded source = %v, want SourceCached", degraded)
	}
	if len(cached) != 2 {
		test.Fatalf("cached models = %d, want 2", len(cached))
	}
}
