package views

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/ideal-robot/internal/catalog"
	"github.com/jt-helsinki/ideal-robot/internal/litellm"
)

// cloudTestCatalogJSON has two cloud models: an openai model and a google model,
// each with release/last-updated/limits/modalities.
const cloudTestCatalogJSON = `{
	"models": {
		"openai/gpt-5.5": {
			"id": "openai/gpt-5.5", "name": "GPT-5.5", "family": "gpt",
			"release_date": "2026-01-15", "last_updated": "2026-03-01",
			"limit": {"context": 400000, "output": 128000},
			"modalities": {"input": ["text", "image"], "output": ["text"]}
		},
		"google/gemini-3.1-pro": {
			"id": "google/gemini-3.1-pro", "name": "Gemini 3.1 Pro", "family": "gemini",
			"release_date": "2026-02-20", "last_updated": "2026-04-10",
			"limit": {"context": 1000000, "output": 65000},
			"modalities": {"input": ["text", "audio"], "output": ["text"]}
		}
	},
	"providers": {
		"openai": {"id": "openai", "name": "OpenAI"},
		"google": {"id": "google", "name": "Google"}
	}
}`

func cloudTestCatalog(test *testing.T) *catalog.Catalog {
	test.Helper()
	cat, err := catalog.Parse([]byte(cloudTestCatalogJSON))
	if err != nil {
		test.Fatalf("parse cloud test catalog: %v", err)
	}
	return cat
}

// buildCloud builds + loads a CloudModels view from the catalog + registered set.
func buildCloud(test *testing.T, registered []litellm.LiveModel, refresh CatalogRefresher, test_ ModelTester) *CloudModels {
	test.Helper()
	cat := cloudTestCatalog(test)
	view := NewCloudModels(
		func() (*catalog.Catalog, catalog.Source, error) { return cat, catalog.SourceFresh, nil },
		func() ([]litellm.LiveModel, error) { return registered, nil },
		refresh,
		test_,
	)
	view.SetSize(120, 40)
	if cmd := view.Init(); cmd != nil {
		_ = view.Update(cmd())
	}
	return view
}

// The cloud table builds MODEL · PROVIDER · STATUS · CONTEXT, registered-first.
func TestCloudModelsBuildsRows(test *testing.T) {
	view := buildCloud(test,
		[]litellm.LiveModel{{Name: "openai/gpt-5.5", Provider: "openai"}},
		nil, noTest)

	if len(view.models) != 2 {
		test.Fatalf("cloud rows = %d, want 2", len(view.models))
	}
	if !view.models[0].registered() {
		test.Errorf("registered row should sort first, got %+v", view.models[0])
	}
	rendered := view.View()
	for _, want := range []string{"openai/gpt-5.5", "google/gemini-3.1-pro", "registered", "available", "CONTEXT", "400K", "1M", "PROVIDER", "openai"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("cloud table missing %q:\n%s", want, rendered)
		}
	}
}

// enter opens the describe pane with the models.dev metadata.
func TestCloudModelsEnterShowsDetail(test *testing.T) {
	view := buildCloud(test, nil, nil, noTest)
	_ = view.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !view.describe.active() {
		test.Fatal("enter should open the describe pane")
	}
	rendered := view.View()
	for _, want := range []string{
		"catalog (models.dev)", "release date", "last updated",
		"context limit", "output limit", "input modalities", "output modalities",
	} {
		if !strings.Contains(rendered, want) {
			test.Errorf("cloud describe pane missing %q:\n%s", want, rendered)
		}
	}
	// The first row (alpha within available) is google/gemini-3.1-pro.
	for _, want := range []string{"2026-02-20", "1M tokens", "65K tokens", "text, audio"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("cloud describe pane missing value %q:\n%s", want, rendered)
		}
	}
}

// t tests a registered model; an unregistered model is a no-op with a hint.
func TestCloudModelsTestRegistered(test *testing.T) {
	var tested string
	view := buildCloud(test,
		[]litellm.LiveModel{{Name: "google/gemini-3.1-pro", Provider: "google"}},
		nil,
		func(model string) (litellm.TestResult, error) {
			tested = model
			return litellm.TestResult{Model: model, OK: true, LatencyMS: 33}, nil
		},
	)
	// Row 0 is the registered gemini model (registered sorts first).
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if cmd == nil {
		test.Fatal("t on a registered model must emit a test command")
	}
	done := cmd()
	if tested != "google/gemini-3.1-pro" {
		test.Fatalf("tested = %q, want google/gemini-3.1-pro", tested)
	}
	_ = view.Update(done)
	if !strings.Contains(view.View(), "reachable") {
		test.Errorf("expected a test-success flash:\n%s", view.View())
	}
}

func TestCloudModelsTestUnregisteredIsNoOp(test *testing.T) {
	view := buildCloud(test, nil, nil, noTest) // nothing registered → row 0 available
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if cmd != nil {
		test.Fatalf("t on an unregistered model must be a no-op, got %T", cmd())
	}
	if !strings.Contains(view.View(), "not registered") {
		test.Errorf("expected a not-registered hint:\n%s", view.View())
	}
}

// r triggers the catalog refresh + gateway resync (the injected refresher runs).
func TestCloudModelsRefreshResyncs(test *testing.T) {
	resyncs := 0
	view := buildCloud(test, nil, func() error { resyncs++; return nil }, noTest)
	cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		test.Fatal("r must return a resync command")
	}
	msg := cmd()
	if resyncs != 1 {
		test.Fatalf("refresher called %d times, want 1", resyncs)
	}
	done, ok := msg.(cloudResyncedMsg)
	if !ok || done.err != nil {
		test.Fatalf("r must produce a clean cloudResyncedMsg, got %+v (ok=%v)", msg, ok)
	}
	if reload := view.Update(done); reload == nil {
		test.Fatal("the resync completion should reload the displayed data")
	}
	if !strings.Contains(view.View(), "resynced") {
		test.Errorf("expected a success flash after the resync:\n%s", view.View())
	}
}

// A models.dev-unreachable load WITH a cached copy shows the cached-copy warning.
func TestCloudModelsSourceCachedFlash(test *testing.T) {
	cat := cloudTestCatalog(test)
	view := NewCloudModels(
		func() (*catalog.Catalog, catalog.Source, error) {
			return cat, catalog.SourceCached, errors.New("dns failure")
		},
		func() ([]litellm.LiveModel, error) { return nil, nil },
		nil, noTest,
	)
	view.SetSize(120, 40)
	if cmd := view.Init(); cmd != nil {
		_ = view.Update(cmd())
	}
	if !strings.Contains(view.View(), "showing the cached copy") {
		test.Errorf("expected the cached-copy warning:\n%s", view.View())
	}
}

// A models.dev-unreachable load WITH NO cached copy shows the no-cache warning.
func TestCloudModelsSourceNoCacheFlash(test *testing.T) {
	view := NewCloudModels(
		func() (*catalog.Catalog, catalog.Source, error) {
			return nil, catalog.SourceCached, errors.New("dns failure")
		},
		func() ([]litellm.LiveModel, error) { return nil, nil },
		nil, noTest,
	)
	view.SetSize(120, 40)
	if cmd := view.Init(); cmd != nil {
		_ = view.Update(cmd())
	}
	if !strings.Contains(view.View(), "no cached copy") {
		test.Errorf("expected the no-cache warning:\n%s", view.View())
	}
}
