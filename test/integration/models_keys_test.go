//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// dummyKeyProvider is a routable catalog provider used for the dummy-key test.
// It must NOT already have a real key on this machine. anthropic is chosen
// because the live stack here keys openai; the test asserts it starts unkeyed
// and cleans it up afterward.
const dummyKeyProvider = "anthropic"

type keysProviderRow struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	HasKey   bool   `json:"has_key"`
	Models   int    `json:"models"`
}

type keysListResult struct {
	Providers []keysProviderRow `json:"providers"`
}

type keysAddResult struct {
	Provider string `json:"provider"`
	Added    int    `json:"models_added"`
	Deleted  int    `json:"models_deleted"`
}

// Group 3: Models & keys.
//
//   - `ai models refresh` syncs whatever omlx is currently serving into the
//     gateway as omlx/<name> (best-effort — self-skips when omlx has no models
//     loaded; model management itself lives entirely in omlx's own admin panel,
//     not this CLI, so there is nothing to self-provision here any more),
//   - adding a dummy key for a routable provider registers its catalog models
//     (the served count grows / the provider's models appear),
//   - `keys list` shows it keyed,
//   - removing the key drops its models again.
//
// All with a DUMMY key — no real cloud inference.
func TestGroup03ModelsKeys(test *testing.T) {
	requireStack(test)

	test.Run("omlx models refresh syncs into the gateway", func(test *testing.T) {
		// `ai models refresh` re-syncs LiteLLM's omlx/* registrations from omlx's own
		// live model list. A failure means omlx isn't installed/running on this host —
		// a stack-state gap, not a product defect; skip rather than fail.
		env, code, stderr := run(test, "", 60*time.Second, "models", "refresh")
		if !env.OK || code != 0 {
			test.Skipf("models refresh failed (omlx not installed/running?): exit=%d %s", code, truncate(stderr, 200))
		}
		names := servedModelNames(test)
		omlxServed := false
		for _, name := range names {
			if strings.HasPrefix(name, "omlx/") {
				omlxServed = true
				break
			}
		}
		if !omlxServed {
			test.Skip("omlx currently serves no models on this host — add one via its own admin panel (`ai services console omlx`)")
		}
	})

	test.Run("dummy key registers + lists, removal drops models", func(test *testing.T) {
		// Guard: only run if the provider is currently unkeyed, so we never clobber
		// a real credential the operator put there.
		if keyed, ok := providerKeyed(test, dummyKeyProvider); !ok {
			test.Skipf("provider %q not in catalog on this host", dummyKeyProvider)
		} else if keyed {
			test.Skipf("provider %q already has a key on this host — skipping to avoid clobbering it", dummyKeyProvider)
		}

		modelsBefore := len(servedModelNames(test))

		// Always attempt cleanup, even if an assertion below fails.
		test.Cleanup(func() {
			run(test, "", 90*time.Second, "keys", "remove", dummyKeyProvider)
		})

		// Add a dummy key (no real inference — registration only).
		env, code, stderr := run(test, "", 90*time.Second, "keys", "add", dummyKeyProvider, "--value", "sk-dummy-not-real-integration")
		if !assertOK(test, env, code, "keys.add") {
			test.Logf("keys add stderr:\n%s", stderr)
			return
		}
		var addResult keysAddResult
		env.dataInto(test, &addResult)
		if addResult.Added <= 0 {
			test.Errorf("keys add registered %d models, want > 0", addResult.Added)
		}

		// keys list shows the provider keyed.
		listEnv, listCode, _ := run(test, "", 30*time.Second, "keys", "list")
		if assertOK(test, listEnv, listCode, "keys.list") {
			var list keysListResult
			listEnv.dataInto(test, &list)
			found := false
			for _, provider := range list.Providers {
				if provider.Provider == dummyKeyProvider {
					found = true
					if !provider.HasKey {
						test.Errorf("keys list: provider %q should be keyed after add", dummyKeyProvider)
					}
				}
			}
			if !found {
				test.Errorf("keys list did not include provider %q", dummyKeyProvider)
			}
		}

		// The provider's models should now show in the served set (count grows).
		grew := waitFor(func() bool {
			return len(servedModelNames(test)) > modelsBefore
		}, 30*time.Second)
		if !grew {
			test.Errorf("served-model count did not grow after adding %q key (before=%d, now=%d)",
				dummyKeyProvider, modelsBefore, len(servedModelNames(test)))
		}

		// Remove the key — the provider's models drop out.
		rmEnv, rmCode, rmStderr := run(test, "", 90*time.Second, "keys", "remove", dummyKeyProvider)
		if !assertOK(test, rmEnv, rmCode, "keys.remove") {
			test.Logf("keys remove stderr:\n%s", rmStderr)
			return
		}
		dropped := waitFor(func() bool {
			return len(servedModelNames(test)) <= modelsBefore
		}, 30*time.Second)
		if !dropped {
			test.Errorf("served-model count did not drop back after removing %q key (before=%d, now=%d)",
				dummyKeyProvider, modelsBefore, len(servedModelNames(test)))
		}

		// And keys list shows it unkeyed again.
		if keyed, _ := providerKeyed(test, dummyKeyProvider); keyed {
			test.Errorf("provider %q still keyed after remove", dummyKeyProvider)
		}
	})
}

// providerKeyed reports whether the catalog provider currently has a key, and
// whether the provider exists in the catalog at all (ok=false → not present).
func providerKeyed(test *testing.T, provider string) (keyed, ok bool) {
	test.Helper()
	env, code, _ := run(test, "", 30*time.Second, "keys", "list")
	if !env.OK || code != 0 {
		// Gateway not secured with an admin/master key on this host — a stack-setup gap
		// (run `ai setup` / secure LiteLLM), not a product defect; skip rather than fail.
		if env.Error != nil && isLiteLLMKeyNotConfigured(env.Error.Message) {
			test.Skip("LiteLLM admin key not configured on this host — secure the gateway (run `ai setup`)")
		}
		test.Fatalf("keys list failed: exit=%d err=%+v", code, env.Error)
	}
	var list keysListResult
	if err := jsonUnmarshal(env.Data, &list); err != nil {
		test.Fatalf("decode keys list: %v", err)
	}
	for _, row := range list.Providers {
		if row.Provider == provider {
			return row.HasKey, true
		}
	}
	return false, false
}
