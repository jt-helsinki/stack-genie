package setup

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
)

// fakeVLLMHTTP swaps vllmHTTPGet for one that returns the given status per URL (or a
// transport error when the status is 0), restoring the original on cleanup. byURL
// keys on a substring of the requested URL so a test can make one endpoint healthy
// and another dead; a URL matching no key is treated as a transport error (down).
func fakeVLLMHTTP(test *testing.T, byURL map[string]int) {
	test.Helper()
	original := vllmHTTPGet
	test.Cleanup(func() { vllmHTTPGet = original })
	vllmHTTPGet = func(url string) (*http.Response, error) {
		for fragment, status := range byURL {
			if strings.Contains(url, fragment) {
				if status == 0 {
					return nil, io.ErrUnexpectedEOF
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			}
		}
		return nil, io.ErrUnexpectedEOF
	}
}

// recordVLLMChoice writes a runtime=vllm selection to the model-runtimes store under
// the test HOME.
func recordVLLMChoice(test *testing.T, alias, model, endpoint string) {
	test.Helper()
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias:    alias,
		Model:    model,
		Runtime:  config.RuntimeVLLM,
		Endpoint: endpoint,
	}); err != nil {
		test.Fatalf("record vllm choice %q: %v", alias, err)
	}
}

// With no recorded vLLM models the summary line is host-mode and stopped, never an
// error and with no install hint (idle, discoverable).
func TestVLLMStatusNoModels(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	status := realServices{}.vllmStatus()
	if status.Name != "vllm" || status.Mode != "host" {
		test.Fatalf("want host-mode vllm line, got %+v", status)
	}
	if !status.Optional {
		test.Error("vllm must be Optional so doctor warns (not errors) when it is down")
	}
	if status.State != "stopped" || status.Healthy {
		test.Errorf("no models: want stopped/unhealthy, got %+v", status)
	}
	if status.Detail != "" {
		test.Errorf("no models: want no install hint, got %q", status.Detail)
	}
}

// A recorded vLLM model whose endpoint answers → running/healthy.
func TestVLLMStatusRunning(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	recordVLLMChoice(test, "my-vllm", "mlx-community/foo", "http://127.0.0.1:8101/v1")
	fakeVLLMHTTP(test, map[string]int{"8101": http.StatusOK})

	status := realServices{}.vllmStatus()
	if status.State != "running" || !status.Healthy {
		test.Errorf("healthy endpoint: want running/healthy, got %+v", status)
	}
}

// A recorded vLLM model whose endpoint does NOT answer → stopped, with an
// actionable install hint (from vllm.InstallGuidance) for doctor to render.
func TestVLLMStatusStoppedWithHint(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	recordVLLMChoice(test, "my-vllm", "mlx-community/foo", "http://127.0.0.1:8101/v1")
	fakeVLLMHTTP(test, map[string]int{"8101": 0}) // transport error → down

	status := realServices{}.vllmStatus()
	if status.State != "stopped" || status.Healthy {
		test.Errorf("down endpoint: want stopped/unhealthy, got %+v", status)
	}
	if !strings.Contains(status.Detail, "install") {
		test.Errorf("down endpoint: want actionable install hint, got %q", status.Detail)
	}
}

// serviceHealthy("vllm") is true iff at least one recorded endpoint answers.
func TestServiceHealthyVLLM(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	services := realServices{}

	// No models → not healthy.
	if services.serviceHealthy("vllm") {
		test.Error("no vllm models: serviceHealthy(vllm) must be false")
	}

	recordVLLMChoice(test, "alpha", "m1", "http://127.0.0.1:8101/v1")
	recordVLLMChoice(test, "beta", "m2", "http://127.0.0.1:8102/v1")

	// Both down → not healthy.
	fakeVLLMHTTP(test, map[string]int{"8101": 0, "8102": 0})
	if services.serviceHealthy("vllm") {
		test.Error("all endpoints down: serviceHealthy(vllm) must be false")
	}

	// One up (beta) → healthy.
	fakeVLLMHTTP(test, map[string]int{"8101": 0, "8102": http.StatusOK})
	if !services.serviceHealthy("vllm") {
		test.Error("one endpoint up: serviceHealthy(vllm) must be true")
	}
}

// statusFor always appends the host-native vLLM summary line so it is discoverable.
func TestStatusForIncludesVLLM(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	statuses, err := realServices{prober: healthProber{running: map[string]bool{}}}.statusFor(nil)
	if err != nil {
		test.Fatalf("statusFor: %v", err)
	}
	var found *ServiceStatus
	for index := range statuses {
		if statuses[index].Name == "vllm" {
			found = &statuses[index]
		}
	}
	if found == nil {
		test.Fatalf("statusFor must include a vllm line, got %d services", len(statuses))
	}
	if found.Mode != "host" {
		test.Errorf("vllm line must be host-mode, got %+v", *found)
	}
}

// ensureVLLMServers is the ONLY vLLM-touching step of Reconcile, and it has no
// error return — so vLLM can never fail setup. This asserts it: a no-model host is
// a silent no-op, and a recorded-but-unreachable model logs guidance (RealRunner is
// an ErrNotWired bring-up stub) without panicking or signalling failure.
func TestEnsureVLLMServersNeverFails(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	services := realServices{}

	var lines []string
	collect := func(line string) { lines = append(lines, line) }

	// No models recorded: a silent no-op (no progress lines).
	services.ensureVLLMServers(collect)
	if len(lines) != 0 {
		test.Errorf("no vllm models: want no-op, got progress %v", lines)
	}

	// Recorded but unreachable: attempts a launch, RealRunner returns ErrNotWired, so
	// it logs the reason + install guidance and moves on — never failing.
	recordVLLMChoice(test, "my-vllm", "mlx-community/foo", "http://127.0.0.1:8101/v1")
	fakeVLLMHTTP(test, map[string]int{"8101": 0}) // endpoint down → attempt launch
	services.ensureVLLMServers(collect)

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "my-vllm") {
		test.Errorf("want an attempt logged for my-vllm, got %v", lines)
	}
	if !strings.Contains(joined, "not started") && !strings.Contains(strings.ToLower(joined), "vllm") {
		test.Errorf("want guidance logged on the not-wired launch, got %v", lines)
	}
}

// vllmDownDetail folds in the per-OS install guidance so doctor's Detail is
// actionable on both host families.
func TestVLLMDownDetail(test *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		detail := vllmDownDetail(goos)
		if !strings.Contains(detail, "install") || !strings.Contains(detail, "vLLM") {
			test.Errorf("goos %q: want actionable install hint, got %q", goos, detail)
		}
	}
}
