package setup

// omlx_host.go wires the HOST-NATIVE omlx local-inference backend into the
// services status / health / reconcile layer: omlx is NOT an aip-* container, so
// its health comes from an HTTP probe (not `docker inspect`) and it is never
// image-pulled.
//
// Unlike the old per-model vLLM design this replaced, omlx is ONE SHARED server
// process serving every locally-served model (managed entirely through omlx's own
// admin panel — no per-model port allocation, LRU eviction, or resource-cap
// recording lives on this side anymore). So there is a SINGLE `omlx` summary line
// (Mode "host"), not a per-model list: running when the one server answers, else
// stopped. Once healthy, its live GET /v1/models is synced into LiteLLM's omlx/*
// registrations (internal/litellm.SyncOmlxModels) — this is the ONLY place that
// sync runs from (both `ai setup` and `ai services start|restart omlx` funnel
// through startOmlxServerHost), so there is no separate user-facing sync command.
//
// hardware bring-up: the real `omlx serve` fork + the live LiteLLM sync round-trip
// are exercised only on a provisioned host.

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/omlx"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
)

// hostNativeProgressWriter is where startOmlxServerHost streams its progress.
// Defaults to stderr for plain CLI use (`ai services start omlx`), where it
// shares a normal terminal with no competing renderer. A bubbletea TUI (`ai ui`)
// MUST silence it via SetHostNativeProgressWriter: writing straight to stderr
// while the alt-screen is active corrupts the rendered frame (stray lines bleed
// through the bordered panes) until a resize forces bubbletea to redraw from
// scratch — the bug this seam fixes.
var hostNativeProgressWriter = func(line string) { _, _ = fmt.Fprintln(os.Stderr, line) }

// SetHostNativeProgressWriter overrides the host-native progress sink (see
// hostNativeProgressWriter). Pass nil to discard progress lines entirely.
func SetHostNativeProgressWriter(writer func(string)) {
	if writer == nil {
		writer = func(string) {}
	}
	hostNativeProgressWriter = writer
}

// omlxDetectFn is the injectable seam for the omlx install probe (omlx.Detect). It
// gates startOmlxServerHost so a host WITHOUT omlx logs the install guidance
// instead of spawning a real `omlx serve` — and so unit tests never fork a
// process regardless of the host.
var omlxDetectFn = omlx.Detect

// omlxManagerFn builds the omlx.Manager used to start/health-check the server.
// Injectable so tests exercise startOmlxServerHost/stopOmlxServerHost without a
// real process.
var omlxManagerFn = func() *omlx.Manager {
	return omlx.NewManager(omlx.Config{Log: func(line string) { hostNativeProgressWriter("      " + line) }})
}

// omlxSyncModelsFn is the injectable seam wrapping SyncOmlxModels so tests exercise
// startOmlxServerHost without a real gateway.
var omlxSyncModelsFn = SyncOmlxModels

// omlxContainerAPIBase is the base URL registered with LiteLLM as the omlx model's
// api_base — DELIBERATELY NOT omlx.BaseURL() (http://127.0.0.1:<port>/v1). That URL
// is correct for THIS CLI's own host-side probes (ai runs on the host, so loopback
// reaches omlx directly), but LiteLLM runs in a CONTAINER: "127.0.0.1" there means
// the container's OWN loopback, which has nothing listening on it — every chat
// completion would fail with "Connection error" regardless of what omlx itself
// binds to. The container reaches the host-native omlx server at
// hostGatewayName:<port> instead (see hostGatewayName's doc for the Linux
// --add-host requirement this relies on).
func omlxContainerAPIBase() string {
	return fmt.Sprintf("http://%s:%d/v1", hostGatewayName, omlx.Port)
}

// SyncOmlxModels reconciles LiteLLM's omlx/* registrations against the server's
// live GET /v1/models (internal/omlx.ListModels → internal/litellm.SyncOmlxModels):
// newly-appeared models are registered, ones that disappeared are removed. This is
// the SAME sync startOmlxServerHost runs automatically at `ai setup` and at
// `ai services start|restart omlx` — it is also exported so `ai models refresh` can
// trigger it ON DEMAND (e.g. after adding/removing a model through omlx's own admin
// panel) without restarting the server itself.
func SyncOmlxModels() (litellm.SyncResult, error) {
	liveModels, err := omlx.ListModels()
	if err != nil {
		return litellm.SyncResult{}, err
	}
	ids := make([]string, 0, len(liveModels))
	for _, model := range liveModels {
		ids = append(ids, model.ID)
	}
	return litellm.NewKeyManager(runtime.RealProber()).SyncOmlxModels(ids, omlxContainerAPIBase(), omlx.APIKey())
}

// startOmlxServerHost brings up the host-native omlx server for `ai services start
// omlx`. It is GATED on omlx being installed: with no `omlx` binary it returns an
// actionable missing-dependency error (exit 3) carrying the install guidance. Once
// the server answers healthy, LiteLLM's omlx/* registrations are synced against
// its live model list — best-effort, a sync failure is reported via progress but
// does NOT fail the start (the server itself came up fine; the next start/restart
// retries the sync). This is an EXPLICIT single-service action (`ai services
// start|restart omlx`, including the TUI's Service-tab s/r keys) AND the step
// `ai setup`'s reconcile calls, so a server that fails to become healthy is a real
// error, not a swallowed one.
func startOmlxServerHost() error {
	if !omlxDetectFn() {
		return output.Errorf(output.ExitMissingDep,
			"omlx is not installed — %s; then run `ai services start omlx`",
			strings.Join(omlx.InstallGuidance(), "; "))
	}
	hostNativeProgressWriter("  • starting the omlx local-inference server (best-effort)…")
	manager := omlxManagerFn()
	if _, err := manager.EnsureServed(); err != nil {
		return output.Errorf(output.ExitRuntimeFailure, "omlx: server failed to start: %s", err.Error())
	}
	hostNativeProgressWriter("      omlx server healthy at " + omlx.BaseURL())
	result, err := omlxSyncModelsFn()
	if err != nil {
		hostNativeProgressWriter("      omlx model sync skipped (" + err.Error() + ")")
		return nil
	}
	if len(result.Added) > 0 || len(result.Deleted) > 0 {
		hostNativeProgressWriter(fmt.Sprintf("      omlx models synced: +%d -%d", len(result.Added), len(result.Deleted)))
	}
	return nil
}

// stopOmlxServerHost stops the host-native omlx server for `ai services stop
// omlx`, best-effort (a not-running server is not an error). It never returns an
// error — omlx is optional.
func stopOmlxServerHost() error {
	_ = omlxManagerFn().Stop()
	return nil
}

// omlxProbeTimeout bounds the endpoint reachability probe so a dead server can't
// stall a status read.
const omlxProbeTimeout = 3 * time.Second

// omlxHTTPGet is the injectable seam for the omlx endpoint reachability probe, so
// Status/serviceHealthy are unit-testable without a live server. The real probe is
// a short-timeout HTTP GET.
var omlxHTTPGet = func(url string) (*http.Response, error) {
	client := &http.Client{Timeout: omlxProbeTimeout}
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if key := omlx.APIKey(); key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	return client.Do(request)
}

// omlxHealthy reports whether the single omlx server answers GET /v1/models. It
// backs serviceHealthy("omlx"): the summary `omlx` line is "running" when the
// server is reachable, else "stopped".
func omlxHealthy() bool {
	response, err := omlxHTTPGet(omlx.BaseURL() + "/models")
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode == http.StatusOK
}

// omlxStatus builds the single host-native `omlx` summary line. Mode is "host"
// (non-container, health from the HTTP probe, never `docker inspect`). Optional so
// `ai doctor` treats a down omlx as a WARNING pointing at the fix, not a hard
// error — mirroring how the old vLLM status treated a down local backend.
func (services realServices) omlxStatus() ServiceStatus {
	status := ServiceStatus{Name: "omlx", Mode: "host", State: "stopped", Optional: true}
	if omlxHealthy() {
		status.State = "running"
		status.Healthy = true
		return status
	}
	status.Detail = omlxDownDetail()
	return status
}

// omlxDownDetail is the actionable one-line hint shown when the omlx server is not
// reachable.
func omlxDownDetail() string {
	if omlxDetectFn() {
		return "not running — start it with `ai services start omlx`"
	}
	return "not installed — run `ai services start omlx` (auto-installs it)"
}

// omlxServerLogTail returns the last `lines` lines of the omlx server's captured
// output (omlx.LogPath(<StoreDir>)) — the host-native analogue of a container's
// log tail. With no log file yet (never started) it returns "" with no error.
func omlxServerLogTail(lines int) (string, error) {
	storeDir, err := omlx.StoreDir()
	if err != nil {
		return "", nil
	}
	content, ok := tailLogFile(omlx.LogPath(storeDir), lines)
	if !ok {
		return "", nil
	}
	return content, nil
}

// tailLogFile reads path and returns its last `lines` lines. A missing/unreadable
// file reports ok=false (never an error — a server that has not been started yet,
// or whose log predates this feature, simply has nothing to show).
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
