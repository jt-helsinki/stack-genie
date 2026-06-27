package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/paths"
)

// LibraryURL is the live source for the installable-Ollama library list. It
// returns a JSON array [{ "name": "...", "description": "...", "tags": ["7b", …] }]
// (no repo_url, no download size).
const LibraryURL = "https://ollama-models.zwz.workers.dev/"

// libraryFileName is the on-disk cache name under the cache dir.
const libraryFileName = "ollama-library.json"

// LibraryModel is one installable model from the Ollama library list. RepoURL is
// DERIVED (the endpoint carries none): the model's ollama.com/library page.
type LibraryModel struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	RepoURL     string   `json:"repo_url"`
}

// Source records whether a returned library came from a fresh network fetch or
// from the on-disk cache (used by callers to message source availability). It is
// intentionally independent of catalog.Source (no cross-package dependency).
type Source int

const (
	// SourceFresh means the library was fetched live and the cache was refreshed.
	SourceFresh Source = iota
	// SourceCached means the live fetch failed and a cached copy was used (or no
	// data was available at all).
	SourceCached
)

// String returns "fresh" or "cached".
func (source Source) String() string {
	if source == SourceFresh {
		return "fresh"
	}
	return "cached"
}

// repoURL derives a model's ollama.com/library page from its name. The endpoint
// has no repo_url field, so every LibraryModel gets RepoURL set via this helper.
func repoURL(name string) string {
	return "https://ollama.com/library/" + name
}

// LibraryPath returns the on-disk library cache location:
// ~/.ai-platform/cache/ollama-library.json.
func LibraryPath() (string, error) {
	cache, err := paths.CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, libraryFileName), nil
}

// FetchLibrary GETs the library list from url (use LibraryURL) over httpClient and
// decodes it. RepoURL is set on every model via repoURL(name). The result is
// sorted by Name for stable output. The client and url are injectable so tests
// never touch the network.
func FetchLibrary(ctx context.Context, httpClient *http.Client, url string) ([]LibraryModel, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if url == "" {
		url = LibraryURL
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ollama: fetch library: build request: %w", err)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("ollama: fetch library: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama: fetch library: unexpected status %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("ollama: fetch library: read body: %w", err)
	}
	var models []LibraryModel
	if err := json.Unmarshal(body, &models); err != nil {
		return nil, fmt.Errorf("ollama: fetch library: parse: %w", err)
	}
	for index := range models {
		models[index].RepoURL = repoURL(models[index].Name)
	}
	sort.Slice(models, func(left, right int) bool {
		return models[left].Name < models[right].Name
	})
	return models, nil
}

// SaveLibrary writes the library list as JSON atomically (temp file + rename),
// creating the cache dir on demand.
func SaveLibrary(models []LibraryModel) error {
	path, err := LibraryPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(models, "", "  ")
	if err != nil {
		return fmt.Errorf("ollama: save library: marshal: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ollama: save library: mkdir: %w", err)
	}
	temp, err := os.CreateTemp(dir, libraryFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("ollama: save library: temp file: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }() // no-op once renamed
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("ollama: save library: write: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("ollama: save library: close: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("ollama: save library: rename: %w", err)
	}
	return nil
}

// LoadLibrary reads and parses the saved library cache copy. RepoURL is re-derived
// in case an older cache lacks it.
func LoadLibrary() ([]LibraryModel, error) {
	path, err := LibraryPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ollama: load library: %w", err)
	}
	var models []LibraryModel
	if err := json.Unmarshal(data, &models); err != nil {
		return nil, fmt.Errorf("ollama: load library: parse: %w", err)
	}
	for index := range models {
		models[index].RepoURL = repoURL(models[index].Name)
	}
	return models, nil
}

// LoadOrFetchLibrary prefers a fresh network fetch and reports the data SOURCE so
// callers can message source availability. On success it persists the cache and
// returns (models, SourceFresh, nil). When the live fetch fails it returns
// SourceCached: with a saved copy it returns (cachedModels, SourceCached, fetchErr)
// — the error is the live-fetch error so callers can warn while still using the
// cache; with no saved copy it returns (nil, SourceCached, fetchErr). Never panics.
func LoadOrFetchLibrary(ctx context.Context, httpClient *http.Client, url string) ([]LibraryModel, Source, error) {
	fetched, fetchErr := FetchLibrary(ctx, httpClient, url)
	if fetchErr == nil {
		// Best-effort persist; a save failure must not lose a good fetch.
		_ = SaveLibrary(fetched)
		return fetched, SourceFresh, nil
	}
	saved, loadErr := LoadLibrary()
	if loadErr == nil {
		return saved, SourceCached, fetchErr
	}
	return nil, SourceCached, fetchErr
}

// Library is a package-level func var (so tests can stub it) that loads the
// installable library, fetching live with a short timeout and falling back to the
// cache. It never panics.
var Library = func() ([]LibraryModel, Source, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return LoadOrFetchLibrary(ctx, http.DefaultClient, LibraryURL)
}
