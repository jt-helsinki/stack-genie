package setup

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/vllm"
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
// error, and carries a self-explaining IDLE hint (vLLM is per-model + lazy-started, so
// a bare "stopped" must not read as broken) pointing at `ai models pull --runtime vllm`.
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
	if !strings.Contains(status.Detail, "--runtime vllm") {
		test.Errorf("no models: want an idle hint pointing at `--runtime vllm`, got %q", status.Detail)
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

// TestVLLMStatusModels proves the per-model resolved config (including the pinned
// GPU-memory-utilization/max-model-len caps) is surfaced on ServiceStatus.Models —
// the fix for "the vllm config should be displayed in the service pane": the
// service-detail TUI pane renders this per model, and it had nothing to render
// before Models existed on ServiceStatus.
func TestVLLMStatusModels(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	if err := config.SetModelRuntime(config.ModelRuntimeChoice{
		Alias: "my-qwen", Model: "mlx-community/Qwen3-8B", Runtime: config.RuntimeVLLM,
		Endpoint: "http://127.0.0.1:8101/v1", GPUMemoryUtilization: 0.5, MaxModelLen: 8192,
	}); err != nil {
		test.Fatalf("seed store: %v", err)
	}
	fakeVLLMHTTP(test, map[string]int{"8101": http.StatusOK})

	status := realServices{}.vllmStatus()
	if len(status.Models) != 1 {
		test.Fatalf("Models = %+v, want exactly 1 entry", status.Models)
	}
	model := status.Models[0]
	if model.Alias != "my-qwen" || model.Model != "mlx-community/Qwen3-8B" || !model.Healthy {
		test.Errorf("model = %+v, want alias/model/healthy populated", model)
	}
	if model.GPUMemoryUtilization != 0.5 || model.MaxModelLen != 8192 {
		test.Errorf("model resource caps = %+v, want {GPUMemoryUtilization:0.5 MaxModelLen:8192}", model)
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

// fakeVLLMDetect overrides the vLLM install probe so ensureVLLMServers never forks a real
// `vllm serve` in a unit test regardless of the host, restoring the original on cleanup.
func fakeVLLMDetect(test *testing.T, installed bool) {
	test.Helper()
	original := vllmDetect
	test.Cleanup(func() { vllmDetect = original })
	vllmDetect = func() (bool, string) { return installed, "mlx" }
}

// ensureVLLMServers is the ONLY vLLM-touching step of Reconcile, and it has no
// error return — so vLLM can never fail setup. This asserts it: a no-model host is
// a silent no-op, and a recorded-but-unreachable model on a host WITHOUT vLLM logs the
// install guidance (never attempting an exec) without panicking or signalling failure.
func TestEnsureVLLMServersNeverFails(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	fakeVLLMDetect(test, false) // no vLLM on PATH → never spawn, just log guidance

	var lines []string
	collect := func(line string) { lines = append(lines, line) }

	// No models recorded: a silent no-op (no progress lines).
	ensureVLLMServers(collect)
	if len(lines) != 0 {
		test.Errorf("no vllm models: want no-op, got progress %v", lines)
	}

	// Recorded but unreachable, vLLM not installed: logs the reason + install guidance and
	// moves on — never failing, never forking.
	recordVLLMChoice(test, "my-vllm", "mlx-community/foo", "http://127.0.0.1:8101/v1")
	fakeVLLMHTTP(test, map[string]int{"8101": 0}) // endpoint down → attempt handling
	ensureVLLMServers(collect)

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "my-vllm") {
		test.Errorf("want an attempt logged for my-vllm, got %v", lines)
	}
	if !strings.Contains(strings.ToLower(joined), "vllm") {
		test.Errorf("want guidance logged for the recorded model, got %v", lines)
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

// writeVLLMLogFile writes content to <StoreDir>/<alias>.log — the file
// vllm.RealRunner.Start redirects a model's stdout/stderr to.
func writeVLLMLogFile(test *testing.T, alias, content string) {
	test.Helper()
	storeDir, err := vllm.StoreDir()
	if err != nil {
		test.Fatalf("StoreDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, alias+".log"), []byte(content), 0o644); err != nil {
		test.Fatalf("write log file: %v", err)
	}
}

// TestServiceLogTailVLLMNoModels: with no recorded vLLM model there is nothing to
// tail — empty string, no error (mirrors the container-service "stopped/absent"
// tolerance, not a failure).
func TestServiceLogTailVLLMNoModels(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	content, err := ServiceLogTail(Deps{}, "vllm", 0)
	if err != nil || content != "" {
		test.Fatalf("no recorded models: got (%q, %v), want (\"\", nil)", content, err)
	}
}

// TestServiceLogTailVLLMSingleModel proves a single recorded model's log file is
// tailed WITHOUT a heading — this is the fix for "no vllm logs under the service
// tab" (ServiceLogTail used to unconditionally return "" for vllm, a host-native
// service with no container to `docker logs`).
func TestServiceLogTailVLLMSingleModel(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	recordVLLMChoice(test, "my-qwen", "mlx-community/Qwen3-8B", "http://127.0.0.1:8101/v1")
	writeVLLMLogFile(test, "my-qwen", "line1\nline2\nline3\n")

	content, err := ServiceLogTail(Deps{}, "vllm", 2)
	if err != nil {
		test.Fatalf("ServiceLogTail: %v", err)
	}
	if content != "line2\nline3" {
		test.Fatalf("content = %q, want the last 2 lines with no per-model heading", content)
	}
}

// TestServiceLogTailVLLMMultipleModels proves multiple recorded models are
// concatenated under a `── <alias> ──` heading each, mirroring the multi-container
// path (e.g. presidio's analyzer + anonymizer).
func TestServiceLogTailVLLMMultipleModels(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	recordVLLMChoice(test, "my-qwen", "mlx-community/Qwen3-8B", "http://127.0.0.1:8101/v1")
	recordVLLMChoice(test, "my-llama", "mlx-community/Llama-4", "http://127.0.0.1:8102/v1")
	writeVLLMLogFile(test, "my-qwen", "qwen output\n")
	writeVLLMLogFile(test, "my-llama", "llama output\n")

	content, err := ServiceLogTail(Deps{}, "vllm", 10)
	if err != nil {
		test.Fatalf("ServiceLogTail: %v", err)
	}
	if !strings.Contains(content, "── my-qwen ──\nqwen output") || !strings.Contains(content, "── my-llama ──\nllama output") {
		test.Fatalf("content = %q, want a heading + tailed content per model", content)
	}
}

// TestServiceLogTailVLLMNeverStarted proves a recorded model with no log file yet
// (never actually started) is skipped, not an error.
func TestServiceLogTailVLLMNeverStarted(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	recordVLLMChoice(test, "my-qwen", "mlx-community/Qwen3-8B", "http://127.0.0.1:8101/v1")

	content, err := ServiceLogTail(Deps{}, "vllm", 10)
	if err != nil || content != "" {
		test.Fatalf("recorded but never started: got (%q, %v), want (\"\", nil)", content, err)
	}
}
