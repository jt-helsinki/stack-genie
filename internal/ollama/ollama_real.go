package ollama

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// realClient talks to a running Ollama server over its HTTP API. The base URL
// comes from OLLAMA_BASE_URL or defaults to the host-published port. List/Remove/
// Show use a short timeout; Pull uses none (a model download can take minutes and
// streams progress).
type realClient struct {
	baseURL    string
	httpClient *http.Client
}

// RealClient returns a Client bound to the local Ollama server. The base URL is
// overridable via OLLAMA_BASE_URL (a remote Ollama); empty uses DefaultBaseURL.
func RealClient() Client {
	baseURL := os.Getenv("OLLAMA_BASE_URL")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return realClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// tagsResponse is GET /api/tags. Each model carries a nested details object.
type tagsResponse struct {
	Models []struct {
		Name       string `json:"name"`
		Size       int64  `json:"size"`
		ModifiedAt string `json:"modified_at"`
		Details    struct {
			ParameterSize     string `json:"parameter_size"`
			QuantizationLevel string `json:"quantization_level"`
		} `json:"details"`
	} `json:"models"`
}

func (client realClient) List() ([]Model, error) {
	response, err := client.httpClient.Get(client.baseURL + "/api/tags")
	if err != nil {
		return nil, unreachable(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, statusError(response)
	}
	var parsed tagsResponse
	if err := json.NewDecoder(response.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("ollama: decoding model list: %w", err)
	}
	models := make([]Model, 0, len(parsed.Models))
	for _, entry := range parsed.Models {
		models = append(models, Model{
			Name:              entry.Name,
			Size:              entry.Size,
			ParameterSize:     entry.Details.ParameterSize,
			QuantizationLevel: entry.Details.QuantizationLevel,
			Modified:          entry.ModifiedAt,
		})
	}
	return models, nil
}

func (client realClient) Pull(name string, progress func(PullProgress)) error {
	payload, _ := json.Marshal(map[string]any{"model": name, "stream": true})
	// No client timeout for a pull: a model download streams over minutes; a
	// fixed deadline would abort it. The request body field is "model" (verified
	// against the current Ollama API docs).
	streamer := &http.Client{}
	request, err := http.NewRequest(http.MethodPost, client.baseURL+"/api/pull", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := streamer.Do(request)
	if err != nil {
		return unreachable(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return statusError(response)
	}
	// Decode the newline-delimited JSON frames, invoking the callback per frame.
	// An {"error":...} frame aborts with that message; the stream ends with a
	// {"status":"success"} frame.
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var frame struct {
			Status    string `json:"status"`
			Digest    string `json:"digest"`
			Total     int64  `json:"total"`
			Completed int64  `json:"completed"`
			Error     string `json:"error"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			return fmt.Errorf("ollama: decoding pull progress: %w", err)
		}
		if frame.Error != "" {
			return fmt.Errorf("ollama: %s", frame.Error)
		}
		if progress != nil {
			progress(PullProgress{
				Status:    frame.Status,
				Digest:    frame.Digest,
				Total:     frame.Total,
				Completed: frame.Completed,
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("ollama: reading pull stream: %w", err)
	}
	return nil
}

func (client realClient) Remove(name string) error {
	// DELETE /api/delete with the body field "model" (verified against the
	// current Ollama API docs).
	payload, _ := json.Marshal(map[string]any{"model": name})
	request, err := http.NewRequest(http.MethodDelete, client.baseURL+"/api/delete", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return unreachable(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return &NotFoundError{Name: name}
	}
	if response.StatusCode != http.StatusOK {
		return statusError(response)
	}
	return nil
}

// showResponse is POST /api/show (verified against the current Ollama API docs):
// top-level license/modelfile/parameters/template, a details object, the free-form
// model_info map, and a capabilities list. Model metadata lives under details; the
// modelfile blocks are top-level.
type showResponse struct {
	License    string `json:"license"`
	Parameters string `json:"parameters"`
	Template   string `json:"template"`
	Details    struct {
		ParameterSize     string `json:"parameter_size"`
		QuantizationLevel string `json:"quantization_level"`
		Family            string `json:"family"`
		Format            string `json:"format"`
		ParentModel       string `json:"parent_model"`
	} `json:"details"`
	ModelInfo    map[string]any `json:"model_info"`
	Capabilities []string       `json:"capabilities"`
}

func (client realClient) Show(name string) (ModelInfo, error) {
	payload, _ := json.Marshal(map[string]any{"model": name})
	request, err := http.NewRequest(http.MethodPost, client.baseURL+"/api/show", bytes.NewReader(payload))
	if err != nil {
		return ModelInfo{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return ModelInfo{}, unreachable(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return ModelInfo{}, &NotFoundError{Name: name}
	}
	if response.StatusCode != http.StatusOK {
		return ModelInfo{}, statusError(response)
	}
	var parsed showResponse
	if err := json.NewDecoder(response.Body).Decode(&parsed); err != nil {
		return ModelInfo{}, fmt.Errorf("ollama: decoding model info: %w", err)
	}
	return ModelInfo{
		Name:              name,
		ParameterSize:     parsed.Details.ParameterSize,
		QuantizationLevel: parsed.Details.QuantizationLevel,
		Family:            parsed.Details.Family,
		Format:            parsed.Details.Format,
		ParentModel:       parsed.Details.ParentModel,
		Parameters:        parsed.Parameters,
		Template:          parsed.Template,
		License:           parsed.License,
		Capabilities:      parsed.Capabilities,
		ModelInfo:         parsed.ModelInfo,
		ContextLength:     contextLengthFromModelInfo(parsed.ModelInfo),
	}, nil
}

// contextLengthFromModelInfo scans the free-form model_info map for the single
// architecture-prefixed key ending in ".context_length" (e.g.
// "qwen3.context_length") and coerces its JSON-number value to int. It returns 0
// when the key is absent or the value is not numeric (best-effort).
func contextLengthFromModelInfo(modelInfo map[string]any) int {
	for key, value := range modelInfo {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		switch typed := value.(type) {
		case float64:
			return int(typed)
		case json.Number:
			if parsed, err := typed.Int64(); err == nil {
				return int(parsed)
			}
		case int:
			return typed
		case int64:
			return int(typed)
		}
		return 0
	}
	return 0
}

// SetNumCtx bakes a num_ctx parameter into an existing model. POST /api/create
// with {"model":name,"from":name,"parameters":{"num_ctx":numCtx}} rebuilds the
// model from itself (reusing blobs) with the added parameter. Ollama streams
// status frames; a non-2xx response is surfaced as a clean error.
func (client realClient) SetNumCtx(name string, numCtx int) error {
	payload, _ := json.Marshal(map[string]any{
		"model":      name,
		"from":       name,
		"parameters": map[string]any{"num_ctx": numCtx},
	})
	request, err := http.NewRequest(http.MethodPost, client.baseURL+"/api/create", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return unreachable(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return statusError(response)
	}
	return nil
}

// NotFoundError reports a model the local store does not have (HTTP 404). The CLI
// maps it to a clear "no such model" error.
type NotFoundError struct{ Name string }

func (notFound *NotFoundError) Error() string {
	return fmt.Sprintf("ollama: model %q not found in the local store", notFound.Name)
}

// unreachable wraps a transport error so the CLI can tell "Ollama is down" apart
// from a protocol/parse failure and point at `ai services start ollama`.
type unreachableError struct{ err error }

func (down *unreachableError) Error() string {
	return fmt.Sprintf("ollama: could not reach the server: %s", down.err)
}
func (down *unreachableError) Unwrap() error { return down.err }

func unreachable(err error) error { return &unreachableError{err: err} }

// NewUnreachable builds an error that IsUnreachable classifies as "Ollama is
// down". It lets callers/tests (and fakes) simulate an unreachable server so the
// CLI's exit-code mapping can be exercised without a live HTTP transport failure.
func NewUnreachable() error {
	return unreachable(fmt.Errorf("connection refused"))
}

// IsUnreachable reports whether err is a transport-level "Ollama is down" error.
func IsUnreachable(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*unreachableError)
	return ok
}

// statusError reads the body of a non-200 response into a readable error.
func statusError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	snippet := strings.TrimSpace(string(body))
	if snippet == "" {
		return fmt.Errorf("ollama: unexpected status %d", response.StatusCode)
	}
	return fmt.Errorf("ollama: status %d: %s", response.StatusCode, snippet)
}
