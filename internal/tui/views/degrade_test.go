package views

import (
	"errors"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/catalog"
	"github.com/jt-helsinki/stack-genie/internal/hf"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
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

// REGRESSION GUARD: Local Models must render the curated Available section even when
// the installed-store lister (`hf cache ls`) errors — the two sides degrade
// independently, so a listing outage never blanks the whole tab.
func TestLocalModelsRendersCuratedWhenStoreErrors(test *testing.T) {
	view := NewLocalModels(
		func() ([]hf.CachedModel, error) { return nil, errors.New("hf down") },
		func() []hf.CuratedModel {
			return []hf.CuratedModel{{Name: "Llama-3.2-3B-Instruct-4bit", Repo: "mlx-community/Llama-3.2-3B-Instruct-4bit", Description: "Meta Llama"}}
		},
		func() []hf.CuratedModel { return nil },
		noTest,
		func() (string, error) { return "", nil },
	)
	view.SetSize(120, 40)
	drive(view, view.Init())
	out := view.View()
	if !strings.Contains(out, "mlx-community/Llama-3.2-3B-Instruct-4bit") {
		test.Errorf("curated Available section must render despite an installed-store error:\n%s", out)
	}
}
