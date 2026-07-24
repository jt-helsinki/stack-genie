package ollama

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient binds a realClient to a test server (no env / default-port path).
func newTestClient(server *httptest.Server) realClient {
	return realClient{baseURL: strings.TrimRight(server.URL, "/"), httpClient: server.Client()}
}

func TestListParsesTags(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/tags" {
			http.Error(writer, "not found", http.StatusNotFound)
			return
		}
		_, _ = writer.Write([]byte(`{"models":[
			{"name":"gemma4:31b","size":12345,"modified_at":"2025-05-10T08:06:48Z",
			 "details":{"parameter_size":"31B","quantization_level":"Q4_K_M"}},
			{"name":"llama3.2:3b","size":67890,"details":{"parameter_size":"3B"}}
		]}`))
	}))
	defer server.Close()

	models, err := newTestClient(server).List()
	if err != nil {
		test.Fatalf("List: %v", err)
	}
	if len(models) != 2 {
		test.Fatalf("got %d models, want 2", len(models))
	}
	if models[0].Name != "gemma4:31b" || models[0].Size != 12345 {
		test.Fatalf("model[0] = %+v", models[0])
	}
	if models[0].ParameterSize != "31B" || models[0].QuantizationLevel != "Q4_K_M" {
		test.Fatalf("model[0] details = %+v", models[0])
	}
	if models[0].Modified != "2025-05-10T08:06:48Z" {
		test.Fatalf("model[0].Modified = %q", models[0].Modified)
	}
}

func TestPullStreamsProgressFrames(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/pull" || request.Method != http.MethodPost {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		// Verify the request body uses the "model" field (current API).
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		if body["model"] != "llama3.2" {
			http.Error(writer, "wrong body field", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(writer, `{"status":"pulling manifest"}`+"\n")
		_, _ = io.WriteString(writer, `{"status":"downloading","digest":"sha256:abc","total":100,"completed":40}`+"\n")
		_, _ = io.WriteString(writer, `{"status":"downloading","digest":"sha256:abc","total":100,"completed":100}`+"\n")
		_, _ = io.WriteString(writer, `{"status":"success"}`+"\n")
	}))
	defer server.Close()

	var frames []PullProgress
	err := newTestClient(server).Pull("llama3.2", func(progress PullProgress) {
		frames = append(frames, progress)
	})
	if err != nil {
		test.Fatalf("Pull: %v", err)
	}
	if len(frames) != 4 {
		test.Fatalf("got %d progress frames, want 4: %+v", len(frames), frames)
	}
	if frames[1].Total != 100 || frames[1].Completed != 40 || frames[1].Digest != "sha256:abc" {
		test.Fatalf("frame[1] = %+v", frames[1])
	}
	if frames[3].Status != "success" {
		test.Fatalf("final frame = %+v", frames[3])
	}
}

func TestPullSurfacesErrorFrame(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"error":"model not found"}`+"\n")
	}))
	defer server.Close()

	err := newTestClient(server).Pull("nope", nil)
	if err == nil || !strings.Contains(err.Error(), "model not found") {
		test.Fatalf("expected error-frame surfaced, got %v", err)
	}
}

func TestRemoveSendsModelBody(test *testing.T) {
	var gotBody map[string]any
	var gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotMethod = request.Method
		_ = json.NewDecoder(request.Body).Decode(&gotBody)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := newTestClient(server).Remove("llama3.2:3b"); err != nil {
		test.Fatalf("Remove: %v", err)
	}
	if gotMethod != http.MethodDelete {
		test.Fatalf("method = %q, want DELETE", gotMethod)
	}
	if gotBody["model"] != "llama3.2:3b" {
		test.Fatalf("delete body = %+v, want field model=llama3.2:3b", gotBody)
	}
	if _, hasName := gotBody["name"]; hasName {
		test.Fatalf("delete body must use 'model', not 'name': %+v", gotBody)
	}
}

func TestRemoveNotFound(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	err := newTestClient(server).Remove("ghost")
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		test.Fatalf("expected NotFoundError, got %v", err)
	}
}

func TestShowParsesDetails(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		if body["model"] != "gemma4" {
			http.Error(writer, "wrong body", http.StatusBadRequest)
			return
		}
		_, _ = writer.Write([]byte(`{"parameters":"stop \"<eot>\"","template":"{{ .Prompt }}",
			"details":{"parameter_size":"31B","quantization_level":"Q4_K_M","family":"gemma","format":"gguf"},
			"capabilities":["completion","tools"]}`))
	}))
	defer server.Close()

	info, err := newTestClient(server).Show("gemma4")
	if err != nil {
		test.Fatalf("Show: %v", err)
	}
	if info.Name != "gemma4" || info.ParameterSize != "31B" || info.Family != "gemma" {
		test.Fatalf("info = %+v", info)
	}
	if len(info.Capabilities) != 2 {
		test.Fatalf("caps = %+v", info.Capabilities)
	}
}

func TestShowParsesContextLength(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		// model_info carries the context length under an architecture-prefixed key.
		_, _ = writer.Write([]byte(`{"details":{"family":"qwen3"},
			"model_info":{"general.architecture":"qwen3","qwen3.context_length":32768,"qwen3.block_count":36}}`))
	}))
	defer server.Close()

	info, err := newTestClient(server).Show("qwen3")
	if err != nil {
		test.Fatalf("Show: %v", err)
	}
	if info.ContextLength != 32768 {
		test.Fatalf("ContextLength = %d, want 32768", info.ContextLength)
	}
}

func TestShowContextLengthAbsentIsZero(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"details":{"family":"gemma"},"model_info":{"general.architecture":"gemma"}}`))
	}))
	defer server.Close()

	info, err := newTestClient(server).Show("gemma")
	if err != nil {
		test.Fatalf("Show: %v", err)
	}
	if info.ContextLength != 0 {
		test.Fatalf("ContextLength = %d, want 0 when absent", info.ContextLength)
	}
}

func TestSetNumCtxPostsCreateBody(test *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		gotMethod = request.Method
		_ = json.NewDecoder(request.Body).Decode(&gotBody)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := newTestClient(server).SetNumCtx("qwen3", 32768); err != nil {
		test.Fatalf("SetNumCtx: %v", err)
	}
	if gotPath != "/api/create" || gotMethod != http.MethodPost {
		test.Fatalf("request = %s %s, want POST /api/create", gotMethod, gotPath)
	}
	if gotBody["model"] != "qwen3" || gotBody["from"] != "qwen3" {
		test.Fatalf("body = %+v, want model+from = qwen3", gotBody)
	}
	parameters, ok := gotBody["parameters"].(map[string]any)
	if !ok {
		test.Fatalf("body.parameters = %+v, want an object", gotBody["parameters"])
	}
	if numCtx, _ := parameters["num_ctx"].(float64); int(numCtx) != 32768 {
		test.Fatalf("parameters.num_ctx = %v, want 32768", parameters["num_ctx"])
	}
}

func TestSetNumCtxSurfacesNon2xx(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	if err := newTestClient(server).SetNumCtx("qwen3", 32768); err == nil {
		test.Fatal("expected error on non-2xx, got nil")
	}
}

func TestRecommendedNumCtx(test *testing.T) {
	cases := []struct {
		name          string
		contextLength int
		want          int
	}{
		{"unknown is zero", 0, 0},
		{"negative is zero", -1, 0},
		{"below ceiling passes through", 2048, 2048},
		{"at ceiling", MaxNumCtx, MaxNumCtx},
		{"above ceiling is capped", 131072, MaxNumCtx},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			if got := RecommendedNumCtx(testCase.contextLength); got != testCase.want {
				test.Fatalf("RecommendedNumCtx(%d) = %d, want %d", testCase.contextLength, got, testCase.want)
			}
		})
	}
}

func TestUnreachableIsClassified(test *testing.T) {
	client := realClient{baseURL: "http://127.0.0.1:1", httpClient: &http.Client{Timeout: time.Second}}
	_, err := client.List()
	if !IsUnreachable(err) {
		test.Fatalf("expected unreachable classification, got %v", err)
	}
}

// TestDefaultBaseURLRoutesThroughNginxOllama verifies the host CLI reaches Ollama
// through the nginx gateway's /ollama route (never the container directly): the
// default base is http://127.0.0.1:18787/ollama (127.0.0.1, NOT localhost → ::1,
// which would refuse the dial), so an /api/* call becomes /ollama/api/* on the wire.
func TestDefaultBaseURLRoutesThroughNginxOllama(test *testing.T) {
	if DefaultBaseURL != "http://127.0.0.1:18787/ollama" {
		test.Errorf("DefaultBaseURL = %q, want the nginx /ollama route on 127.0.0.1", DefaultBaseURL)
	}
	// With a base ending in /ollama, the realClient must hit /ollama/api/tags.
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		_, _ = writer.Write([]byte(`{"models":[]}`))
	}))
	defer server.Close()
	client := realClient{baseURL: strings.TrimRight(server.URL, "/") + "/ollama", httpClient: server.Client()}
	if _, err := client.List(); err != nil {
		test.Fatalf("List: %v", err)
	}
	if gotPath != "/ollama/api/tags" {
		test.Errorf("queried %q, want /ollama/api/tags (through nginx)", gotPath)
	}
}
