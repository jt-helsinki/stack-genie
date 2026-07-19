package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// indexFixture mirrors the CURRENT ollama.com/library index structure: each model is a
// card anchor `href="/library/<name>"` (ollama.com REMOVED the old
// `x-test-model-title`/`x-test-model` markers — see the regression this guards) whose card
// carries the name in the title div and a description <p …text-md>. qwen2.5 is listed
// before llama3.2 to exercise the by-name sort. A trailing tag link (`/library/<name>:tag`)
// and a sub-path (`/tags`) are included to prove the anchor regex does NOT treat those as
// models.
const indexFixture = `<ul role="list">
  <li class="flex">
    <a href="/library/qwen2.5" class="group w-full space-y-5">
      <div title="qwen2.5" class="flex flex-col">
        <h2 class="truncate text-xl"><div class="flex space-x-2 items-center"><span class="group-hover:underline truncate">qwen2.5</span></div></h2>
        <p class="max-w-lg break-words text-neutral-800 text-md">Qwen 2.5 models.</p>
      </div>
      <span class="text-xs">7b</span>
    </a>
  </li>
  <li class="flex">
    <a href="/library/llama3.2" class="group w-full space-y-5">
      <div title="llama3.2" class="flex flex-col">
        <h2 class="truncate text-xl"><div class="flex space-x-2 items-center"><span class="group-hover:underline truncate">llama3.2</span></div></h2>
        <p class="max-w-lg break-words text-neutral-800 text-md">Llama 3.2 &amp; friends.</p>
      </div>
      <a href="/library/llama3.2:3b" class="tag">llama3.2:3b</a>
      <a href="/library/llama3.2/tags" class="all">See all</a>
    </a>
  </li>
</ul>`

// tagRow renders one model's /tags row in BOTH the mobile (compact bullet line)
// and desktop (grid cells) layouts ollama.com emits, so tests exercise the
// duplicate-anchor de-duplication.
func tagRow(model, tag, size, context, input string) string {
	return `<div class="group px-4 py-3">
  <a href="/library/` + model + `:` + tag + `" class="md:hidden flex flex-col group">
    <span>` + model + `:` + tag + `</span>
    <span><span class="font-mono">deadbeef</span> • ` + size + ` • ` + context + ` context window • ` + input + ` input • 1 year ago</span>
  </a>
  <div class="hidden md:grid grid-cols-12">
    <a href="/library/` + model + `:` + tag + `" class="group-hover:underline">` + model + `:` + tag + `</a>
    <input class="command hidden" value="` + model + `:` + tag + `" />
    <div class="col-span-2">` + size + `</div>
    <div class="col-span-2">` + context + `</div>
    <div class="col-span-2">` + input + `</div>
  </div>
</div>`
}

// newLibraryServer stands up an httptest server routing /library to the index
// fixture and /library/<model>/tags to that model's fixture. Unknown tag pages
// 404 so a model with no fixture keeps empty tags (the graceful-degradation path).
func newLibraryServer(tags map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/library":
			_, _ = writer.Write([]byte(indexFixture))
		case strings.HasPrefix(request.URL.Path, "/library/") && strings.HasSuffix(request.URL.Path, "/tags"):
			model := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/library/"), "/tags")
			body, ok := tags[model]
			if !ok {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = writer.Write([]byte(body))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
}

// REGRESSION GUARD: ollama.com dropped the `x-test-model-title` marker the index parser
// used to key on, which silently produced ZERO models ("no models parsed") — blanking the
// Local Models installable list. The parser must enumerate models from the card anchors
// `href="/library/<name>"` and must NOT treat tag links (`:tag`) or sub-paths (`/tags`) as
// models.
func TestParseLibraryIndexUsesAnchorsNotTestMarker(test *testing.T) {
	if strings.Contains(indexFixture, "x-test-model-title") {
		test.Fatal("fixture must reflect the current ollama.com HTML (no x-test-model-title)")
	}
	models := parseLibraryIndex(indexFixture)
	names := make([]string, 0, len(models))
	byName := make(map[string]LibraryModel, len(models))
	for _, model := range models {
		names = append(names, model.Name)
		byName[model.Name] = model
	}
	if len(models) != 2 {
		test.Fatalf("parseLibraryIndex = %v, want exactly [llama3.2 qwen2.5] (tag/sub-path links excluded)", names)
	}
	if _, ok := byName["qwen2.5"]; !ok {
		test.Errorf("qwen2.5 missing from parsed models: %v", names)
	}
	if byName["llama3.2"].Description != "Llama 3.2 & friends." {
		test.Errorf("llama3.2 description = %q, want the card <p text-md> text", byName["llama3.2"].Description)
	}
	for _, bad := range []string{"llama3.2:3b", "llama3.2/tags"} {
		if _, ok := byName[bad]; ok {
			test.Errorf("%q is a tag/sub-path link, not a model — must not be parsed as one", bad)
		}
	}
}

func TestFetchLibraryParsesIndexAndTags(test *testing.T) {
	server := newLibraryServer(map[string]string{
		"llama3.2": tagRow("llama3.2", "latest", "2.0GB", "128K", "Text") +
			tagRow("llama3.2", "1b", "1.3GB", "128K", "Text"),
		"qwen2.5": tagRow("qwen2.5", "7b", "4.7GB", "32K", "Text") +
			tagRow("qwen2.5", "72b", "47GB", "32K", "Text"),
	})
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
	if models[0].Description != "Llama 3.2 & friends." {
		test.Errorf("description = %q, want unescaped", models[0].Description)
	}
	for _, model := range models {
		want := "https://ollama.com/library/" + model.Name
		if model.RepoURL != want {
			test.Errorf("RepoURL = %q, want %q", model.RepoURL, want)
		}
	}
	// Tags de-duplicated across mobile+desktop rows, fields populated, order kept.
	llama := models[0]
	if len(llama.Tags) != 2 {
		test.Fatalf("llama3.2 tags = %v, want 2 (deduped)", llama.Tags)
	}
	first := llama.Tags[0]
	if first.Name != "latest" || first.Size != "2.0GB" || first.Context != "128K" || first.Input != "Text" {
		test.Errorf("first tag = %+v, want {latest 2.0GB 128K Text}", first)
	}
	if got := llama.TagNames(); len(got) != 2 || got[0] != "latest" || got[1] != "1b" {
		test.Errorf("TagNames = %v, want [latest 1b]", got)
	}
}

func TestFetchLibraryTagPageFailureKeepsModel(test *testing.T) {
	// Only qwen2.5 has a tag fixture; llama3.2's /tags 404s.
	server := newLibraryServer(map[string]string{
		"qwen2.5": tagRow("qwen2.5", "7b", "4.7GB", "32K", "Text"),
	})
	defer server.Close()

	models, err := FetchLibrary(context.Background(), server.Client(), server.URL)
	if err != nil {
		test.Fatalf("FetchLibrary: %v", err)
	}
	if len(models) != 2 {
		test.Fatalf("got %d models, want 2", len(models))
	}
	if len(models[0].Tags) != 0 { // llama3.2 kept, tags empty
		test.Errorf("llama3.2 tags = %v, want empty on tag-page failure", models[0].Tags)
	}
	if len(models[1].Tags) != 1 {
		test.Errorf("qwen2.5 tags = %v, want 1", models[1].Tags)
	}
}

func TestParseTagsMultimodalAndEmbedding(test *testing.T) {
	// A vision variant reports "Text, Image"; an embedding variant reports no
	// input/context cells.
	vision := tagRow("llava", "latest", "4.7GB", "4K", "Text") // base line…
	// …plus a desktop row that lists two modalities.
	vision += `<a href="/library/llava:13b" class="x">llava:13b</a>
	<div>4.7GB</div><div>4K</div><div>Text</div><div>Image</div>`
	tags := parseTags(vision, "llava")
	if len(tags) != 2 {
		test.Fatalf("got %d tags, want 2", len(tags))
	}
	if tags[1].Input != "Text, Image" {
		test.Errorf("multimodal input = %q, want %q", tags[1].Input, "Text, Image")
	}

	embed := `<a href="/library/nomic-embed-text:latest" class="x">nomic-embed-text:latest</a>
	<div>274MB</div>`
	et := parseTags(embed, "nomic-embed-text")
	if len(et) != 1 || et[0].Size != "274MB" || et[0].Context != "" || et[0].Input != "" {
		test.Errorf("embedding tag = %+v, want {latest 274MB  }", et)
	}
}

func TestLoadOrFetchLibraryFreshPersistsYAMLCache(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	server := newLibraryServer(map[string]string{
		"llama3.2": tagRow("llama3.2", "1b", "1.3GB", "128K", "Text"),
		"qwen2.5":  tagRow("qwen2.5", "7b", "4.7GB", "32K", "Text"),
	})
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

	path := filepath.Join(home, ".ai-platform", "cache", "ollama-models.yaml")
	if _, statErr := os.Stat(path); statErr != nil {
		test.Fatalf("YAML cache not written at %s: %v", path, statErr)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "name: llama3.2") || !strings.Contains(string(data), "context: 128K") {
		test.Errorf("cache YAML missing expected fields:\n%s", data)
	}
}

func TestSaveLoadLibraryYAMLRoundTrip(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	want := []LibraryModel{{
		Name:        "llama3.2",
		Description: "Llama 3.2",
		Tags:        []LibraryTag{{Name: "1b", Size: "1.3GB", Context: "128K", Input: "Text"}},
	}}
	if err := SaveLibrary(want); err != nil {
		test.Fatalf("SaveLibrary: %v", err)
	}
	got, err := LoadLibrary()
	if err != nil {
		test.Fatalf("LoadLibrary: %v", err)
	}
	if len(got) != 1 || got[0].Name != "llama3.2" || len(got[0].Tags) != 1 {
		test.Fatalf("round-trip = %+v", got)
	}
	if got[0].Tags[0].Context != "128K" || got[0].Tags[0].Size != "1.3GB" {
		test.Errorf("tag detail lost: %+v", got[0].Tags[0])
	}
	// RepoURL is re-derived on load even though it isn't persisted.
	if got[0].RepoURL != "https://ollama.com/library/llama3.2" {
		test.Errorf("RepoURL = %q, want derived", got[0].RepoURL)
	}
}

func TestLoadOrFetchLibraryCachedOnLiveFail(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	if err := SaveLibrary([]LibraryModel{{Name: "qwen2.5", Description: "cached", Tags: []LibraryTag{{Name: "7b"}}}}); err != nil {
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

// TestLoadCachedOrFetchLibraryUsesCacheWithoutNetwork proves the cache-first path
// returns the cached copy WITHOUT contacting the network (the base URL is
// unreachable — if it were used the call would error).
func TestLoadCachedOrFetchLibraryUsesCacheWithoutNetwork(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if err := SaveLibrary([]LibraryModel{{Name: "cached-model", Tags: []LibraryTag{{Name: "1b"}}}}); err != nil {
		test.Fatalf("SaveLibrary: %v", err)
	}
	models, source, err := LoadCachedOrFetchLibrary(context.Background(), http.DefaultClient, "http://127.0.0.1:0/")
	if err != nil {
		test.Fatalf("cache-first must not error when a cache exists: %v", err)
	}
	if source != SourceCached {
		test.Errorf("source = %v, want cached", source)
	}
	if len(models) != 1 || models[0].Name != "cached-model" {
		test.Fatalf("cache-first must return the cached copy, got %+v", models)
	}
}
