package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/catalog"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/spf13/cobra"
)

// keysTestCatalogJSON has two LiteLLM-routable providers (openai, google→gemini)
// with two openai models + one google model, plus an UNROUTABLE provider
// (megarouter) so the routable filter is observable.
const keysTestCatalogJSON = `{
	"models": {
		"openai/gpt-5.5":         {"id": "openai/gpt-5.5", "name": "GPT-5.5", "family": "gpt"},
		"openai/gpt-5.5-mini":    {"id": "openai/gpt-5.5-mini", "name": "GPT-5.5 mini", "family": "gpt"},
		"google/gemini-3.1-pro":  {"id": "google/gemini-3.1-pro", "name": "Gemini 3.1 Pro", "family": "gemini"},
		"megarouter/some-model":  {"id": "megarouter/some-model", "name": "Some Model"}
	},
	"providers": {
		"openai":     {"id": "openai", "name": "OpenAI"},
		"google":     {"id": "google", "name": "Google"},
		"megarouter": {"id": "megarouter", "name": "MegaRouter"}
	}
}`

func keysTestCatalog(test *testing.T) *catalog.Catalog {
	test.Helper()
	cat, err := catalog.Parse([]byte(keysTestCatalogJSON))
	if err != nil {
		test.Fatalf("parse test catalog: %v", err)
	}
	return cat
}

// fakeKeysGateway is an in-memory keysGateway: it records the credentials set/deleted
// and the SyncModels arguments, and never touches the network. The api key VALUE is
// captured only so the tests can assert it is what was passed (it is never surfaced).
type fakeKeysGateway struct {
	creds       map[string]string // CredentialName -> api key (provider prefix derived from name)
	credSet     []string          // providers (prefixes) set, in order
	credDeleted []string          // credential names deleted, in order

	syncCalls      int
	lastKeyed      []string // catalog provider ids passed to the last SyncModels
	syncResult     litellm.SyncResult
	listErr        error
	setErr         error
	deleteErr      error
	syncErr        error
	providerByName map[string]string // credential name -> provider prefix (for ListCredentials)
}

func newFakeKeysGateway() *fakeKeysGateway {
	return &fakeKeysGateway{creds: map[string]string{}, providerByName: map[string]string{}}
}

func (gateway *fakeKeysGateway) SetCredential(provider, apiKey string) error {
	if gateway.setErr != nil {
		return gateway.setErr
	}
	name := litellm.CredentialName(provider)
	gateway.creds[name] = apiKey
	gateway.providerByName[name] = provider
	gateway.credSet = append(gateway.credSet, provider)
	return nil
}

func (gateway *fakeKeysGateway) DeleteCredential(name string) error {
	if gateway.deleteErr != nil {
		return gateway.deleteErr
	}
	delete(gateway.creds, name)
	delete(gateway.providerByName, name)
	gateway.credDeleted = append(gateway.credDeleted, name)
	return nil
}

func (gateway *fakeKeysGateway) ListCredentials() ([]litellm.Credential, error) {
	if gateway.listErr != nil {
		return nil, gateway.listErr
	}
	out := make([]litellm.Credential, 0, len(gateway.creds))
	for name := range gateway.creds {
		out = append(out, litellm.Credential{Name: name, Provider: gateway.providerByName[name]})
	}
	return out, nil
}

func (gateway *fakeKeysGateway) SyncModels(_ *catalog.Catalog, keyedProviders []string) (litellm.SyncResult, error) {
	gateway.syncCalls++
	gateway.lastKeyed = append([]string(nil), keyedProviders...)
	if gateway.syncErr != nil {
		return litellm.SyncResult{}, gateway.syncErr
	}
	return gateway.syncResult, nil
}

// withKeysFakes installs the catalog loader + gateway factory package vars for the
// duration of a test, restoring them after.
func withKeysFakes(test *testing.T, cat *catalog.Catalog, catErr error, gateway keysGateway) {
	test.Helper()
	origCat, origGateway := keysCatalogLoader, keysGatewayFactory
	keysCatalogLoader = func() (*catalog.Catalog, error) { return cat, catErr }
	keysGatewayFactory = func() keysGateway { return gateway }
	test.Cleanup(func() {
		keysCatalogLoader, keysGatewayFactory = origCat, origGateway
	})
}

// runKeys executes a `ai keys` subcommand built by `build`, capturing stdout. It
// returns the exit code and the captured output. stdinText, when non-empty, is fed
// to the command's stdin.
func runKeys(test *testing.T, jsonMode bool, build func(*output.Emitter, *int) *cobraCommand, args []string, stdinText string) (int, string) {
	test.Helper()
	var out bytes.Buffer
	emitter := &output.Emitter{Out: &out, Err: io.Discard, JSON: jsonMode}
	exit := output.ExitOK
	cmd := build(emitter, &exit)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if stdinText != "" {
		cmd.SetIn(strings.NewReader(stdinText))
	}
	if err := cmd.Execute(); err != nil {
		test.Fatalf("keys command returned error: %v", err)
	}
	return exit, out.String()
}

// cobraCommand aliases the cobra command type so the test helper signature stays
// short.
type cobraCommand = cobra.Command

// TestKeysListJoinsCatalogAndCredentials verifies `ai keys list` shows each routable
// provider with the keyed flag (from the fake credential lister) and the catalog
// model count, and drops the unroutable provider.
func TestKeysListJoinsCatalogAndCredentials(test *testing.T) {
	gateway := newFakeKeysGateway()
	// openai is keyed (credential prefix "openai"); google is not.
	_ = gateway.SetCredential("openai", "sk-openai")
	withKeysFakes(test, keysTestCatalog(test), nil, gateway)

	exit, output := runKeys(test, true, newKeysListCmd, nil, "")
	if exit != 0 {
		test.Fatalf("exit = %d, want 0", exit)
	}
	var envelope struct {
		Data keysListResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		test.Fatalf("envelope not valid JSON: %v\n%s", err, output)
	}
	rows := envelope.Data.Providers
	if len(rows) != 2 {
		test.Fatalf("rows = %d, want 2 (openai, google; megarouter dropped)", len(rows))
	}
	byID := map[string]keysProviderRow{}
	for _, row := range rows {
		byID[row.Provider] = row
	}
	if got := byID["openai"]; !got.HasKey || got.Models != 2 {
		test.Errorf("openai row = %+v, want keyed with 2 models", got)
	}
	if got := byID["google"]; got.HasKey || got.Models != 1 {
		test.Errorf("google row = %+v, want unkeyed with 1 model", got)
	}
	if strings.Contains(output, "megarouter") {
		test.Error("list must drop the unroutable provider megarouter")
	}
}

// TestKeysAddStoresAndSyncs verifies `ai keys add openai --value` stores the
// credential under the LiteLLM prefix and triggers SyncModels with the keyed set
// (read back from ListCredentials) + the installed Ollama models.
func TestKeysAddStoresAndSyncs(test *testing.T) {
	gateway := newFakeKeysGateway()
	gateway.syncResult = litellm.SyncResult{Added: []string{"openai/gpt-5.5", "openai/gpt-5.5-mini"}}
	withKeysFakes(test, keysTestCatalog(test), nil, gateway)

	exit, output := runKeys(test, true, newKeysAddCmd, []string{"openai", "--value", "sk-secret-openai"}, "")
	if exit != 0 {
		test.Fatalf("exit = %d, want 0\n%s", exit, output)
	}
	// Credential stored under the openai prefix with the given value.
	if gateway.creds[litellm.CredentialName("openai")] != "sk-secret-openai" {
		test.Errorf("stored credential = %q, want the key", gateway.creds[litellm.CredentialName("openai")])
	}
	if gateway.syncCalls != 1 {
		test.Fatalf("SyncModels calls = %d, want 1", gateway.syncCalls)
	}
	if len(gateway.lastKeyed) != 1 || gateway.lastKeyed[0] != "openai" {
		test.Errorf("sync keyed set = %v, want [openai] (catalog id)", gateway.lastKeyed)
	}
	// The report counts the synced models and never leaks the key value.
	if strings.Contains(output, "sk-secret-openai") {
		test.Error("key value must not appear in the output envelope")
	}
	var envelope struct {
		Data keysAddResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		test.Fatalf("envelope not valid JSON: %v\n%s", err, output)
	}
	if envelope.Data.Added != 2 || envelope.Data.Provider != "openai" {
		test.Errorf("add result = %+v, want openai/2", envelope.Data)
	}
}

// TestKeysAddGoogleUsesGeminiPrefix verifies the catalog "google" provider stores its
// credential under the LiteLLM "gemini" prefix (the documented mismatch).
func TestKeysAddGoogleUsesGeminiPrefix(test *testing.T) {
	gateway := newFakeKeysGateway()
	withKeysFakes(test, keysTestCatalog(test), nil, gateway)

	exit, _ := runKeys(test, true, newKeysAddCmd, []string{"google", "--value", "sk-gemini"}, "")
	if exit != 0 {
		test.Fatalf("exit = %d, want 0", exit)
	}
	if _, ok := gateway.creds[litellm.CredentialName("gemini")]; !ok {
		test.Errorf("google key should be stored under the gemini prefix; creds = %v", keysOf(gateway.creds))
	}
	// The synced keyed set is expressed as the CATALOG id "google".
	if len(gateway.lastKeyed) != 1 || gateway.lastKeyed[0] != "google" {
		test.Errorf("sync keyed set = %v, want [google]", gateway.lastKeyed)
	}
}

// TestKeysAddViaStdin verifies --stdin feeds the key without it touching argv.
func TestKeysAddViaStdin(test *testing.T) {
	gateway := newFakeKeysGateway()
	withKeysFakes(test, keysTestCatalog(test), nil, gateway)

	exit, _ := runKeys(test, true, newKeysAddCmd, []string{"openai", "--stdin"}, "sk-from-stdin")
	if exit != 0 {
		test.Fatalf("exit = %d, want 0", exit)
	}
	if gateway.creds[litellm.CredentialName("openai")] != "sk-from-stdin" {
		test.Errorf("stdin key not stored: %v", keysOf(gateway.creds))
	}
}

// TestKeysAddInvalidProvider rejects an unknown/unroutable provider with exit 2,
// listing the valid providers, and never contacts the gateway.
func TestKeysAddInvalidProvider(test *testing.T) {
	for _, provider := range []string{"nope", "megarouter"} {
		gateway := newFakeKeysGateway()
		withKeysFakes(test, keysTestCatalog(test), nil, gateway)
		exit, output := runKeys(test, true, newKeysAddCmd, []string{provider, "--value", "x"}, "")
		if exit != 2 {
			test.Fatalf("add %q exit = %d, want 2", provider, exit)
		}
		if len(gateway.credSet) != 0 {
			test.Errorf("add %q should not store a credential", provider)
		}
		if !strings.Contains(output, "openai") || !strings.Contains(output, "google") {
			test.Errorf("add %q error should list valid providers: %s", provider, output)
		}
	}
}

// TestKeysAddMissingValueNonInteractive errors (exit 2) when no value is given under
// --json (no TTY) — the old `ai secrets set` discipline.
func TestKeysAddMissingValueNonInteractive(test *testing.T) {
	gateway := newFakeKeysGateway()
	withKeysFakes(test, keysTestCatalog(test), nil, gateway)
	exit, _ := runKeys(test, true, newKeysAddCmd, []string{"openai"}, "")
	if exit != 2 {
		test.Fatalf("exit = %d, want 2 (missing --value/--stdin)", exit)
	}
	if len(gateway.credSet) != 0 {
		test.Error("no credential should be stored when the value is missing")
	}
}

// TestKeysRemoveDeletesAndResyncs verifies `ai keys remove` deletes the provider's
// credential then re-syncs (so its models drop out).
func TestKeysRemoveDeletesAndResyncs(test *testing.T) {
	gateway := newFakeKeysGateway()
	_ = gateway.SetCredential("openai", "sk-openai")
	gateway.syncResult = litellm.SyncResult{Deleted: []string{"openai/gpt-5.5", "openai/gpt-5.5-mini"}}
	withKeysFakes(test, keysTestCatalog(test), nil, gateway)

	exit, output := runKeys(test, true, newKeysRemoveCmd, []string{"openai"}, "")
	if exit != 0 {
		test.Fatalf("exit = %d, want 0\n%s", exit, output)
	}
	if len(gateway.credDeleted) != 1 || gateway.credDeleted[0] != litellm.CredentialName("openai") {
		test.Errorf("deleted = %v, want [openai-key]", gateway.credDeleted)
	}
	if gateway.syncCalls != 1 {
		test.Errorf("SyncModels calls = %d, want 1 (resync after delete)", gateway.syncCalls)
	}
	// After delete, the live keyed set passed to sync is empty.
	if len(gateway.lastKeyed) != 0 {
		test.Errorf("post-delete keyed set = %v, want empty", gateway.lastKeyed)
	}
	var envelope struct {
		Data keysRemoveResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		test.Fatalf("envelope not valid JSON: %v\n%s", err, output)
	}
	if envelope.Data.Deleted != 2 || envelope.Data.Provider != "openai" {
		test.Errorf("remove result = %+v, want openai/2", envelope.Data)
	}
}

// TestKeysGatewayMissingExit3 verifies a gateway ExitMissingDep (no master key / not
// reachable) maps through to exit 3 — the `ai gateway`/`ai litellm` convention.
func TestKeysGatewayMissingExit3(test *testing.T) {
	gateway := newFakeKeysGateway()
	gateway.listErr = output.Errorf(output.ExitMissingDep, "LiteLLM gateway is not reachable — run `ai services start`")
	withKeysFakes(test, keysTestCatalog(test), nil, gateway)

	exit, _ := runKeys(test, true, newKeysListCmd, nil, "")
	if exit != output.ExitMissingDep {
		test.Fatalf("exit = %d, want 3 (ExitMissingDep)", exit)
	}
}

// TestKeysListHumanRendersTable verifies the human table renders the routable
// providers, the keyed marker, and the model count — no key value anywhere.
func TestKeysListHumanRendersTable(test *testing.T) {
	result := keysListResult{Providers: []keysProviderRow{
		{Provider: "openai", Name: "OpenAI", HasKey: true, Models: 2},
		{Provider: "google", Name: "Google", HasKey: false, Models: 1},
	}}
	rendered := result.Human()
	for _, want := range []string{"PROVIDER", "NAME", "KEY?", "MODELS", "openai", "google", "OpenAI"} {
		if !strings.Contains(rendered, want) {
			test.Errorf("keys list table missing %q:\n%s", want, rendered)
		}
	}
}

// keysOf is a small test helper to list a map's keys for error messages.
func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}
