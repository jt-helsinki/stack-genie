package setup

// colibri_host.go wires the HOST-NATIVE colibri local-inference backend into the
// services status/health/reconcile layer — the mirror image of omlx_host.go for
// the SECOND, OPTIONAL local-inference backend (internal/colibri,
// https://github.com/JustVugg/colibri). Every difference from omlx_host.go
// follows from two facts verified against colibri's own source/docs (see
// internal/colibri's package doc comment): it keeps exactly ONE model loaded per
// process (so, like omlx, ONE host-native summary line — not per-model), and
// model selection/download is a conscious one-time step the USER runs themselves
// (`coli setup`) rather than something this platform ever triggers. So unlike
// omlx (REQUIRED — a failed start is a hard reconcile error), colibri is
// OPTIONAL end to end: a missing binary, an unconfigured install, or a failed
// start are all best-effort — logged, never fatal to `ai setup` or to `ai
// services start|restart colibri`.
//
// hardware bring-up: the real `coli start`/`coli stop` round-trips and the live
// LiteLLM sync are exercised only on a provisioned host with colibri configured.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/colibri"
	"github.com/jt-helsinki/stack-genie/internal/litellm"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
)

// installColibriFn is the injectable seam for the one-shot colibri CLI/engine
// install (colibri.Install). A package var so tests exercise
// ensureColibriInstalled without a real network download.
var installColibriFn = colibri.Install

// ensureColibriInstalled installs the colibri CLI/engine archive into the
// platform store when it is not already present. Mirrors ensureOmlxInstalled's
// shape exactly, EXCEPT it never blocks/fails anything it's part of — colibri is
// optional end to end (see this file's package comment) — and the download is a
// small (a few MB) CLI/engine archive, NOT a model (see internal/colibri's
// package doc comment for why model setup stays the user's own `coli setup`).
func ensureColibriInstalled(progress func(string)) {
	if colibriDetectFn() {
		progress("  • colibri present in the platform store")
		return
	}
	progress("  • installing colibri (optional local-inference backend)…")
	if err := installColibriFn(); err != nil {
		progress("  • colibri install skipped (" + err.Error() + ") — install later with `ai services start colibri`")
		return
	}
	if colibriDetectFn() {
		progress("  • colibri installed")
		return
	}
	progress("  • colibri install ran but the binary was not detected — check its store directory")
}

// colibriDetectFn is the injectable seam for the colibri install probe
// (colibri.Detect). Gates startColibriServerHost so a host WITHOUT colibri logs
// install guidance instead of attempting a real `coli start` — and so unit tests
// never fork a process regardless of the host.
var colibriDetectFn = colibri.Detect

// colibriStatusFn is the injectable seam for colibri's own configuration check
// (colibri.Status). Gates startColibriServerHost/colibriDownDetail so tests never
// shell out to a real `coli status --json`.
var colibriStatusFn = colibri.Status

// colibriManagerFn builds the colibri.Manager used to start/health-check the
// server. Injectable so tests exercise startColibriServerHost/
// stopColibriServerHost without a real process.
var colibriManagerFn = func() *colibri.Manager {
	return colibri.NewManager(colibri.Config{
		// StatusFn reads colibriStatusFn's CURRENT value at call time (not just at
		// NewManager time), so the ONE seam (colibriStatusFn) controls both
		// colibriDownDetail's own check and the Manager's internal "is a model
		// configured" gate — one source of truth, including in tests that stub
		// colibriStatusFn.
		StatusFn: func() colibri.StatusInfo { return colibriStatusFn() },
		Log:      func(line string) { hostNativeProgressWriter("      " + line) },
	})
}

// colibriSyncModelsFn is the injectable seam wrapping SyncColibriModels so tests
// exercise startColibriServerHost without a real gateway.
var colibriSyncModelsFn = SyncColibriModels

// colibriContainerAPIBase is the base URL registered with LiteLLM as colibri's
// model api_base — the exact mirror of omlxContainerAPIBase, for the exact same
// reason: LiteLLM runs in a CONTAINER, so "127.0.0.1" there is the container's own
// loopback, not the host's. See hostGatewayName's doc comment for the Linux
// --add-host requirement this relies on.
func colibriContainerAPIBase() string {
	return fmt.Sprintf("http://%s:%d/v1", hostGatewayName, colibri.Port)
}

// SyncColibriModels reconciles LiteLLM's colibri/* registrations against
// colibri's live GET /v1/models — the exact mirror of SyncOmlxModels. This is the
// ONLY place that sync runs from (both `ai setup` and `ai services
// start|restart colibri` funnel through startColibriServerHost), and it is
// exported so `ai models refresh` can trigger it on demand too.
func SyncColibriModels() (litellm.SyncResult, error) {
	liveModels, err := colibri.ListModels()
	if err != nil {
		return litellm.SyncResult{}, err
	}
	ids := make([]string, 0, len(liveModels))
	for _, model := range liveModels {
		ids = append(ids, model.ID)
	}
	return litellm.NewKeyManager(runtime.RealProber()).SyncColibriModels(ids, colibriContainerAPIBase(), colibri.APIKey())
}

// ensureColibriAPIKey mirrors ensureOmlxAPIKey exactly, for colibri's own
// COLI_API_KEY: colibri's key requirement is opt-in on ITS side (unlike omlx's
// hard requirement), but this platform's own security policy is to always
// generate and pass one anyway, for the same reason — an unauthenticated
// host-native server bound so a container can reach it is otherwise reachable by
// anything else on the host's loopback/network too. Best-effort: never fails.
func ensureColibriAPIKey() {
	if os.Getenv("COLI_API_KEY") != "" {
		return
	}
	if value, err := findCredentialFn("COLI_API_KEY"); err == nil && value != "" {
		_ = os.Setenv("COLI_API_KEY", value)
		return
	}
	key := generateColibriAPIKey()
	_ = os.Setenv("COLI_API_KEY", key)
	_ = storeCredentialFn("COLI_API_KEY", key)
}

// generateColibriAPIKey returns a random colibri API key, mirroring
// generateOmlxAPIKey's shape (a fixed prefix + 24 random bytes hex-encoded).
func generateColibriAPIKey() string {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return "colibri-aip-fallback-key"
	}
	return "colibri-" + hex.EncodeToString(buffer)
}

// LoadColibriAPIKeyFromKeychain mirrors LoadOmlxAPIKeyFromKeychain: fills
// COLI_API_KEY into this process's environment from the OS keychain when not
// already set, so every `ai` command (not just the start path) can authenticate
// its own requests to an already-running colibri server.
func LoadColibriAPIKeyFromKeychain() {
	if os.Getenv("COLI_API_KEY") != "" {
		return
	}
	if value, err := findCredentialFn("COLI_API_KEY"); err == nil && value != "" {
		_ = os.Setenv("COLI_API_KEY", value)
	}
}

// startColibriServerHost brings up the host-native colibri server for `ai
// services start colibri` and `ai setup`'s reconcile. UNLIKE startOmlxServerHost,
// every failure here is best-effort and NEVER returns an error: colibri is
// OPTIONAL end to end (see this file's package comment) — a missing binary, an
// unconfigured install (colibriStatusFn().Configured == false, meaning the user
// has not yet run `coli setup` themselves to pick a model), or a failed start are
// all logged via progress and swallowed, never failing `ai setup` or the
// service-start action.
func startColibriServerHost() error {
	if !colibriDetectFn() {
		hostNativeProgressWriter("  • colibri not installed (optional) — " + strings.Join(colibri.InstallGuidance(), "; "))
		return nil
	}
	ensureColibriAPIKey()
	hostNativeProgressWriter("  • starting the colibri local-inference server (optional, best-effort)…")
	manager := colibriManagerFn()
	if err := manager.EnsureServed(); err != nil {
		if _, notConfigured := err.(colibri.NotConfiguredError); notConfigured { //nolint:errorlint // exact-type check: this sentinel carries no wrapped cause
			hostNativeProgressWriter("      colibri: " + err.Error())
			return nil
		}
		hostNativeProgressWriter("      colibri: server failed to start (" + err.Error() + ") — skipping, optional backend")
		return nil
	}
	hostNativeProgressWriter("      colibri server healthy at " + colibri.BaseURL())
	result, err := colibriSyncModelsFn()
	if err != nil {
		hostNativeProgressWriter("      colibri model sync skipped (" + err.Error() + ")")
		return nil
	}
	if len(result.Added) > 0 || len(result.Deleted) > 0 {
		hostNativeProgressWriter(fmt.Sprintf("      colibri models synced: +%d -%d", len(result.Added), len(result.Deleted)))
	}
	return nil
}

// stopColibriServerHost stops the host-native colibri server for `ai services
// stop colibri`, best-effort (a not-running server is not an error).
func stopColibriServerHost() error {
	_ = colibriManagerFn().Stop()
	return nil
}

// colibriProbeTimeout bounds the endpoint reachability probe so a dead server
// can't stall a status read.
const colibriProbeTimeout = 3 * time.Second

// colibriHTTPGet is the injectable seam for the colibri health probe, mirroring
// omlxHTTPGet — so Status/serviceHealthy are unit-testable without a live server.
var colibriHTTPGet = func(url string) (*http.Response, error) {
	client := &http.Client{Timeout: colibriProbeTimeout}
	return client.Get(url) //nolint:gosec // url is colibri's own fixed loopback health endpoint
}

// colibriHealthy reports whether the colibri server answers GET /health. It backs
// serviceHealthy("colibri").
func colibriHealthy() bool {
	response, err := colibriHTTPGet(fmt.Sprintf("http://127.0.0.1:%d/health", colibri.Port))
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode == http.StatusOK
}

// colibriStatus builds the single host-native `colibri` summary line — the mirror
// of omlxStatus. Always Optional (unlike omlx), so `ai doctor`/`ai services`
// never treat a down/unconfigured colibri as anything worse than informational.
func (services realServices) colibriStatus() ServiceStatus {
	status := ServiceStatus{Name: "colibri", Mode: "host", State: "stopped", Optional: true}
	if colibriHealthy() {
		status.State = "running"
		status.Healthy = true
		return status
	}
	status.Detail = colibriDownDetail()
	return status
}

// colibriDownDetail is the actionable one-line hint shown when the colibri server
// is not reachable — THREE possible reasons, unlike omlx's two, since colibri
// also needs the user's own one-time `coli setup` before this platform can start
// it at all.
func colibriDownDetail() string {
	if !colibriDetectFn() {
		return "not installed — run `ai services start colibri` (auto-installs the CLI/engine)"
	}
	if !colibriStatusFn().Configured {
		return "installed but not configured — run `coli setup` to pick and download a model, then `ai services start colibri`"
	}
	return "not running — start it with `ai services start colibri`"
}
