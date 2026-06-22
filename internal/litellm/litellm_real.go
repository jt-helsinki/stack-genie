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
	start := time.Now()
	response, err := client.httpClient.Post(client.baseURL+"/v1/chat/completions", "application/json", bytes.NewReader(payload))
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
