package setup

import (
	"net/http"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/output"
)

// Docker Model Runner (DMR) is a HOST-side inference backend (arch §14), a peer to
// the host-native Ollama backend. It is ALWAYS available as an OPTION: a served
// model whose runtime is docker-model-runner routes to it. Like host-native Ollama,
// DMR runs on the HOST (not as an aip-* container): the platform process probes it
// directly on the host loopback, while the LiteLLM/nginx CONTAINERS reach it through
// the host gateway (host.docker.internal — see hostGatewayAddArg). The actual
// install/enable is a hardware bring-up seam (bringUpDMR); the platform never mutates
// the host.

// dmrServiceName is the status/services vocabulary for the DMR backend. It matches
// config.RuntimeDockerModelRunner so the model-runtime store and the service line
// use the same identifier.
const dmrServiceName = "docker-model-runner"

// dmrModelsURL is the host-native DMR liveness endpoint the platform probes from
// its OWN (on-host) perspective. The platform process runs on the host, so it
// reaches DMR at the loopback default port (12434) under the OpenAI-compatible
// /engines/v1 path — NOT through the host.docker.internal gateway (that name is for
// the LiteLLM/nginx CONTAINERS). This mirrors hostOllamaVersionURL.
const dmrModelsURL = "http://127.0.0.1:12434/engines/v1/models"

// dmrDisplayAddress is the host-reachable DMR OpenAI endpoint shown on the DMR
// status line. DMR listens on the host at :12434, so the platform (and any host
// tool) reaches it here directly; it is an HTTP API with no admin console.
const dmrDisplayAddress = "http://localhost:12434/engines/v1"

// dmrHTTPGet is the indirection the DMR reachability probe uses, so unit tests can
// substitute a fake without a live server (mirrors hostOllamaHTTPGet / proxyHTTPGet).
// It defaults to a short-timeout client GET.
var dmrHTTPGet = func(url string) (*http.Response, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	return client.Get(url)
}

// dmrReachable reports whether Docker Model Runner answers GET /engines/v1/models
// with 200 on the host loopback. It backs both ensureDMR's precondition and the DMR
// status line in statusFor. Mirrors hostOllamaReachable.
func dmrReachable() bool {
	response, err := dmrHTTPGet(dmrModelsURL)
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode == http.StatusOK
}

// ensureDMR verifies Docker Model Runner is serving before the platform routes
// docker-model-runner/* traffic to it. When it is unreachable it returns an
// actionable error; DMR is always AVAILABLE as an option but may not be running, so
// callers (Reconcile) treat that error as a non-fatal hint rather than failing setup.
// The actual enable/install is a hardware bring-up seam (bringUpDMR).
func ensureDMR() error {
	if dmrReachable() {
		return nil
	}
	// hardware bring-up: on a provisioned host, attempt to bring DMR up automatically
	// (enable the Docker Model Runner plugin/feature) before failing. bringUpDMR is the
	// unwired seam today — it returns a not-yet-wired error, so this falls through to
	// the actionable manual-enable error below.
	if err := bringUpDMR(); err == nil && dmrReachable() {
		return nil
	}
	return output.Errorf(output.ExitMissingDep,
		"Docker Model Runner (DMR) not reachable at 127.0.0.1:12434 — enable it to use "+
			"docker-model-runner models (Docker Desktop: `docker desktop enable model-runner` "+
			"or Settings ▸ AI ▸ Enable Docker Model Runner; Linux: install the `docker model` "+
			"CLI plugin and runtime)")
}

// bringUpDMR enables (and, if needed, installs) Docker Model Runner on the host.
//
// hardware bring-up: this is the seam ensureDMR calls when DMR is unreachable. It is
// NOT wired — enableDMR returns a not-yet-wired error, so this returns that error and
// the caller surfaces the actionable manual-enable guidance. It documents the exact
// per-platform steps for when it is wired on a provisioned host:
//
//   - Docker Desktop (macOS/Windows): `docker desktop enable model-runner`
//     (optionally `--tcp 12434` to expose the host port), or Settings ▸ AI ▸ Enable
//     Docker Model Runner. The Desktop backend defaults to the llama.cpp engine.
//   - Linux: install the `docker model` CLI plugin + runtime (Docker Engine's Model
//     Runner), then `docker model` serves the OpenAI-compatible endpoint on :12434.
//
// Engine note: llama.cpp is the DEFAULT engine and the only one on macOS (Metal
// acceleration). The vLLM engine is Linux + NVIDIA-only, so it is unavailable on the
// Apple Silicon standalone host — DMR on macOS is always llama.cpp/Metal.
func bringUpDMR() error {
	return enableDMR()
}

// enableDMR enables Docker Model Runner on the host.
//
// hardware bring-up: this is NOT wired — it must mutate the host (enable a Docker
// feature / install a CLI plugin), which the platform does not do until validated on
// a provisioned host. When wired, the per-platform commands are documented on
// bringUpDMR.
func enableDMR() error {
	return output.Errorf(output.ExitRuntimeFailure,
		"Docker Model Runner enable is not yet wired (hardware bring-up) — enable it manually")
}
