package hf

// This file adds a LIVE-REFRESH path for the curated available-models list. The
// hand-curated CuratedModels(goos) set in hf.go goes stale; this fetches the
// current top models straight from the Hugging Face Hub JSON API, caches the
// result as YAML under the platform cache dir, and degrades gracefully (cache,
// then the built-in list) when the network is unavailable. It mirrors the
// established fetch → YAML cache → fresh/cached Source shape used by
// internal/catalog and the retired internal/ollama library scraper.
//
// The two OS lists are queried from DIFFERENT sources and are therefore NOT
// paired the way the hand-curated MLX↔safetensors slices are: darwin pulls the
// most-downloaded mlx-community/* conversions (the Metal-servable repos), linux
// pulls the currently-trending text-generation repos. The live list is an honest
// "what's popular now" snapshot per OS, not a curated cross-reference.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/conffile"
	"github.com/jt-helsinki/stack-genie/internal/paths"
)

// hfModelsAPI is the Hugging Face Hub models endpoint.
const hfModelsAPI = "https://huggingface.co/api/models"

// curatedFetchLimit is how many rows to request from the API; curatedTakeLimit is
// how many survive filtering into the returned list. Fetching a few extra covers
// the rows dropped by the servable filter.
const (
	curatedFetchLimit = 45
	curatedTakeLimit  = 30
)

// hfUserAgent identifies the platform to the Hub API.
const hfUserAgent = "stack-genie/hf-curated (+https://github.com/jt-helsinki/stack-genie)"

// curatedCacheFileName is the on-disk cache name under the cache dir. Both OS
// lists coexist in the one file, keyed by GOOS.
const curatedCacheFileName = "hf-models.yaml"

// Source records where a returned curated list came from: a fresh live fetch, the
// on-disk cache, or the compiled-in built-in fallback. It is intentionally
// independent of catalog.Source (no cross-package dependency).
type Source int

const (
	// SourceFresh means the list was fetched live from the Hub and the cache was
	// refreshed.
	SourceFresh Source = iota
	// SourceCached means a previously-cached copy was used (a plain read with a
	// cache present, or a refresh whose live fetch failed but a cache existed).
	SourceCached
	// SourceBuiltin means the compiled-in hand-curated list was used (no cache and
	// no reachable Hub).
	SourceBuiltin
)

// String returns "live", "cached", or "built-in".
func (source Source) String() string {
	switch source {
	case SourceFresh:
		return "live"
	case SourceCached:
		return "cached"
	default:
		return "built-in"
	}
}

// hfAPIGet is the injectable HTTP seam: tests replace it to serve a canned API
// body with no network. It GETs url and returns the response body, erroring on a
// non-200 status.
var hfAPIGet = func(url string) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", hfUserAgent)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hf: models api: unexpected status %d", response.StatusCode)
	}
	return io.ReadAll(response.Body)
}

// nowFn is the injectable clock for the cache's fetched-at stamp.
var nowFn = time.Now

// rawHFModel mirrors the fields the Hub /api/models list carries that we use.
// `gated` is a mixed type upstream (false, or "auto"/"manual"), so it is decoded
// raw and interpreted by isGated.
type rawHFModel struct {
	ID          string          `json:"id"`
	Downloads   int64           `json:"downloads"`
	Gated       json.RawMessage `json:"gated"`
	Tags        []string        `json:"tags"`
	PipelineTag string          `json:"pipeline_tag"`
}

// isGated reports whether the repo requires a license click-through / login. The
// Hub encodes an ungated repo as `false` and a gated one as a string.
func (raw rawHFModel) isGated() bool {
	trimmed := strings.Trim(strings.TrimSpace(string(raw.Gated)), `"`)
	return trimmed != "" && trimmed != "false" && trimmed != "null"
}

// toCurated maps a raw API row to a CuratedModel. Name is the repo's last path
// segment (the gateway alias vLLM uses); Description is derived from the download
// count plus a gated-login note; Size is left empty (the list endpoint does not
// carry a reliable on-disk size).
func (raw rawHFModel) toCurated() CuratedModel {
	description := humanDownloads(raw.Downloads)
	if raw.isGated() {
		description += " · gated — run `ai models login`"
	}
	return CuratedModel{
		Name:        repoBaseName(raw.ID),
		Repo:        raw.ID,
		Description: description,
	}
}

// nonServableTokens are substrings in a repo id that mark a model as not a
// vLLM-servable text generator (embeddings, rerankers, audio, image, OCR). The
// filter is deliberately LIGHT — it drops the obvious non-servables the "top
// downloads"/"trending" queries surface, not every edge case.
var nonServableTokens = []string{
	"embed", "rerank", "bge-", "gte-", "e5-", "sentence-transformers",
	"whisper", "-tts", "-asr", "parakeet", "wav2vec", "musicgen",
	"stable-diffusion", "-sdxl", "flux.", "-vae", "clip-", "diffusion", "-ocr",
}

// servableCurated applies the light filter: drop rows whose id matches a
// non-servable token, and — when the row carries a pipeline tag — keep only text
// generation.
func servableCurated(raw rawHFModel) bool {
	id := strings.ToLower(raw.ID)
	for _, token := range nonServableTokens {
		if strings.Contains(id, token) {
			return false
		}
	}
	if raw.PipelineTag != "" &&
		raw.PipelineTag != "text-generation" &&
		raw.PipelineTag != "text2text-generation" {
		return false
	}
	return true
}

// humanDownloads renders a download count into a compact description.
func humanDownloads(count int64) string {
	switch {
	case count >= 1_000_000:
		return fmt.Sprintf("%.1fM downloads", float64(count)/1_000_000)
	case count >= 1_000:
		return fmt.Sprintf("%.0fK downloads", float64(count)/1_000)
	case count > 0:
		return fmt.Sprintf("%d downloads", count)
	default:
		return "text-generation"
	}
}

// repoBaseName returns the last path segment of a repo id
// ("mlx-community/Qwen3-4bit" → "Qwen3-4bit").
func repoBaseName(repo string) string {
	if slash := strings.LastIndexByte(repo, '/'); slash >= 0 && slash < len(repo)-1 {
		return repo[slash+1:]
	}
	return repo
}

// curatedFetchURL builds the Hub API query for goos: the most-downloaded
// mlx-community/* conversions on darwin (the Metal-servable repos), the current
// trending text-generation repos elsewhere (Linux/CUDA).
func curatedFetchURL(goos string) string {
	if goos == "darwin" {
		return fmt.Sprintf("%s?author=mlx-community&sort=downloads&direction=-1&limit=%d",
			hfModelsAPI, curatedFetchLimit)
	}
	return fmt.Sprintf("%s?pipeline_tag=text-generation&sort=trendingScore&direction=-1&limit=%d",
		hfModelsAPI, curatedFetchLimit)
}

// fetchCurated queries the Hub for goos and parses the response into a filtered
// curated list.
func fetchCurated(goos string) ([]CuratedModel, error) {
	body, err := hfAPIGet(curatedFetchURL(goos))
	if err != nil {
		return nil, fmt.Errorf("hf: fetch curated models: %w", err)
	}
	return parseCurated(body)
}

// parseCurated decodes the Hub /api/models array, applies the servable filter,
// and returns up to curatedTakeLimit models in API order.
func parseCurated(body []byte) ([]CuratedModel, error) {
	var raw []rawHFModel
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("hf: parse curated models: %w", err)
	}
	models := make([]CuratedModel, 0, len(raw))
	for _, item := range raw {
		if item.ID == "" || !servableCurated(item) {
			continue
		}
		models = append(models, item.toCurated())
		if len(models) >= curatedTakeLimit {
			break
		}
	}
	if len(models) == 0 {
		return nil, errors.New("hf: parse curated models: no servable models in response")
	}
	return models, nil
}

// curatedCache is the on-disk cache: one entry per GOOS so both lists coexist.
type curatedCache struct {
	Lists map[string]curatedCacheEntry `yaml:"lists"`
}

// curatedCacheEntry is one OS's cached list plus the time it was fetched.
type curatedCacheEntry struct {
	FetchedAt time.Time      `yaml:"fetched_at"`
	Models    []CuratedModel `yaml:"models"`
}

// curatedCachePath is ~/.ai-platform/cache/hf-models.yaml.
func curatedCachePath() (string, error) {
	cache, err := paths.CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, curatedCacheFileName), nil
}

// loadCuratedCache reads the whole cache file (best-effort: a missing/corrupt
// file reads as absent).
func loadCuratedCache() (curatedCache, bool) {
	path, err := curatedCachePath()
	if err != nil {
		return curatedCache{}, false
	}
	var loaded curatedCache
	if err := conffile.Read(path, &loaded); err != nil {
		return curatedCache{}, false
	}
	return loaded, true
}

// cachedCuratedEntry returns the cached entry for goos, if present.
func cachedCuratedEntry(goos string) (curatedCacheEntry, bool) {
	loaded, ok := loadCuratedCache()
	if !ok {
		return curatedCacheEntry{}, false
	}
	entry, present := loaded.Lists[goos]
	return entry, present
}

// saveCuratedCacheEntry merges the goos list into the cache and writes it
// atomically, stamped with the current time.
func saveCuratedCacheEntry(goos string, models []CuratedModel) error {
	path, err := curatedCachePath()
	if err != nil {
		return err
	}
	loaded, _ := loadCuratedCache()
	if loaded.Lists == nil {
		loaded.Lists = make(map[string]curatedCacheEntry)
	}
	loaded.Lists[goos] = curatedCacheEntry{FetchedAt: nowFn(), Models: models}
	return conffile.WriteAtomic(path, loaded)
}

// CachedCuratedInfo reports when the goos list was last fetched, if a cached copy
// exists. Callers use it to message cache freshness.
func CachedCuratedInfo(goos string) (time.Time, bool) {
	entry, ok := cachedCuratedEntry(goos)
	if !ok || len(entry.Models) == 0 {
		return time.Time{}, false
	}
	return entry.FetchedAt, true
}

// LoadOrFetchCurated returns the curated available-models list for goos plus its
// Source. It NEVER errors on a normal read — it always degrades to the built-in
// list.
//
//   - refresh=true fetches live and, on success, writes the cache and returns
//     (list, SourceFresh). A failed fetch falls through to the cache→built-in
//     order below.
//   - refresh=false (and a failed refresh) returns the cached list when present
//     (SourceCached), otherwise the compiled-in built-in list (SourceBuiltin).
func LoadOrFetchCurated(goos string, refresh bool) ([]CuratedModel, Source, error) {
	if refresh {
		if fetched, err := fetchCurated(goos); err == nil && len(fetched) > 0 {
			_ = saveCuratedCacheEntry(goos, fetched) // best-effort; a save failure never loses a good fetch
			return fetched, SourceFresh, nil
		}
	}
	if entry, ok := cachedCuratedEntry(goos); ok && len(entry.Models) > 0 {
		return entry.Models, SourceCached, nil
	}
	return CuratedModels(goos), SourceBuiltin, nil
}
