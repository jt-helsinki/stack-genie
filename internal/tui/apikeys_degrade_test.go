package tui

import (
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/catalog"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
)

// apiKeyTestCatalogJSON has two LiteLLM-routable providers (openai, google).
const apiKeyTestCatalogJSON = `{
	"models": {
		"openai/gpt-5.5": {"id": "openai/gpt-5.5", "name": "GPT-5.5"},
		"google/gemini-3.1-pro": {"id": "google/gemini-3.1-pro", "name": "Gemini 3.1 Pro"}
	},
	"providers": {
		"openai": {"id": "openai", "name": "OpenAI"},
		"google": {"id": "google", "name": "Google"}
	}
}`

func apiKeyTestCatalog(test *testing.T) *catalog.Catalog {
	test.Helper()
	cat, err := catalog.Parse([]byte(apiKeyTestCatalogJSON))
	if err != nil {
		test.Fatalf("parse catalog: %v", err)
	}
	return cat
}

// REGRESSION GUARD: an unreachable gateway (ListCredentials error → nil creds) must NOT
// blank the API Keys tab. The routable providers are known from the catalog and must still
// render, all marked unkeyed. Previously listAPIKeyProviders returned the error, blanking
// the whole provider list when the gateway/services were down.
func TestAPIKeyProviderRowsDegradeWithoutCreds(test *testing.T) {
	rows := apiKeyProviderRows(apiKeyTestCatalog(test), nil)
	if len(rows) == 0 {
		test.Fatal("gateway-down (nil creds) must still list the catalog providers, got none")
	}
	for _, row := range rows {
		if row.HasKey {
			test.Errorf("with no credentials every provider must be unkeyed, got HasKey for %q", row.Provider)
		}
		if row.Models == 0 {
			test.Errorf("provider %q should carry its catalog model count", row.Provider)
		}
	}
}

// With a credential present, only the matching provider is marked keyed.
func TestAPIKeyProviderRowsMarksKeyed(test *testing.T) {
	prefix, ok := litellm.LiteLLMPrefix("openai")
	if !ok {
		test.Fatal("openai must be a LiteLLM-routable provider")
	}
	rows := apiKeyProviderRows(apiKeyTestCatalog(test), []litellm.Credential{{Provider: prefix}})
	sawOpenAI := false
	for _, row := range rows {
		switch row.Provider {
		case "openai":
			sawOpenAI = true
			if !row.HasKey {
				test.Error("openai has a credential and must be marked keyed")
			}
		default:
			if row.HasKey {
				test.Errorf("provider %q has no credential and must be unkeyed", row.Provider)
			}
		}
	}
	if !sawOpenAI {
		test.Errorf("openai provider row missing: %+v", rows)
	}
}
