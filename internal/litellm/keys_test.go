package litellm

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
)

// asError is errors.As specialized for the test assertions below.
func asError(err error, target **output.Error) bool { return errors.As(err, target) }

const testMasterKey = "sk-test-master"

// fakeProber answers ContainerRuntimeName (LookPath docker) and the master-key
// inspect, so the KeyManager can read LITELLM_MASTER_KEY without a real host.
type fakeProber struct {
	inspectOut string
	inspectErr error
	noRuntime  bool
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
	if prober.inspectErr != nil {
		return nil, prober.inspectErr
	}
	return []byte(prober.inspectOut), nil
}

func (prober fakeProber) Exists(string) bool { return true }

func okProber() fakeProber {
	return fakeProber{inspectOut: "PATH=/usr/bin\nLITELLM_MASTER_KEY=" + testMasterKey + "\nHOME=/root\n"}
}

func TestGenerateKeyRequestShape(test *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotMethod = request.Method
		gotPath = request.URL.Path
		gotAuth = request.Header.Get("Authorization")
		payload, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(payload, &gotBody)
		_, _ = writer.Write([]byte(`{"key":"sk-virtual-abc"}`))
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	key, err := manager.GenerateKey(KeyScope{
		Models:    []string{"gemma4"},
		MaxBudget: 5,
		RPMLimit:  60,
		TPMLimit:  1000,
		Alias:     "ws-demo",
		Duration:  "24h",
		Metadata:  map[string]any{"project": "demo"},
	})
	if err != nil {
		test.Fatalf("GenerateKey: %v", err)
	}
	if key != "sk-virtual-abc" {
		test.Fatalf("key = %q, want sk-virtual-abc", key)
	}
	if gotMethod != http.MethodPost || gotPath != "/key/generate" {
		test.Fatalf("request = %s %s, want POST /key/generate", gotMethod, gotPath)
	}
	if gotAuth != "Bearer "+testMasterKey {
		test.Fatalf("auth = %q, want Bearer %s", gotAuth, testMasterKey)
	}
	if budget, _ := gotBody["max_budget"].(float64); budget != 5 {
		test.Fatalf("max_budget = %v, want 5", gotBody["max_budget"])
	}
	if alias, _ := gotBody["key_alias"].(string); alias != "ws-demo" {
		test.Fatalf("key_alias = %v, want ws-demo", gotBody["key_alias"])
	}
	if duration, _ := gotBody["duration"].(string); duration != "24h" {
		test.Fatalf("duration = %v, want 24h", gotBody["duration"])
	}
	models, _ := gotBody["models"].([]any)
	if len(models) != 1 || models[0] != "gemma4" {
		test.Fatalf("models = %v, want [gemma4]", gotBody["models"])
	}
}

func TestGenerateKeyOmitsZeroFields(test *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		payload, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(payload, &gotBody)
		_, _ = writer.Write([]byte(`{"key":"sk-x"}`))
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	if _, err := manager.GenerateKey(KeyScope{}); err != nil {
		test.Fatalf("GenerateKey: %v", err)
	}
	for _, field := range []string{"models", "max_budget", "rpm_limit", "tpm_limit", "key_alias", "duration", "metadata"} {
		if _, present := gotBody[field]; present {
			test.Fatalf("zero-value field %q should be omitted, body=%v", field, gotBody)
		}
	}
}

func TestDeleteKeyByValueAndAlias(test *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/key/delete" || request.Method != http.MethodPost {
			test.Errorf("unexpected %s %s", request.Method, request.URL.Path)
		}
		payload, _ := io.ReadAll(request.Body)
		var body map[string]any
		_ = json.Unmarshal(payload, &body)
		bodies = append(bodies, body)
		_, _ = writer.Write([]byte(`{"deleted_keys":["sk-x"]}`))
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	if err := manager.DeleteKey("sk-x"); err != nil {
		test.Fatalf("DeleteKey: %v", err)
	}
	if err := manager.DeleteKeyByAlias("ws-demo"); err != nil {
		test.Fatalf("DeleteKeyByAlias: %v", err)
	}
	if _, present := bodies[0]["keys"]; !present {
		test.Fatalf("DeleteKey body should carry keys, got %v", bodies[0])
	}
	if _, present := bodies[1]["key_aliases"]; !present {
		test.Fatalf("DeleteKeyByAlias body should carry key_aliases, got %v", bodies[1])
	}
}

func TestKeyInfoParses(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/key/info" || request.URL.Query().Get("key") != "sk-x" {
			test.Errorf("unexpected request %s?%s", request.URL.Path, request.URL.RawQuery)
		}
		_, _ = writer.Write([]byte(`{"info":{"key_alias":"ws-demo","models":["gemma4"],"max_budget":5,"spend":0.1}}`))
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	details, err := manager.KeyInfo("sk-x")
	if err != nil {
		test.Fatalf("KeyInfo: %v", err)
	}
	if details.KeyAlias != "ws-demo" || details.MaxBudget != 5 || len(details.Models) != 1 {
		test.Fatalf("unexpected details: %+v", details)
	}
}

func TestGenerateKeyGatewayMissing(test *testing.T) {
	test.Setenv("LITELLM_BASE_URL", "http://127.0.0.1:4000")
	manager := NewKeyManager(fakeProber{noRuntime: true})
	_, err := manager.GenerateKey(KeyScope{})
	var platformErr *output.Error
	if !asError(err, &platformErr) || platformErr.Code != output.ExitMissingDep {
		test.Fatalf("err = %v, want ExitMissingDep", err)
	}
}

func TestGenerateKeyHTTPError(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"error":{"message":"invalid master key"}}`))
	}))
	defer server.Close()
	test.Setenv("LITELLM_BASE_URL", server.URL)

	manager := NewKeyManager(okProber())
	_, err := manager.GenerateKey(KeyScope{})
	var platformErr *output.Error
	if !asError(err, &platformErr) || platformErr.Code != output.ExitRuntimeFailure {
		test.Fatalf("err = %v, want ExitRuntimeFailure", err)
	}
}
