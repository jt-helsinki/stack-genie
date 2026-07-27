package vllm

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrNotWired is returned by the `hardware bring-up` seams (RealRunner, Pull) that
// must mutate the host. The platform does not run/install vLLM until validated on a
// provisioned host; until then these surface an actionable error rather than acting.
var ErrNotWired = errors.New("vllm: not yet wired (hardware bring-up)")

// RealRunner is the production Runner: it shells out to `vllm serve`. It is a
// `hardware bring-up` seam — NOT wired, because starting a real process is host
// mutation the platform defers until validated on a provisioned host. The Manager
// takes a Runner interface, so tests inject FakeRunner instead.
//
// When wired, Start launches a DETACHED process (setsid, as the invoking user) with
// argv:
//
//	vllm serve <model> \
//	    --host 127.0.0.1 \
//	    --port <port> \
//	    --served-model-name <alias>
//
// and environment HF_HOME=<storeDir> so weights download into / load from the
// platform store (StoreDir), plus `--download-dir <storeDir>` as a belt-and-braces
// hint. On darwin the vLLM-Metal plugin serves MLX weights (mlx-community/*); on
// Linux it is CUDA + HF safetensors. Start returns a ServerHandle wrapping the PID;
// Stop signals that PID (SIGTERM, then SIGKILL after a grace period).
type RealRunner struct{}

// Start is the `hardware bring-up` seam — see RealRunner. Not wired.
func (RealRunner) Start(alias, model string, port int, storeDir string) (ServerHandle, error) {
	return ServerHandle{}, fmt.Errorf("start %q (%s) on port %d with store %s: %w",
		alias, model, port, storeDir, ErrNotWired)
}

// Stop is the `hardware bring-up` seam — see RealRunner. Not wired.
func (RealRunner) Stop(handle ServerHandle) error {
	return fmt.Errorf("stop pid %d: %w", handle.PID, ErrNotWired)
}

// RealHealthProbe returns a HealthProbe that GETs http://127.0.0.1:<port>/v1/models
// with a short timeout and reports 200 as healthy. This is a read-only network
// probe (no host mutation), the analogue of the host-Ollama reachability check; it
// is safe to wire, but only exercises a real server once one is actually started
// (which is the `hardware bring-up` part).
func RealHealthProbe() HealthProbe {
	client := &http.Client{Timeout: 3 * time.Second}
	return func(port int) bool {
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", port))
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode == http.StatusOK
	}
}

// Pull downloads a model's weights into the platform store (StoreDir) via the
// Hugging Face hub: MLX weights (mlx-community/*) on darwin, safetensors on Linux.
//
// hardware bring-up: NOT wired — this must mutate the host (a multi-GB download).
// When wired it runs the HF download into StoreDir with HF_HOME set (e.g.
// `huggingface-cli download <model> --local-dir <storeDir>/<model>`, or lets the
// first `vllm serve` fetch it lazily). Tests use a fake (see PullFunc / FakeRunner).
func Pull(model string) error {
	return fmt.Errorf("pull %q: %w", model, ErrNotWired)
}
