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
	var withSize int
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
		if !strings.HasSuffix(model.RepoURL, "/"+model.Name) {
			test.Fatalf("%s RepoURL = %q, want it to end in /%s", model.Name, model.RepoURL, model.Name)
		}
		if model.DownloadSize < 0 {
			test.Fatalf("%s DownloadSize = %d, must not be negative", model.Name, model.DownloadSize)
		}
		if model.DownloadSize > 0 {
			withSize++
		}
		if seen[model.Name] {
			test.Fatalf("duplicate model %q in the snapshot", model.Name)
		}
		seen[model.Name] = true
	}

	// The snapshot is a curated top-N list; a handful is the floor.
	if len(models) < 5 {
		test.Fatalf("snapshot has only %d models, expected a fuller popular list", len(models))
	}
	// Sizes are baked from the registry; most models should carry one.
	if withSize == 0 {
		test.Fatal("no model in the snapshot has a download_size; the bake likely failed")
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
