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
// is an Ollama-local concept and is intentionally absent here.
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

	"github.com/jt-helsinki/ideal-robot/internal/paths"
)

// DefaultURL is the canonical models.dev catalog endpoint.
const DefaultURL = "https://models.dev/catalog.json"

// catalogFileName is the on-disk name under the volumes dir.
const catalogFileName = "catalog.json"

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
	ID          string `json:"id"`
	Name        string `json:"name"`
	Family      string `json:"family"`
	Knowledge   string `json:"knowledge"`
	ReleaseDate string `json:"release_date"`
	LastUpdated string `json:"last_updated"`
	OpenWeights bool   `json:"open_weights"`
	Reasoning   bool   `json:"reasoning"`
	ToolCall    bool   `json:"tool_call"`
	Attachment  bool   `json:"attachment"`

	StructuredOutput bool `json:"structured_output"`
	Temperature      bool `json:"temperature"`

	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
		Input   int `json:"input"`
	} `json:"limit"`

	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
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
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Env  []string `json:"env"`
}

// rawCatalog mirrors the catalog's two top-level keys.
type rawCatalog struct {
	Models    map[string]rawModel    `json:"models"`
	Providers map[string]rawProvider `json:"providers"`
}

// Catalog is the parsed, provider-grouped model catalog. It also retains the raw
// bytes it was parsed from so Save can persist the exact source-of-truth JSON.
type Catalog struct {
	providers map[string]Provider // keyed by provider id
	order     []string            // provider ids, sorted ascending
	raw       []byte              // the exact JSON this Catalog was parsed from
}

// Parse parses catalog JSON into a provider-grouped Catalog. It tolerates extra
// fields (the catalog carries many we don't model). The supplied bytes are
// retained so Save can write back the exact source JSON.
func Parse(data []byte) (*Catalog, error) {
	var raw rawCatalog
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("catalog: parse: %w", err)
	}
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

	return &Catalog{providers: providers, order: order, raw: data}, nil
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

// Raw returns the exact JSON bytes this Catalog was parsed from.
func (catalog *Catalog) Raw() []byte {
	return catalog.raw
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

// Path returns the on-disk catalog location: ~/.ai-platform/volumes/catalog.json.
func Path() (string, error) {
	volumes, err := paths.VolumesDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(volumes, catalogFileName), nil
}

// Save writes the catalog's RAW source JSON atomically (temp file + rename) so
// the on-disk copy is byte-for-byte the source-of-truth JSON. It creates the
// volumes dir on demand.
func Save(catalog *Catalog) error {
	if catalog == nil || len(catalog.raw) == 0 {
		return errors.New("catalog: save: nothing to write")
	}
	path, err := Path()
	if err != nil {
		return err
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
	if _, err := temp.Write(catalog.raw); err != nil {
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

// Load reads and parses the saved catalog copy.
func Load() (*Catalog, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("catalog: load: %w", err)
	}
	return Parse(data)
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
