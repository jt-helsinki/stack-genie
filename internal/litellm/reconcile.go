package litellm

// Sync / reconcile engine (Phase B). The DESIRED model set is derived from the
// catalog (every model of each LiteLLM-routable provider the user has keyed); the
// CURRENT set is the live gateway list (ListModels). Local omlx models
// (omlx/<id>) are NOT part of the desired set here — they are synced separately
// via SyncOmlxModels (driven by omlx's own live GET /v1/models, not the catalog)
// and SHIELDED from this cloud-key resync's delete pass (see
// localModelPrefixes/nonLocalModels). Reconcile is a PURE diff (add/delete by
// model_name) and is unit-tested without a client; SyncModels wires the catalog →
// desired build, the live list, the diff, and the apply behind the injectable
// KeyManager — SyncOmlxModels (below) reuses the exact same Reconcile/ApplyPlan
// machinery for the omlx side.

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
// the old per-model vLLM design, every id shares the SAME endpoint). Result is
// sorted by model_name.
func DesiredOmlxModels(liveModelIDs []string, apiBase string) []DesiredModel {
	desired := make([]DesiredModel, 0, len(liveModelIDs))
	for _, id := range liveModelIDs {
		if id == "" {
			continue
		}
		params, info := omlxModelParamsInfo(id, apiBase)
		desired = append(desired, DesiredModel{Name: OmlxModelName(id), Params: params, Info: info})
	}
	sort.Slice(desired, func(left, right int) bool { return desired[left].Name < desired[right].Name })
	return desired
}

// SyncOmlxModels reconciles the gateway's live omlx/* registrations against
// omlx's own live model list (liveModelIDs — see DesiredOmlxModels): unlike
// SyncModels' pure add/delete diff (Reconcile), every CURRENTLY-registered
// omlx/* model is deleted and every DESIRED one re-added fresh, even when its
// name is unchanged — so a stale registration (e.g. omlx reports different
// capabilities for the same model id under the hood) can never survive a
// refresh. It is scoped to ONLY omlx/*-prefixed live entries (onlyLocalModels)
// so it never touches cloud registrations — the mirror image of SyncModels'
// nonLocalModels guard. Called automatically at `ai setup` and at omlx service
// start/restart, and on demand via `ai models refresh` / the TUI's Service
// Detail `m` key (model management itself lives in omlx's own admin panel —
// this sync just mirrors whatever it reports into the gateway).
//
// hardware bring-up: the live /model/new + /model/delete round-trips run only
// against a running aip-litellm.
func (manager *KeyManager) SyncOmlxModels(liveModelIDs []string, apiBase string) (SyncResult, error) {
	desired := DesiredOmlxModels(liveModelIDs, apiBase)
	current, err := manager.ListModels()
	if err != nil {
		return SyncResult{}, err
	}
	return manager.applyOmlxRefresh(desired, onlyLocalModels(current))
}

// applyOmlxRefresh deletes every existing registration THEN adds every desired
// one — the reverse of ApplyPlan's add-then-delete order. ApplyPlan's order is
// safe only when add/delete are disjoint by name (SyncModels' diff); here the
// add and delete sets deliberately overlap by name (a full teardown+rebuild), so
// deleting first avoids briefly registering a duplicate model_name. It stops at
// the first error, returning what was applied so far.
func (manager *KeyManager) applyOmlxRefresh(desired []DesiredModel, existing []LiveModel) (SyncResult, error) {
	var result SyncResult
	for _, model := range existing {
		if err := manager.DeleteModel(model.ID); err != nil {
			return result, err
		}
		result.Deleted = append(result.Deleted, model.Name)
	}
	for _, model := range desired {
		if err := manager.AddModel(model.Name, model.Params, model.Info); err != nil {
			return result, err
		}
		result.Added = append(result.Added, model.Name)
	}
	return result, nil
}

// localModelPrefixes are the public model_name prefixes owned by the local-inference
// (omlx) sync, NOT by the catalog resync — so SyncModels must never delete them
// (see nonLocalModels), and SyncOmlxModels must never consider anything else (see
// onlyLocalModels).
var localModelPrefixes = []string{"omlx/"}

// onlyLocalModels returns the models whose public name IS a local-backend route
// (omlx/*) — the inverse of nonLocalModels, scoping SyncOmlxModels's reconcile to
// exactly the entries it owns so it never proposes deleting a cloud model.
func onlyLocalModels(models []LiveModel) []LiveModel {
	kept := make([]LiveModel, 0, len(models))
	for _, model := range models {
		if isLocalModel(model.Name) {
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
