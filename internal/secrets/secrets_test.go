package secrets

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
)

const testMasterKey = "sk-test-master"

// fakeProber answers ContainerRuntimeName and the master-key inspect.
type fakeProber struct {
	noRuntime  bool
	inspectOut string
}

func (prober fakeProber) LookPath(file string) (string, error) {
	if prober.noRuntime {
		return "", os.ErrNotExist
	}
	if file == "docker" {
		return "/usr/bin/docker", nil
	}
	return "", os.ErrNotExist
}

func (prober fakeProber) Run(string, ...string) ([]byte, error) {
	return []byte(prober.inspectOut), nil
}

func (prober fakeProber) Exists(string) bool { return true }

func okProber() fakeProber {
	return fakeProber{inspectOut: "LITELLM_MASTER_KEY=" + testMasterKey + "\n"}
}

// newTestBroker builds a realBroker pointed at the test server with a fake prober.
func newTestBroker(baseURL string, prober fakeProber) realBroker {
	return realBroker{
		prober:     prober,
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: http.DefaultClient,
	}
}

func TestSetSendsCredential(test *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotMethod = request.Method
		gotPath = request.URL.Path
		gotAuth = request.Header.Get("Authorization")
		payload, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(payload, &gotBody)
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	broker := newTestBroker(server.URL, okProber())
	if err := broker.Set("OPENAI_API_KEY", []byte("sk-secret-value")); err != nil {
		test.Fatalf("Set: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/credentials" {
		test.Fatalf("request = %s %s, want POST /credentials", gotMethod, gotPath)
	}
	if gotAuth != "Bearer "+testMasterKey {
		test.Fatalf("auth = %q", gotAuth)
	}
	if name, _ := gotBody["credential_name"].(string); name != "OPENAI_API_KEY" {
		test.Fatalf("credential_name = %v", gotBody["credential_name"])
	}
	info, _ := gotBody["credential_info"].(map[string]any)
	if provider, _ := info["provider"].(string); provider != "openai" {
		test.Fatalf("provider = %v, want openai", info["provider"])
	}
	values, _ := gotBody["credential_values"].(map[string]any)
	if apiKey, _ := values["api_key"].(string); apiKey != "sk-secret-value" {
		test.Fatalf("api_key not forwarded, got %v", values["api_key"])
	}
}

func TestSetUnknownNameOmitsProvider(test *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		payload, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(payload, &gotBody)
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	broker := newTestBroker(server.URL, okProber())
	if err := broker.Set("MY_CUSTOM_KEY", []byte("value")); err != nil {
		test.Fatalf("Set: %v", err)
	}
	info, _ := gotBody["credential_info"].(map[string]any)
	if _, present := info["provider"]; present {
		test.Fatalf("provider should be omitted for unknown name, got %v", info)
	}
}

func TestListReturnsNamesOnly(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/credentials" {
			test.Errorf("unexpected %s %s", request.Method, request.URL.Path)
		}
		_, _ = writer.Write([]byte(`{"credentials":[{"credential_name":"OPENAI_API_KEY","credential_info":{"provider":"openai"},"credential_values":{"api_key":"sk-...masked"}}]}`))
	}))
	defer server.Close()

	broker := newTestBroker(server.URL, okProber())
	entries, err := broker.List()
	if err != nil {
		test.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "OPENAI_API_KEY" {
		test.Fatalf("entries = %+v", entries)
	}
	// Entry must never carry a value: the struct has no value field, but guard
	// against any masked secret leaking into EnvVar.
	if entries[0].EnvVar != "" {
		test.Fatalf("EnvVar should be empty, got %q", entries[0].EnvVar)
	}
}

func TestRemoveDeletesByName(test *testing.T) {
	var gotMethod, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotMethod = request.Method
		gotPath = request.URL.Path
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	broker := newTestBroker(server.URL, okProber())
	if err := broker.Remove("OPENAI_API_KEY"); err != nil {
		test.Fatalf("Remove: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/credentials/OPENAI_API_KEY" {
		test.Fatalf("request = %s %s, want DELETE /credentials/OPENAI_API_KEY", gotMethod, gotPath)
	}
}

func TestMapNotApplicable(test *testing.T) {
	broker := newTestBroker("http://127.0.0.1:14000", okProber())
	err := broker.Map("OPENAI_API_KEY", "OPENAI_API_KEY")
	var platformErr *output.Error
	if !errors.As(err, &platformErr) || platformErr.Code != output.ExitInvalidInput {
		test.Fatalf("Map err = %v, want ExitInvalidInput", err)
	}
}

func TestGatewayMissing(test *testing.T) {
	broker := newTestBroker("http://127.0.0.1:14000", fakeProber{noRuntime: true})
	if err := broker.Set("OPENAI_API_KEY", []byte("x")); !errors.Is(err, ErrGatewayMissing) {
		test.Fatalf("Set err = %v, want ErrGatewayMissing", err)
	}
}

func TestHTTPErrorIsRuntimeFailure(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"error":{"message":"bad credential"}}`))
	}))
	defer server.Close()

	broker := newTestBroker(server.URL, okProber())
	err := broker.Set("OPENAI_API_KEY", []byte("x"))
	var platformErr *output.Error
	if !errors.As(err, &platformErr) || platformErr.Code != output.ExitRuntimeFailure {
		test.Fatalf("err = %v, want ExitRuntimeFailure", err)
	}
}
