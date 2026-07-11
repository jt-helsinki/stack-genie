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
// LiteLLM routes (LiteLLMModelParam(catalogID) for cloud, "ollama/<name>" for
// local). Exactly one credential strategy is used: CredentialName references a
// stored DB credential (cloud), while APIBase is set for Ollama (no key). Empty
// fields are omitted from the request.
type ModelParams struct {
	Model          string `json:"model"`
	CredentialName string `json:"litellm_credential_name,omitempty"`
	APIBase        string `json:"api_base,omitempty"`
	APIKey         string `json:"api_key,omitempty"`
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
}

// AddModel registers a DB-backed model (POST /model/new). model_name is the
// PUBLIC handle the agent names (the catalog id verbatim for cloud, "ollama/<name>"
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

// OllamaModelName is the PUBLIC model handle for a locally-installed Ollama model:
// "ollama/<name>" verbatim (the name as Ollama reports it, e.g. "llama3.2:3b"). This
// is exactly what the in-workspace agent names and what LiteLLM's ollama/* wildcard
// otherwise routes — registering it as an explicit DB-backed model gives it a stable
// model_info.id (and surfaces it in the live model list) without changing the handle.
func OllamaModelName(name string) string {
	return "ollama/" + name
}

// OllamaRoutedModel is the value LiteLLM ROUTES on (litellm_params.model) for a local
// Ollama model: "ollama_chat/<name>", NOT "ollama/<name>". LiteLLM's `ollama` provider
// targets Ollama's legacy /api/generate (a single-prompt completion endpoint) which does
// NOT properly handle a chat `messages` array, `tools`, or streamed tool-call deltas — so
// a coding agent (opencode/pi) sees the request reach Ollama but gets no usable output.
// The `ollama_chat` provider targets /api/chat, which supports chat messages, function
// calling, and proper streaming (docs.litellm.ai — "we recommend using ollama_chat for
// chat"). The PUBLIC handle stays "ollama/<name>" (OllamaModelName), so the agent-facing
// model id is unchanged; only the internal routing switches to the chat endpoint.
func OllamaRoutedModel(name string) string {
	return "ollama_chat/" + name
}

// RegisterOllamaModel registers a locally-installed Ollama model as a DB-backed model
// in the gateway, so a freshly-pulled model appears in the live catalogue with its own
// id. The public model_name is "ollama/<name>" (OllamaModelName, the agent-facing handle)
// while the routed litellm_params.model is "ollama_chat/<name>" (OllamaRoutedModel) so
// LiteLLM uses Ollama's /api/chat (messages + tools + streaming) instead of the legacy
// /api/generate; api_base is the in-network Ollama the gateway reaches on aip-net
// (OllamaAPIBase = http://aip-ollama:11434), and no credential is referenced (Ollama needs none).
//
// Idempotent-ish: if a model with this model_name already exists (ListModels), the add
// is skipped so re-pulling does not create a duplicate.
//
// hardware bring-up: the LIVE POST /model/new round-trip is exercised only against a
// running aip-litellm — verify on a provisioned host.
func (manager *KeyManager) RegisterOllamaModel(name string) error {
	modelName := OllamaModelName(name)
	existing, err := manager.ListModels()
	if err != nil {
		return err
	}
	for _, model := range existing {
		if model.Name == modelName {
			return nil // already registered — don't duplicate
		}
	}
	return manager.AddModel(modelName,
		ModelParams{Model: OllamaRoutedModel(name), APIBase: OllamaAPIBase}, ModelInfo{})
}

// RegisterOllamaModels registers every given local Ollama model in LiteLLM that is not
// ALREADY served (idempotent), returning the model_names it newly added. It lists the
// current set ONCE (unlike calling RegisterOllamaModel per name, which lists each call), so
// a bulk reconcile of the installed store is one list + one AddModel per missing model.
// It only ADDS — it never deletes — so it safely re-registers models the gateway lost
// (e.g. a model still installed in Ollama but missing from LiteLLM) without touching the
// rest. Stops at the first AddModel error, returning what was added so far.
func (manager *KeyManager) RegisterOllamaModels(names []string) ([]string, error) {
	current, err := manager.ListModels()
	if err != nil {
		return nil, err
	}
	served := make(map[string]bool, len(current))
	for _, model := range current {
		served[model.Name] = true
	}
	var added []string
	for _, name := range names {
		if name == "" {
			continue
		}
		modelName := OllamaModelName(name)
		if served[modelName] {
			continue
		}
		if err := manager.AddModel(modelName, ModelParams{Model: OllamaRoutedModel(name), APIBase: OllamaAPIBase}, ModelInfo{}); err != nil {
			return added, err
		}
		served[modelName] = true
		added = append(added, modelName)
	}
	return added, nil
}

// UnregisterOllamaModel removes the DB-backed model registered for a local Ollama
// model. It looks up the entry whose model_name == "ollama/<name>" (OllamaModelName)
// and deletes it by its LiteLLM-assigned id. A no-op (no error) when no such model is
// registered.
//
// hardware bring-up: the LIVE POST /model/delete round-trip is exercised only against
// a running aip-litellm — verify on a provisioned host.
func (manager *KeyManager) UnregisterOllamaModel(name string) error {
	modelName := OllamaModelName(name)
	existing, err := manager.ListModels()
	if err != nil {
		return err
	}
	for _, model := range existing {
		if model.Name == modelName {
			return manager.DeleteModel(model.ID)
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
				ID string `json:"id"`
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
		models = append(models, LiveModel{
			ID:       entry.ModelInfo.ID,
			Name:     name,
			RoutedTo: routed,
			Provider: provider,
		})
	}
	return models, nil
}
