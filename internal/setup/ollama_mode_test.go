package setup

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
)

// probeRan reports whether the recording prober issued a call whose joined argv
// contains substr (recordingProber + runArgsFor live in setup_test.go).
func probeRan(prober *recordingProber, substr string) bool {
	for _, call := range prober.calls {
		if strings.Contains(strings.Join(call, " "), substr) {
			return true
		}
	}
	return false
}

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

func TestOllamaProxyTargetByMode(test *testing.T) {
	if got := ollamaProxyTarget(runtime.OllamaModeContainer); got != "http://"+ollamaContainer+":11434/" {
		test.Errorf("container target = %q", got)
	}
	if got := ollamaProxyTarget(runtime.OllamaModeHost); got != "http://"+hostGatewayName+":11434/" {
		test.Errorf("host target = %q, want the host gateway", got)
	}
	// An empty/unknown mode is the safe container default.
	if got := ollamaProxyTarget(""); got != "http://"+ollamaContainer+":11434/" {
		test.Errorf("empty-mode target = %q, want container", got)
	}
}

func TestHostGatewayRunArgsByMode(test *testing.T) {
	if args := hostGatewayRunArgs(runtime.OllamaModeContainer, false); len(args) != 0 {
		test.Errorf("container mode must add no host-gateway args, got %v", args)
	}
	args := hostGatewayRunArgs(runtime.OllamaModeHost, false)
	if len(args) != 1 || args[0] != hostGatewayAddArg {
		test.Errorf("host mode args = %v, want [%s]", args, hostGatewayAddArg)
	}
	// DMR enabled adds the host-gateway arg even in container Ollama mode…
	dmr := hostGatewayRunArgs(runtime.OllamaModeContainer, true)
	if len(dmr) != 1 || dmr[0] != hostGatewayAddArg {
		test.Errorf("DMR-enabled container mode args = %v, want [%s]", dmr, hostGatewayAddArg)
	}
	// …and host Ollama + DMR together still add it exactly ONCE (never double).
	both := hostGatewayRunArgs(runtime.OllamaModeHost, true)
	if len(both) != 1 || both[0] != hostGatewayAddArg {
		test.Errorf("host+DMR args = %v, want a single %s (no double-add)", both, hostGatewayAddArg)
	}
}

func TestLitellmRunArgsHostGateway(test *testing.T) {
	container := strings.Join(litellmRunArgs("/cfg.yaml", "127.0.0.1", "img", runtime.OllamaModeContainer, false), " ")
	if strings.Contains(container, hostGatewayAddArg) {
		test.Errorf("container mode must NOT add %s: %s", hostGatewayAddArg, container)
	}
	host := strings.Join(litellmRunArgs("/cfg.yaml", "127.0.0.1", "img", runtime.OllamaModeHost, false), " ")
	if !strings.Contains(host, hostGatewayAddArg) {
		test.Errorf("host mode must add %s: %s", hostGatewayAddArg, host)
	}
	// DMR enabled adds the host-gateway wiring even in container Ollama mode.
	dmr := strings.Join(litellmRunArgs("/cfg.yaml", "127.0.0.1", "img", runtime.OllamaModeContainer, true), " ")
	if !strings.Contains(dmr, hostGatewayAddArg) {
		test.Errorf("DMR-enabled container mode must add %s: %s", hostGatewayAddArg, dmr)
	}
}

func TestProxyNginxConfOllamaRouteByMode(test *testing.T) {
	container := proxyNginxConf("aip.local", runtime.OllamaModeContainer)
	if !strings.Contains(container, "proxy_pass http://"+ollamaContainer+":11434/;") {
		test.Errorf("container /ollama route missing the container upstream:\n%s", container)
	}
	if strings.Contains(container, hostGatewayName) {
		test.Errorf("container mode must not reference the host gateway:\n%s", container)
	}
	host := proxyNginxConf("aip.local", runtime.OllamaModeHost)
	if !strings.Contains(host, "proxy_pass http://"+hostGatewayName+":11434/;") {
		test.Errorf("host /ollama route missing the host-gateway upstream:\n%s", host)
	}
}

func TestOllamaEnvPairsContextLength(test *testing.T) {
	pairs := ollamaEnvPairs()
	joined := strings.Join(pairs, " ")
	if !strings.Contains(joined, "OLLAMA_CONTEXT_LENGTH="+defaultOllamaContextLength) {
		test.Errorf("container env pairs missing default context length: %v", pairs)
	}
	if !strings.Contains(joined, "OLLAMA_MODELS="+ollamaModelsGuest) {
		test.Errorf("container env pairs must point OLLAMA_MODELS at the in-container store: %v", pairs)
	}
}

func TestHostOllamaEnvPairs(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	pairs := hostOllamaEnvPairs()
	joined := strings.Join(pairs, " ")
	if !strings.Contains(joined, "OLLAMA_CONTEXT_LENGTH="+defaultOllamaContextLength) {
		test.Errorf("host env pairs missing default context length: %v", pairs)
	}
	// Host mode points OLLAMA_MODELS at the dedicated host subdir (separate store).
	wantModels := "OLLAMA_MODELS=" + hostOllamaModelsDir()
	if !strings.Contains(joined, wantModels) || !strings.HasSuffix(hostOllamaModelsDir(), "/models/"+ollamaHostModelsSubdir) {
		test.Errorf("host env pairs OLLAMA_MODELS = %v, want %s under the host models subdir", pairs, wantModels)
	}
}

func TestEnsureHostOllamaReachability(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	fakeOllamaHTTP(test, http.StatusOK)
	if err := ensureHostOllama(); err != nil {
		test.Errorf("reachable host Ollama should pass, got %v", err)
	}

	fakeOllamaHTTP(test, 0) // transport error
	err := ensureHostOllama()
	if err == nil {
		test.Fatal("unreachable host Ollama must return an actionable error")
	}
	if !strings.Contains(err.Error(), "11434") {
		test.Errorf("error should be actionable (mention the port/install steps): %v", err)
	}
}

func TestEnsureOllamaHostModeSkipsContainer(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fakeOllamaHTTP(test, http.StatusOK)
	prober := &recordingProber{}
	if err := ensureOllama(prober, "docker", "127.0.0.1", runtime.OllamaModeHost); err != nil {
		test.Fatalf("host-mode ensureOllama with a reachable host should pass: %v", err)
	}
	// It must NOT run the aip-ollama container in host mode.
	if probeRan(prober, "run -d --name "+ollamaContainer) {
		test.Errorf("host mode must not start the %s container: %v", ollamaContainer, prober.calls)
	}
}

func TestRequiredImagesHostModeSkipsOllama(test *testing.T) {
	container := requiredImages(nil, nil, runtime.OllamaModeContainer)
	if !containsSubstr(container, "ollama") {
		test.Errorf("container mode should pull an ollama image: %v", container)
	}
	host := requiredImages(nil, nil, runtime.OllamaModeHost)
	if containsSubstr(host, "ollama") {
		test.Errorf("host mode must not pull any ollama image: %v", host)
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

func TestServiceDisplayMode(test *testing.T) {
	ollamaSpec := serviceSpec{Name: "ollama", Mode: "container"}
	litellmSpec := serviceSpec{Name: "litellm", Mode: "container"}
	if got := serviceDisplayMode(ollamaSpec, runtime.OllamaModeContainer); got != "container" {
		test.Errorf("ollama container display mode = %q", got)
	}
	if got := serviceDisplayMode(ollamaSpec, runtime.OllamaModeHost); got != runtime.OllamaModeHost {
		test.Errorf("ollama host display mode = %q, want host", got)
	}
	// Only Ollama changes mode; other services stay container even in host Ollama mode.
	if got := serviceDisplayMode(litellmSpec, runtime.OllamaModeHost); got != "container" {
		test.Errorf("litellm display mode = %q, want container", got)
	}
}

func TestApplyOllamaModeSetsAPIBase(test *testing.T) {
	original := litellm.OllamaAPIBase
	test.Cleanup(func() { litellm.OllamaAPIBase = original })

	applyOllamaMode(runtime.OllamaModeHost)
	if litellm.OllamaAPIBase != litellm.OllamaHostAPIBase {
		test.Errorf("host mode api_base = %q, want %q", litellm.OllamaAPIBase, litellm.OllamaHostAPIBase)
	}
	applyOllamaMode(runtime.OllamaModeContainer)
	if litellm.OllamaAPIBase != litellm.OllamaContainerAPIBase {
		test.Errorf("container mode api_base = %q, want %q", litellm.OllamaAPIBase, litellm.OllamaContainerAPIBase)
	}
}
