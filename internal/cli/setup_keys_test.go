package cli

import (
	"io"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
)

// setup no longer prompts for cloud-provider API keys — those are added from the
// TUI (API Keys tab) or `ai keys add`. syncInitialModels only runs the initial
// catalog → gateway model sync, reflecting whatever providers are already keyed +
// installed Ollama models. It never sets a credential.
func TestSyncInitialModelsSyncsWithoutAddingKeys(test *testing.T) {
	gateway := newFakeKeysGateway()
	// Pre-key one provider so the keyed set is observable in the sync inputs.
	_ = gateway.SetCredential("openai", "sk-existing")
	withKeysFakes(test, keysTestCatalog(test), nil, gateway, []string{"llama3.2:3b"})

	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
	syncInitialModels(emitter, true /* interactive */)

	// No NEW credential is ever set by setup (the pre-existing one is untouched).
	if len(gateway.credSet) != 1 {
		test.Fatalf("setup must not add keys; credSet=%v", gateway.credSet)
	}
	// The initial sync ran exactly once with the LIVE keyed set + installed Ollama.
	if gateway.syncCalls != 1 {
		test.Fatalf("SyncModels calls = %d, want 1 (the initial sync)", gateway.syncCalls)
	}
	if len(gateway.lastKeyed) != 1 || gateway.lastKeyed[0] != "openai" {
		test.Errorf("sync keyed providers = %v, want [openai] (the catalog id of the keyed credential)", gateway.lastKeyed)
	}
	if len(gateway.lastOllama) != 1 || gateway.lastOllama[0] != "llama3.2:3b" {
		test.Errorf("sync installed-ollama = %v, want [llama3.2:3b]", gateway.lastOllama)
	}
}

// With NO providers keyed and no Ollama models, the initial sync still runs (a
// no-op desired set) — NO default models are added by setup.
func TestSyncInitialModelsRegistersNoDefaults(test *testing.T) {
	gateway := newFakeKeysGateway()
	withKeysFakes(test, keysTestCatalog(test), nil, gateway, nil)

	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
	syncInitialModels(emitter, false)

	if len(gateway.credSet) != 0 {
		test.Fatalf("setup must add no keys; credSet=%v", gateway.credSet)
	}
	if gateway.syncCalls != 1 {
		test.Fatalf("SyncModels calls = %d, want 1", gateway.syncCalls)
	}
	// The sync's desired set is empty (no keyed providers, no Ollama) — no defaults.
	if len(gateway.lastKeyed) != 0 || len(gateway.lastOllama) != 0 {
		test.Errorf("expected an empty desired set, got keyed=%v ollama=%v", gateway.lastKeyed, gateway.lastOllama)
	}
}

// A catalog-load failure is tolerated: the sync is skipped (no panic, no sync call).
func TestSyncInitialModelsToleratesNoCatalog(test *testing.T) {
	gateway := newFakeKeysGateway()
	withKeysFakes(test, nil, errNoCatalog, gateway, nil)

	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
	syncInitialModels(emitter, false)

	if gateway.syncCalls != 0 {
		test.Fatalf("no catalog → no sync, got %d sync calls", gateway.syncCalls)
	}
}

// errNoCatalog stands in for an offline + no-saved-copy catalog load.
var errNoCatalog = &output.Error{Code: output.ExitRuntimeFailure, Message: "no catalog"}
