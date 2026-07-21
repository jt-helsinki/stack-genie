package ollama

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/paths"
	"gopkg.in/yaml.v3"
)

// LibraryBaseURL is the live source for the installable-Ollama library. The list
// is SCRAPED from ollama.com: the index page https://ollama.com/library enumerates
// every model (name + description), and each model's tag table at
// https://ollama.com/library/<model>/tags carries the per-variant size, context
// window, and input modality. The old third-party JSON worker endpoint was stale;
// ollama.com is the ground truth.
const LibraryBaseURL = "https://ollama.com"

// libraryFileName is the on-disk cache name under the cache dir. The cache is YAML
// (human-inspectable) rather than the scraped HTML.
const libraryFileName = "ollama-models.yaml"

// libraryUserAgent is sent on every scrape request; ollama.com serves the
// server-rendered HTML we parse to a browser-like UA.
const libraryUserAgent = "Mozilla/5.0 (compatible; ai-platform/ollama-library; +https://github.com/jt-helsinki/stack-genie)"

// tagFetchConcurrency bounds the per-model tag-page fetches so a refresh does not
// hammer ollama.com with hundreds of simultaneous requests.
const tagFetchConcurrency = 10

// LibraryModel is one installable model from the Ollama library. RepoURL is
// DERIVED (the index carries none): the model's ollama.com/library page. Tags
// carries the per-variant detail scraped from the model's /tags table.
type LibraryModel struct {
	Name        string       `yaml:"name" json:"name"`
	Description string       `yaml:"description,omitempty" json:"description,omitempty"`
	RepoURL     string       `yaml:"repo_url,omitempty" json:"repo_url,omitempty"`
	Tags        []LibraryTag `yaml:"tags,omitempty" json:"tags,omitempty"`
}

// LibraryTag is one pullable variant of a model, as listed in the model's
// ollama.com /tags table. Name is the SHORT tag (the part after the colon, e.g.
// "1b", "latest"); Size/Context/Input are the table columns verbatim (e.g.
// "2.0GB", "128K", "Text") and are empty when the table omits them (embeddings).
type LibraryTag struct {
	Name    string `yaml:"name" json:"name"`
	Size    string `yaml:"size,omitempty" json:"size,omitempty"`
	Context string `yaml:"context,omitempty" json:"context,omitempty"`
	Input   string `yaml:"input,omitempty" json:"input,omitempty"`
}

// TagNames returns the model's short tag names in listed order (a convenience for
// callers that only need the names, e.g. building pull refs).
func (model LibraryModel) TagNames() []string {
	names := make([]string, 0, len(model.Tags))
	for _, tag := range model.Tags {
		names = append(names, tag.Name)
	}
	return names
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

// repoURL derives a model's ollama.com/library page from its name.
func repoURL(name string) string {
	return LibraryBaseURL + "/library/" + name
}

// LibraryPath returns the on-disk library cache location:
// ~/.ai-platform/cache/ollama-models.yaml.
func LibraryPath() (string, error) {
	cache, err := paths.CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, libraryFileName), nil
}

// FetchLibrary scrapes the installable library from baseURL (use LibraryBaseURL)
// over httpClient. It GETs baseURL+"/library" to enumerate models (name +
// description) then fetches each model's baseURL+"/library/<name>/tags" table for
// the per-variant size/context/input, bounded by tagFetchConcurrency. A model
// whose tag page fails to load is still returned (with no tags) rather than
// failing the whole scrape. The result is sorted by Name. The client and baseURL
// are injectable so tests never touch the network.
func FetchLibrary(ctx context.Context, httpClient *http.Client, baseURL string) ([]LibraryModel, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = LibraryBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	indexBody, err := fetchPage(ctx, httpClient, baseURL+"/library")
	if err != nil {
		return nil, fmt.Errorf("ollama: fetch library index: %w", err)
	}
	models := parseLibraryIndex(indexBody)
	if len(models) == 0 {
		return nil, fmt.Errorf("ollama: fetch library index: no models parsed")
	}

	fetchTagsForModels(ctx, httpClient, baseURL, models)

	for index := range models {
		models[index].RepoURL = repoURL(models[index].Name)
	}
	sort.Slice(models, func(left, right int) bool {
		return models[left].Name < models[right].Name
	})
	return models, nil
}

// fetchTagsForModels populates every model's Tags by scraping its /tags page,
// bounded by tagFetchConcurrency. Per-model errors are swallowed (the model keeps
// empty tags) so one bad page never fails the whole library.
func fetchTagsForModels(ctx context.Context, httpClient *http.Client, baseURL string, models []LibraryModel) {
	semaphore := make(chan struct{}, tagFetchConcurrency)
	var group sync.WaitGroup
	for index := range models {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			name := models[index].Name
			body, err := fetchPage(ctx, httpClient, baseURL+"/library/"+name+"/tags")
			if err != nil {
				return
			}
			models[index].Tags = parseTags(body, name)
		}(index)
	}
	group.Wait()
}

// fetchPage GETs url over httpClient with the browser-like UA and returns the body.
func fetchPage(ctx context.Context, httpClient *http.Client, url string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("User-Agent", libraryUserAgent)
	response, err := httpClient.Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return string(body), nil
}

var (
	// libraryIndexAnchorRe captures each model's canonical name from its index-page
	// card anchor `href="/library/<name>"`. ollama.com dropped the old
	// `x-test-model-title` marker; the per-model link is now the stable anchor. The
	// trailing `"` (no `:` allowed in the name class) excludes tag links like
	// `/library/<name>:<tag>` and sub-paths like `/library/<name>/tags`.
	libraryIndexAnchorRe = regexp.MustCompile(`href="/library/([a-zA-Z0-9][a-zA-Z0-9._-]*)"`)
	// libraryDescRe captures the model description <p> that follows the anchor.
	libraryDescRe = regexp.MustCompile(`<p[^>]*text-md[^>]*>([^<]*)`)
	// tagSizeRe matches a size cell like "2.0GB" / "500MB".
	tagSizeRe = regexp.MustCompile(`\d+(?:\.\d+)?\s*[GMK]B`)
	// tagContextRe matches a context-window cell like "128K" / "1M". A size such
	// as "2.0GB" or "500MB" ends in B (no trailing word boundary after G/M), so it
	// never matches here — RE2 has no lookahead, and this relies on that boundary.
	tagContextRe = regexp.MustCompile(`\b\d+(?:\.\d+)?[KM]\b`)
	// tagInputRe matches an input-modality keyword.
	tagInputRe = regexp.MustCompile(`\b(?:Text|Image|Vision|Audio|Video|Embedding)\b`)
	// tagOpenRe matches any HTML tag (for stripping to plain text).
	tagOpenRe = regexp.MustCompile(`<[^>]+>`)
)

// parseLibraryIndex parses the ollama.com/library index HTML into models with
// name + description (no tags — those come from each model's /tags page). Each
// model on the index is a `<li x-test-model …>` block carrying an
// `x-test-model-title title="NAME"` and a description `<p …text-md>`.
func parseLibraryIndex(body string) []LibraryModel {
	// Each model is a card anchor `href="/library/<name>"`; the description is the
	// following <p ...text-md> within that card, bounded by the NEXT model anchor so it
	// can't bleed into the following card.
	anchors := libraryIndexAnchorRe.FindAllStringSubmatchIndex(body, -1)
	models := make([]LibraryModel, 0, len(anchors))
	seen := make(map[string]bool, len(anchors))
	for position, anchor := range anchors {
		name := strings.TrimSpace(html.UnescapeString(body[anchor[2]:anchor[3]]))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		regionEnd := len(body)
		if position+1 < len(anchors) {
			regionEnd = anchors[position+1][0]
		}
		description := ""
		if desc := libraryDescRe.FindStringSubmatch(body[anchor[0]:regionEnd]); desc != nil {
			description = strings.TrimSpace(html.UnescapeString(desc[1]))
		}
		models = append(models, LibraryModel{Name: name, Description: description})
	}
	return models
}

// parseTags parses a model's /tags table into per-variant LibraryTags. Every tag
// row links to href="/library/<model>:<tag>"; the row's grid cells carry the
// Size, Context, and Input columns. Rows appear twice (mobile + desktop layouts),
// so tags are de-duplicated by short name (first occurrence wins) preserving the
// listed order. A row with a bare (colon-less) or empty tag is skipped.
func parseTags(body, model string) []LibraryTag {
	anchorRe := regexp.MustCompile(`href="/library/` + regexp.QuoteMeta(model) + `:([^"?#]+)"`)
	matches := anchorRe.FindAllStringSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return nil
	}
	tags := make([]LibraryTag, 0, len(matches))
	seen := make(map[string]bool, len(matches))
	for position, match := range matches {
		short := strings.TrimSpace(body[match[2]:match[3]])
		if short == "" || seen[short] {
			continue
		}
		seen[short] = true
		// Bound the row's text to the region up to the next tag anchor so field
		// extraction never bleeds into the following variant's cells.
		regionEnd := len(body)
		if position+1 < len(matches) {
			regionEnd = matches[position+1][0]
		}
		region := stripTags(body[match[1]:regionEnd])
		tags = append(tags, LibraryTag{
			Name:    short,
			Size:    tagSizeRe.FindString(region),
			Context: tagContextRe.FindString(region),
			Input:   parseInputs(region),
		})
	}
	return tags
}

// stripTags removes HTML tags and collapses whitespace so a row's textual cells
// (size/context/input) can be matched by simple token regexes.
func stripTags(fragment string) string {
	text := tagOpenRe.ReplaceAllString(fragment, " ")
	text = html.UnescapeString(text)
	return strings.Join(strings.Fields(text), " ")
}

// parseInputs collects the distinct input-modality keywords present in the row's
// text (in first-seen order) and joins them with ", " (e.g. "Text, Image").
func parseInputs(region string) string {
	found := tagInputRe.FindAllString(region, -1)
	if len(found) == 0 {
		return ""
	}
	inputs := make([]string, 0, len(found))
	seen := make(map[string]bool, len(found))
	for _, input := range found {
		if seen[input] {
			continue
		}
		seen[input] = true
		inputs = append(inputs, input)
	}
	return strings.Join(inputs, ", ")
}

// SaveLibrary writes the library list as YAML atomically (temp file + rename),
// creating the cache dir on demand.
func SaveLibrary(models []LibraryModel) error {
	path, err := LibraryPath()
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(models)
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

// LoadLibrary reads and parses the saved YAML library cache. RepoURL is re-derived
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
	if err := yaml.Unmarshal(data, &models); err != nil {
		return nil, fmt.Errorf("ollama: load library: parse: %w", err)
	}
	for index := range models {
		models[index].RepoURL = repoURL(models[index].Name)
	}
	return models, nil
}

// LoadOrFetchLibrary prefers a fresh network scrape and reports the data SOURCE so
// callers can message source availability. On success it persists the cache and
// returns (models, SourceFresh, nil). When the live scrape fails it returns
// SourceCached: with a saved copy it returns (cachedModels, SourceCached, fetchErr)
// — the error is the live-fetch error so callers can warn while still using the
// cache; with no saved copy it returns (nil, SourceCached, fetchErr). Never panics.
func LoadOrFetchLibrary(ctx context.Context, httpClient *http.Client, baseURL string) ([]LibraryModel, Source, error) {
	fetched, fetchErr := FetchLibrary(ctx, httpClient, baseURL)
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

// LoadCachedOrFetchLibrary is CACHE-FIRST: when a cached copy exists it is used
// as-is with NO network call (the installable library changes rarely, and the live
// scrape is expensive — hundreds of tag-page GETs). Only when no cache exists does
// it scrape live (and persist). Use RefreshLibrary / LoadOrFetchLibrary to force a
// fresh scrape (the `r` refresh). Never panics.
func LoadCachedOrFetchLibrary(ctx context.Context, httpClient *http.Client, baseURL string) ([]LibraryModel, Source, error) {
	if cached, err := LoadLibrary(); err == nil {
		return cached, SourceCached, nil
	}
	return LoadOrFetchLibrary(ctx, httpClient, baseURL)
}

// libraryFetchTimeout bounds a full library scrape (the index plus every model's
// tag page). It is generous: a cold refresh fans out hundreds of tag-page GETs.
const libraryFetchTimeout = 3 * time.Minute

// Library is a package-level func var (so tests can stub it) that loads the
// installable library CACHE-FIRST: a cached copy is returned as-is, and only a
// first run (no cache) scrapes live. It never panics.
var Library = func() ([]LibraryModel, Source, error) {
	ctx, cancel := context.WithTimeout(context.Background(), libraryFetchTimeout)
	defer cancel()
	return LoadCachedOrFetchLibrary(ctx, http.DefaultClient, LibraryBaseURL)
}

// RefreshLibrary FORCE-scrapes the library live and refreshes the cache (the `r`
// refresh in the TUI). On a live failure it falls back to the cache. It never
// panics.
var RefreshLibrary = func() ([]LibraryModel, Source, error) {
	ctx, cancel := context.WithTimeout(context.Background(), libraryFetchTimeout)
	defer cancel()
	return LoadOrFetchLibrary(ctx, http.DefaultClient, LibraryBaseURL)
}
