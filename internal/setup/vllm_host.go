package setup

// vllm_host.go wires the HOST-NATIVE vLLM backend into the services status /
// health / reconcile layer, mirroring how host-native Ollama is handled: vLLM is
// NOT an aip-* container, so its health comes from an HTTP probe (not
// `docker inspect`) and it is never image-pulled.
//
// CROSS-INVOCATION REALITY: `ai` is a short-lived CLI — there is no daemon holding
// a vllm.Manager between runs. So "which vLLM servers are up" is discovered from the
// persisted config store (config/model-runtimes.yaml, the runtime=vllm choices, each
// carrying an Endpoint), by HTTP-probing each recorded endpoint — NOT from Manager
// memory. vLLM servers are launched DETACHED so they outlive the CLI.
//
// vLLM serves ONE model per process (each an OpenAI endpoint on a host loopback
// port), so there can be 0..N of them; the platform surfaces a SINGLE `vllm`
// summary line (Mode "host") — running when ≥1 recorded endpoint answers, else
// stopped — matching serviceHealthy("vllm") and keeping the status shape flat.
//
// hardware bring-up: actually LAUNCHING a vLLM server is host mutation deferred
// until validated on a provisioned host — vllm.RealRunner is an ErrNotWired stub.
// Reconcile ATTEMPTS the launch best-effort and logs the install guidance on
// failure; it NEVER fails setup (vLLM is optional).

import (
	"net/http"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/vllm"
)

// vllmProbeTimeout bounds the per-endpoint reachability probe so a dead server
// can't stall a status read.
const vllmProbeTimeout = 3 * time.Second

// vllmHTTPGet is the injectable seam for the vLLM endpoint reachability probe
// (the analogue of hostOllamaHTTPGet), so Status/serviceHealthy are unit-testable
// without a live server. The real probe is a short-timeout HTTP GET.
var vllmHTTPGet = func(url string) (*http.Response, error) {
	client := &http.Client{Timeout: vllmProbeTimeout}
	return client.Get(url)
}

// vllmRuntimeChoices returns the recorded runtime=vllm model-runtime choices from
// the machine-wide store (config/model-runtimes.yaml). An absent/unreadable store
// yields none (never an error — vLLM is optional).
func vllmRuntimeChoices() []config.ModelRuntimeChoice {
	choices, err := config.LoadModelRuntimes()
	if err != nil {
		return nil
	}
	var vllmChoices []config.ModelRuntimeChoice
	for _, choice := range choices {
		if choice.Runtime == config.RuntimeVLLM {
			vllmChoices = append(vllmChoices, choice)
		}
	}
	return vllmChoices
}

// vllmEndpointHealthy probes GET <endpoint>/models and reports a 200 OK. The
// recorded Endpoint is an OpenAI base URL (e.g. http://127.0.0.1:8101/v1); the
// probe hits its /models sub-path (the same signal vllm.RealHealthProbe uses).
// An empty endpoint or any transport/non-200 response is "not healthy".
func vllmEndpointHealthy(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	response, err := vllmHTTPGet(strings.TrimRight(endpoint, "/") + "/models")
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode == http.StatusOK
}

// vllmServersHealthy reports whether AT LEAST ONE recorded vLLM endpoint answers.
// It backs serviceHealthy("vllm"): the summary `vllm` line is "running" when any
// server is reachable, else "stopped".
func vllmServersHealthy() bool {
	for _, choice := range vllmRuntimeChoices() {
		if vllmEndpointHealthy(choice.Endpoint) {
			return true
		}
	}
	return false
}

// vllmStatus builds the single host-native `vllm` summary line. Mode is "host"
// (non-container, health from the HTTP probe, never `docker inspect`). With no
// runtime=vllm models recorded there is nothing to run, so it reports "stopped"
// with no error and no hint (vLLM is discoverable but idle). With models recorded
// but no server reachable it reports "stopped" plus an actionable install/enable
// hint (from vllm.InstallGuidance), which `ai doctor` renders from Detail.
func (services realServices) vllmStatus() ServiceStatus {
	// Optional so `ai doctor` treats a down vLLM as a WARNING, not an error (vLLM is
	// opt-in — only models set to the vllm runtime need it). The flag is a
	// display/severity marker only; it does NOT make vLLM an enable/disable service
	// (that set is isOptionalService, driven by the services registry).
	status := ServiceStatus{Name: "vllm", Mode: "host", State: "stopped", Optional: true}
	choices := vllmRuntimeChoices()
	if len(choices) == 0 {
		return status // no vLLM models recorded — idle, not an error
	}
	if services.serviceHealthy("vllm") {
		status.State = "running"
		status.Healthy = true
		return status
	}
	// Models recorded but no server up: surface the recovery path. vLLM is
	// OPTIONAL, so doctor renders this as a warning (down), not an error.
	status.Detail = vllmDownDetail(goruntime.GOOS)
	return status
}

// vllmDownDetail is the actionable one-line hint shown when vLLM models are
// recorded but no server is reachable. It folds in the first per-OS install step
// from vllm.InstallGuidance so `ai doctor` points the user at the fix.
func vllmDownDetail(goos string) string {
	detail := "no vLLM server reachable — install & start host-native vLLM"
	if guidance := vllm.InstallGuidance(goos); len(guidance) > 0 {
		detail += " (" + guidance[0] + ")"
	}
	return detail + "; optional (only for models set to the vllm runtime)"
}

// vllmServeModel is the underlying model name a recorded choice should serve: the
// stored Model when set, else the alias itself.
func vllmServeModel(choice config.ModelRuntimeChoice) string {
	if choice.Model != "" {
		return choice.Model
	}
	return choice.Alias
}

// ensureVLLMServers best-effort brings up a vLLM server for every recorded
// runtime=vllm model whose endpoint is not already answering. It is host-native
// and OPTIONAL: it NEVER returns an error and NEVER fails the reconcile — a launch
// failure (today vllm.RealRunner is an ErrNotWired `hardware bring-up` stub) is
// logged with the install guidance and skipped. Servers are launched DETACHED (by
// the Runner) so they outlive this short-lived CLI.
func (services realServices) ensureVLLMServers(progress func(string)) {
	choices := vllmRuntimeChoices()
	if len(choices) == 0 {
		return // nothing to serve
	}
	manager := vllm.NewManager(vllm.Config{
		Runner: vllm.RealRunner{},
		Probe:  vllm.RealHealthProbe(),
		Log:    func(line string) { progress("      " + line) },
	})
	for _, choice := range choices {
		// Already up (a detached launch from a previous CLI run)? Leave it alone.
		if vllmEndpointHealthy(choice.Endpoint) {
			continue
		}
		progress("  • vLLM server for " + choice.Alias + " (host-native, best-effort)…")
		if _, err := manager.EnsureServed(choice.Alias, vllmServeModel(choice)); err != nil {
			// Best-effort: vLLM is optional and RealRunner is a bring-up stub. Surface
			// the reason + install guidance and continue — NEVER fail the reconcile.
			progress("      vLLM not started for " + choice.Alias + ": " + err.Error())
			for _, line := range vllm.InstallGuidance(goruntime.GOOS) {
				progress("      - " + line)
			}
		}
	}
}
