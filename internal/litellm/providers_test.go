package litellm

import (
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/catalog"
)

// testCatalog parses a small catalog with both LiteLLM-routable providers
// (openai, google) and an unroutable one (a fictional router "megarouter") so the
// filter's in/out behavior is observable.
func testCatalog(test *testing.T) *catalog.Catalog {
	test.Helper()
	const data = `{
		"models": {
			"openai/gpt-5.5":        {"id": "openai/gpt-5.5", "name": "GPT-5.5", "family": "gpt"},
			"google/gemini-3.1-pro": {"id": "google/gemini-3.1-pro", "name": "Gemini 3.1 Pro", "family": "gemini", "limit": {"context": 1000000, "output": 8192}, "modalities": {"input": ["text","image"], "output": ["text"]}},
			"megarouter/some-model": {"id": "megarouter/some-model", "name": "Some Model"}
		},
		"providers": {
			"openai":     {"id": "openai", "name": "OpenAI"},
			"google":     {"id": "google", "name": "Google"},
			"megarouter": {"id": "megarouter", "name": "MegaRouter"}
		}
	}`
	cat, err := catalog.Parse([]byte(data))
	if err != nil {
		test.Fatalf("parse test catalog: %v", err)
	}
	return cat
}

// TestLiteLLMPrefix pins the catalog→LiteLLM prefix mapping: identity for matching
// ids, the google→gemini rewrite, and not-ok for an unmapped provider.
func TestLiteLLMPrefix(test *testing.T) {
	cases := []struct {
		catalogID  string
		wantPrefix string
		wantOK     bool
	}{
		{"openai", "openai", true},
		{"anthropic", "anthropic", true},
		{"groq", "groq", true},
		{"mistral", "mistral", true},
		{"google", "gemini", true}, // the notable mismatch
		{"google-ai", "gemini", true},
		{"megarouter", "", false}, // unmapped → filtered out
		{"totally-unknown", "", false},
	}
	for _, testCase := range cases {
		prefix, ok := LiteLLMPrefix(testCase.catalogID)
		if ok != testCase.wantOK || prefix != testCase.wantPrefix {
			test.Errorf("LiteLLMPrefix(%q) = (%q, %v), want (%q, %v)",
				testCase.catalogID, prefix, ok, testCase.wantPrefix, testCase.wantOK)
		}
	}
}

// TestLiteLLMProvidersFilter verifies the catalog providers are split into the
// routable (kept) and unroutable (dropped) sets.
func TestLiteLLMProvidersFilter(test *testing.T) {
	cat := testCatalog(test)

	usable := LiteLLMProviders(cat)
	usableIDs := make([]string, 0, len(usable))
	for _, provider := range usable {
		usableIDs = append(usableIDs, provider.ID)
	}
	// Sorted by catalog order: google, openai (megarouter dropped).
	if strings.Join(usableIDs, ",") != "google,openai" {
		test.Errorf("LiteLLMProviders ids = %v, want [google openai]", usableIDs)
	}

	out := UnsupportedProviders(cat)
	if len(out) != 1 || out[0].ID != "megarouter" {
		test.Errorf("UnsupportedProviders = %v, want [megarouter]", out)
	}

	// nil catalog is tolerated.
	if LiteLLMProviders(nil) != nil {
		test.Error("LiteLLMProviders(nil) should be nil")
	}
}

// TestLiteLLMModelParam pins the id-prefix rewrite: google→gemini, identity for a
// matching provider, and verbatim for an unmapped/slashless id.
func TestLiteLLMModelParam(test *testing.T) {
	cases := map[string]string{
		"google/gemini-3.1-pro": "gemini/gemini-3.1-pro", // rewrite
		"openai/gpt-5.5":        "openai/gpt-5.5",        // identity
		"anthropic/claude-opus": "anthropic/claude-opus", // identity
		"megarouter/x":          "megarouter/x",          // unmapped → verbatim
		"no-slash-id":           "no-slash-id",           // no provider prefix → verbatim
	}
	for input, want := range cases {
		if got := LiteLLMModelParam(input); got != want {
			test.Errorf("LiteLLMModelParam(%q) = %q, want %q", input, got, want)
		}
	}
}
