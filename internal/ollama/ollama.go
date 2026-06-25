// Package ollama provides a health probe for the required Ollama service
// (arch §16) and a client for managing its local model store (`ai models
// list|pull|rm|show`). Ollama is the local model backend LiteLLM routes to; it
// exposes GET /api/version as a lightweight liveness endpoint and an HTTP API
// (GET /api/tags, POST /api/pull, DELETE /api/delete, POST /api/show) for model
// management (docs.ollama.com). The package stays importable by `ai doctor` (it
// uses RealProbe as its OllamaProbe) — no import cycles.
package ollama

import (
	"fmt"
	"net/http"
	"time"
)

// Model is one entry from the local Ollama store (GET /api/tags). The fields are
// the subset surfaced by `ai models list`.
type Model struct {
	Name              string `json:"name"`
	Size              int64  `json:"size"`
	ParameterSize     string `json:"parameter_size,omitempty"`
	QuantizationLevel string `json:"quantization_level,omitempty"`
	Modified          string `json:"modified,omitempty"`
}

// PullProgress is one NDJSON progress frame from POST /api/pull. Total/Completed
// are byte counts for a layer download (zero on status-only frames such as
// "pulling manifest" / "success").
type PullProgress struct {
	Status    string `json:"status"`
	Digest    string `json:"digest,omitempty"`
	Total     int64  `json:"total,omitempty"`
	Completed int64  `json:"completed,omitempty"`
}

// ModelInfo is the metadata for one model (POST /api/show), surfaced by
// `ai models show` and the `ai ui` Models describe pane. It captures the full set
// of fields the /api/show response exposes: the details block (family,
// parameter_size, quantization_level, format, parent_model), the modelfile
// parameters/template/license blocks, the capabilities list, and the free-form
// model_info map.
type ModelInfo struct {
	Name              string         `json:"name"`
	ParameterSize     string         `json:"parameter_size,omitempty"`
	QuantizationLevel string         `json:"quantization_level,omitempty"`
	Family            string         `json:"family,omitempty"`
	Format            string         `json:"format,omitempty"`
	ParentModel       string         `json:"parent_model,omitempty"`
	Parameters        string         `json:"parameters,omitempty"`
	Template          string         `json:"template,omitempty"`
	License           string         `json:"license,omitempty"`
	Capabilities      []string       `json:"capabilities,omitempty"`
	ModelInfo         map[string]any `json:"model_info,omitempty"`
}

// Client manages the local Ollama model store. The real impl makes HTTP calls;
// tests use a fake. Pull streams: progress is reported one callback per NDJSON
// frame. Every method surfaces a clean error when Ollama is unreachable so the
// CLI can map it to an exit code.
type Client interface {
	List() ([]Model, error)
	Pull(name string, progress func(PullProgress)) error
	Remove(name string) error
	Show(name string) (ModelInfo, error)
}

// DefaultBaseURL is the host CLI's route to Ollama through the nginx gateway
// (aip-proxy). The Ollama container is internal-only on aip-net now (it no longer
// publishes :11434 to the host); nginx fronts it via `location /ollama/` which
// strips the prefix and forwards to aip-ollama:11434. So the /api/* calls become
// /ollama/api/* through nginx. The host MUST use 127.0.0.1 (IPv4), not "localhost"
// (which resolves to IPv6 ::1 and fails — nginx publishes on the IPv4 bindHost in
// standalone). Overridable via OLLAMA_BASE_URL (a remote Ollama).
const DefaultBaseURL = "http://127.0.0.1:18787/ollama"

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
