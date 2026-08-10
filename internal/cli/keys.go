package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/catalog"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/spf13/cobra"
)

// keysGateway is the slice of litellm.KeyManager `ai keys` drives: the DB-backed
// provider-credential CRUD plus the catalog→gateway model sync. It is an interface
// so tests inject a fake (no network); production wires litellm.NewKeyManager.
type keysGateway interface {
	SetCredential(provider, apiKey string) error
	DeleteCredential(name string) error
	ListCredentials() ([]litellm.Credential, error)
	SyncModels(cat *catalog.Catalog, keyedProviders []string) (litellm.SyncResult, error)
}

// keysGatewayFactory builds the gateway client. A package var so tests inject a
// fake; production binds a KeyManager over the real container prober.
var keysGatewayFactory = func() keysGateway {
	return litellm.NewKeyManager(runtime.RealProber())
}

// keysCatalogLoader loads the model catalog (saved copy preferred, fetch if
// absent). A package var so tests inject a fixture catalog with no network.
var keysCatalogLoader = func() (*catalog.Catalog, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return catalog.LoadOrFetch(ctx, nil, "")
}

// keysProviderRow is one row of `ai keys list`: a LiteLLM-routable catalog provider
// with whether the user has stored a key for it and how many models the catalog
// lists under it.
type keysProviderRow struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	HasKey   bool   `json:"has_key"`
	Models   int    `json:"models"`
}

// keysListResult is the `ai keys list` payload — the LiteLLM-routable providers
// with their keyed status. It never carries any key value.
type keysListResult struct {
	Providers []keysProviderRow `json:"providers"`
}

// Human renders the providers as a table PROVIDER · NAME · KEY? · MODELS.
func (result keysListResult) Human() string {
	if len(result.Providers) == 0 {
		return ui.Muted.Render("No routable providers in the catalog.")
	}
	rows := make([][]string, 0, len(result.Providers))
	for _, provider := range result.Providers {
		key := ui.Muted.Render("—")
		if provider.HasKey {
			key = ui.Success.Render(ui.IconOK + " yes")
		}
		rows = append(rows, []string{
			ui.Value.Render(provider.Provider),
			provider.Name,
			key,
			strconv.Itoa(provider.Models),
		})
	}
	return ui.Table([]string{"PROVIDER", "NAME", "KEY?", "MODELS"}, rows)
}

// keysAddResult is the `ai keys add` payload: the provider keyed plus the number of
// catalog models synced into the gateway as a result. The key value is never here.
type keysAddResult struct {
	Provider string `json:"provider"`
	Added    int    `json:"models_added"`
	Deleted  int    `json:"models_deleted"`
}

func (result keysAddResult) Human() string {
	return ui.Success.Render(ui.IconOK+" added key for "+result.Provider) +
		ui.Muted.Render(fmt.Sprintf(" — registered %d models", result.Added))
}

// keysRemoveResult is the `ai keys remove` payload.
type keysRemoveResult struct {
	Provider string `json:"provider"`
	Deleted  int    `json:"models_deleted"`
}

func (result keysRemoveResult) Human() string {
	return ui.Success.Render(ui.IconOK+" removed key for "+result.Provider) +
		ui.Muted.Render(fmt.Sprintf(" — dropped %d models", result.Deleted))
}

// newKeysCmd builds `ai keys` (CLI §keys) — per-provider API keys stored encrypted
// in the LiteLLM DB, with the catalog's models synced into the gateway when a key is
// added/removed. It REPLACES `ai secrets`: real provider keys live in LiteLLM
// (keys-in-LiteLLM), never on platform disk, and the key value never appears in
// argv/logs/the JSON envelope.
func newKeysCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage per-provider API keys (stored encrypted in the LiteLLM gateway)",
		Long: "Manage the per-provider API keys the model gateway uses. Keys are stored\n" +
			"encrypted in the LiteLLM DB (never on platform disk) and, when added or\n" +
			"removed, the provider's catalog models are synced into the gateway so the\n" +
			"agent can route to them. The key value never appears in argv, logs, or the\n" +
			"--json envelope.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		newKeysListCmd(emitter, exit),
		newKeysAddCmd(emitter, exit),
		newKeysRemoveCmd(emitter, exit),
	)
	return cmd
}

// mapKeysErr maps a gateway error to an exit code (§18), passing through errors
// that already carry a specific code (*output.Error — e.g. the KeyManager's
// ExitMissingDep when the gateway/master key is unreachable).
func mapKeysErr(err error) error {
	var platformErr *output.Error
	if errors.As(err, &platformErr) {
		return platformErr
	}
	return output.Errorf(output.ExitRuntimeFailure, "%s", err)
}

func newKeysListCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List routable providers, whether each has a key, and its catalog model count",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cat, err := keysCatalogLoader()
			if err != nil {
				*exit = emitter.Failure("keys.list", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			gateway := keysGatewayFactory()
			creds, err := gateway.ListCredentials()
			if err != nil {
				*exit = emitter.Failure("keys.list", mapKeysErr(err))
				return nil
			}
			*exit = emitter.Success("keys.list", keysListResult{Providers: buildKeysRows(cat, creds)})
			return nil
		},
	}
}

// buildKeysRows joins the catalog's LiteLLM-routable providers with the stored
// credentials. A provider is keyed when a credential's provider prefix matches the
// provider's LiteLLM prefix (ListCredentials reports the LiteLLM prefix, e.g.
// "gemini" for the catalog's "google").
func buildKeysRows(cat *catalog.Catalog, creds []litellm.Credential) []keysProviderRow {
	keyedPrefix := make(map[string]bool, len(creds))
	for _, cred := range creds {
		keyedPrefix[cred.Provider] = true
	}
	providers := litellm.LiteLLMProviders(cat)
	rows := make([]keysProviderRow, 0, len(providers))
	for _, provider := range providers {
		prefix, _ := litellm.LiteLLMPrefix(provider.ID)
		rows = append(rows, keysProviderRow{
			Provider: provider.ID,
			Name:     provider.Name,
			HasKey:   keyedPrefix[prefix],
			Models:   len(provider.Models),
		})
	}
	return rows
}

func newKeysAddCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var value string
	var fromStdin bool
	command := &cobra.Command{
		Use:   "add <provider>",
		Short: "Store a provider API key and register its catalog models",
		Long: "Store the API key for a routable catalog provider and register that\n" +
			"provider's catalog models into the gateway. On a terminal you are prompted\n" +
			"for the key (hidden); under --json/no TTY pass it via --value/--stdin. The\n" +
			"key is encrypted at rest in the LiteLLM DB and never echoed/logged.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := strings.TrimSpace(args[0])

			// Load the catalog FIRST: it both validates the provider and drives the
			// post-key model sync. A load failure is a runtime error (offline + no
			// saved copy).
			cat, err := keysCatalogLoader()
			if err != nil {
				*exit = emitter.Failure("keys.add", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			if !isRoutableProvider(cat, provider) {
				*exit = emitter.Failure("keys.add", output.Errorf(output.ExitInvalidInput,
					"unknown provider %q — valid providers: %s", provider, strings.Join(routableProviderIDs(cat), ", ")))
				return nil
			}

			// Resolve the key value: --stdin/--value are used directly (a hidden field
			// can't display a seed); otherwise prompt hidden on a TTY. Under --json/no
			// TTY a missing value is a hard error (§1.8) — exactly the old `ai secrets
			// set` discipline.
			apiKey, ok := resolveKeyValue(cmd, emitter, value, fromStdin, exit)
			if !ok {
				return nil
			}

			gateway := keysGatewayFactory()
			if err := gateway.SetCredential(litellmProviderPrefix(cat, provider), apiKey); err != nil {
				*exit = emitter.Failure("keys.add", mapKeysErr(err))
				return nil
			}

			// Sync: register this provider's catalog models (plus all currently-keyed
			// providers + the installed local models). The keyed set is read back from
			// the gateway so the desired set always reflects the live credential store.
			result, err := syncKeyedModels(gateway, cat)
			if err != nil {
				*exit = emitter.Failure("keys.add", mapKeysErr(err))
				return nil
			}
			*exit = emitter.Success("keys.add", keysAddResult{
				Provider: provider,
				Added:    len(result.Added),
				Deleted:  len(result.Deleted),
			})
			return nil
		},
	}
	command.Flags().StringVar(&value, "value", "", "API key value (discouraged — leaks to shell history; prefer --stdin)")
	command.Flags().BoolVar(&fromStdin, "stdin", false, "read the API key from stdin")
	return command
}

func newKeysRemoveCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <provider>",
		Short: "Remove a provider API key and drop its models from the gateway",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			provider := strings.TrimSpace(args[0])
			cat, err := keysCatalogLoader()
			if err != nil {
				*exit = emitter.Failure("keys.remove", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			if !isRoutableProvider(cat, provider) {
				*exit = emitter.Failure("keys.remove", output.Errorf(output.ExitInvalidInput,
					"unknown provider %q — valid providers: %s", provider, strings.Join(routableProviderIDs(cat), ", ")))
				return nil
			}
			gateway := keysGatewayFactory()
			if err := gateway.DeleteCredential(litellm.CredentialName(litellmProviderPrefix(cat, provider))); err != nil {
				*exit = emitter.Failure("keys.remove", mapKeysErr(err))
				return nil
			}
			// Re-sync: the provider's models drop out since it is no longer keyed.
			result, err := syncKeyedModels(gateway, cat)
			if err != nil {
				*exit = emitter.Failure("keys.remove", mapKeysErr(err))
				return nil
			}
			*exit = emitter.Success("keys.remove", keysRemoveResult{
				Provider: provider,
				Deleted:  len(result.Deleted),
			})
			return nil
		},
	}
}

// resolveKeyValue reads the API key from --stdin/--value, else prompts hidden on a
// TTY. It returns ok=false and sets *exit (a failure envelope already emitted) when
// no value can be obtained. The value is never trimmed (a key may carry whitespace)
// and never echoed.
func resolveKeyValue(cmd *cobra.Command, emitter *output.Emitter, value string, fromStdin bool, exit *int) (string, bool) {
	switch {
	case fromStdin:
		read, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			*exit = emitter.Failure("keys.add", output.Errorf(output.ExitRuntimeFailure, "read stdin: %s", err))
			return "", false
		}
		return string(read), true
	case value != "":
		return value, true
	}
	if interactive(emitter) {
		entered, err := promptSecret(
			"API key",
			"hidden — stored encrypted in the LiteLLM gateway, never on platform disk",
			func(candidate string) error {
				if candidate == "" {
					return fmt.Errorf("API key is required")
				}
				return nil
			})
		if err != nil {
			*exit = emitter.Failure("keys.add", err)
			return "", false
		}
		return entered, true
	}
	*exit = emitter.Failure("keys.add", output.Errorf(output.ExitInvalidInput, "provide the key with --value or --stdin"))
	return "", false
}

// syncKeyedModels reconciles the gateway's model set from the LIVE keyed-provider
// set (read back from ListCredentials). The keyed set is expressed as CATALOG provider
// ids (DesiredModels matches on those), mapped back from each credential's LiteLLM
// prefix. Local vLLM models are managed separately by `ai models pull|rm` and shielded
// from this resync's delete pass.
func syncKeyedModels(gateway keysGateway, cat *catalog.Catalog) (litellm.SyncResult, error) {
	creds, err := gateway.ListCredentials()
	if err != nil {
		return litellm.SyncResult{}, err
	}
	keyedCatalogIDs := catalogIDsForCredentials(cat, creds)
	return gateway.SyncModels(cat, keyedCatalogIDs)
}

// catalogIDsForCredentials maps the stored credentials' LiteLLM provider prefixes
// back to the CATALOG provider ids that route under them (e.g. a "gemini" credential
// keys the catalog's "google" provider). Only routable catalog providers are
// considered. The result is sorted for determinism.
func catalogIDsForCredentials(cat *catalog.Catalog, creds []litellm.Credential) []string {
	keyedPrefix := make(map[string]bool, len(creds))
	for _, cred := range creds {
		keyedPrefix[cred.Provider] = true
	}
	ids := make([]string, 0, len(creds))
	for _, provider := range litellm.LiteLLMProviders(cat) {
		prefix, _ := litellm.LiteLLMPrefix(provider.ID)
		if keyedPrefix[prefix] {
			ids = append(ids, provider.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// isRoutableProvider reports whether <provider> is a LiteLLM-routable catalog
// provider id.
func isRoutableProvider(cat *catalog.Catalog, provider string) bool {
	for _, candidate := range litellm.LiteLLMProviders(cat) {
		if candidate.ID == provider {
			return true
		}
	}
	return false
}

// routableProviderIDs lists the LiteLLM-routable catalog provider ids (for error
// messages), in the catalog's sorted order.
func routableProviderIDs(cat *catalog.Catalog) []string {
	providers := litellm.LiteLLMProviders(cat)
	ids := make([]string, 0, len(providers))
	for _, provider := range providers {
		ids = append(ids, provider.ID)
	}
	return ids
}

// litellmProviderPrefix returns the LiteLLM prefix for a catalog provider id, used
// to name its credential. The caller has already validated routability.
func litellmProviderPrefix(cat *catalog.Catalog, provider string) string {
	_ = cat // catalog kept for symmetry; the mapping is catalog-independent
	prefix, _ := litellm.LiteLLMPrefix(provider)
	return prefix
}
