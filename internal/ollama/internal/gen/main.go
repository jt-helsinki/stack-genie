// Command gen regenerates the bundled popular-models snapshot
// (internal/ollama/models.yaml) by scraping ollama.com.
//
// It fetches the most-popular library listing (ollama.com/library?sort=popular),
// parses the listed models by their x-test-* markers (ollama.com has no API), caps
// to the top N, looks up each model's default-tag (`latest`) download size from the
// public Ollama registry manifest, and writes the YAML snapshot the runtime ollama
// package embeds. The RUNTIME never does this — Popular() only reads the embedded
// file. This tool does the only live HTTP, and only when a maintainer runs it.
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
	"time"
)

const (
	libraryURL      = "https://ollama.com/library?sort=popular"
	registryBaseURL = "https://registry.ollama.ai"
	// topN caps the snapshot to the most popular models — enough to be a useful
	// picker without bloating the binary or hammering the registry.
	topN           = 30
	httpTimeout    = 20 * time.Second
	perModelBudget = 10 * time.Second
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
	models := parseLibraryHTML(string(html))
	if len(models) == 0 {
		return fmt.Errorf("could not parse any models from the library page (markup may have changed)")
	}
	if len(models) > topN {
		models = models[:topN]
	}
	_, _ = fmt.Fprintf(os.Stderr, "parsed %d models; looking up download sizes\n", len(models))

	sizes := make([]int64, len(models))
	for index, model := range models {
		size, sizeErr := manifestSize(client, model.Name, "latest")
		if sizeErr != nil {
			// A missing/odd size must not fail the whole snapshot — it degrades to 0
			// (rendered "—"), just like the old runtime behaviour.
			_, _ = fmt.Fprintf(os.Stderr, "  %-24s size unavailable: %v\n", model.Name, sizeErr)
		}
		sizes[index] = size
		_, _ = fmt.Fprintf(os.Stderr, "  %-24s %d bytes\n", model.Name, size)
	}

	out := renderYAML(models, sizes)
	dest, err := outputPath()
	if err != nil {
		return err
	}
	if err := os.WriteFile(dest, []byte(out), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	_, _ = fmt.Fprintf(os.Stderr, "wrote %s (%d models)\n", dest, len(models))
	return nil
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
	return io.ReadAll(io.LimitReader(response.Body, 8<<20))
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

// renderYAML formats the scraped models + sizes into the models.yaml document.
func renderYAML(models []scrapedModel, sizes []int64) string {
	var builder strings.Builder
	builder.WriteString("# Popular Ollama models — a BUNDLED, regenerable snapshot. DO NOT hand-edit.\n")
	builder.WriteString("#\n")
	builder.WriteString("# Source: https://ollama.com/library?sort=popular (the most-pulled models),\n")
	builder.WriteString("# captured by the regeneration tool. download_size is the default-tag (`latest`)\n")
	builder.WriteString("# manifest layer-size sum from https://registry.ollama.ai. The runtime ollama\n")
	builder.WriteString("# package EMBEDS this file and never re-scrapes; regenerate with one of:\n")
	builder.WriteString("#   make models-refresh\n")
	builder.WriteString("#   go generate ./internal/ollama/...\n")
	builder.WriteString("models:\n")
	for index, model := range models {
		_, _ = fmt.Fprintf(&builder, "  - name: %s\n", model.Name)
		if len(model.Parameters) > 0 {
			_, _ = fmt.Fprintf(&builder, "    parameters: [%s]\n", strings.Join(model.Parameters, ", "))
		}
		if sizes[index] > 0 {
			_, _ = fmt.Fprintf(&builder, "    download_size: %d\n", sizes[index])
		}
		_, _ = fmt.Fprintf(&builder, "    repo_url: %s\n", model.RepoURL)
		if model.PullCount != "" {
			_, _ = fmt.Fprintf(&builder, "    pull_count: %q\n", model.PullCount)
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
