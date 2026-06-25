package main

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

// scrapedModel is one model parsed off the ollama.com library page. It mirrors the
// fields of ollama.PopularModel; the generator converts it to the embedded YAML.
type scrapedModel struct {
	Name       string
	Parameters []string
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
// order (the page is sorted most-popular-first), de-duplicated by name. It is
// tolerant: a malformed item is skipped, never panics.
func parseLibraryHTML(html string) []scrapedModel {
	items := modelItemRe.FindAllString(html, -1)
	models := make([]scrapedModel, 0, len(items))
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
		var params []string
		for _, sizeMatch := range sizeRe.FindAllStringSubmatch(item, -1) {
			variant := stripTags(sizeMatch[1])
			if variant != "" {
				params = append(params, variant)
			}
		}
		pullCount := ""
		if pullMatch := pullCountRe.FindStringSubmatch(item); pullMatch != nil {
			pullCount = stripTags(pullMatch[1])
		}
		seen[name] = true
		models = append(models, scrapedModel{
			Name:       name,
			Parameters: params,
			RepoURL:    repoBaseURL + "/library/" + name,
			PullCount:  pullCount,
		})
	}
	return models
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
