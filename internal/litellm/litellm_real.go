package litellm

import (
	"bytes"
	"encoding/json"
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
	routing := DefaultRouting()
	info := StatusInfo{Default: routing.Default, Providers: providersOf(routing), Ollama: hasOllama(routing), BaseURL: client.baseURL}
	// Use the unauthenticated liveness probe: /health is auth-gated and returns
	// 401 once a master key is set (the secured-UI default), which would make a
	// healthy proxy look down. /health/liveliness needs no credential.
	response, err := client.httpClient.Get(client.baseURL + "/health/liveliness")
	if err != nil {
		return info, nil // unreachable → Healthy stays false; not a CLI error
	}
	defer func() { _ = response.Body.Close() }()
	info.Healthy = response.StatusCode == http.StatusOK
	return info, nil
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
	if key := gatewayMasterKey(runtime.RealProber()); key != "" {
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
