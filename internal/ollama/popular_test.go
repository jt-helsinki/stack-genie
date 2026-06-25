package ollama

import (
	"strings"
	"testing"
)

// TestPopularReadsEmbeddedSnapshot asserts Popular() parses the BAKED, embedded
// models.yaml into well-formed entries. It does NO network access — the snapshot is
// compiled into the test binary via go:embed.
func TestPopularReadsEmbeddedSnapshot(test *testing.T) {
	models, err := Popular()
	if err != nil {
		test.Fatalf("Popular: %v", err)
	}
	if len(models) == 0 {
		test.Fatal("Popular returned no models; the embedded snapshot is empty")
	}

	seen := map[string]bool{}
	var withSize, withSizeTag int
	for _, model := range models {
		if strings.TrimSpace(model.Name) == "" {
			test.Fatalf("a model has an empty Name: %+v", model)
		}
		if model.RepoURL == "" {
			test.Fatalf("%s has an empty RepoURL", model.Name)
		}
		if !strings.HasPrefix(model.RepoURL, "https://ollama.com/library/") {
			test.Fatalf("%s RepoURL = %q, want an ollama.com/library URL", model.Name, model.RepoURL)
		}
		// Entries are per-variant: Name is the exact pullable ref. A sized variant
		// looks like "<model>:<size>" with Parameters == that size, and its Name ends
		// in ":<Parameters>". A no-variant model (e.g. an embedding) has an empty
		// Parameters and a bare Name (no tag). RepoURL always ends in the bare model
		// name (the part before any ":").
		bareModel := model.Name
		if colon := strings.IndexByte(model.Name, ':'); colon >= 0 {
			bareModel = model.Name[:colon]
		}
		if !strings.HasSuffix(model.RepoURL, "/"+bareModel) {
			test.Fatalf("%s RepoURL = %q, want it to end in /%s", model.Name, model.RepoURL, bareModel)
		}
		if model.Parameters == "" {
			if strings.Contains(model.Name, ":") {
				test.Fatalf("%s has empty Parameters but a tagged Name", model.Name)
			}
		} else {
			if !strings.HasSuffix(model.Name, ":"+model.Parameters) {
				test.Fatalf("%s Name does not end in its size tag :%s", model.Name, model.Parameters)
			}
			withSizeTag++
		}
		if model.DownloadSize < 0 {
			test.Fatalf("%s DownloadSize = %d, must not be negative", model.Name, model.DownloadSize)
		}
		if model.DownloadSize > 0 {
			withSize++
		}
		if seen[model.Name] {
			test.Fatalf("duplicate entry %q in the snapshot", model.Name)
		}
		seen[model.Name] = true
	}

	// The snapshot is a curated top-N list expanded into size variants; a handful is
	// the floor.
	if len(models) < 5 {
		test.Fatalf("snapshot has only %d entries, expected a fuller popular list", len(models))
	}
	// The expansion produces many tagged size variants.
	if withSizeTag == 0 {
		test.Fatal("no entry carries a size tag; the per-variant expansion likely failed")
	}
	// Sizes are baked from the registry; entries should carry one.
	if withSize == 0 {
		test.Fatal("no entry in the snapshot has a download_size; the bake likely failed")
	}
}

// TestPopularIsStable confirms repeated reads are deterministic (pure embedded
// read, no network, no shared mutation).
func TestPopularIsStable(test *testing.T) {
	first, err := Popular()
	if err != nil {
		test.Fatalf("Popular: %v", err)
	}
	second, err := Popular()
	if err != nil {
		test.Fatalf("Popular (second): %v", err)
	}
	if len(first) != len(second) {
		test.Fatalf("Popular returned %d then %d models", len(first), len(second))
	}
	for index := range first {
		if first[index].Name != second[index].Name {
			test.Fatalf("order/content differs at %d: %q vs %q", index, first[index].Name, second[index].Name)
		}
	}
}

// TestLoadPopularRejectsMalformedYAML covers the only error path: a malformed
// embedded document.
func TestLoadPopularRejectsMalformedYAML(test *testing.T) {
	var file popularFile
	if err := parseInto([]byte("models: [this is : not valid"), &file); err == nil {
		test.Fatal("expected a parse error on malformed YAML")
	}
}
