package views

import (
	"errors"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/catalog"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/ollama"
)

// REGRESSION GUARD: Cloud Models must still render the registered (keyed) models when the
// catalog comes from the CACHE (offline / models.dev unreachable) — the tab must not blank
// just because the live fetch failed and the cached copy was used.
func TestCloudModelsRendersRegisteredWithCachedCatalog(test *testing.T) {
	cat := cloudTestCatalog(test)
	view := NewCloudModels(
		func() (*catalog.Catalog, catalog.Source, error) { return cat, catalog.SourceCached, nil },
		func() ([]litellm.LiveModel, error) {
			return []litellm.LiveModel{{Name: "openai/gpt-5.5", Provider: "openai"}}, nil
		},
		nil, noTest,
	)
	view.SetSize(120, 40)
	_ = view.Update(view.Init()())
	out := view.View()
	if !strings.Contains(out, "openai/gpt-5.5") {
		test.Errorf("cloud models must render the registered model from a cached catalog:\n%s", out)
	}
}

// REGRESSION GUARD: Local Models must render the installable library even when the
// installed-store lister (Ollama) errors — the two sides degrade independently, so an
// Ollama outage never blanks the whole tab.
func TestLocalModelsRendersLibraryWhenStoreErrors(test *testing.T) {
	view := NewLocalModels(
		func() ([]ollama.Model, error) { return nil, errors.New("ollama down") },
		freshLibrary([]ollama.LibraryModel{{Name: "llama3.2", Description: "Meta Llama"}}),
		nil, nil, noTest, nil,
	)
	view.SetSize(120, 40)
	drive(view, view.Init())
	out := view.View()
	if !strings.Contains(out, "llama3.2") {
		test.Errorf("installable library must render despite an installed-store error:\n%s", out)
	}
}
