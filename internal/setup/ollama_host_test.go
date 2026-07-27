package setup

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeOllamaHTTP swaps hostOllamaHTTPGet for one that returns the given status (or a
// transport error when status == 0), restoring the original on cleanup.
func fakeOllamaHTTP(test *testing.T, status int) {
	test.Helper()
	original := hostOllamaHTTPGet
	test.Cleanup(func() { hostOllamaHTTPGet = original })
	hostOllamaHTTPGet = func(string) (*http.Response, error) {
		if status == 0 {
			return nil, io.ErrUnexpectedEOF
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}
}

// Ollama is host-native, so the LiteLLM container ALWAYS gets the host-gateway
// mapping (host.docker.internal) — it reaches Ollama on the host.
func TestLitellmRunArgsAlwaysHostGateway(test *testing.T) {
	joined := strings.Join(litellmRunArgs("/cfg.yaml", "127.0.0.1", "img"), " ")
	if !strings.Contains(joined, hostGatewayAddArg) {
		test.Errorf("litellm run args must add %s: %s", hostGatewayAddArg, joined)
	}
}

// nginx always proxies /ollama to the host-native Ollama through the host gateway.
func TestProxyNginxConfOllamaRouteHost(test *testing.T) {
	conf := proxyNginxConf("aip.local")
	if !strings.Contains(conf, "proxy_pass http://"+hostGatewayName+":11434/;") {
		test.Errorf("/ollama route missing the host-gateway upstream:\n%s", conf)
	}
}

func TestHostOllamaEnvPairs(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	pairs := hostOllamaEnvPairs()
	joined := strings.Join(pairs, " ")
	if !strings.Contains(joined, "OLLAMA_CONTEXT_LENGTH="+defaultOllamaContextLength) {
		test.Errorf("host env pairs missing default context length: %v", pairs)
	}
	// The host process points OLLAMA_MODELS at the dedicated host store subdir.
	wantModels := "OLLAMA_MODELS=" + hostOllamaModelsDir()
	if !strings.Contains(joined, wantModels) || !strings.HasSuffix(hostOllamaModelsDir(), "/models/"+ollamaHostModelsSubdir) {
		test.Errorf("host env pairs OLLAMA_MODELS = %v, want %s under the host models subdir", pairs, wantModels)
	}
}

func TestEnsureOllamaReachability(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	fakeOllamaHTTP(test, http.StatusOK)
	if err := ensureOllama(); err != nil {
		test.Errorf("reachable host Ollama should pass, got %v", err)
	}

	fakeOllamaHTTP(test, 0) // transport error
	err := ensureOllama()
	if err == nil {
		test.Fatal("unreachable host Ollama must return an actionable error")
	}
	if !strings.Contains(err.Error(), "11434") {
		test.Errorf("error should be actionable (mention the port/install steps): %v", err)
	}
}

// Ollama is host-native, so its image is never pulled.
func TestRequiredImagesSkipsOllama(test *testing.T) {
	refs := requiredImages(nil, nil)
	if containsSubstr(refs, "ollama") {
		test.Errorf("host-native Ollama must not pull any ollama image: %v", refs)
	}
}

func containsSubstr(refs []string, substr string) bool {
	for _, ref := range refs {
		if strings.Contains(ref, substr) {
			return true
		}
	}
	return false
}
