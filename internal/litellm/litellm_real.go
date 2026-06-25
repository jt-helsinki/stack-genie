package litellm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// realClient talks to a running LiteLLM gateway over its OpenAI-compatible HTTP
// API (`/health/liveliness`, `/v1/chat/completions`). Base URL comes from LITELLM_BASE_URL
// or defaults to the local gateway port.
type realClient struct {
	baseURL    string
	httpClient *http.Client
}

// RealClient returns a Client bound to the local LiteLLM gateway.
func RealClient() Client {
	baseURL := os.Getenv("LITELLM_BASE_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:14000"
	}
	return realClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

func (client realClient) Status() (StatusInfo, error) {
	// The DEFAULT model legitimately stays from platform config: the gateway's
	// model-list endpoints (/model/info, /v1/models) do NOT mark a default model, so
	// DefaultRouting().Default is the source of truth for the default handle. The
	// served-model list and the derived provider set, by contrast, come LIVE from the
	// gateway below.
	info := StatusInfo{Default: DefaultRouting().Default, BaseURL: client.baseURL}
	// Use the unauthenticated liveness probe: /health is auth-gated and returns
	// 401 once a master key is set (the secured-UI default), which would make a
	// healthy proxy look down. /health/liveliness needs no credential.
	response, err := client.httpClient.Get(client.baseURL + "/health/liveliness")
	if err != nil {
		return info, nil // unreachable → Healthy stays false; not a CLI error
	}
	defer func() { _ = response.Body.Close() }()
	info.Healthy = response.StatusCode == http.StatusOK
	if !info.Healthy {
		return info, nil
	}
	// Gateway is up: fetch the LIVE served-model list and derive providers from it.
	// A failure here (e.g. unauthorized) must NOT fail the whole status command —
	// record a note and still render health.
	models, err := client.Models()
	if err != nil {
		info.ModelsNote = "could not list served models: " + err.Error()
		return info, nil
	}
	info.Models = models
	info.Providers = providersFromModels(models)
	info.Ollama = hasOllamaModel(models)
	return info, nil
}

// Models fetches the LIVE served-model list from the gateway. It queries
// /model/info (the richer LiteLLM admin endpoint: it gives model_name +
// litellm_params.model for the provider prefix + model_info.mode), authenticating
// with the master key the same way Test does. /model/info is auth-gated on a
// secured proxy, so the Bearer key is required there; an unsecured gateway needs
// none (best-effort). Both endpoints return wildcard handles (e.g. `openai/*`) and
// named aliases — that is expected and reflects the real config.
func (client realClient) Models() ([]Model, error) {
	request, err := http.NewRequest(http.MethodGet, client.baseURL+"/model/info", nil)
	if err != nil {
		return nil, err
	}
	if key := resolveMasterKey(); key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("unauthorized (HTTP %d) — the gateway requires a master key to list models", response.StatusCode)
	}
	if response.StatusCode != http.StatusOK {
		if message := parseProviderError(body); message != "" {
			return nil, fmt.Errorf("gateway returned HTTP %d: %s", response.StatusCode, message)
		}
		return nil, fmt.Errorf("gateway returned HTTP %d", response.StatusCode)
	}
	return parseModelInfo(body)
}

// parseModelInfo parses LiteLLM's /model/info response shape:
//
//	{"data":[{"model_name":"gpt-5.5","litellm_params":{"model":"openai/gpt-5.5"},
//	          "model_info":{"mode":"chat", ...}}, ...]}
//
// The provider prefix is taken from litellm_params.model (e.g. "openai/gpt-5.5" →
// "openai"), falling back to the model_name itself when litellm_params has no slash
// (so a bare alias still classifies). Wildcard ids (e.g. "openai/*") are handled
// gracefully — the prefix is still the provider. Results are de-duplicated by name
// and sorted for stable rendering.
func parseModelInfo(body []byte) ([]Model, error) {
	var parsed struct {
		Data []struct {
			ModelName     string `json:"model_name"`
			LitellmParams struct {
				Model string `json:"model"`
			} `json:"litellm_params"`
			ModelInfo struct {
				Mode string `json:"mode"`
			} `json:"model_info"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("could not parse model list: %w", err)
	}
	seen := map[string]bool{}
	models := make([]Model, 0, len(parsed.Data))
	for _, entry := range parsed.Data {
		name := strings.TrimSpace(entry.ModelName)
		if name == "" {
			name = strings.TrimSpace(entry.LitellmParams.Model)
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		// Derive the provider from the routed target (litellm_params.model) when it
		// carries a slash; otherwise from the served name.
		source := entry.LitellmParams.Model
		if !strings.Contains(source, "/") {
			source = name
		}
		models = append(models, Model{
			Name:     name,
			Provider: providerPrefix(source),
			Mode:     strings.TrimSpace(entry.ModelInfo.Mode),
		})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, nil
}

// providerPrefix returns the provider portion of a model id ("openai/gpt-5.5" →
// "openai", "openai/*" → "openai"), or "" when the id has no slash.
func providerPrefix(id string) string {
	if slashIndex := strings.IndexByte(id, '/'); slashIndex >= 0 {
		return id[:slashIndex]
	}
	return ""
}

// providersFromModels returns the distinct, sorted provider prefixes across the
// LIVE served-model list — replacing the hardcoded providersOf(DefaultRouting()).
func providersFromModels(models []Model) []string {
	seen := map[string]bool{}
	var providers []string
	for _, model := range models {
		if model.Provider == "" || seen[model.Provider] {
			continue
		}
		seen[model.Provider] = true
		providers = append(providers, model.Provider)
	}
	sort.Strings(providers)
	return providers
}

// hasOllamaModel reports whether any served model routes through Ollama (so the
// status can show the local-models, no-key framing).
func hasOllamaModel(models []Model) bool {
	for _, model := range models {
		if model.Provider == "ollama" {
			return true
		}
	}
	return false
}

func (client realClient) Test(model string) (TestResult, error) {
	payload, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
	})
	request, err := http.NewRequest(http.MethodPost, client.baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return TestResult{Model: model, OK: false}, err
	}
	request.Header.Set("Content-Type", "application/json")
	// Authenticate as admin when the gateway is secured: with a master key set,
	// every completion 401s, so `ai models test` could never probe a model. The key
	// is read from the running container (never disk); an unsecured gateway needs
	// none, so this is best-effort.
	if key := resolveMasterKey(); key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	start := time.Now()
	response, err := client.httpClient.Do(request)
	if err != nil {
		return TestResult{Model: model, OK: false}, err
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	result := TestResult{
		Model:     model,
		Status:    response.StatusCode,
		OK:        response.StatusCode == http.StatusOK,
		LatencyMS: int(time.Since(start).Milliseconds()),
	}
	if !result.OK {
		result.Error = parseProviderError(body)
	}
	return result, nil
}

// resolveMasterKey returns the gateway master key used to authenticate admin
// diagnostics (`ai models test`, `ai models status`'s model-list call) against a
// secured gateway. It is a package var so tests can inject a key (to assert the
// Bearer header) without a running container; production reads it from the live
// LiteLLM container via gatewayMasterKey.
var resolveMasterKey = func() string { return gatewayMasterKey(runtime.RealProber()) }

// gatewayMasterKey best-effort reads LITELLM_MASTER_KEY from the running LiteLLM
// container so admin diagnostics (`ai models test`) can authenticate against a
// secured gateway. Returns "" when the runtime/container/key is unavailable (an
// unsecured gateway needs no key). The value transits process memory only.
func gatewayMasterKey(prober runtime.Prober) string {
	containerRuntime, err := runtime.ContainerRuntimeName(prober)
	if err != nil {
		return ""
	}
	out, err := prober.Run(containerRuntime.Name, "inspect", "--format",
		"{{range .Config.Env}}{{println .}}{{end}}", litellmContainer)
	if err != nil {
		return ""
	}
	const prefix = "LITELLM_MASTER_KEY="
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, prefix) {
			if value := strings.TrimSpace(line[len(prefix):]); value != "" {
				return value
			}
		}
	}
	return ""
}

// parseProviderError pulls a human-readable message out of a LiteLLM/OpenAI-style
// error body ({"error":{"message":...}}), falling back to a trimmed raw snippet.
func parseProviderError(body []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		return strings.TrimSpace(parsed.Error.Message)
	}
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 300 {
		snippet = snippet[:300] + "…"
	}
	return snippet
}

func providersOf(routing Routing) []string {
	seen := map[string]bool{}
	var providers []string
	for _, target := range routing.Aliases {
		provider := target
		if slashIndex := strings.IndexByte(target, '/'); slashIndex >= 0 {
			provider = target[:slashIndex]
		}
		if !seen[provider] {
			seen[provider] = true
			providers = append(providers, provider)
		}
	}
	sort.Strings(providers)
	return providers
}

func hasOllama(routing Routing) bool {
	for _, target := range routing.Aliases {
		if strings.HasPrefix(target, "ollama/") {
			return true
		}
	}
	return false
}
