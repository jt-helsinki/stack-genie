// Package catalog fetches, parses, and persists the models.dev model catalog
// (https://models.dev/catalog.json) — the ground-truth list of known models and
// the providers that serve them.
//
// This package is PURE catalog: it models exactly what the upstream JSON carries
// and exposes provider-grouped access plus an offline-tolerant fetch/save/load
// cycle. It does NOT apply any LiteLLM-specific compatibility filtering or
// provider-prefix mapping — that lives in Phase B.
//
// # Upstream shape (verified against the live catalog, 2026-06)
//
// The catalog JSON has two top-level keys:
//
//	{
//	  "models":    { "<provider>/<model>": { id, name, family, … }, … },
//	  "providers": { "<provider>": { id, name, env, npm, api, doc, models }, … }
//	}
//
// "models" is the canonical, deduplicated model list — each entry is keyed by its
// VERBATIM "<provider>/<model>" id with the provider embedded in the id (e.g.
// "google/gemini-3-pro"). We parse THIS as the source of truth and group by the
// id prefix. "providers" is a much larger map (routers/gateways included) whose
// entries each carry id/name/env; we use it ONLY to enrich a provider's display
// name + env-var list when the prefix matches a provider key (best-effort — some
// model prefixes, e.g. "meta", have no matching provider entry, in which case the
// provider Name falls back to the id).
//
// Notable: there is NO parameter-count / model-size field in the catalog — that
// is a local-model concept and is intentionally absent here.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/paths"
	"gopkg.in/yaml.v3"
)

// DefaultURL is the canonical models.dev catalog endpoint.
const DefaultURL = "https://models.dev/catalog.json"

// catalogFileName is the on-disk cache name under the cache dir. The upstream
// fetch is JSON (models.dev serves JSON), but the local cache is persisted as YAML
// (human-inspectable), converting a legacy catalog.json copy on first access.
const catalogFileName = "catalog.yaml"

// legacyCatalogFileName is the pre-YAML cache name, migrated on first access.
const legacyCatalogFileName = "catalog.json"

// Source records whether a returned catalog came from a fresh network fetch or
// from the on-disk cache (used by callers to message source availability).
type Source int

const (
	// SourceFresh means the catalog was fetched live and the cache was refreshed.
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

// Model is a single catalog model entry, carrying exactly the fields the
// upstream catalog actually populates. The id is kept VERBATIM (e.g.
// "google/gemini-3.1-pro").
type Model struct {
	ID          string
	Name        string
	Family      string
	Knowledge   string // training-knowledge cutoff (e.g. "2025-01"); may be empty
	ReleaseDate string
	LastUpdated string
	OpenWeights bool
	Reasoning   bool
	ToolCall    bool
	Attachment  bool

	// StructuredOutput / Temperature are present on most but not all models.
	StructuredOutput bool
	Temperature      bool

	Limit struct {
		Context int
		Output  int
		Input   int // present on a minority of models (e.g. separate input cap)
	}

	Modalities struct {
		Input  []string
		Output []string
	}
}

// rawModel mirrors the upstream JSON snake_case keys. Extra fields the catalog
// carries (benchmarks, weights, cost, npm, …) are tolerated and ignored.
type rawModel struct {
	ID          string `json:"id" yaml:"id"`
	Name        string `json:"name" yaml:"name"`
	Family      string `json:"family" yaml:"family,omitempty"`
	Knowledge   string `json:"knowledge" yaml:"knowledge,omitempty"`
	ReleaseDate string `json:"release_date" yaml:"release_date,omitempty"`
	LastUpdated string `json:"last_updated" yaml:"last_updated,omitempty"`
	OpenWeights bool   `json:"open_weights" yaml:"open_weights,omitempty"`
	Reasoning   bool   `json:"reasoning" yaml:"reasoning,omitempty"`
	ToolCall    bool   `json:"tool_call" yaml:"tool_call,omitempty"`
	Attachment  bool   `json:"attachment" yaml:"attachment,omitempty"`

	StructuredOutput bool `json:"structured_output" yaml:"structured_output,omitempty"`
	Temperature      bool `json:"temperature" yaml:"temperature,omitempty"`

	Limit struct {
		Context int `json:"context" yaml:"context,omitempty"`
		Output  int `json:"output" yaml:"output,omitempty"`
		Input   int `json:"input" yaml:"input,omitempty"`
	} `json:"limit" yaml:"limit,omitempty"`

	Modalities struct {
		Input  []string `json:"input" yaml:"input,omitempty"`
		Output []string `json:"output" yaml:"output,omitempty"`
	} `json:"modalities" yaml:"modalities,omitempty"`
}

func (raw rawModel) toModel() Model {
	model := Model{
		ID:               raw.ID,
		Name:             raw.Name,
		Family:           raw.Family,
		Knowledge:        raw.Knowledge,
		ReleaseDate:      raw.ReleaseDate,
		LastUpdated:      raw.LastUpdated,
		OpenWeights:      raw.OpenWeights,
		Reasoning:        raw.Reasoning,
		ToolCall:         raw.ToolCall,
		Attachment:       raw.Attachment,
		StructuredOutput: raw.StructuredOutput,
		Temperature:      raw.Temperature,
	}
	model.Limit.Context = raw.Limit.Context
	model.Limit.Output = raw.Limit.Output
	model.Limit.Input = raw.Limit.Input
	model.Modalities.Input = raw.Modalities.Input
	model.Modalities.Output = raw.Modalities.Output
	return model
}

// Provider groups the models served under one provider id (the "<provider>"
// prefix of the model ids). Name/Env are enriched from the catalog's "providers"
// map when available; otherwise Name falls back to the id and Env is empty.
type Provider struct {
	ID     string
	Name   string
	Env    []string
	Models []Model
}

// rawProvider mirrors the upstream "providers" map entry. We only need
// id/name/env here; other fields (npm/api/doc/models) are tolerated/ignored.
type rawProvider struct {
	ID   string   `json:"id" yaml:"id"`
	Name string   `json:"name" yaml:"name,omitempty"`
	Env  []string `json:"env" yaml:"env,omitempty"`
}

// rawCatalog mirrors the catalog's two top-level keys.
type rawCatalog struct {
	Models    map[string]rawModel    `json:"models" yaml:"models"`
	Providers map[string]rawProvider `json:"providers" yaml:"providers,omitempty"`
}

// Catalog is the parsed, provider-grouped model catalog. It also retains the
// parsed raw form so Save can persist it as YAML.
type Catalog struct {
	providers map[string]Provider // keyed by provider id
	order     []string            // provider ids, sorted ascending
	source    rawCatalog          // the parsed raw form, re-marshaled to YAML by Save
}

// Parse parses catalog JSON (the upstream models.dev format) into a provider-
// grouped Catalog. It tolerates extra fields (the catalog carries many we don't
// model).
func Parse(data []byte) (*Catalog, error) {
	var raw rawCatalog
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("catalog: parse: %w", err)
	}
	return build(raw)
}

// parseYAML parses the local YAML cache (the format Save writes) into a Catalog.
func parseYAML(data []byte) (*Catalog, error) {
	var raw rawCatalog
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("catalog: parse yaml: %w", err)
	}
	return build(raw)
}

// build groups a raw catalog by provider prefix into a Catalog, retaining the raw
// form for YAML persistence. It is the shared tail of Parse and parseYAML.
func build(raw rawCatalog) (*Catalog, error) {
	if len(raw.Models) == 0 {
		return nil, errors.New("catalog: parse: no models in catalog")
	}

	// Group models by the provider prefix of their (verbatim) id.
	grouped := make(map[string][]Model)
	for key, rawEntry := range raw.Models {
		// Prefer the explicit id field; fall back to the map key.
		id := rawEntry.ID
		if id == "" {
			id = key
			rawEntry.ID = key
		}
		providerID := id
		if slash := strings.IndexByte(id, '/'); slash >= 0 {
			providerID = id[:slash]
		}
		grouped[providerID] = append(grouped[providerID], rawEntry.toModel())
	}

	providers := make(map[string]Provider, len(grouped))
	for providerID, models := range grouped {
		sort.Slice(models, func(left, right int) bool {
			return models[left].ID < models[right].ID
		})
		provider := Provider{ID: providerID, Name: providerID, Models: models}
		// Enrich name + env from the providers map when the prefix matches.
		if meta, ok := raw.Providers[providerID]; ok {
			if meta.Name != "" {
				provider.Name = meta.Name
			}
			provider.Env = meta.Env
		}
		providers[providerID] = provider
	}

	order := make([]string, 0, len(providers))
	for providerID := range providers {
		order = append(order, providerID)
	}
	sort.Strings(order)

	return &Catalog{providers: providers, order: order, source: raw}, nil
}

// Providers returns every provider, sorted by id ascending.
func (catalog *Catalog) Providers() []Provider {
	result := make([]Provider, 0, len(catalog.order))
	for _, providerID := range catalog.order {
		result = append(result, catalog.providers[providerID])
	}
	return result
}

// Provider returns the provider with the given id, if present.
func (catalog *Catalog) Provider(id string) (Provider, bool) {
	provider, ok := catalog.providers[id]
	return provider, ok
}

// Models returns every model across all providers as a flat list, sorted by id.
func (catalog *Catalog) Models() []Model {
	var result []Model
	for _, providerID := range catalog.order {
		result = append(result, catalog.providers[providerID].Models...)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].ID < result[right].ID
	})
	return result
}

// Fetch GETs the catalog from url (use DefaultURL) over httpClient and parses it.
// The client and url are injectable so tests never touch the network.
func Fetch(ctx context.Context, httpClient *http.Client, url string) (*Catalog, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if url == "" {
		url = DefaultURL
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("catalog: fetch: build request: %w", err)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("catalog: fetch: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog: fetch: unexpected status %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("catalog: fetch: read body: %w", err)
	}
	return Parse(body)
}

// Path returns the on-disk catalog location: ~/.ai-platform/cache/catalog.yaml.
// It lazily converts a legacy JSON copy (cache/catalog.json or the older
// volumes/catalog.json) into the YAML cache (best-effort).
func Path() (string, error) {
	cache, err := paths.CacheDir()
	if err != nil {
		return "", err
	}
	migrateLegacyCatalog()
	return filepath.Join(cache, catalogFileName), nil
}

// migrateLegacyCatalog converts a pre-existing legacy JSON catalog into the new
// cache/catalog.yaml once, when the YAML copy is absent and a legacy JSON copy
// exists (checked at cache/catalog.json first, then the older volumes/catalog.json).
// It parses the JSON and re-serializes to YAML, then removes the legacy file. It is
// best-effort: any error (including a missing dir) is ignored so an existing
// install's catalog isn't lost — it just re-fetches on the next online run.
func migrateLegacyCatalog() {
	cache, err := paths.CacheDir()
	if err != nil {
		return
	}
	newPath := filepath.Join(cache, catalogFileName)
	if _, err := os.Stat(newPath); err == nil {
		return // YAML cache already present
	}
	candidates := []string{filepath.Join(cache, legacyCatalogFileName)}
	if volumes, volErr := paths.VolumesDir(); volErr == nil {
		candidates = append(candidates, filepath.Join(volumes, legacyCatalogFileName))
	}
	for _, legacy := range candidates {
		data, readErr := os.ReadFile(legacy)
		if readErr != nil {
			continue
		}
		parsed, parseErr := Parse(data)
		if parseErr != nil {
			continue
		}
		out, marshalErr := yaml.Marshal(parsed.source)
		if marshalErr != nil {
			continue
		}
		if err := os.MkdirAll(cache, 0o755); err != nil {
			return
		}
		if err := os.WriteFile(newPath, out, 0o644); err != nil {
			return
		}
		_ = os.Remove(legacy) // best-effort cleanup of the legacy JSON copy
		return
	}
}

// Save writes the catalog as YAML atomically (temp file + rename), converting the
// upstream JSON form to the local YAML cache. It creates the cache dir on demand.
func Save(catalog *Catalog) error {
	if catalog == nil || len(catalog.source.Models) == 0 {
		return errors.New("catalog: save: nothing to write")
	}
	path, err := Path()
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(catalog.source)
	if err != nil {
		return fmt.Errorf("catalog: save: marshal: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("catalog: save: mkdir: %w", err)
	}
	temp, err := os.CreateTemp(dir, catalogFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("catalog: save: temp file: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }() // no-op once renamed
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("catalog: save: write: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("catalog: save: close: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("catalog: save: rename: %w", err)
	}
	return nil
}

// Load reads and parses the saved YAML catalog copy.
func Load() (*Catalog, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("catalog: load: %w", err)
	}
	return parseYAML(data)
}

// LoadOrFetch prefers a fresh network fetch and, on success, persists it before
// returning. If the fetch fails but a saved copy exists, it returns the saved
// copy WITHOUT an error (offline-tolerant). It errors only when neither a live
// fetch nor a saved copy is available.
func LoadOrFetch(ctx context.Context, httpClient *http.Client, url string) (*Catalog, error) {
	fetched, fetchErr := Fetch(ctx, httpClient, url)
	if fetchErr == nil {
		// Best-effort persist; a save failure must not lose a good fetch.
		_ = Save(fetched)
		return fetched, nil
	}
	saved, loadErr := Load()
	if loadErr == nil {
		return saved, nil
	}
	return nil, fmt.Errorf("catalog: load-or-fetch: fetch failed (%v) and no saved copy (%v)", fetchErr, loadErr)
}

// LoadOrFetchStatus is LoadOrFetch but reports the data SOURCE so callers can
// message source availability. On a successful live fetch it persists the cache
// and returns (cat, SourceFresh, nil). When the live fetch fails it returns
// SourceCached: with a saved copy it returns (cachedCat, SourceCached, fetchErr)
// — the error is the live-fetch error so callers can warn while still using the
// cache; with no saved copy it returns (nil, SourceCached, fetchErr).
func LoadOrFetchStatus(ctx context.Context, httpClient *http.Client, url string) (*Catalog, Source, error) {
	fetched, fetchErr := Fetch(ctx, httpClient, url)
	if fetchErr == nil {
		// Best-effort persist; a save failure must not lose a good fetch.
		_ = Save(fetched)
		return fetched, SourceFresh, nil
	}
	saved, loadErr := Load()
	if loadErr == nil {
		return saved, SourceCached, fetchErr
	}
	return nil, SourceCached, fetchErr
}

// LoadCachedOrFetchStatus is CACHE-FIRST: when a cached catalog exists it is used
// as-is with NO network call (the catalog changes rarely). Only when none exists
// does it fetch (and persist). Callers use this for normal reads and
// LoadOrFetchStatus (fetch-first) only for an explicit refresh (the `r` key).
func LoadCachedOrFetchStatus(ctx context.Context, httpClient *http.Client, url string) (*Catalog, Source, error) {
	if cached, err := Load(); err == nil {
		return cached, SourceCached, nil
	}
	return LoadOrFetchStatus(ctx, httpClient, url)
}
