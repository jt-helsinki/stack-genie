// Package ollama provides a minimal health probe for the required Ollama service
// (arch §16). `ai doctor` uses it to report whether Ollama — the local model
// backend LiteLLM routes to — is reachable. Ollama exposes GET /api/version as a
// lightweight liveness endpoint (docs.ollama.com).
package ollama

import (
	"fmt"
	"net/http"
	"time"
)

// DefaultBaseURL is where the platform's Ollama container publishes on the host.
const DefaultBaseURL = "http://localhost:11434"

// Probe checks whether an Ollama server is reachable. BaseURL/Client are
// overridable (remote Ollama, tests); zero values use sensible defaults.
type Probe struct {
	BaseURL string
	Client  *http.Client
}

// RealProbe returns a Probe pointed at the local Ollama service with a short
// timeout (doctor should never hang on it).
func RealProbe() Probe {
	return Probe{BaseURL: DefaultBaseURL, Client: &http.Client{Timeout: 3 * time.Second}}
}

// Reachable returns nil when Ollama answers GET /api/version with 200.
func (probe Probe) Reachable() error {
	baseURL := probe.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	httpClient := probe.Client
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Get(baseURL + "/api/version")
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama: unexpected status %d", response.StatusCode)
	}
	return nil
}
