package main

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

// scrapedModel is one parameter-size VARIANT parsed off the ollama.com library
// page. It mirrors the fields of ollama.PopularModel — each library model expands
// into one scrapedModel per size variant (a model with no variants yields a single
// entry with an empty Parameters / SizeTag). The generator converts it to the
// embedded YAML.
//
//   - Name      the exact pullable reference incl. the size tag (e.g. "qwen2.5:7b"),
//     or the bare model name when the model lists no size variants.
//   - Parameters the single size (e.g. "7b"), or "" for a no-variant model.
//   - SizeTag   the registry manifest tag to fetch this variant's download size from
//     (the size label, e.g. "7b"); "latest" for a no-variant model.
//   - Model     the bare library model name (e.g. "qwen2.5"), used for the RepoURL.
//   - RepoURL   the model's ollama.com/library page (shared across its variants).
//   - PullCount the model's pull count (shared across its variants).
type scrapedModel struct {
	Name       string
	Parameters string
	SizeTag    string
	Model      string
	RepoURL    string
	PullCount  string
}

// Regexes over the ollama.com/library page's x-test-* markers (ollama.com has no
// API). Each <li … x-test-model …> … </li> is one model; the canonical name comes
// from the /library/<name> link, the size variants from x-test-size, and the pull
// count from x-test-pull-count. The (?s) flag lets the per-item slice span lines.
var (
	modelItemRe   = regexp.MustCompile(`(?s)<li[^>]*\bx-test-model\b.*?</li>`)
	hrefRe        = regexp.MustCompile(`href="(/library/[^"]+)"`)
	sizeRe        = regexp.MustCompile(`(?s)<span[^>]*\bx-test-size\b[^>]*>(.*?)</span>`)
	pullCountRe   = regexp.MustCompile(`(?s)<span[^>]*\bx-test-pull-count\b[^>]*>(.*?)</span>`)
	libraryNameRe = regexp.MustCompile(`^/library/([^/?#]+)`)
	tagStripRe    = regexp.MustCompile(`<[^>]*>`)
)

const repoBaseURL = "https://ollama.com"

// parseLibraryHTML extracts the listed models from the library page HTML, in page
// order (the page is sorted most-popular-first), de-duplicated by model name, and
// EXPANDED into one entry per parameter-size variant. A model that lists no size
// variants (e.g. an embedding model) yields a single bare entry (Parameters "",
// SizeTag "latest"). Page order is preserved: a model's variants follow each other
// in the order the page lists them, immediately after the previous model's variants.
// It is tolerant: a malformed item is skipped, never panics.
func parseLibraryHTML(html string) []scrapedModel {
	items := modelItemRe.FindAllString(html, -1)
	variants := make([]scrapedModel, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		hrefMatch := hrefRe.FindStringSubmatch(item)
		if hrefMatch == nil {
			continue
		}
		nameMatch := libraryNameRe.FindStringSubmatch(hrefMatch[1])
		if nameMatch == nil {
			continue
		}
		name := nameMatch[1]
		if name == "" || seen[name] {
			continue
		}
		var sizes []string
		for _, sizeMatch := range sizeRe.FindAllStringSubmatch(item, -1) {
			variant := stripTags(sizeMatch[1])
			if variant != "" {
				sizes = append(sizes, variant)
			}
		}
		pullCount := ""
		if pullMatch := pullCountRe.FindStringSubmatch(item); pullMatch != nil {
			pullCount = stripTags(pullMatch[1])
		}
		seen[name] = true
		repoURL := repoBaseURL + "/library/" + name
		if len(sizes) == 0 {
			// No size variants — a single bare entry (e.g. an embedding model). The
			// default-tag manifest gives its size.
			variants = append(variants, scrapedModel{
				Name:       name,
				Parameters: "",
				SizeTag:    "latest",
				Model:      name,
				RepoURL:    repoURL,
				PullCount:  pullCount,
			})
			continue
		}
		for _, size := range sizes {
			variants = append(variants, scrapedModel{
				Name:       name + ":" + size,
				Parameters: size,
				SizeTag:    size,
				Model:      name,
				RepoURL:    repoURL,
				PullCount:  pullCount,
			})
		}
	}
	return variants
}

func stripTags(fragment string) string {
	return strings.TrimSpace(tagStripRe.ReplaceAllString(fragment, ""))
}

// manifestResponse is the subset of a registry manifest we need: the layer sizes.
type manifestResponse struct {
	Layers []struct {
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
	} `json:"layers"`
}

// sumManifestLayers parses a registry manifest JSON document and sums its layer
// sizes (the large model layer dominates) — the default-tag download size in bytes.
func sumManifestLayers(reader io.Reader) (int64, error) {
	var parsed manifestResponse
	if err := json.NewDecoder(reader).Decode(&parsed); err != nil {
		return 0, err
	}
	var total int64
	for _, layer := range parsed.Layers {
		total += layer.Size
	}
	return total, nil
}
