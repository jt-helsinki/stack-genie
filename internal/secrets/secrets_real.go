package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// ErrGatewayMissing is returned when the LiteLLM gateway is not running, so its
// credential store cannot be reached (→ exit 3).
var ErrGatewayMissing = errors.New("litellm gateway is not running")

// litellmContainer is the platform-owned LiteLLM container whose environment
// carries LITELLM_MASTER_KEY (the admin credential for the credentials API).
const litellmContainer = "aip-litellm"

// realBroker fronts the LiteLLM gateway's provider-credential store
// (keys-in-LiteLLM, CLI §16.1) over its admin HTTP API (`/credentials`). Provider
// API keys are handed to LiteLLM (stored encrypted in its Postgres DB) and never
// written to platform disk; Entry carries names/metadata only. The master key is
// read at runtime from the running container's environment via the prober.
type realBroker struct {
	prober     runtime.Prober
	baseURL    string
	httpClient *http.Client
}

// RealBroker returns a Broker bound to the host's LiteLLM gateway.
func RealBroker() Broker {
	baseURL := os.Getenv("LITELLM_BASE_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:14000"
	}
	return realBroker{
		prober:     runtime.RealProber(),
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// masterKey reads LITELLM_MASTER_KEY from the running LiteLLM container's
// environment via the prober. Returns ErrGatewayMissing when the gateway/master
// key is not reachable.
func (broker realBroker) masterKey() (string, error) {
	containerRuntime, err := runtime.ContainerRuntimeName(broker.prober)
	if err != nil {
		return "", ErrGatewayMissing
	}
	out, err := broker.prober.Run(containerRuntime.Name, "inspect", "--format",
		"{{range .Config.Env}}{{println .}}{{end}}", litellmContainer)
	if err != nil {
		return "", ErrGatewayMissing
	}
	const prefix = "LITELLM_MASTER_KEY="
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, prefix) {
			value := strings.TrimSpace(line[len(prefix):])
			if value != "" {
				return value, nil
			}
		}
	}
	return "", ErrGatewayMissing
}

// do performs an authenticated admin request against the credentials API and
// decodes a JSON response into result (when non-nil). Transport/HTTP failures
// surface as ExitRuntimeFailure; a missing gateway surfaces as ErrGatewayMissing.
func (broker realBroker) do(method, path string, body any, result any) error {
	master, err := broker.masterKey()
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		encoded, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return output.Errorf(output.ExitRuntimeFailure, "encode request: %v", marshalErr)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, broker.baseURL+path, reader)
	if err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "build request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+master)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := broker.httpClient.Do(request)
	if err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "litellm request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, _ := io.ReadAll(response.Body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return output.Errorf(output.ExitRuntimeFailure, "litellm %s %s: %s", method, path, credentialError(payload, response.StatusCode))
	}
	if result != nil {
		if err := json.Unmarshal(payload, result); err != nil {
			return output.Errorf(output.ExitRuntimeFailure, "decode response: %v", err)
		}
	}
	return nil
}

// credentialError pulls a message out of a LiteLLM error body, falling back to a
// status-coded snippet.
func credentialError(body []byte, status int) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Detail any `json:"detail"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		if parsed.Error.Message != "" {
			return strings.TrimSpace(parsed.Error.Message)
		}
		if detail, ok := parsed.Detail.(string); ok && detail != "" {
			return strings.TrimSpace(detail)
		}
	}
	return fmt.Sprintf("status %d", status)
}

// providerFor maps a recognizable credential name to a LiteLLM provider slug, so
// LiteLLM can route the stored key. Unknown names return "" (provider omitted).
func providerFor(name string) string {
	switch strings.ToUpper(name) {
	case "OPENAI_API_KEY":
		return "openai"
	case "ANTHROPIC_API_KEY":
		return "anthropic"
	case "GEMINI_API_KEY":
		return "gemini"
	case "OPENROUTER_API_KEY":
		return "openrouter"
	case "GROQ_API_KEY":
		return "groq"
	default:
		return ""
	}
}

// Set stores (or replaces) a provider credential by name. The value flows
// straight to LiteLLM's encrypted store and is never written to platform disk.
func (broker realBroker) Set(name string, value []byte) error {
	credentialInfo := map[string]any{}
	if provider := providerFor(name); provider != "" {
		credentialInfo["provider"] = provider
	}
	body := map[string]any{
		"credential_name":   name,
		"credential_info":   credentialInfo,
		"credential_values": map[string]any{"api_key": string(value)},
	}
	return broker.do(http.MethodPost, "/credentials", body, nil)
}

// Map has no meaning under keys-in-LiteLLM: provider keys live in the LiteLLM
// store and are resolved by LiteLLM's os.environ placeholders, so there is no
// per-workspace placeholder to bind. It existed only for the retired
// placeholder-swap model.
func (broker realBroker) Map(string, string) error {
	return output.Errorf(output.ExitInvalidInput,
		"secrets map is not applicable under keys-in-LiteLLM: provider keys are resolved by the gateway, not bound to a workspace placeholder")
}

// Remove deletes a stored provider credential by name.
func (broker realBroker) Remove(name string) error {
	return broker.do(http.MethodDelete, "/credentials/"+name, nil, nil)
}

// List returns stored credential names/metadata only — never values (the API
// returns them masked, but Entry has no value field by design either way).
func (broker realBroker) List() ([]Entry, error) {
	var response struct {
		Credentials []struct {
			CredentialName string `json:"credential_name"`
			CredentialInfo struct {
				Provider string `json:"provider"`
			} `json:"credential_info"`
		} `json:"credentials"`
	}
	if err := broker.do(http.MethodGet, "/credentials", nil, &response); err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(response.Credentials))
	for _, credential := range response.Credentials {
		entries = append(entries, Entry{Name: credential.CredentialName})
	}
	return entries, nil
}
