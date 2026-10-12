// Package colibri is the host-side manager for the colibri local-inference
// server (https://github.com/JustVugg/colibri) — a SECOND, OPTIONAL local-
// inference backend alongside omlx (internal/omlx), never required: `ai setup`
// best-effort installs the colibri BINARY, but never fails if it can't, and never
// starts a server that colibri itself has not been configured to run.
//
// Unlike omlx (one shared server, every model it finds, downloaded through its
// OWN admin panel), colibri's `coli serve`/`coli web` keeps EXACTLY ONE model
// process loaded — confirmed against colibri's own source (its GET /v1/models
// lists only the one model the running process was started with). So this
// platform runs it the same shape as omlx regardless: ONE host-native colibri
// process, registered in LiteLLM as whatever single model it currently reports.
//
// Model selection/download is intentionally NOT automated by this platform, for
// the same reason omlx's is not: `coli setup` (run directly by the user, outside
// this CLI) is an interactive hardware-detection + model-recommendation wizard
// that can download tens of GB — auto-triggering that from `ai setup`/`ai
// services start colibri` would turn a normal platform setup into a surprise,
// possibly very long, unattended download. So this package's Install only
// fetches the (small, a few MB) colibri CLI/engine release archive; starting the
// server (StartColibriServerHost, internal/setup/colibri_host.go) checks
// colibri's OWN `coli status --json` first and does nothing (best-effort, no
// error) until the user has run `coli setup` themselves at least once.
//
// `coli start`/`coli web --no-browser`/`coli stop` are colibri's OWN lifecycle
// subcommands (it tracks its own PID/port/log state in its setup.json) — unlike
// omlx, this package never forks/tracks a raw process itself; EnsureServed just
// execs the right subcommand and polls the HTTP health endpoint.
package colibri

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/paths"
	"github.com/jt-helsinki/stack-genie/internal/services"
)

const (
	// Port is the fixed host loopback port the colibri server binds. Single
	// source of truth is services.ColibriPort (a leaf package other packages
	// derive from) — see its own doc comment for why it differs from colibri's
	// own 8000 default.
	Port = services.ColibriPort

	// DefaultStartTimeout bounds how long EnsureServed waits for an already-
	// configured server to answer its health probe after `coli start`. A loaded
	// MoE model can take a while to mmap/warm up even when no download is needed.
	DefaultStartTimeout = 5 * time.Minute

	// DefaultPollInterval is the gap between health probes while waiting for start.
	DefaultPollInterval = time.Second

	binSubdir = "colibri"
)

// StoreDir is ~/.ai-platform/bin/colibri — where the platform-managed colibri
// release archive (the `coli` Python launcher, the compiled engine binary, and
// its support modules) is extracted. Distinct from ModelsDir: this holds the
// CLI/engine, never model weights.
func StoreDir() (string, error) {
	binDir, err := paths.BinDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(binDir, binSubdir), nil
}

// ModelsDir is ~/.ai-platform/volumes/models/colibri — passed to `coli setup`'s
// own `--dir` flag as where IT downloads model weights (never written to by this
// platform directly), mirroring omlx's volumes/models/omlx convention.
func ModelsDir() (string, error) {
	volumesDir, err := paths.VolumesDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(volumesDir, "models", "colibri"), nil
}

// BaseURL is the OpenAI-compatible API root colibri's single served model is
// reached at (host-native, not through the gateway) — what LiteLLM's omlx/*-style
// colibri/* registration points at via host.docker.internal, and what this CLI's
// own health probe/ListModels use directly.
func BaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", Port)
}

// lookPath is the injectable seam locating the coli launcher (defaults to
// exec.LookPath); overridable in tests without touching the host.
var lookPath = exec.LookPath

// BinaryPath resolves the `coli` launcher script this platform runs:
// StoreDir/coli if it exists, else the bare name "coli" for a PATH lookup (a
// manual/external install, e.g. from colibri's own start-here.sh, is honored
// too).
func BinaryPath() (string, error) {
	storeDir, err := StoreDir()
	if err != nil {
		return "coli", nil //nolint:nilerr // a resolve failure just falls back to PATH
	}
	candidate := filepath.Join(storeDir, "coli")
	if _, statErr := os.Stat(candidate); statErr == nil {
		return candidate, nil
	}
	return "coli", nil
}

// Detect reports whether a usable colibri install is present (in the platform
// store or on PATH). Read-only.
func Detect() bool {
	binary, err := BinaryPath()
	if err != nil {
		return false
	}
	_, lookErr := lookPath(binary)
	return lookErr == nil
}

// pythonLookPath is the injectable seam checking for a python3 interpreter (the
// `coli` launcher's shebang requires one on PATH — it is a Python script, not a
// standalone binary).
var pythonLookPath = exec.LookPath

// PythonAvailable reports whether a python3 interpreter is on PATH, required to
// run the `coli` launcher script at all (colibri ships its own compiled engine
// binary, but the CLI wrapper around it is plain Python).
func PythonAvailable() bool {
	_, err := pythonLookPath("python3")
	return err == nil
}

// releaseGet is the injectable HTTP seam for querying the GitHub releases API.
// Tests replace it so no network is touched.
var releaseGet = func(url string) ([]byte, error) {
	resp, err := http.Get(url) //nolint:gosec // fixed GitHub API host, best-effort bring-up
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// latestReleaseAPI is the GitHub API endpoint for the newest colibri release.
const latestReleaseAPI = "https://api.github.com/repos/JustVugg/colibri/releases/latest"

// ghAsset / ghRelease decode only the GitHub release fields the resolver needs.
type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

// archiveSuffixFor returns the release-asset substring identifying this host's
// platform/arch, e.g. "macos-arm64" or "linux-x86_64" — verified against the real
// v2.0.0 release assets (colibri-v2.0.0-macos-arm64.tar.gz,
// colibri-v2.0.0-linux-x86_64.tar.gz). Returns "" for an unsupported combination
// (Windows is not a target OS for this platform — see AGENTS.md) so Install fails
// fast with a clear message instead of silently picking the wrong asset.
func archiveSuffixFor(goos, goarch string) string {
	switch {
	case goos == "darwin" && goarch == "arm64":
		return "macos-arm64"
	case goos == "linux" && goarch == "amd64":
		return "linux-x86_64"
	default:
		return ""
	}
}

// resolveAsset queries the latest colibri release and returns the download URL of
// the archive matching this host's platform/arch. Any failure (network,
// malformed JSON, no matching asset) is returned as an error for the caller to
// treat best-effort.
func resolveAsset(goos, goarch string) (downloadURL string, err error) {
	suffix := archiveSuffixFor(goos, goarch)
	if suffix == "" {
		return "", fmt.Errorf("colibri: no published release for %s/%s", goos, goarch)
	}
	body, err := releaseGet(latestReleaseAPI)
	if err != nil {
		return "", fmt.Errorf("resolve colibri latest release: %w", err)
	}
	var release ghRelease
	if unmarshalErr := json.Unmarshal(body, &release); unmarshalErr != nil {
		return "", fmt.Errorf("parse colibri release JSON: %w", unmarshalErr)
	}
	for _, asset := range release.Assets {
		if strings.Contains(asset.Name, suffix) && (strings.HasSuffix(asset.Name, ".tar.gz") || strings.HasSuffix(asset.Name, ".zip")) {
			return asset.BrowserDownloadURL, nil
		}
	}
	return "", fmt.Errorf("colibri latest release has no asset matching %q", suffix)
}

// archiveFetch is the injectable seam downloading the resolved archive's bytes.
// Tests replace it so no network is touched.
var archiveFetch = func(url string) ([]byte, error) {
	resp, err := http.Get(url) //nolint:gosec // fixed GitHub release host, best-effort bring-up
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// Install resolves the latest colibri release's archive matching this host's
// platform/arch and extracts it into StoreDir (tar.gz only — colibri publishes
// .zip for Windows, which this platform does not target). The archive is a small
// (a few MB) CLI/engine download, NOT a model — see this package's doc comment
// for why model setup is deliberately left to the user's own `coli setup`.
func Install() error {
	suffix := archiveSuffixFor(runtime.GOOS, runtime.GOARCH)
	if suffix == "" {
		return fmt.Errorf("colibri: unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	downloadURL, err := resolveAsset(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	archiveBytes, err := archiveFetch(downloadURL)
	if err != nil {
		return fmt.Errorf("download colibri release: %w", err)
	}
	storeDir, err := StoreDir()
	if err != nil {
		return err
	}
	if err := extractTarGz(archiveBytes, storeDir); err != nil {
		return fmt.Errorf("extract colibri release into %s: %w", storeDir, err)
	}
	// The launcher + engine binary must be executable — tar preserves the mode
	// bit from the release archive, but re-assert it defensively (e.g. a FAT/exFAT
	// extraction target that drops exec bits).
	for _, name := range []string{"coli", "colibri"} {
		_ = os.Chmod(filepath.Join(storeDir, name), 0o755) //nolint:gosec // intentionally executable
	}
	return nil
}

// extractTarGz extracts a .tar.gz archive's contents into dir (created if
// absent), preserving each entry's executable bit. Guards against path
// traversal (a malicious/corrupt archive entry escaping dir via "..") by
// rejecting any entry name that resolves outside dir.
func extractTarGz(archiveBytes []byte, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	gzipReader, err := gzip.NewReader(strings.NewReader(string(archiveBytes)))
	if err != nil {
		return err
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dir, header.Name) //nolint:gosec // validated below
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) && target != filepath.Clean(dir) {
			return fmt.Errorf("archive entry %q escapes the extraction directory", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			//nolint:gosec // mode comes from the tar header, clamped to a safe exec/read range below
			mode := os.FileMode(header.Mode) & 0o777
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, copyErr := io.Copy(file, tarReader); copyErr != nil { //nolint:gosec // archive size bounded by GitHub release assets, not user input
				_ = file.Close()
				return copyErr
			}
			if err := file.Close(); err != nil {
				return err
			}
		}
	}
}

// InstallGuidance returns ordered manual steps to install colibri when the
// best-effort automatic install fails or was skipped. It performs NO host
// mutation.
func InstallGuidance() []string {
	return []string{
		"colibri requires a python3 interpreter on PATH (its CLI launcher is a Python script around a compiled engine).",
		"`ai setup` AUTO-installs the colibri CLI/engine into " + mustDisplayStoreDir() + " from the latest GitHub release — https://github.com/JustVugg/colibri/releases.",
		"Install/repair manually by extracting that release archive yourself, or run colibri's own `./start-here.sh` (see https://github.com/JustVugg/colibri).",
		"Model selection/download is NOT automated by this platform (it can be tens of GB) — run `coli setup` yourself once from " + mustDisplayStoreDir() + " to pick and download a model, THEN `ai services start colibri` will start it.",
		"Verify with `" + mustDisplayStoreDir() + "/coli --version`; the platform then starts + serves through it automatically once `coli status --json` reports configured.",
	}
}

// mustDisplayStoreDir returns StoreDir for display in guidance text, falling
// back to the literal path pattern if HOME cannot be resolved (display only,
// never used for an actual file operation).
func mustDisplayStoreDir() string {
	if dir, err := StoreDir(); err == nil {
		return dir
	}
	return "~/.ai-platform/bin/colibri"
}

// LiveModel is one model colibri's live GET /v1/models reports — always exactly
// one entry in practice (see this package's doc comment), but returned as a
// slice for call-site symmetry with internal/omlx.ListModels.
type LiveModel struct {
	ID string
}

// settingsFile is the injectable seam for APIKey's env-only lookup. colibri's own
// API-key requirement is opt-in (its docs: "needs no API key by default"), so
// unlike omlx there is no settings-file fallback to read — this platform's own
// security policy is to always generate and pass one anyway (see
// internal/setup/colibri_host.go's ensureColibriAPIKey), exactly mirroring
// ensureOmlxAPIKey, so APIKey here is simply the env var this process (and thus,
// via os.Environ() passthrough, the coli subprocess) was given.
func APIKey() string {
	return os.Getenv("COLI_API_KEY")
}

// httpClient is the shared client for authorizedRequest/ListModels/DefaultProbe —
// short timeouts since every call is a local-loopback call.
var httpClient = &http.Client{Timeout: 5 * time.Second}

// authorizedRequest builds a GET request against url, attaching a Bearer
// Authorization header when COLI_API_KEY is set — colibri's OpenAI-compatible
// surface accepts a Bearer token the same way omlx's does, so this keeps the
// platform's own probes/list calls consistent with how the gateway itself
// authenticates.
func authorizedRequest(url string) (*http.Request, error) {
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if key := APIKey(); key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	return request, nil
}

// ListModels queries the running colibri server's live GET /v1/models. Returns an
// error if the server is unreachable or the response is malformed; callers treat
// that as "nothing to sync right now", never a fatal condition — same contract as
// internal/omlx.ListModels.
func ListModels() ([]LiveModel, error) {
	request, err := authorizedRequest(BaseURL() + "/models")
	if err != nil {
		return nil, fmt.Errorf("colibri: GET /v1/models: %w", err)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("colibri: GET /v1/models: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("colibri: GET /v1/models: unexpected status %s", response.Status)
	}
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("colibri: parse /v1/models response: %w", err)
	}
	models := make([]LiveModel, 0, len(decoded.Data))
	for _, entry := range decoded.Data {
		if entry.ID != "" {
			models = append(models, LiveModel{ID: entry.ID})
		}
	}
	return models, nil
}

// HealthProbe reports whether the colibri server on the host loopback is
// answering. The real probe (DefaultProbe) is a short-timeout HTTP GET against
// /health (colibri's own liveness endpoint — "exposes active/queued/completed/
// rejected counters" per its docs); tests inject a fake so no network is touched.
type HealthProbe func() bool

// DefaultProbe is the real HealthProbe: a short-timeout GET against colibri's
// /health endpoint (unauthenticated — colibri's docs document it as a plain
// liveness check, unlike the model-serving routes).
var DefaultProbe HealthProbe = func() bool {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", Port))
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// StatusInfo is the subset of `coli status --json`'s output this platform acts
// on: whether a model has ever been configured (Configured) — gating whether
// EnsureServed should even attempt a start, since an unconfigured colibri has no
// model to serve and `coli start` would just fail — see this package's doc
// comment for why that configuration step is deliberately left to the user.
type StatusInfo struct {
	Configured bool `json:"configured"`
}

// statusCommand is the injectable seam running `coli status --json`. Tests
// replace it so no real process is spawned.
var statusCommand = func(binary string) ([]byte, error) {
	return exec.Command(binary, "status", "--json").Output() //nolint:gosec // binary is BinaryPath()'s own resolved value, args are fixed literals
}

// Status reports colibri's own configuration state via `coli status --json`.
// Best-effort: any failure (colibri absent, malformed output) reports
// Configured=false rather than erroring — the caller treats that identically to
// "nothing to start yet".
func Status() StatusInfo {
	binary, err := BinaryPath()
	if err != nil {
		return StatusInfo{}
	}
	output, err := statusCommand(binary)
	if err != nil {
		return StatusInfo{}
	}
	var info StatusInfo
	if jsonErr := json.Unmarshal(output, &info); jsonErr != nil {
		return StatusInfo{}
	}
	return info
}
