// Command gen regenerates the bundled popular-models snapshot
// (internal/ollama/models.yaml) by scraping ollama.com.
//
// It fetches the FULL library listing (ollama.com/library?sort=popular — a single
// page lists EVERY model; ollama.com does not paginate the library), parses the
// listed models by their x-test-* markers (ollama.com has no API), expands each
// model into one entry per parameter-size variant, looks up each variant's per-tag
// download size from the public Ollama registry manifest (bounded-concurrently, with
// a per-request timeout — a variant whose manifest doesn't resolve in time DEGRADES
// to size 0 rather than being dropped, so coverage stays complete), sorts the whole
// list ALPHABETICALLY by model name then ascending by parameter size, and writes the
// YAML snapshot the runtime ollama package embeds. The RUNTIME never does this —
// Popular() only reads the embedded file. This tool does the only live HTTP, and only
// when a maintainer runs it.
//
// Run it via:
//
//	make models-refresh
//	# or
//	go generate ./internal/ollama/...
//	# or directly (from the repo root)
//	go run ./internal/ollama/internal/gen
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	libraryURL      = "https://ollama.com/library?sort=popular"
	registryBaseURL = "https://registry.ollama.ai"
	// httpTimeout bounds the single library-page fetch.
	httpTimeout = 30 * time.Second
	// perModelBudget bounds EACH per-tag manifest lookup; a slow/unresolvable tag
	// times out and that variant degrades to size 0 rather than hanging the regen.
	perModelBudget = 8 * time.Second
	// sizeFetchWorkers bounds concurrent per-tag manifest lookups. The full library
	// is hundreds of variants, so a generous-but-bounded pool keeps the one-time
	// maintainer regen to minutes while never running unbounded.
	sizeFetchWorkers = 14
	// libraryByteCap bounds the library-page read. The full /library page is ~0.8 MB;
	// allow generous headroom for growth.
	libraryByteCap = 16 << 20
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "models-refresh: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	client := &http.Client{Timeout: httpTimeout}

	_, _ = fmt.Fprintf(os.Stderr, "fetching %s\n", libraryURL)
	html, err := fetch(client, libraryURL)
	if err != nil {
		return fmt.Errorf("fetching library page: %w", err)
	}
	variants := parseLibraryHTML(string(html))
	if len(variants) == 0 {
		return fmt.Errorf("could not parse any models from the library page (markup may have changed)")
	}
	// Capture the ENTIRE library — every model, every size variant. No top-N cap.
	_, _ = fmt.Fprintf(os.Stderr, "parsed %d size-variant entries; looking up per-tag download sizes\n", len(variants))

	sizes := fetchVariantSizes(client, variants)

	// COMPLETE coverage: keep every variant. A variant whose per-tag manifest could
	// not be resolved (timeout / missing tag / registry hiccup) DEGRADES to size 0
	// (rendered as "—") rather than being dropped — the goal is the full list.
	unresolved := 0
	for index, variant := range variants {
		if sizes[index] <= 0 {
			unresolved++
			_, _ = fmt.Fprintf(os.Stderr, "  %-32s size unavailable — kept as —\n", variant.Name)
			continue
		}
		_, _ = fmt.Fprintf(os.Stderr, "  %-32s %d bytes\n", variant.Name, sizes[index])
	}

	// Snapshot the size-by-name map BEFORE sorting (sortVariants reorders in place,
	// which would otherwise desync the parallel sizes slice).
	sizeByName := make(map[string]int64, len(variants))
	for index, variant := range variants {
		sizeByName[variant.Name] = sizes[index]
	}
	// Sort ALPHA by model name, then ascending by parameter size within a model.
	ordered := sortVariants(variants)
	orderedSizes := make([]int64, len(ordered))
	for index, variant := range ordered {
		orderedSizes[index] = sizeByName[variant.Name]
	}

	out := renderYAML(ordered, orderedSizes)
	dest, err := outputPath()
	if err != nil {
		return err
	}
	if err := os.WriteFile(dest, []byte(out), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	_, _ = fmt.Fprintf(os.Stderr, "wrote %s (%d size-variant entries; %d without a resolved size)\n",
		dest, len(ordered), unresolved)
	return nil
}

// fetchVariantSizes looks up each variant's per-tag download size from the registry,
// bounded-concurrently. A failed/timed-out lookup yields 0 (the caller keeps it as
// an unknown size).
func fetchVariantSizes(client *http.Client, variants []scrapedModel) []int64 {
	sizes := make([]int64, len(variants))
	var waitGroup sync.WaitGroup
	tokens := make(chan struct{}, sizeFetchWorkers)
	for index, variant := range variants {
		waitGroup.Add(1)
		tokens <- struct{}{}
		go func(index int, variant scrapedModel) {
			defer waitGroup.Done()
			defer func() { <-tokens }()
			size, sizeErr := manifestSize(client, variant.Model, variant.SizeTag)
			if sizeErr != nil {
				_, _ = fmt.Fprintf(os.Stderr, "  %-28s lookup failed: %v\n", variant.Name, sizeErr)
				return
			}
			sizes[index] = size
		}(index, variant)
	}
	waitGroup.Wait()
	return sizes
}

// fetch GETs url and returns its body (bounded), erroring on non-200.
func fetch(client *http.Client, url string) ([]byte, error) {
	response, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, libraryByteCap))
}

// manifestSize fetches the registry manifest for <model>:<tag> and sums its layers.
func manifestSize(client *http.Client, model, tag string) (int64, error) {
	scoped := &http.Client{Timeout: perModelBudget, Transport: client.Transport}
	url := fmt.Sprintf("%s/v2/library/%s/manifests/%s", registryBaseURL, model, tag)
	response, err := scoped.Get(url)
	if err != nil {
		return 0, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("registry status %d", response.StatusCode)
	}
	return sumManifestLayers(io.LimitReader(response.Body, 1<<20))
}

// renderYAML formats the scraped per-variant entries + sizes into the models.yaml
// document. One YAML entry per parameter-size variant (name includes the size tag).
func renderYAML(variants []scrapedModel, sizes []int64) string {
	var builder strings.Builder
	builder.WriteString("# Ollama models — a BUNDLED, regenerable snapshot of the FULL library. DO NOT hand-edit.\n")
	builder.WriteString("#\n")
	builder.WriteString("# Source: https://ollama.com/library?sort=popular (EVERY model in the library),\n")
	builder.WriteString("# captured by the regeneration tool. ONE entry PER parameter-size variant,\n")
	builder.WriteString("# sorted ALPHABETICALLY by model name then ascending by parameter size:\n")
	builder.WriteString("# name is the exact pullable ref incl. the size tag (e.g. qwen2.5:7b) and\n")
	builder.WriteString("# download_size is THAT tag's manifest layer-size sum from\n")
	builder.WriteString("# https://registry.ollama.ai (omitted when the manifest did not resolve —\n")
	builder.WriteString("# rendered as \"—\"). The runtime ollama package EMBEDS this file and\n")
	builder.WriteString("# never re-scrapes; regenerate with one of:\n")
	builder.WriteString("#   make models-refresh\n")
	builder.WriteString("#   go generate ./internal/ollama/...\n")
	builder.WriteString("models:\n")
	for index, variant := range variants {
		_, _ = fmt.Fprintf(&builder, "  - name: %s\n", variant.Name)
		if variant.Parameters != "" {
			_, _ = fmt.Fprintf(&builder, "    parameters: %s\n", variant.Parameters)
		}
		if sizes[index] > 0 {
			_, _ = fmt.Fprintf(&builder, "    download_size: %d\n", sizes[index])
		}
		_, _ = fmt.Fprintf(&builder, "    repo_url: %s\n", variant.RepoURL)
		if variant.PullCount != "" {
			_, _ = fmt.Fprintf(&builder, "    pull_count: %q\n", variant.PullCount)
		}
	}
	return builder.String()
}

// outputPath resolves internal/ollama/models.yaml relative to this source file, so
// the tool writes to the right place regardless of the working directory.
func outputPath() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot resolve generator source path")
	}
	// thisFile = .../internal/ollama/internal/gen/main.go
	ollamaDir := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	dest := filepath.Join(ollamaDir, "models.yaml")
	if _, err := os.Stat(ollamaDir); err != nil {
		return "", fmt.Errorf("resolving output dir %s: %w", ollamaDir, err)
	}
	return dest, nil
}
