package cli

import (
	"io"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/output"
)

// setup no longer prompts for cloud-provider API keys — those are added from the
// TUI (API Keys tab) or `ai keys add`. syncInitialModels only runs the initial
// catalog → gateway model sync, reflecting whatever providers are already keyed. It
// never sets a credential. Local vLLM models are managed separately.
func TestSyncInitialModelsSyncsWithoutAddingKeys(test *testing.T) {
	gateway := newFakeKeysGateway()
	// Pre-key one provider so the keyed set is observable in the sync inputs.
	_ = gateway.SetCredential("openai", "sk-existing")
	withKeysFakes(test, keysTestCatalog(test), nil, gateway)

	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
	syncInitialModels(emitter, true /* interactive */)

	// No NEW credential is ever set by setup (the pre-existing one is untouched).
	if len(gateway.credSet) != 1 {
		test.Fatalf("setup must not add keys; credSet=%v", gateway.credSet)
	}
	// The initial sync ran exactly once with the LIVE keyed set.
	if gateway.syncCalls != 1 {
		test.Fatalf("SyncModels calls = %d, want 1 (the initial sync)", gateway.syncCalls)
	}
	if len(gateway.lastKeyed) != 1 || gateway.lastKeyed[0] != "openai" {
		test.Errorf("sync keyed providers = %v, want [openai] (the catalog id of the keyed credential)", gateway.lastKeyed)
	}
}

// With NO providers keyed, the initial sync still runs (a no-op desired set) — NO
// default models are added by setup.
func TestSyncInitialModelsRegistersNoDefaults(test *testing.T) {
	gateway := newFakeKeysGateway()
	withKeysFakes(test, keysTestCatalog(test), nil, gateway)

	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
	syncInitialModels(emitter, false)

	if len(gateway.credSet) != 0 {
		test.Fatalf("setup must add no keys; credSet=%v", gateway.credSet)
	}
	if gateway.syncCalls != 1 {
		test.Fatalf("SyncModels calls = %d, want 1", gateway.syncCalls)
	}
	// The sync's desired set is empty (no keyed providers) — no defaults.
	if len(gateway.lastKeyed) != 0 {
		test.Errorf("expected an empty desired set, got keyed=%v", gateway.lastKeyed)
	}
}

// A catalog-load failure is tolerated: the sync is skipped (no panic, no sync call).
func TestSyncInitialModelsToleratesNoCatalog(test *testing.T) {
	gateway := newFakeKeysGateway()
	withKeysFakes(test, nil, errNoCatalog, gateway)

	emitter := &output.Emitter{Out: io.Discard, Err: io.Discard, JSON: true}
	syncInitialModels(emitter, false)

	if gateway.syncCalls != 0 {
		test.Fatalf("no catalog → no sync, got %d sync calls", gateway.syncCalls)
	}
}

// errNoCatalog stands in for an offline + no-saved-copy catalog load.
var errNoCatalog = &output.Error{Code: output.ExitRuntimeFailure, Message: "no catalog"}
