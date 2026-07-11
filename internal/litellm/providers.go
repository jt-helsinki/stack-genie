package litellm

// Catalog → LiteLLM provider mapping (Phase B). The models.dev catalog
// (internal/catalog) groups models by a provider id taken from the verbatim
// "<provider>/<model>" id. LiteLLM routes a model by its own provider PREFIX
// (the leading segment before the first "/"), which is MOSTLY identical to the
// catalog provider id but differs in a few well-known cases — notably the
// catalog's "google" provider routes under LiteLLM's "gemini" prefix.
//
// A catalog provider is USABLE by the platform iff it maps to a LiteLLM-routable
// prefix (i.e. it appears in litellmProviderPrefix). Providers that have no
// LiteLLM mapping (routers/aggregators/unknown vendors the catalog also lists)
// are filtered OUT — the platform cannot register their models against LiteLLM.

import (
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/catalog"
)

// litellmProviderPrefix maps a models.dev catalog provider id to the LiteLLM
// provider prefix used in litellm_params.model. The map is the SOURCE OF TRUTH
// for which catalog providers are LiteLLM-routable: a catalog provider id absent
// from this map is treated as not usable and filtered out.
//
// Entries are identity where the catalog id already equals the LiteLLM prefix
// (openai, anthropic, groq, mistral, deepseek, cohere, …) and an explicit
// rewrite where they differ. Verified against LiteLLM's documented providers
// (docs.litellm.ai/docs/providers); the notable mismatch is google → gemini
// (the catalog uses "google" for the Gemini family, LiteLLM routes it as
// "gemini/<model>").
var litellmProviderPrefix = map[string]string{
	// Identity — catalog id == LiteLLM prefix.
	"openai":      "openai",
	"anthropic":   "anthropic",
	"groq":        "groq",
	"mistral":     "mistral",
	"deepseek":    "deepseek",
	"cohere":      "cohere",
	"openrouter":  "openrouter",
	"ollama":      "ollama",
	"xai":         "xai",
	"perplexity":  "perplexity",
	"fireworks":   "fireworks_ai",
	"together":    "together_ai",
	"deepinfra":   "deepinfra",
	"cerebras":    "cerebras",
	"sambanova":   "sambanova",
	"ai21":        "ai21",
	"voyage":      "voyage",
	"nvidia":      "nvidia_nim",
	"databricks":  "databricks",
	"cloudflare":  "cloudflare",
	"vertex":      "vertex_ai",
	"bedrock":     "bedrock",
	"azure":       "azure",
	"replicate":   "replicate",
	"anyscale":    "anyscale",
	"huggingface": "huggingface",
	// Mismatches — catalog id != LiteLLM prefix.
	"google":       "gemini",       // models.dev "google" → LiteLLM "gemini"
	"google-ai":    "gemini",       // some catalog snapshots use google-ai
	"x-ai":         "xai",          // alternate spelling
	"mistralai":    "mistral",      // alternate spelling
	"fireworks-ai": "fireworks_ai", // alternate spelling
	"together-ai":  "together_ai",  // alternate spelling
}

// LiteLLMPrefix returns the LiteLLM provider prefix for a catalog provider id and
// whether the provider is LiteLLM-routable. ok is false for a provider with no
// mapping (filtered out of the desired model set).
func LiteLLMPrefix(catalogProviderID string) (string, bool) {
	prefix, ok := litellmProviderPrefix[catalogProviderID]
	return prefix, ok
}

// LiteLLMProviders returns the catalog providers that map to a LiteLLM-routable
// prefix, preserving the catalog's sorted order. Providers with no LiteLLM
// mapping are dropped. A nil catalog yields nil.
func LiteLLMProviders(cat *catalog.Catalog) []catalog.Provider {
	if cat == nil {
		return nil
	}
	all := cat.Providers()
	usable := make([]catalog.Provider, 0, len(all))
	for _, provider := range all {
		if _, ok := litellmProviderPrefix[provider.ID]; ok {
			usable = append(usable, provider)
		}
	}
	return usable
}

// UnsupportedProviders returns the catalog providers that DO NOT map to a LiteLLM
// prefix (the filtered-out set), preserving the catalog's sorted order — useful
// for reporting which providers were dropped.
func UnsupportedProviders(cat *catalog.Catalog) []catalog.Provider {
	if cat == nil {
		return nil
	}
	all := cat.Providers()
	out := make([]catalog.Provider, 0)
	for _, provider := range all {
		if _, ok := litellmProviderPrefix[provider.ID]; !ok {
			out = append(out, provider)
		}
	}
	return out
}

// LiteLLMModelParam rewrites a catalog model id's provider prefix to the LiteLLM
// prefix, leaving the model portion untouched: "google/gemini-3.1-pro" →
// "gemini/gemini-3.1-pro"; identity when the catalog provider already matches the
// LiteLLM prefix ("openai/gpt-5.5" → "openai/gpt-5.5"); and identity (returned
// verbatim) for an id with no "/" or whose provider has no mapping (the caller is
// expected to have filtered to LiteLLMProviders, but this never panics).
func LiteLLMModelParam(catalogModelID string) string {
	slash := strings.IndexByte(catalogModelID, '/')
	if slash < 0 {
		return catalogModelID
	}
	providerID := catalogModelID[:slash]
	rest := catalogModelID[slash+1:]
	prefix, ok := litellmProviderPrefix[providerID]
	if !ok {
		return catalogModelID
	}
	return prefix + "/" + rest
}
