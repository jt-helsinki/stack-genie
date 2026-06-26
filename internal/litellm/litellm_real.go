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

// The host CLI reaches LiteLLM ONLY through the nginx gateway (aip-proxy) on the
// host — never the LiteLLM container directly (every service container is
// internal-only on aip-net now). Two routes through nginx:
//
//   - the ADMIN/management surface (/model/info, /v1/models, /health*, /key*,
//     /credentials, …) is fronted by nginx's `location /llm/` which strips the
//     prefix and forwards to aip-litellm:4000. AdminBaseURL is that base.
//   - the CHAT/model path (/v1/chat/completions) goes through nginx's `location
//     /v1/` → Headroom → LiteLLM, exercising the REAL model path (compression +
//     guardrails). GatewayBaseURL is that base.
//
// nginx publishes proxyHostPort (18787) on the host; the CLI reaches its own host
// gateway on 127.0.0.1 (IPv4) — NOT "localhost", which resolves to IPv6 ::1 and
// fails the dial, since nginx publishes on the IPv4 bindHost in standalone.
// AdminBaseURL is overridable via LITELLM_BASE_URL (e.g. a remote gateway); the
// chat base derives from the same host so a remote admin base keeps the chat test
// on the matching gateway.
const (
	// proxyHostPort mirrors setup.proxyHostPort (the nginx gateway host port). Kept
	// as a local const to avoid an import cycle (setup imports litellm).
	proxyHostPort = "18787"
	// defaultAdminBaseURL is the LiteLLM admin surface via nginx `/llm`.
	defaultAdminBaseURL = "http://127.0.0.1:" + proxyHostPort + "/llm"
)

// AdminBaseURL is the LiteLLM admin/management base URL (through nginx `/llm`),
// overridable via LITELLM_BASE_URL. Trailing slash trimmed.
func AdminBaseURL() string {
	baseURL := os.Getenv("LITELLM_BASE_URL")
	if baseURL == "" {
		baseURL = defaultAdminBaseURL
	}
	return strings.TrimRight(baseURL, "/")
}

// GatewayBaseURL is the model-path base URL (nginx `/v1` → Headroom → LiteLLM),
// where the chat test runs so it exercises the real compression+guardrail path.
// It derives from the admin base's scheme+host so a LITELLM_BASE_URL override
// keeps the chat test on the matching gateway: it replaces a trailing "/llm" with
// "/v1", else appends "/v1".
func GatewayBaseURL() string {
	admin := AdminBaseURL()
	if trimmed := strings.TrimSuffix(admin, "/llm"); trimmed != admin {
		return trimmed + "/v1"
	}
	return admin + "/v1"
}

// realClient talks to a running LiteLLM gateway through the nginx host gateway.
// adminURL fronts the management surface (`/health/liveliness`, `/model/info`),
// gatewayURL fronts the model path (`/v1/chat/completions`).
type realClient struct {
	adminURL   string
	gatewayURL string
	httpClient *http.Client
}

// RealClient returns a Client bound to the local LiteLLM gateway via nginx.
func RealClient() Client {
	return realClient{
		adminURL:   AdminBaseURL(),
		gatewayURL: GatewayBaseURL(),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

func (client realClient) Status() (StatusInfo, error) {
	// There is NO default model in the catalog-driven system (DefaultRouting() is
	// empty), so Default stays "" and the Human renderer omits the default line. The
	// served-model list and the derived provider set come LIVE from the gateway below.
	info := StatusInfo{Default: DefaultRouting().Default, BaseURL: client.adminURL}
	// Use the unauthenticated liveness probe: /health is auth-gated and returns
	// 401 once a master key is set (the secured-UI default), which would make a
	// healthy proxy look down. /health/liveliness needs no credential.
	response, err := client.httpClient.Get(client.adminURL + "/health/liveliness")
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
	request, err := http.NewRequest(http.MethodGet, client.adminURL+"/model/info", nil)
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
	request, err := http.NewRequest(http.MethodPost, client.gatewayURL+"/chat/completions", bytes.NewReader(payload))
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
