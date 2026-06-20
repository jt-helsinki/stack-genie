package acceptance

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// mockProvider is a local HTTPS, OpenAI-compatible endpoint used by the [S1]
// credential/egress tests (acceptance-tests §1.6). It records the Authorization
// header of every request so a test can assert that ClawPatrol injected the real
// credential on the wire (and that the workspace never held it).
type mockProvider struct {
	server *httptest.Server
	mu     sync.Mutex
	auths  []string
}

func startMockProvider(test *testing.T) *mockProvider {
	test.Helper()
	provider := &mockProvider{}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/chat/completions", func(writer http.ResponseWriter, request *http.Request) {
		provider.mu.Lock()
		provider.auths = append(provider.auths, request.Header.Get("Authorization"))
		provider.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"mock","object":"chat.completion",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	provider.server = httptest.NewTLSServer(mux)
	test.Cleanup(provider.server.Close)
	return provider
}

// url is the https:// base URL of the mock provider.
func (provider *mockProvider) url() string { return provider.server.URL }

// sawCredential reports whether any recorded request carried the sentinel
// (exact substring of the Authorization header), per §1.6 secret assertions.
func (provider *mockProvider) sawCredential(sentinel string) bool {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	for _, auth := range provider.auths {
		if strings.Contains(auth, sentinel) {
			return true
		}
	}
	return false
}

// writeCertPEM writes the mock server's TLS certificate so the harness can tell
// ClawPatrol to trust it (the gateway re-originates TLS to the provider, §17).
func (provider *mockProvider) writeCertPEM(test *testing.T, path string) {
	test.Helper()
	certificate := provider.server.Certificate()
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		test.Fatal(err)
	}
}

// TestMockProviderRecordsCredential exercises the fixture itself (no hardware):
// it serves HTTPS and records the Authorization header for the secret assertions.
func TestMockProviderRecordsCredential(test *testing.T) {
	provider := startMockProvider(test)
	client := provider.server.Client()

	health, err := client.Get(provider.url() + "/health")
	if err != nil || health.StatusCode != http.StatusOK {
		test.Fatalf("health: %v status=%v", err, health)
	}
	_ = health.Body.Close()

	request, _ := http.NewRequest(http.MethodPost, provider.url()+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5"}`))
	request.Header.Set("Authorization", "Bearer sk-test-sentinel")
	response, err := client.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		test.Fatalf("chat: %v status=%v", err, response)
	}
	_ = response.Body.Close()

	if !provider.sawCredential("sk-test-sentinel") {
		test.Fatal("mock provider did not record the credential it received")
	}
	if provider.sawCredential("not-sent") {
		test.Fatal("mock provider reported a credential it never received")
	}
}
