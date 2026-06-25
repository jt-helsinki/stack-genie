package ollama

import (
	"strconv"
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

	// The snapshot is now the FULL ollama.com library expanded into size variants —
	// hundreds of entries. A generous floor guards against a truncated/failed regen
	// (the library has 230+ models, 390+ variants).
	if len(models) < 100 {
		test.Fatalf("snapshot has only %d entries, expected the FULL library (hundreds)", len(models))
	}
	// The expansion produces many tagged size variants.
	if withSizeTag == 0 {
		test.Fatal("no entry carries a size tag; the per-variant expansion likely failed")
	}
	// Sizes are baked from the registry; most entries should carry one (some degrade
	// to 0/"—" when a manifest didn't resolve, but the bulk must have a size).
	if withSize == 0 {
		test.Fatal("no entry in the snapshot has a download_size; the bake likely failed")
	}
}

// TestPopularIsSortedAlphaThenParams asserts the embedded snapshot is ordered
// ALPHABETICALLY by bare model name (case-insensitive), then ascending by parameter
// size within a model — the order the maintainer regen writes.
func TestPopularIsSortedAlphaThenParams(test *testing.T) {
	models, err := Popular()
	if err != nil {
		test.Fatalf("Popular: %v", err)
	}
	for index := 1; index < len(models); index++ {
		prevModel := bareName(models[index-1].Name)
		curModel := bareName(models[index].Name)
		if curModel < prevModel {
			test.Fatalf("snapshot not alpha-sorted at %d: %q after %q",
				index, models[index].Name, models[index-1].Name)
		}
		if curModel == prevModel {
			prevSize := paramMagnitude(models[index-1].Parameters)
			curSize := paramMagnitude(models[index].Parameters)
			if curSize < prevSize {
				test.Fatalf("within %q not size-ascending at %d: %q (%g) after %q (%g)",
					curModel, index, models[index].Name, curSize, models[index-1].Name, prevSize)
			}
		}
	}
}

// bareName lowercases a model reference and strips its ":<size>" tag.
func bareName(name string) string {
	if colon := strings.IndexByte(name, ':'); colon >= 0 {
		name = name[:colon]
	}
	return strings.ToLower(name)
}

// paramMagnitude converts a parameter-size label into a comparable magnitude so the
// within-model order can be checked (270m < 1b < 3b < 70b); unparseable/empty → 0.
func paramMagnitude(label string) float64 {
	label = strings.TrimSpace(strings.ToLower(label))
	if label == "" {
		return 0
	}
	suffix := byte(0)
	if last := label[len(label)-1]; last == 'm' || last == 'b' {
		suffix = last
		label = label[:len(label)-1]
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(label), 64)
	if err != nil {
		return 0
	}
	switch suffix {
	case 'm':
		return value * 1e6
	case 'b':
		return value * 1e9
	default:
		return value
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
