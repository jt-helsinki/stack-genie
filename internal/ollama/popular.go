package ollama

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// PopularModel is one entry from the LIVE popular-models source: the models
// listed on ollama.com/search. The list is fetched fresh every call (ollama.com
// has no API — there is no hardcoded catalog), so it always reflects the current
// library. DownloadSize is the default-tag download size in bytes, looked up from
// Ollama's public registry manifest; it is 0 when the lookup fails/times out (the
// CLI renders that as "—"). Parameters are the parameter-size variants the search
// page lists (e.g. ["1b","8b"]); RepoURL is the model's ollama.com/library page.
type PopularModel struct {
	Name         string   `json:"name"`
	Parameters   []string `json:"parameters,omitempty"`
	DownloadSize int64    `json:"download_size,omitempty"`
	RepoURL      string   `json:"repo_url"`
	PullCount    string   `json:"pull_count,omitempty"`
}

// PopularSource fetches the live popular-models list. The two hosts are
// overridable so tests can point them at httptest servers; production uses the
// public ollama.com / registry.ollama.ai endpoints. A zero-value PopularSource is
// usable (it fills in the public defaults and a sensible HTTP client).
type PopularSource struct {
	// SearchBaseURL is the host serving /search (default https://ollama.com).
	SearchBaseURL string
	// RegistryBaseURL is the host serving /v2/library/<model>/manifests/<tag>
	// (default https://registry.ollama.ai).
	RegistryBaseURL string
	// HTTPClient is used for both the search fetch and the per-model manifest
	// lookups; a short timeout keeps the command responsive (default 15s).
	HTTPClient *http.Client
	// Limit caps how many listed models are returned / size-probed, keeping the
	// view and the concurrent manifest lookups bounded (default popularLimit).
	Limit int
}

const (
	defaultSearchBaseURL   = "https://ollama.com"
	defaultRegistryBaseURL = "https://registry.ollama.ai"
	popularLimit           = 25
	manifestWorkers        = 6
	// manifestTimeout bounds a single registry round-trip; a slow/missing size
	// must not stall the whole list, so it degrades to DownloadSize=0.
	manifestTimeout = 4 * time.Second
)

// RealPopularSource returns a PopularSource pointed at the public ollama.com and
// registry.ollama.ai hosts with a default HTTP client and limit.
func RealPopularSource() PopularSource { return PopularSource{} }

// Popular is a package-level convenience that fetches the live popular models
// from the public hosts. It is the entry point the CLI/TUI wire (overridable in
// tests via the package var below).
var Popular = func() ([]PopularModel, error) { return RealPopularSource().Popular() }

func (source PopularSource) searchBaseURL() string {
	if source.SearchBaseURL != "" {
		return strings.TrimRight(source.SearchBaseURL, "/")
	}
	return defaultSearchBaseURL
}

func (source PopularSource) registryBaseURL() string {
	if source.RegistryBaseURL != "" {
		return strings.TrimRight(source.RegistryBaseURL, "/")
	}
	return defaultRegistryBaseURL
}

func (source PopularSource) httpClient() *http.Client {
	if source.HTTPClient != nil {
		return source.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (source PopularSource) limit() int {
	if source.Limit > 0 {
		return source.Limit
	}
	return popularLimit
}

// Popular fetches ollama.com/search, parses the listed models (in listed order,
// which the page orders by relevance/popularity), caps to the limit, then looks
// up each model's default-tag download size from the registry manifest CONCURRENTLY
// with a bounded worker pool. A size lookup that errors or times out leaves
// DownloadSize=0 (rendered "—") — it never fails the whole list. Only a failed
// search fetch/parse is a hard error (the list cannot be built without it).
//
// The scrape is inherently brittle: ollama.com has no API, so this parses HTML by
// its x-test-* markers and will need updating if that markup changes.
func (source PopularSource) Popular() ([]PopularModel, error) {
	models, err := source.fetchListed()
	if err != nil {
		return nil, err
	}
	if len(models) > source.limit() {
		models = models[:source.limit()]
	}
	source.fillSizes(models)
	return models, nil
}

// fetchListed fetches the search page and parses it into PopularModels (no sizes).
func (source PopularSource) fetchListed() ([]PopularModel, error) {
	request, err := http.NewRequest(http.MethodGet, source.searchBaseURL()+"/search", nil)
	if err != nil {
		return nil, err
	}
	response, err := source.httpClient().Do(request)
	if err != nil {
		return nil, fmt.Errorf("ollama: fetching popular models from %s: %w", source.searchBaseURL(), err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama: %s/search returned status %d", source.searchBaseURL(), response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("ollama: reading popular-models page: %w", err)
	}
	models := parseSearchHTML(string(body))
	if len(models) == 0 {
		return nil, fmt.Errorf("ollama: could not parse any models from %s/search (the page markup may have changed)", source.searchBaseURL())
	}
	return models, nil
}

// Regexes over the page's x-test-* markers (ollama.com has no API). Each
// <li x-test-model> … </li> is one model; within it the title, the href to
// /library/<name>, the x-test-size variants, and the x-test-pull-count are pulled
// out by these focused patterns. The (?s) flag lets the per-item slice span lines.
var (
	modelItemRe   = regexp.MustCompile(`(?s)<li[^>]*\bx-test-model\b.*?</li>`)
	titleRe       = regexp.MustCompile(`(?s)<span[^>]*\bx-test-search-response-title\b[^>]*>(.*?)</span>`)
	hrefRe        = regexp.MustCompile(`href="(/library/[^"]+)"`)
	sizeRe        = regexp.MustCompile(`(?s)<span[^>]*\bx-test-size\b[^>]*>(.*?)</span>`)
	pullCountRe   = regexp.MustCompile(`(?s)<span[^>]*\bx-test-pull-count\b[^>]*>(.*?)</span>`)
	libraryNameRe = regexp.MustCompile(`^/library/([^/?#]+)`)
)

// parseSearchHTML extracts the listed models from the search page HTML. It is
// tolerant: a malformed item is skipped, never panics. Items are returned in page
// order (relevance/popularity), de-duplicated by name.
func parseSearchHTML(html string) []PopularModel {
	items := modelItemRe.FindAllString(html, -1)
	models := make([]PopularModel, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		titleMatch := titleRe.FindStringSubmatch(item)
		if titleMatch == nil {
			continue
		}
		name := strings.TrimSpace(stripTags(titleMatch[1]))
		if name == "" || seen[name] {
			continue
		}
		repoPath := ""
		if hrefMatch := hrefRe.FindStringSubmatch(item); hrefMatch != nil {
			repoPath = hrefMatch[1]
		}
		// Prefer the library-link name (canonical) when present; fall back to the
		// title text.
		if nameMatch := libraryNameRe.FindStringSubmatch(repoPath); nameMatch != nil {
			name = nameMatch[1]
		}
		if seen[name] {
			continue
		}
		var params []string
		for _, sizeMatch := range sizeRe.FindAllStringSubmatch(item, -1) {
			variant := strings.TrimSpace(stripTags(sizeMatch[1]))
			if variant != "" {
				params = append(params, variant)
			}
		}
		pullCount := ""
		if pullMatch := pullCountRe.FindStringSubmatch(item); pullMatch != nil {
			pullCount = strings.TrimSpace(stripTags(pullMatch[1]))
		}
		seen[name] = true
		models = append(models, PopularModel{
			Name:       name,
			Parameters: params,
			RepoURL:    defaultSearchBaseURL + "/library/" + name,
			PullCount:  pullCount,
		})
	}
	return models
}

// tagStripRe removes any leftover HTML tags from a captured fragment.
var tagStripRe = regexp.MustCompile(`<[^>]*>`)

func stripTags(fragment string) string {
	return strings.TrimSpace(tagStripRe.ReplaceAllString(fragment, ""))
}

// fillSizes looks up each model's default-tag download size from the registry,
// concurrently with a bounded worker pool. Failures/timeouts leave DownloadSize=0.
func (source PopularSource) fillSizes(models []PopularModel) {
	if len(models) == 0 {
		return
	}
	workers := manifestWorkers
	if workers > len(models) {
		workers = len(models)
	}
	indexes := make(chan int)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range indexes {
				size, err := source.manifestSize(models[index].Name, "latest")
				if err == nil {
					models[index].DownloadSize = size
				}
			}
		}()
	}
	for index := range models {
		indexes <- index
	}
	close(indexes)
	wait.Wait()
}

// manifestResponse is the subset of a registry manifest we need: the layer sizes.
type manifestResponse struct {
	Layers []struct {
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
	} `json:"layers"`
}

// manifestSize fetches the registry manifest for <model>:<tag> and sums its layer
// sizes (the large model layer dominates) — the download size in bytes. A short
// per-request timeout bounds the call so one slow model can't stall the list.
func (source PopularSource) manifestSize(model, tag string) (int64, error) {
	base := source.httpClient()
	client := &http.Client{
		Timeout:       manifestTimeout,
		Transport:     base.Transport,
		CheckRedirect: base.CheckRedirect,
	}
	if base.Timeout > 0 && base.Timeout < manifestTimeout {
		client.Timeout = base.Timeout
	}
	url := fmt.Sprintf("%s/v2/library/%s/manifests/%s", source.registryBaseURL(), model, tag)
	response, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("registry status %d", response.StatusCode)
	}
	var parsed manifestResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&parsed); err != nil {
		return 0, err
	}
	var total int64
	for _, layer := range parsed.Layers {
		total += layer.Size
	}
	return total, nil
}
