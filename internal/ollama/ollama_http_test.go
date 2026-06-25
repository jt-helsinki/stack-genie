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

func TestUnreachableIsClassified(test *testing.T) {
	client := realClient{baseURL: "http://127.0.0.1:1", httpClient: &http.Client{Timeout: time.Second}}
	_, err := client.List()
	if !IsUnreachable(err) {
		test.Fatalf("expected unreachable classification, got %v", err)
	}
}

func TestCatalogIncludesDefault(test *testing.T) {
	if DefaultCatalogModel() != "gemma4" {
		test.Fatalf("default catalog model = %q, want gemma4", DefaultCatalogModel())
	}
	found := false
	for _, entry := range Catalog() {
		if entry.Default && entry.Name == "gemma4" {
			found = true
		}
	}
	if !found {
		test.Fatal("gemma4 must be in the catalog and marked Default")
	}
}
