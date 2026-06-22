package litellm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
)

// litellmContainer is the platform-owned LiteLLM container whose environment
// carries LITELLM_MASTER_KEY (the admin credential for the key-management API).
const litellmContainer = "aip-litellm"

// KeyScope describes a LiteLLM virtual key to mint. Zero-value fields are
// omitted from the request body, so an empty/omitted Models means "all models
// allowed" (LiteLLM's default).
type KeyScope struct {
	Models    []string       // empty = all models allowed
	MaxBudget float64        // USD spend cap; 0 = no cap
	RPMLimit  int            // requests/min; 0 = unlimited
	TPMLimit  int            // tokens/min; 0 = unlimited
	Alias     string         // key_alias — a stable handle for delete-by-alias
	Duration  string         // e.g. "24h"; "" = no expiry
	Metadata  map[string]any // arbitrary tags stored with the key
}

// KeyDetails is the subset of LiteLLM's /key/info response the platform surfaces.
// It never carries the plaintext sk- value (only /key/generate returns that).
type KeyDetails struct {
	Token     string         `json:"token"`
	KeyName   string         `json:"key_name"`
	KeyAlias  string         `json:"key_alias"`
	Models    []string       `json:"models"`
	MaxBudget float64        `json:"max_budget"`
	Spend     float64        `json:"spend"`
	RPMLimit  int            `json:"rpm_limit"`
	TPMLimit  int            `json:"tpm_limit"`
	Expires   string         `json:"expires"`
	Metadata  map[string]any `json:"metadata"`
}

// KeyManager mints, inspects, and deletes LiteLLM virtual keys over the gateway's
// admin HTTP API (keys-in-LiteLLM, arch §17). It reads LITELLM_MASTER_KEY from the
// running LiteLLM container's environment via the injected runtime.Prober, so the
// master key is never read from platform disk and the manager is unit-testable.
type KeyManager struct {
	baseURL    string
	prober     runtime.Prober
	httpClient *http.Client
}

// NewKeyManager binds a KeyManager to the local LiteLLM gateway, resolving the
// base URL the same way as the chat client (LITELLM_BASE_URL or the default port).
func NewKeyManager(prober runtime.Prober) *KeyManager {
	return &KeyManager{
		baseURL:    resolveBaseURL(),
		prober:     prober,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// resolveBaseURL matches RealClient's base-URL resolution so the key API targets
// the same gateway as `ai models status|test`.
func resolveBaseURL() string {
	baseURL := os.Getenv("LITELLM_BASE_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:14000"
	}
	return strings.TrimRight(baseURL, "/")
}

// masterKey reads LITELLM_MASTER_KEY from the running LiteLLM container's
// environment via the prober (docker/podman inspect). It returns ExitMissingDep
// when the gateway/master key is not reachable.
func (manager *KeyManager) masterKey() (string, error) {
	containerRuntime, err := runtime.ContainerRuntimeName(manager.prober)
	if err != nil {
		return "", output.Errorf(output.ExitMissingDep, "litellm gateway is not reachable: %v", err)
	}
	out, err := manager.prober.Run(containerRuntime.Name, "inspect", "--format",
		"{{range .Config.Env}}{{println .}}{{end}}", litellmContainer)
	if err != nil {
		return "", output.Errorf(output.ExitMissingDep, "litellm gateway (%s) is not running", litellmContainer)
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
	return "", output.Errorf(output.ExitMissingDep, "LITELLM_MASTER_KEY is not set on %s", litellmContainer)
}

// doJSON performs an authenticated admin request and decodes a JSON response into
// result (when result is non-nil). HTTP/transport failures surface as
// ExitRuntimeFailure; a missing master key surfaces as ExitMissingDep.
func (manager *KeyManager) doJSON(method, path string, body any, result any) error {
	master, err := manager.masterKey()
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
	request, err := http.NewRequest(method, manager.baseURL+path, reader)
	if err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "build request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+master)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := manager.httpClient.Do(request)
	if err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "litellm request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, _ := io.ReadAll(response.Body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return output.Errorf(output.ExitRuntimeFailure, "litellm %s %s: %s", method, path, errorMessage(payload, response.StatusCode))
	}
	if result != nil {
		if err := json.Unmarshal(payload, result); err != nil {
			return output.Errorf(output.ExitRuntimeFailure, "decode response: %v", err)
		}
	}
	return nil
}

// errorMessage extracts a human-readable message from a LiteLLM error body,
// falling back to a status-coded snippet.
func errorMessage(body []byte, status int) string {
	if message := parseProviderError(body); message != "" {
		return message
	}
	return fmt.Sprintf("status %d", status)
}

// GenerateKey mints a virtual key for the given scope and returns the plaintext
// sk- secret to hand to the workspace agent (the only place LiteLLM returns it).
func (manager *KeyManager) GenerateKey(scope KeyScope) (string, error) {
	body := map[string]any{}
	if len(scope.Models) > 0 {
		body["models"] = scope.Models
	}
	if scope.MaxBudget > 0 {
		body["max_budget"] = scope.MaxBudget
	}
	if scope.RPMLimit > 0 {
		body["rpm_limit"] = scope.RPMLimit
	}
	if scope.TPMLimit > 0 {
		body["tpm_limit"] = scope.TPMLimit
	}
	if scope.Alias != "" {
		body["key_alias"] = scope.Alias
	}
	if scope.Duration != "" {
		body["duration"] = scope.Duration
	}
	if len(scope.Metadata) > 0 {
		body["metadata"] = scope.Metadata
	}
	var generated struct {
		Key string `json:"key"`
	}
	if err := manager.doJSON(http.MethodPost, "/key/generate", body, &generated); err != nil {
		return "", err
	}
	if generated.Key == "" {
		return "", output.Errorf(output.ExitRuntimeFailure, "litellm returned an empty key")
	}
	return generated.Key, nil
}

// DeleteKey revokes a virtual key by its sk- value.
func (manager *KeyManager) DeleteKey(key string) error {
	return manager.doJSON(http.MethodPost, "/key/delete", map[string]any{"keys": []string{key}}, nil)
}

// DeleteKeyByAlias revokes a virtual key by its key_alias.
func (manager *KeyManager) DeleteKeyByAlias(alias string) error {
	return manager.doJSON(http.MethodPost, "/key/delete", map[string]any{"key_aliases": []string{alias}}, nil)
}

// KeyInfo returns LiteLLM's metadata for a virtual key. The plaintext value is
// never returned here (only GenerateKey yields it).
func (manager *KeyManager) KeyInfo(key string) (KeyDetails, error) {
	var wrapper struct {
		Info KeyDetails `json:"info"`
	}
	if err := manager.doJSON(http.MethodGet, "/key/info?key="+key, nil, &wrapper); err != nil {
		return KeyDetails{}, err
	}
	return wrapper.Info, nil
}
