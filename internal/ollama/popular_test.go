package ollama

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// newPopularServers stands up two httptest servers: one serving the search-page
// fixture at /search, one serving the manifest fixture for /v2/library/.../manifests/...
// (every model resolves to the same fixture size). No live network is touched.
func newPopularServers(test *testing.T) (search *httptest.Server, registry *httptest.Server) {
	test.Helper()
	searchHTML := readFixture(test, "search.html")
	manifest := readFixture(test, "manifest.json")

	search = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/search" {
			http.Error(writer, "not found", http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "text/html")
		_, _ = writer.Write(searchHTML)
	}))
	test.Cleanup(search.Close)

	registry = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Path looks like /v2/library/<model>/manifests/latest.
		if filepath.Base(request.URL.Path) != "latest" {
			http.Error(writer, "not found", http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(manifest)
	}))
	test.Cleanup(registry.Close)
	return search, registry
}

func TestPopularParsesSearchAndSumsManifest(test *testing.T) {
	search, registry := newPopularServers(test)
	source := PopularSource{SearchBaseURL: search.URL, RegistryBaseURL: registry.URL}

	models, err := source.Popular()
	if err != nil {
		test.Fatalf("Popular: %v", err)
	}
	if len(models) != 3 {
		test.Fatalf("got %d models, want 3 (gemma4, lfm2.5, glm-5.2)", len(models))
	}

	// Page order is preserved: gemma4 first.
	first := models[0]
	if first.Name != "gemma4" {
		test.Fatalf("models[0].Name = %q, want gemma4", first.Name)
	}
	wantParams := []string{"e2b", "e4b", "12b", "26b", "31b"}
	if len(first.Parameters) != len(wantParams) {
		test.Fatalf("gemma4 params = %v, want %v", first.Parameters, wantParams)
	}
	for index, want := range wantParams {
		if first.Parameters[index] != want {
			test.Fatalf("gemma4 params[%d] = %q, want %q", index, first.Parameters[index], want)
		}
	}
	if first.RepoURL != "https://ollama.com/library/gemma4" {
		test.Fatalf("gemma4 RepoURL = %q", first.RepoURL)
	}
	if first.PullCount != "15.5M" {
		test.Fatalf("gemma4 PullCount = %q, want 15.5M", first.PullCount)
	}
	// 9608338848 + 11355 + 42 = 9608350245 (summed manifest layers).
	const wantSize = int64(9608338848 + 11355 + 42)
	if first.DownloadSize != wantSize {
		test.Fatalf("gemma4 DownloadSize = %d, want %d", first.DownloadSize, wantSize)
	}

	// A model with a single size variant.
	if models[1].Name != "lfm2.5" {
		test.Fatalf("models[1].Name = %q, want lfm2.5", models[1].Name)
	}
	if len(models[1].Parameters) != 1 || models[1].Parameters[0] != "8b" {
		test.Fatalf("lfm2.5 params = %v, want [8b]", models[1].Parameters)
	}

	// A cloud-only model with no size variants still parses (empty Parameters).
	if models[2].Name != "glm-5.2" {
		test.Fatalf("models[2].Name = %q, want glm-5.2", models[2].Name)
	}
	if len(models[2].Parameters) != 0 {
		test.Fatalf("glm-5.2 params = %v, want none", models[2].Parameters)
	}
}

func TestPopularDegradesWhenManifestUnavailable(test *testing.T) {
	search, _ := newPopularServers(test)
	// Registry that always 404s: sizes must degrade to 0, the list must still build.
	registry := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "not found", http.StatusNotFound)
	}))
	test.Cleanup(registry.Close)

	source := PopularSource{SearchBaseURL: search.URL, RegistryBaseURL: registry.URL}
	models, err := source.Popular()
	if err != nil {
		test.Fatalf("Popular should not fail on size lookup errors: %v", err)
	}
	if len(models) != 3 {
		test.Fatalf("got %d models, want 3", len(models))
	}
	for _, model := range models {
		if model.DownloadSize != 0 {
			test.Fatalf("%s DownloadSize = %d, want 0 (degraded)", model.Name, model.DownloadSize)
		}
	}
}

func TestPopularLimitCaps(test *testing.T) {
	search, registry := newPopularServers(test)
	source := PopularSource{SearchBaseURL: search.URL, RegistryBaseURL: registry.URL, Limit: 2}
	models, err := source.Popular()
	if err != nil {
		test.Fatalf("Popular: %v", err)
	}
	if len(models) != 2 {
		test.Fatalf("limit=2 returned %d models", len(models))
	}
}

func TestPopularSearchFetchFailureIsError(test *testing.T) {
	// Search server that 500s: building the list is impossible -> hard error.
	search := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "boom", http.StatusInternalServerError)
	}))
	test.Cleanup(search.Close)
	source := PopularSource{SearchBaseURL: search.URL, RegistryBaseURL: "http://127.0.0.1:1"}
	if _, err := source.Popular(); err == nil {
		test.Fatal("expected an error when the search fetch fails")
	}
}

func TestParseSearchHTMLEmptyOnGarbage(test *testing.T) {
	if got := parseSearchHTML("<html><body>nothing here</body></html>"); len(got) != 0 {
		test.Fatalf("parseSearchHTML on garbage returned %d models, want 0", len(got))
	}
}
