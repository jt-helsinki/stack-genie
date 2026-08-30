package litellm

// DB-backed model + credential management (Phase B). Models are no longer baked
// into the rendered config.yaml — the config sets general_settings
// store_model_in_db: true and the platform manages the live model list over
// LiteLLM's admin HTTP API:
//
//	POST   /credentials                 add/update a provider credential
//	GET    /credentials                 list stored credentials
//	DELETE /credentials/{name}          remove a credential
//	POST   /model/new                   add a DB-backed model
//	POST   /model/delete  {"id": ...}   remove a model by its model_info.id
//	GET    /model/info                  list models (incl. model_info.id)
//
// All round-trips reuse KeyManager.doJSON (Bearer master key, injectable HTTP
// client) so they are unit-tested against an httptest fake. The credential/model
// VALUES (api keys) are encrypted at rest in the DB by LITELLM_SALT_KEY (see
// internal/setup litellmRunArgs). The api keys transit process memory only.
//
// hardware bring-up: the LIVE round-trips against a running aip-litellm have only
// been exercised by these endpoints' documented shapes — verify the /model/delete
// id-keyed body and the /credentials list/delete shapes against the running
// gateway on a provisioned host (docs.litellm.ai/docs/proxy/model_management,
// /docs/proxy/credentials).

import (
	"net/http"
	"net/url"

	"github.com/jt-helsinki/stack-genie/internal/output"
)

// Credential is a stored provider credential as returned by GET /credentials. The
// secret api key is NEVER returned by LiteLLM's list endpoint (credential_values
// is omitted), so this carries only the name + the provider it serves.
type Credential struct {
	Name     string `json:"credential_name"`
	Provider string `json:"custom_llm_provider"`
}

// CredentialName is the conventional stored-credential name for a LiteLLM provider
// prefix: "<provider>-key" (e.g. "openai" → "openai-key"). A model references it
// via litellm_params.litellm_credential_name.
func CredentialName(provider string) string {
	return provider + "-key"
}

// SetCredential adds or updates a provider credential in the DB-backed store. The
// credential is keyed by CredentialName(provider); credential_info.custom_llm_provider
// records the LiteLLM provider prefix; credential_values.api_key carries the secret
// (encrypted at rest by LITELLM_SALT_KEY). Idempotent: re-setting the same name
// updates the stored value.
//
// LiteLLM does not expose an "upsert" — POST /credentials creates and rejects a
// duplicate name — so SetCredential best-effort deletes any existing credential of
// the same name first, then creates. A delete of a missing credential is ignored.
func (manager *KeyManager) SetCredential(provider, apiKey string) error {
	name := CredentialName(provider)
	_ = manager.DeleteCredential(name) // ignore "not found" on first set
	body := map[string]any{
		"credential_name": name,
		"credential_info": map[string]any{
			"custom_llm_provider": provider,
		},
		"credential_values": map[string]any{
			"api_key": apiKey,
		},
	}
	return manager.doJSON(http.MethodPost, "/credentials", body, nil)
}

// DeleteCredential removes a stored credential by name (DELETE /credentials/{name}).
func (manager *KeyManager) DeleteCredential(name string) error {
	return manager.doJSON(http.MethodDelete, "/credentials/"+url.PathEscape(name), nil, nil)
}

// ListCredentials returns the stored provider credentials (names + providers; no
// secret values). LiteLLM's GET /credentials response wraps the list in a
// "credentials" array, each entry exposing credential_name and
// credential_info.custom_llm_provider.
func (manager *KeyManager) ListCredentials() ([]Credential, error) {
	var wrapper struct {
		Credentials []struct {
			Name           string `json:"credential_name"`
			CredentialInfo struct {
				Provider string `json:"custom_llm_provider"`
			} `json:"credential_info"`
		} `json:"credentials"`
	}
	if err := manager.doJSON(http.MethodGet, "/credentials", nil, &wrapper); err != nil {
		return nil, err
	}
	creds := make([]Credential, 0, len(wrapper.Credentials))
	for _, entry := range wrapper.Credentials {
		creds = append(creds, Credential{Name: entry.Name, Provider: entry.CredentialInfo.Provider})
	}
	return creds, nil
}

// ModelParams is the litellm_params payload for AddModel. Model is the value
// LiteLLM routes (LiteLLMModelParam(catalogID) for cloud, "vllm/<alias>" for
// local). Exactly one credential strategy is used: CredentialName references a
// stored DB credential (cloud), while APIBase is set for a local model (no key). Empty
// fields are omitted from the request.
type ModelParams struct {
	Model          string `json:"model"`
	CredentialName string `json:"litellm_credential_name,omitempty"`
	APIBase        string `json:"api_base,omitempty"`
	APIKey         string `json:"api_key,omitempty"`
	// DropParams, when true, tells LiteLLM to drop request params the backend does not
	// support instead of erroring. Set for local models as defense-in-depth for param
	// mismatches. NOTE it does NOT by itself stop a "does not support tools" 500 when the
	// backend is forwarded `tools` for a completion-only model. The actual guard against that 500 is
	// CLIENT-side — opencode's per-model tool_call is set false for non-tool models
	// (from supports_function_calling) so no tool schema is sent. Pointer: omitted unless set.
	DropParams *bool `json:"drop_params,omitempty"`
}

// ModelInfo is the catalog metadata surfaced through model_info so the enriched
// model table (Phase E) can render it from the live gateway. The id field is
// LiteLLM-assigned (returned by GET /model/info) and must NOT be sent on create.
type ModelInfo struct {
	ID          string   `json:"id,omitempty"`
	Family      string   `json:"family,omitempty"`
	ReleaseDate string   `json:"release_date,omitempty"`
	LastUpdated string   `json:"last_updated,omitempty"`
	ContextLen  int      `json:"context_length,omitempty"`
	OutputLen   int      `json:"max_output_tokens,omitempty"`
	InputModes  []string `json:"input_modalities,omitempty"`
	OutputModes []string `json:"output_modalities,omitempty"`
	// SupportsFunctionCalling records whether the model can do tool/function calling.
	// Set at model registration from the model's advertised capabilities so the live
	// model list (ListModels) and the in-VM agent configs (opencode's per-model
	// `tool_call`) reflect reality — a completion-only model is marked false so opencode
	// does not present it as agentic. Pointer/tri-state: nil means "unknown" (treated as
	// tool-capable, preserving prior behavior for cloud models that do not set it).
	SupportsFunctionCalling *bool `json:"supports_function_calling,omitempty"`
}

// AddModel registers a DB-backed model (POST /model/new). model_name is the
// PUBLIC handle the agent names (the catalog id verbatim for cloud, "vllm/<alias>"
// for local); litellmParams.Model is what LiteLLM routes; modelInfo carries the
// surfaced catalog metadata. Persisted iff the gateway config has
// store_model_in_db: true (which Render now sets).
func (manager *KeyManager) AddModel(modelName string, litellmParams ModelParams, modelInfo ModelInfo) error {
	if modelName == "" {
		return output.Errorf(output.ExitRuntimeFailure, "model_name must not be empty")
	}
	body := map[string]any{
		"model_name":     modelName,
		"litellm_params": litellmParams,
		"model_info":     modelInfo,
	}
	return manager.doJSON(http.MethodPost, "/model/new", body, nil)
}

// DeleteModel removes a DB-backed model by its LiteLLM-assigned id (POST
// /model/delete with {"id": <id>}). The id comes from ListModels (model_info.id).
func (manager *KeyManager) DeleteModel(id string) error {
	if id == "" {
		return output.Errorf(output.ExitRuntimeFailure, "model id must not be empty")
	}
	return manager.doJSON(http.MethodPost, "/model/delete", map[string]any{"id": id}, nil)
}

// VLLMModelName is the PUBLIC model handle for a vLLM-served model: "vllm/<alias>".
// This is the agent-facing id. The
// public-handle-vs-routed-value split matters because "vllm/" is NOT a real LiteLLM
// provider prefix — LiteLLM would not know how to route it — so the routed value
// (VLLMRoutedModel) rewrites to a provider LiteLLM understands while this stable public
// handle is what the in-workspace agent names and what surfaces in the live model list.
func VLLMModelName(alias string) string {
	return "vllm/" + alias
}

// VLLMRoutedModel is the value LiteLLM ROUTES on (litellm_params.model) for a vLLM model:
// "openai/<alias>". vLLM exposes an OpenAI-compatible HTTP server started with
// `--served-model-name <alias>`, so the endpoint answers to model=<alias> (NOT the Hugging
// Face model id) — the routed value MUST therefore be built from the ALIAS, paired with the
// model's dedicated api_base. Via the public-handle-vs-routed-value split, the
// PUBLIC handle stays "vllm/<alias>" (VLLMModelName) so the agent-facing id is unchanged;
// only the internal routing switches to the OpenAI-compatible provider.
func VLLMRoutedModel(alias string) string {
	return "openai/" + alias
}

// vllmPlaceholderAPIKey is a non-empty placeholder credential for vLLM-routed models.
// A local `vllm serve` endpoint validates NO key at all, but LiteLLM's "openai/" provider
// goes through the actual OpenAI Python client underneath, which hard-requires a non-empty
// api_key at load time regardless of whether the backend checks it — an empty/absent key
// fails every request with "AuthenticationError: ... The api_key client option must be set"
// before the request ever reaches vLLM. "EMPTY" is the same placeholder convention vLLM's
// own docs and llama.cpp's OpenAI-compatible server use for this exact situation.
const vllmPlaceholderAPIKey = "EMPTY"

// vllmModelParamsInfo builds the LiteLLM params + info for a vLLM model. It routes on the
// served-model-name ALIAS (VLLMRoutedModel(alias) = "openai/<alias>") because that is what
// the `vllm serve --served-model-name <alias>` endpoint answers to; it always sets
// drop_params (defense-in-depth for unsupported params) and records tool-calling support in
// model_info when known (supportsTools nil = unknown). Each vLLM model runs its OWN
// `vllm serve` endpoint, so apiBase is a PARAMETER (e.g.
// http://host.docker.internal:8101/v1); APIKey is the non-empty vllmPlaceholderAPIKey (the
// backend itself checks no credential, but LiteLLM's openai/ provider requires one to be
// set to construct its client at all).
func vllmModelParamsInfo(alias, apiBase string, supportsTools *bool) (ModelParams, ModelInfo) {
	dropParams := true
	return ModelParams{Model: VLLMRoutedModel(alias), APIBase: apiBase, APIKey: vllmPlaceholderAPIKey, DropParams: &dropParams},
		ModelInfo{SupportsFunctionCalling: supportsTools}
}

// RegisterVLLMModel registers a vLLM-served model as a DB-backed model in the gateway. The
// public model_name is "vllm/<alias>" (VLLMModelName, the agent-facing handle) while the
// routed litellm_params.model is "openai/<alias>" (VLLMRoutedModel — the SERVED-MODEL-NAME
// the endpoint answers to, NOT the HF model id) with api_base pointing at the model's
// dedicated `vllm serve` endpoint, with the non-empty vllmPlaceholderAPIKey (a real
// provider credential is never referenced — the backend itself checks no credential).
// model is retained in the signature (interface parity, records/logging) even though
// routing keys on the alias.
//
// Idempotent-ish and HEALING: if a model with this model_name already
// exists (ListModels) it is re-registered (delete + add) when the routed target is stale
// (existing.RoutedTo != VLLMRoutedModel(model)) OR the recorded tool capability disagrees
// with supportsTools; otherwise the add is skipped so a re-register does not create a
// duplicate. NOTE: LiveModel/ListModels does not expose the registered api_base, so a
// changed endpoint alone cannot be detected here — healing on routing + capability is
// sufficient (a moved endpoint is corrected by an explicit re-register with a new alias, or
// by an UnregisterVLLMModel + RegisterVLLMModel).
//
// hardware bring-up: the LIVE POST /model/new round-trip is exercised only against a
// running aip-litellm — verify on a provisioned host.
func (manager *KeyManager) RegisterVLLMModel(alias, model, apiBase string, supportsTools bool) error {
	modelName := VLLMModelName(alias)
	existing, err := manager.ListModels()
	if err != nil {
		return err
	}
	for _, served := range existing {
		if served.Name == modelName {
			// Re-register when EITHER the routing OR the recorded capability is stale. Routing
			// is on the served-model-name ALIAS (openai/<alias>), not the HF model id.
			if served.SupportsTools == supportsTools && served.RoutedTo == VLLMRoutedModel(alias) {
				return nil // already registered correctly
			}
			if err := manager.DeleteModel(served.ID); err != nil {
				return err
			}
			break // re-add below with corrected routing/capability
		}
	}
	params, info := vllmModelParamsInfo(alias, apiBase, &supportsTools)
	return manager.AddModel(modelName, params, info)
}

// UnregisterVLLMModel removes the DB-backed model registered for a vLLM model. It looks up
// the entry whose model_name == "vllm/<alias>" (VLLMModelName) and deletes it by its
// LiteLLM-assigned id. A no-op (no error) when no such model is registered.
//
// hardware bring-up: the LIVE POST /model/delete round-trip is exercised only against a
// running aip-litellm — verify on a provisioned host.
func (manager *KeyManager) UnregisterVLLMModel(alias string) error {
	modelName := VLLMModelName(alias)
	existing, err := manager.ListModels()
	if err != nil {
		return err
	}
	for _, served := range existing {
		if served.Name == modelName {
			return manager.DeleteModel(served.ID)
		}
	}
	return nil // not registered — nothing to do
}

// LiveModel is a model currently served by the gateway, parsed from GET
// /model/info: the public model_name, the routed litellm_params.model (whose
// prefix is the Provider), and the LiteLLM-assigned model_info.id used to delete it.
type LiveModel struct {
	ID       string
	Name     string
	RoutedTo string
	Provider string
	// SupportsTools reports whether the model can do tool/function calling. It reflects
	// model_info.supports_function_calling from the gateway, defaulting to TRUE when the
	// field is absent (unknown) so cloud models — which don't set it — keep their prior
	// tool-capable treatment; only a model explicitly registered false (a completion-only
	// local model) reports false.
	SupportsTools bool
}

// ListModels returns the live model set from GET /model/info (model_name +
// litellm_params.model + model_info.id), de-duplicated by model_name. The provider
// is derived from the routed target's prefix, falling back to the name's prefix.
func (manager *KeyManager) ListModels() ([]LiveModel, error) {
	var parsed struct {
		Data []struct {
			ModelName     string `json:"model_name"`
			LitellmParams struct {
				Model string `json:"model"`
			} `json:"litellm_params"`
			ModelInfo struct {
				ID                      string `json:"id"`
				SupportsFunctionCalling *bool  `json:"supports_function_calling"`
			} `json:"model_info"`
		} `json:"data"`
	}
	if err := manager.doJSON(http.MethodGet, "/model/info", nil, &parsed); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(parsed.Data))
	models := make([]LiveModel, 0, len(parsed.Data))
	for _, entry := range parsed.Data {
		name := entry.ModelName
		if name == "" {
			name = entry.LitellmParams.Model
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		routed := entry.LitellmParams.Model
		provider := providerPrefix(routed)
		if provider == "" {
			provider = providerPrefix(name)
		}
		// Absent supports_function_calling = unknown = tool-capable (preserves cloud
		// models' prior treatment); only an explicit false marks a model non-tool.
		supportsTools := entry.ModelInfo.SupportsFunctionCalling == nil || *entry.ModelInfo.SupportsFunctionCalling
		models = append(models, LiveModel{
			ID:            entry.ModelInfo.ID,
			Name:          name,
			RoutedTo:      routed,
			Provider:      provider,
			SupportsTools: supportsTools,
		})
	}
	return models, nil
}
