package agentcfg

import (
	"sort"
	"testing"
)

// TestCloudModelsParsesEmbeddedSeed asserts the embedded curated seed parses into a
// non-empty, sorted, deduped list that includes well-known handles across providers.
func TestCloudModelsParsesEmbeddedSeed(test *testing.T) {
	models := CloudModels()
	if len(models) == 0 {
		test.Fatal("CloudModels() returned an empty list — the embedded seed failed to parse")
	}

	// Sorted.
	if !sort.StringsAreSorted(models) {
		test.Errorf("CloudModels() is not sorted: %v", models)
	}

	// Deduped.
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if _, dup := seen[model]; dup {
			test.Errorf("CloudModels() has a duplicate entry: %q", model)
		}
		seen[model] = struct{}{}
	}

	// Includes a known ref per provider so the seed stays usefully populated.
	for _, want := range []string{
		"openai/gpt-5.5",
		"anthropic/claude-opus-4-8",
		"gemini/gemini-3.5-flash",
		"groq/llama-3.3-70b-versatile",
	} {
		if _, ok := seen[want]; !ok {
			test.Errorf("CloudModels() is missing the expected handle %q (got %v)", want, models)
		}
	}
}
