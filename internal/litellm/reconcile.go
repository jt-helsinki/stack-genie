package litellm

// Sync / reconcile engine (Phase B). The DESIRED model set is derived from the
// catalog (every model of each LiteLLM-routable provider the user has keyed); the
// CURRENT set is the live gateway list (ListModels). Local models from EITHER
// backend (omlx/<id>, colibri/<id>) are NOT part of the desired set here — each
// is synced separately via SyncOmlxModels/SyncColibriModels (driven by that
// backend's own live GET /v1/models, not the catalog) and SHIELDED from this
// cloud-key resync's delete pass (see localModelPrefixes/nonLocalModels).
// Reconcile is a PURE diff (add/delete by model_name) and is unit-tested without
// a client; SyncModels wires the catalog → desired build, the live list, the
// diff, and the apply behind the injectable KeyManager — SyncOmlxModels/
// SyncColibriModels (below) reuse the exact same Reconcile/ApplyPlan machinery,
// each scoped to its own prefix (syncLocalModels) so neither backend's sync can
// delete the other's registrations.

import (
	"sort"
	"strings"

	"github.com/jt-helsinki/stack-genie/internal/catalog"
)

// DesiredModel is one model the platform wants the gateway to serve. Name is the
// public model_name (the catalog id verbatim for cloud, "omlx/<id>" for
// local); Params + Info are the AddModel payload.
type DesiredModel struct {
	Name   string
	Params ModelParams
	Info   ModelInfo
}

// Plan is the result of Reconcile: the models to add (DesiredModel, with their
// AddModel payload) and the live model ids to delete. Both are sorted/stable for
// deterministic application and testing.
type Plan struct {
	Add    []DesiredModel
	Delete []LiveModel
}

// Empty reports whether the plan is a no-op (nothing to add or delete).
func (plan Plan) Empty() bool {
	return len(plan.Add) == 0 && len(plan.Delete) == 0
}

// Reconcile computes the pure diff between the desired model set and the current
// live set, keyed by model_name. A desired model not currently served is ADDED; a
// currently-served model not desired is DELETED (by its live id). When desired and
// current carry the same names the plan is empty (idempotent). The add list is
// sorted by name; the delete list preserves no particular live order but is sorted
// by name for determinism.
func Reconcile(desired []DesiredModel, current []LiveModel) Plan {
	currentByName := make(map[string]bool, len(current))
	for _, model := range current {
		currentByName[model.Name] = true
	}
	desiredByName := make(map[string]bool, len(desired))

	var add []DesiredModel
	for _, model := range desired {
		if desiredByName[model.Name] {
			continue // de-dup desired by name
		}
		desiredByName[model.Name] = true
		if !currentByName[model.Name] {
			add = append(add, model)
		}
	}

	var del []LiveModel
	for _, model := range current {
		if !desiredByName[model.Name] {
			del = append(del, model)
		}
	}

	sort.Slice(add, func(left, right int) bool { return add[left].Name < add[right].Name })
	sort.Slice(del, func(left, right int) bool { return del[left].Name < del[right].Name })
	return Plan{Add: add, Delete: del}
}

// DesiredModels builds the desired model set from the catalog: for each keyed
// provider that is BOTH LiteLLM-routable AND in keyedProviders (matched by the catalog
// provider id), every catalog model of that provider is desired — model_name = the
// catalog id verbatim, litellm_params.model = LiteLLMModelParam(id),
// litellm_credential_name = CredentialName(<litellm prefix>), model_info = the catalog
// metadata. Local omlx models are NOT part of this set (they are synced separately
// by SyncOmlxModels).
//
// keyedProviders are CATALOG provider ids (e.g. "openai", "google") the user has a
// credential for; a keyed provider that is not LiteLLM-routable is skipped. The
// result is sorted by model_name.
func DesiredModels(cat *catalog.Catalog, keyedProviders []string) []DesiredModel {
	keyed := make(map[string]bool, len(keyedProviders))
	for _, provider := range keyedProviders {
		keyed[provider] = true
	}

	var desired []DesiredModel
	if cat != nil {
		for _, provider := range LiteLLMProviders(cat) {
			if !keyed[provider.ID] {
				continue
			}
			prefix, _ := LiteLLMPrefix(provider.ID)
			credential := CredentialName(prefix)
			for _, model := range provider.Models {
				desired = append(desired, DesiredModel{
					Name: model.ID,
					Params: ModelParams{
						Model:          LiteLLMModelParam(model.ID),
						CredentialName: credential,
					},
					Info: catalogModelInfo(model),
				})
			}
		}
	}

	sort.Slice(desired, func(left, right int) bool { return desired[left].Name < desired[right].Name })
	return desired
}

// catalogModelInfo projects the catalog metadata we surface through model_info.
func catalogModelInfo(model catalog.Model) ModelInfo {
	return ModelInfo{
		Family:      model.Family,
		ReleaseDate: model.ReleaseDate,
		LastUpdated: model.LastUpdated,
		ContextLen:  model.Limit.Context,
		OutputLen:   model.Limit.Output,
		InputModes:  model.Modalities.Input,
		OutputModes: model.Modalities.Output,
	}
}

// SyncResult reports what a SyncModels run applied.
type SyncResult struct {
	Added   []string // model_names added
	Deleted []string // model_names deleted
}

// SyncModels reconciles the gateway's live model set to the desired set derived from
// the catalog + keyed providers, applying the diff via the injectable KeyManager
// (AddModel / DeleteModel). It is the high-level trigger called after a provider key is
// added or removed. The pure diff (Reconcile) is tested separately; SyncModels is
// tested against an httptest-backed KeyManager.
//
// hardware bring-up: the live /model/new + /model/delete round-trips run only
// against a running aip-litellm.
func (manager *KeyManager) SyncModels(cat *catalog.Catalog, keyedProviders []string) (SyncResult, error) {
	desired := DesiredModels(cat, keyedProviders)
	current, err := manager.ListModels()
	if err != nil {
		return SyncResult{}, err
	}
	plan := Reconcile(desired, current)
	// A cloud-key/catalog resync must NEVER delete LOCAL models: omlx ("omlx/<id>")
	// models are owned exclusively by SyncOmlxModels (below), never by the catalog
	// resync. Without this guard, a cloud-key resync would wipe every registered local
	// model from LiteLLM. Keep only the non-local deletes; cloud adds still apply.
	plan.Delete = nonLocalModels(plan.Delete)
	return manager.ApplyPlan(plan)
}

// DesiredOmlxModels builds the desired model set from omlx's own live model ids
// (as returned by omlx's GET /v1/models — see internal/omlx.ListModels; the
// CALLER converts that to plain ids so this package stays backend-agnostic, never
// importing internal/omlx directly). Every id is desired as "omlx/<id>", routed to
// "openai/<id>" against the single shared apiBase (the one omlx server — unlike
// the old per-model vLLM design, every id shares the SAME endpoint), authenticated
// with apiKey — the REAL key when the caller found one configured on omlx's side
// (see internal/omlx.APIKey), else omlxModelParamsInfo falls back to the
// non-empty placeholder LiteLLM's client requires regardless. Result is sorted by
// model_name.
func DesiredOmlxModels(liveModelIDs []string, apiBase, apiKey string) []DesiredModel {
	return desiredLocalModels(liveModelIDs, OmlxModelName, omlxModelParamsInfo, apiBase, apiKey)
}

// DesiredColibriModels mirrors DesiredOmlxModels exactly, for colibri's live
// model list (internal/colibri.ListModels — colibri keeps exactly one model
// loaded per process, so liveModelIDs has at most one entry in practice, but the
// shape stays a slice for symmetry with the omlx path).
func DesiredColibriModels(liveModelIDs []string, apiBase, apiKey string) []DesiredModel {
	return desiredLocalModels(liveModelIDs, ColibriModelName, colibriModelParamsInfo, apiBase, apiKey)
}

// desiredLocalModels is the shared build behind DesiredOmlxModels/
// DesiredColibriModels: every live id becomes one DesiredModel, named by
// publicName and routed/authenticated by paramsInfo, sorted by model_name.
func desiredLocalModels(
	liveModelIDs []string,
	publicName func(id string) string,
	paramsInfo func(id, apiBase, apiKey string) (ModelParams, ModelInfo),
	apiBase, apiKey string,
) []DesiredModel {
	desired := make([]DesiredModel, 0, len(liveModelIDs))
	for _, id := range liveModelIDs {
		if id == "" {
			continue
		}
		params, info := paramsInfo(id, apiBase, apiKey)
		desired = append(desired, DesiredModel{Name: publicName(id), Params: params, Info: info})
	}
	sort.Slice(desired, func(left, right int) bool { return desired[left].Name < desired[right].Name })
	return desired
}

// SyncOmlxModels reconciles the gateway's live omlx/* registrations against
// omlx's own live model list (liveModelIDs — see DesiredOmlxModels), using the
// SAME pure add/delete diff as SyncModels (Reconcile/ApplyPlan): a model
// already registered under an unchanged name is left ALONE, so any settings a
// user hand-edited on it in LiteLLM's admin UI (rate limits, tags, custom
// pricing, ...) survive a refresh untouched. Only a model omlx newly reports
// gets added, and only one that disappeared from omlx's list gets deleted. It
// is scoped to ONLY omlx/*-prefixed live entries so it never touches cloud (or
// colibri/*) registrations — the mirror image of SyncModels' nonLocalModels
// guard. Called automatically at `ai setup` and at omlx service start/restart,
// and on demand via `ai models refresh` / the TUI's Service Detail `m` key
// (model management itself lives in omlx's own admin panel — this sync just
// mirrors whatever it reports into the gateway).
//
// hardware bring-up: the live /model/new + /model/delete round-trips run only
// against a running aip-litellm.
func (manager *KeyManager) SyncOmlxModels(liveModelIDs []string, apiBase, apiKey string) (SyncResult, error) {
	return manager.syncLocalModels(OmlxModelPrefix, DesiredOmlxModels(liveModelIDs, apiBase, apiKey))
}

// SyncColibriModels mirrors SyncOmlxModels exactly, scoped to ONLY colibri/*-
// prefixed live entries — it never touches omlx/* or cloud registrations, so
// running both backends' syncs (as `ai models refresh`/the setup reconcile now
// do) never has one backend's sync delete the other's registrations.
func (manager *KeyManager) SyncColibriModels(liveModelIDs []string, apiBase, apiKey string) (SyncResult, error) {
	return manager.syncLocalModels(ColibriModelPrefix, DesiredColibriModels(liveModelIDs, apiBase, apiKey))
}

// syncLocalModels is the shared reconcile behind SyncOmlxModels/SyncColibriModels:
// diff desired against ONLY the live entries already under prefix (never the
// other local backend's, never a cloud model's), then apply the add/delete plan.
func (manager *KeyManager) syncLocalModels(prefix string, desired []DesiredModel) (SyncResult, error) {
	current, err := manager.ListModels()
	if err != nil {
		return SyncResult{}, err
	}
	plan := Reconcile(desired, onlyPrefixedModels(current, prefix))
	return manager.ApplyPlan(plan)
}

// OmlxModelPrefix / ColibriModelPrefix are the public model_name prefixes each
// local-inference backend owns — the single source of truth localModelPrefixes
// and syncLocalModels's per-backend scoping both derive from, so the two never
// drift apart.
const (
	OmlxModelPrefix    = "omlx/"
	ColibriModelPrefix = "colibri/"
)

// localModelPrefixes are the public model_name prefixes owned by a local-inference
// sync (omlx OR colibri), NOT by the catalog resync — so SyncModels must never
// delete them (see nonLocalModels).
var localModelPrefixes = []string{OmlxModelPrefix, ColibriModelPrefix}

// onlyPrefixedModels returns the models whose public name starts with prefix —
// scoping a single backend's sync (SyncOmlxModels/SyncColibriModels) to exactly
// the entries IT owns, so it never proposes deleting a cloud model OR the OTHER
// local backend's model.
func onlyPrefixedModels(models []LiveModel, prefix string) []LiveModel {
	kept := make([]LiveModel, 0, len(models))
	for _, model := range models {
		if strings.HasPrefix(model.Name, prefix) {
			kept = append(kept, model)
		}
	}
	return kept
}

// nonLocalModels returns the models whose public name is NOT a local-backend route
// (omlx/*), shielding local registrations from the cloud-key resync's delete pass.
func nonLocalModels(models []LiveModel) []LiveModel {
	kept := make([]LiveModel, 0, len(models))
	for _, model := range models {
		if isLocalModel(model.Name) {
			continue
		}
		kept = append(kept, model)
	}
	return kept
}

// isLocalModel reports whether a public model_name belongs to a local-inference backend.
func isLocalModel(name string) bool {
	for _, prefix := range localModelPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// ApplyPlan applies a reconcile Plan: adds each desired model then deletes each
// stale live model. It stops at the first error, returning what was applied so far
// (so a partial sync is observable). Deletes use the live model's id.
func (manager *KeyManager) ApplyPlan(plan Plan) (SyncResult, error) {
	var result SyncResult
	for _, model := range plan.Add {
		if err := manager.AddModel(model.Name, model.Params, model.Info); err != nil {
			return result, err
		}
		result.Added = append(result.Added, model.Name)
	}
	for _, model := range plan.Delete {
		if err := manager.DeleteModel(model.ID); err != nil {
			return result, err
		}
		result.Deleted = append(result.Deleted, model.Name)
	}
	return result, nil
}
