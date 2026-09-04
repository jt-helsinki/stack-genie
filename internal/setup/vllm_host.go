package setup

// vllm_host.go wires the HOST-NATIVE vLLM backend into the services status /
// health / reconcile layer: vLLM is NOT an aip-* container, so its health comes
// from an HTTP probe (not `docker inspect`) and it is never image-pulled.
//
// CROSS-INVOCATION REALITY: `ai` is a short-lived CLI — there is no daemon holding
// a vllm.Manager between runs. So "which vLLM servers are up" is discovered from the
// persisted config store (config/model-runtimes.yaml, the runtime=vllm choices, each
// carrying an Endpoint), by HTTP-probing each recorded endpoint — NOT from Manager
// memory. vLLM servers are launched DETACHED so they outlive the CLI.
//
// vLLM serves ONE model per process (each an OpenAI-API-COMPATIBLE HTTP endpoint on
// a host loopback port — NOT a call to real OpenAI), so there can be 0..N of them;
// the platform surfaces a SINGLE `vllm`
// summary line (Mode "host") — running when ≥1 recorded endpoint answers, else
// stopped — matching serviceHealthy("vllm") and keeping the status shape flat.
//
// vllm.RealRunner now actually spawns a DETACHED `vllm serve`; ensureVLLMServers
// ATTEMPTS the launch best-effort (only when vLLM is installed — gated on vllmDetect)
// and logs the install guidance on failure. It NEVER fails setup (vLLM is optional).
// hardware bring-up: the real launch is exercised only on a provisioned host with vLLM.

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/hf"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/vllm"
)

// vllmStopByPort / vllmPkill are the injectable seams for the host-native vLLM STOP
// path so unit tests never signal a real process. vllmStopByPort stops the server
// bound to a recorded loopback port; vllmPkill is the fallback (no ports recorded)
// that mirrors internal/uninstall's stopVLLMServers.
var (
	vllmStopByPort = vllm.StopByPort
	vllmPkill      = func() error { return exec.Command("pkill", "-f", "vllm serve").Run() }
)

// hostNativeProgressWriter is where startVLLMServersHost streams its per-model
// launch progress. Defaults to stderr for plain CLI use (`ai services start
// vllm`), where it shares a normal terminal with no competing renderer. A
// bubbletea TUI (`ai ui`) MUST silence it via SetHostNativeProgressWriter: writing
// straight to stderr while the alt-screen is active corrupts the rendered frame
// (stray lines bleed through the bordered panes) until a resize forces bubbletea
// to redraw from scratch — the bug this seam fixes.
var hostNativeProgressWriter = func(line string) { _, _ = fmt.Fprintln(os.Stderr, line) }

// SetHostNativeProgressWriter overrides the host-native progress sink (see
// hostNativeProgressWriter). Pass nil to discard progress lines entirely.
func SetHostNativeProgressWriter(writer func(string)) {
	if writer == nil {
		writer = func(string) {}
	}
	hostNativeProgressWriter = writer
}

// startVLLMServersHost brings up the host-native vLLM servers for `ai services start
// vllm`. vLLM is per-model, so it starts a `vllm serve` for every recorded runtime=vllm
// model (reusing each model's reserved loopback port). It is GATED on vLLM being
// installed: with no `vllm` binary it returns an actionable missing-dependency error
// (exit 3) carrying the install guidance. With vLLM installed but NO models recorded it
// SUCCEEDS with nothing to do (the caller notes "pull one with `ai models pull --runtime
// vllm`"). The launch itself is best-effort per model via ensureVLLMServers (it never
// panics or aborts early), but — unlike the general `ai setup` reconcile, which must
// never fail on an optional local model — this is an EXPLICIT single-service action
// (`ai services start|restart vllm`, including the TUI's Service-tab s/r keys), so a
// model that failed to come up (e.g. an OOM'd KV cache) is now reported as a real error
// instead of a false success: previously this unconditionally returned nil, so the CLI's
// own exit code/JSON envelope said "ok" and the TUI's restart flash showed a misleading
// green "restarted vllm" while the server stayed dead — see the `max_model_len` OOM
// incident this was found from. hardware bring-up: the real `vllm serve` fork runs only
// on a provisioned host.
func startVLLMServersHost() error {
	installed, _ := vllmDetect()
	if !installed {
		return output.Errorf(output.ExitMissingDep,
			"vLLM is not installed — %s; then run `ai services start vllm`",
			strings.Join(vllm.InstallGuidance(goruntime.GOOS), "; "))
	}
	// Installed but idle (no runtime=vllm models): nothing to serve — success.
	if len(vllmRuntimeChoices()) == 0 {
		return nil
	}
	// A discarded (no-op) progress callback here used to make `ai services start
	// vllm` completely SILENT while a fresh model loaded (up to
	// vllm.DefaultStartTimeout, 15m) — reading as a hang with nothing to show for
	// it. ensureVLLMServers already reports each model attempt plus the Manager's
	// own internal events (adopt/evict/busy-port); just stop throwing them away.
	failed := ensureVLLMServers(hostNativeProgressWriter)
	if len(failed) > 0 {
		return output.Errorf(output.ExitRuntimeFailure,
			"%d of %d vLLM model(s) failed to start: %s — see the per-model log under %s",
			len(failed), len(vllmRuntimeChoices()), strings.Join(failed, "; "), vllmStoreDirOrDefault())
	}
	return nil
}

// vllmStoreDirOrDefault is vllm.StoreDir() with a fallback description for the rare case
// the store path itself can't be resolved — only used to compose the actionable message
// above, never to gate any real behavior.
func vllmStoreDirOrDefault() string {
	dir, err := vllm.StoreDir()
	if err != nil {
		return "the vLLM model store"
	}
	return dir
}

// stopVLLMServersHost stops every host-native vLLM server for `ai services stop vllm`,
// best-effort (a not-running server is not an error): it stops the server on each
// recorded runtime=vllm loopback port, falling back to a `pkill -f "vllm serve"` when no
// ports are recorded. It never returns an error (vLLM is optional).
func stopVLLMServersHost() error {
	stoppedAny := false
	for _, choice := range vllmRuntimeChoices() {
		if port, ok := vllm.PortOf(choice.Endpoint); ok {
			_ = vllmStopByPort(port)
			stoppedAny = true
		}
	}
	if !stoppedAny {
		_ = vllmPkill()
	}
	return nil
}

// vllmProbeTimeout bounds the per-endpoint reachability probe so a dead server
// can't stall a status read.
const vllmProbeTimeout = 3 * time.Second

// vllmHTTPGet is the injectable seam for the vLLM endpoint reachability probe,
// so Status/serviceHealthy are unit-testable without a live server. The real
// probe is a short-timeout HTTP GET.
var vllmHTTPGet = func(url string) (*http.Response, error) {
	client := &http.Client{Timeout: vllmProbeTimeout}
	return client.Get(url)
}

// vllmDetect is the injectable seam for the vLLM install probe (vllm.Detect). It gates
// ensureVLLMServers so a host WITHOUT vLLM logs the install guidance instead of spawning
// a real `vllm serve` — and so unit tests never fork a process regardless of the host.
var vllmDetect = vllm.Detect

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

// vllmHostLogTail returns the last `lines` lines of each recorded vLLM model's
// captured output (`<vllm.StoreDir()>/<alias>.log`, redirected there by
// vllm.RealRunner.Start), concatenated under a `── <alias> ──` heading per model —
// the host-native analogue of the multi-container heading path above (e.g.
// presidio's analyzer + anonymizer). With no recorded models, or none that have ever
// produced a log file, it returns "" with no error (nothing to show yet is not a
// failure — the same tolerance a stopped/absent container gets).
func vllmHostLogTail(lines int) (string, error) {
	choices := vllmRuntimeChoices()
	if len(choices) == 0 {
		return "", nil
	}
	storeDir, err := vllm.StoreDir()
	if err != nil {
		return "", nil
	}
	if len(choices) == 1 {
		content, ok := tailLogFile(filepath.Join(storeDir, choices[0].Alias+".log"), lines)
		if !ok {
			return "", nil
		}
		return content, nil
	}
	var combined strings.Builder
	for index, choice := range choices {
		content, ok := tailLogFile(filepath.Join(storeDir, choice.Alias+".log"), lines)
		if !ok {
			continue
		}
		if index > 0 && combined.Len() > 0 {
			combined.WriteString("\n")
		}
		combined.WriteString("── " + choice.Alias + " ──\n")
		combined.WriteString(content)
	}
	return combined.String(), nil
}

// tailLogFile reads path and returns its last `lines` lines. A missing/unreadable
// file reports ok=false (never an error — a model that has not been started yet, or
// whose log predates this feature, simply has nothing to show).
func tailLogFile(path string, lines int) (string, bool) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	text := strings.TrimRight(string(content), "\n")
	if text == "" {
		return "", false
	}
	split := strings.Split(text, "\n")
	if len(split) > lines {
		split = split[len(split)-lines:]
	}
	return strings.Join(split, "\n"), true
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
		// No vLLM models recorded — idle, not an error. vLLM is per-model and
		// lazy-started, so a fresh `ai setup` has nothing to serve; spell that out
		// (and whether vLLM is even installed) so a bare "stopped" doesn't read as broken.
		status.Detail = vllmIdleDetail()
		return status
	}
	// Surface each recorded model's resolved config regardless of health — useful
	// even while down, to see what WOULD be served / what caps are pinned.
	status.Models = vllmModelInfos(choices)
	anyHealthy := false
	for _, model := range status.Models {
		if model.Healthy {
			anyHealthy = true
			break
		}
	}
	if anyHealthy {
		status.State = "running"
		status.Healthy = true
		return status
	}
	// Models recorded but no server up: surface the recovery path. vLLM is
	// OPTIONAL, so doctor renders this as a warning (down), not an error.
	status.Detail = vllmDownDetail(goruntime.GOOS)
	return status
}

// vllmModelInfos maps the recorded runtime choices to the ServiceStatus.Models
// shape, probing each endpoint's live health.
func vllmModelInfos(choices []config.ModelRuntimeChoice) []VLLMModelInfo {
	models := make([]VLLMModelInfo, 0, len(choices))
	for _, choice := range choices {
		models = append(models, VLLMModelInfo{
			Alias:                choice.Alias,
			Model:                choice.Model,
			Endpoint:             choice.Endpoint,
			Healthy:              vllmEndpointHealthy(choice.Endpoint),
			GPUMemoryUtilization: choice.GPUMemoryUtilization,
			MaxModelLen:          choice.MaxModelLen,
			Disabled:             choice.Disabled,
		})
	}
	return models
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

// vllmIdleDetail is the one-line hint shown when NO models are set to the vllm runtime
// (the fresh-`ai setup` state). vLLM is per-model and lazy-started, so "stopped" here is
// idle, not broken — say so, and distinguish installed-but-idle from not-installed so the
// user knows the next step.
func vllmIdleDetail() string {
	if installed, _ := vllmDetect(); installed {
		return "idle — no models set to the vllm runtime; add one with `ai models pull --runtime vllm <model>`"
	}
	return "not installed — run `ai models install-vllm`, then `ai models pull --runtime vllm <model>`"
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
// failure (e.g. `vllm serve` exiting immediately, or a port conflict) is logged
// with the install guidance and skipped. Servers are launched DETACHED (by the
// Runner) so they outlive this short-lived CLI. It also returns a short "alias:
// reason" line per model that failed to start, so an explicit single-service caller
// (startVLLMServersHost, backing `ai services start|restart vllm`) can surface a
// real failure instead of reporting success — the general `ai setup` reconcile
// (setup_real.go) calls this the same way it always has and simply ignores the
// return value, so that invariant is unchanged.
func ensureVLLMServers(progress func(string)) []string {
	choices := vllmRuntimeChoices()
	if len(choices) == 0 {
		return nil // nothing to serve
	}
	installed, _ := vllmDetect()
	// Seed the Manager with the recorded alias→port map so each model REUSES its recorded
	// loopback port and a fresh launch never steals another recorded model's port.
	reserved := map[string]int{}
	for _, choice := range choices {
		if port, ok := vllm.PortOf(choice.Endpoint); ok {
			reserved[choice.Alias] = port
		}
	}
	var manager *vllm.Manager
	if installed {
		manager = vllm.NewManager(vllm.Config{
			Runner:   vllm.RealRunner{},
			Probe:    vllm.RealHealthProbe(),
			Reserved: reserved,
			Log:      func(line string) { progress("      " + line) },
		})
	}
	var failed []string
	for _, choice := range choices {
		// Disabled (`ai models disable`): deliberately excluded from the auto-start
		// pass — e.g. to keep a second local model pulled/registered without it
		// fighting another for GPU/unified memory. Weights/registration untouched;
		// `ai models enable` starts it again.
		if choice.Disabled {
			continue
		}
		// Already up (a detached launch from a previous CLI run)? Leave it alone.
		if vllmEndpointHealthy(choice.Endpoint) {
			continue
		}
		progress("  • vLLM server for " + choice.Alias + " (host-native, best-effort)…")
		if !installed {
			// No vLLM on PATH — never attempt a launch (nothing to exec). Surface the
			// install guidance and continue; vLLM is OPTIONAL, so this never fails setup.
			progress("      vLLM not installed for " + choice.Alias)
			for _, line := range vllm.InstallGuidance(goruntime.GOOS) {
				progress("      - " + line)
			}
			failed = append(failed, choice.Alias+": vLLM not installed")
			continue
		}
		opts := vllm.ServeOptions{
			ToolCallParser:       hf.ToolCallParserFor(goruntime.GOOS, vllmServeModel(choice)),
			ReasoningParser:      hf.ReasoningParserFor(goruntime.GOOS, vllmServeModel(choice)),
			GPUMemoryUtilization: choice.GPUMemoryUtilization,
			MaxModelLen:          choice.MaxModelLen,
		}
		if _, _, err := manager.EnsureServedWithOptions(choice.Alias, vllmServeModel(choice), opts); err != nil {
			// Best-effort: vLLM is optional, so the RECONCILE never fails on this — but
			// the failure is still collected (see the doc comment above) so an explicit
			// single-service start/restart can report it rather than claiming success.
			progress("      vLLM not started for " + choice.Alias + ": " + err.Error())
			for _, line := range vllm.InstallGuidance(goruntime.GOOS) {
				progress("      - " + line)
			}
			failed = append(failed, choice.Alias+": "+err.Error())
		}
	}
	return failed
}
