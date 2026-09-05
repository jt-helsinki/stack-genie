// Package omlx is the host-side manager for the omlx local-inference server — the
// SOLE local-inference backend on this platform (macOS + Apple Silicon only).
//
// Unlike the old per-model vLLM design this replaced, omlx runs ONE shared server
// process for every locally-served model: `omlx serve --model-dir <dir> --port
// <port>`, an OpenAI-compatible endpoint at http://127.0.0.1:<port>/v1 on the host
// loopback. LiteLLM (a container) reaches it as http://host.docker.internal:<port>/v1
// (the containers already get `--add-host=host.docker.internal:host-gateway`).
//
// Models are downloaded/added/removed/tuned entirely through omlx's OWN admin panel
// at http://127.0.0.1:<port>/admin — this platform does not reimplement any of
// that (no `ai models pull/rm/configure` equivalent). Its job is only to (1) install
// omlx into the platform venv, (2) keep the ONE server process running as a
// host-native service (`ai services start|stop|restart omlx`), and (3) keep
// LiteLLM's registered model set in sync with whatever omlx's live GET /v1/models
// currently reports (see internal/litellm's SyncOmlxModels) — run automatically at
// `ai setup` and at omlx service start/restart, never a user-facing command.
//
// Install resolves the latest github.com/jundot/omlx release's prebuilt wheel
// matching the platform venv's Python version (cp311/312/313 — one universal2
// wheel per version, no core+plugin pairing needed unlike the old vllm-metal
// install) and pip-installs it. The actual network resolve/install and the real
// `omlx serve` process are `hardware bring-up`, exercised via the injectable
// releaseGet/Runner seams — no real process or network is touched in tests.
package omlx

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/paths"
	"github.com/jt-helsinki/stack-genie/internal/pyenv"
	"github.com/jt-helsinki/stack-genie/internal/services"
)

const (
	// Port is the fixed host loopback port the omlx server binds. Single source of
	// truth is services.OmlxPort (a leaf package other packages derive from).
	Port = services.OmlxPort

	// DefaultStartTimeout bounds how long EnsureServed waits for a freshly-started
	// server to answer its health probe. Generous: the process itself (FastAPI +
	// MLX import) can take tens of seconds to come up even before it serves.
	DefaultStartTimeout = 5 * time.Minute

	// DefaultPollInterval is the gap between health probes while waiting for start.
	DefaultPollInterval = time.Second

	storeSubdir = "omlx"
)

// StoreDir is ~/.ai-platform/volumes/models/omlx — the model directory omlx's
// `--model-dir` points at (MLX-format model subdirectories, managed entirely
// through omlx's own admin panel, never by this platform). Created-on-use
// (MkdirAll), so `ai uninstall --purge` (RemoveAll ~/.ai-platform) removes it.
func StoreDir() (string, error) {
	volumesDir, err := paths.VolumesDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(volumesDir, "models", storeSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("omlx: create store dir %s: %w", dir, err)
	}
	return dir, nil
}

// LogPath returns the omlx server's captured-output log path — a SIBLING of
// modelDir (StoreDir), never inside it, so it can't be mistaken for a model
// subdirectory by omlx's own directory scan. Surfaced by `ai services` Logs the
// same way every other host-native service's log is.
func LogPath(modelDir string) string {
	return filepath.Join(filepath.Dir(modelDir), "omlx-server.log")
}

// BaseURL is the omlx server's OpenAI-compatible base URL on the host loopback —
// what LiteLLM's model registrations point at (see internal/litellm).
func BaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", Port)
}

// lookPath is the injectable seam locating the omlx binary (defaults to
// exec.LookPath); overridable in tests without touching the host.
var lookPath = exec.LookPath

// BinaryPath resolves the `omlx` executable the platform runs: the platform-managed
// venv's omlx (~/.ai-platform/venv/bin/omlx, pyenv.BinPath) if that file exists,
// else the bare name "omlx" for a PATH lookup. Once omlx is installed into the
// venv, both Detect and RealRunner.Start pick it up automatically; a PATH install
// (e.g. Homebrew) is kept as a fallback.
func BinaryPath() string {
	venvBinary, err := pyenv.BinPath("omlx")
	if err != nil {
		return "omlx"
	}
	if _, statErr := os.Stat(venvBinary); statErr == nil {
		return venvBinary
	}
	return "omlx"
}

// Detect reports whether a usable omlx install is present (on PATH or in the
// platform venv). Read-only.
func Detect() bool {
	_, err := lookPath(BinaryPath())
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

// latestReleaseAPI is the GitHub API endpoint for the newest omlx release.
const latestReleaseAPI = "https://api.github.com/repos/jundot/omlx/releases/latest"

// ghAsset / ghRelease decode only the GitHub release fields the resolver needs.
type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

// cpTagPattern matches a cpython wheel tag (cp311, cp312, cp313, …).
var cpTagPattern = regexp.MustCompile(`cp3(\d+)`)

// resolveWheel queries the latest omlx release and returns the NEWEST-Python-version
// wheel asset's download URL plus the "major.minor" version its cp tag implies (a
// fresh venv has no existing version to match, so newest is the reasonable
// default — unlike the old vllm-metal core wheel, omlx ships one universal2 wheel
// PER Python version with no cross-version pairing constraint). Any failure
// (network, malformed JSON, no matching wheel asset) is returned as an error for
// the caller to treat best-effort.
func resolveWheel() (wheelURL, pythonVersion string, err error) {
	body, err := releaseGet(latestReleaseAPI)
	if err != nil {
		return "", "", fmt.Errorf("resolve omlx latest release: %w", err)
	}
	var release ghRelease
	if unmarshalErr := json.Unmarshal(body, &release); unmarshalErr != nil {
		return "", "", fmt.Errorf("parse omlx release JSON: %w", unmarshalErr)
	}
	bestMinor := -1
	for _, asset := range release.Assets {
		if !strings.HasSuffix(asset.Name, ".whl") {
			continue
		}
		match := cpTagPattern.FindStringSubmatch(asset.Name)
		if match == nil {
			continue
		}
		minor, convErr := strconv.Atoi(match[1])
		if convErr != nil || minor <= bestMinor {
			continue
		}
		bestMinor = minor
		wheelURL = asset.BrowserDownloadURL
		pythonVersion = "3." + match[1]
	}
	if wheelURL == "" {
		return "", "", fmt.Errorf("omlx latest release has no .whl asset with a recognizable cp tag")
	}
	return wheelURL, pythonVersion, nil
}

// pyenv seams (package vars) so Install is unit-tested without a real Python
// toolchain or network. They default to the real pyenv functions.
var (
	pyenvEnsureVersionFn = pyenv.EnsureVersion
	pyenvPipInstallFn    = pyenv.PipInstall
)

// Install resolves and pip-installs the latest omlx wheel into the platform-managed
// host venv (~/.ai-platform/venv), pinning the venv to the wheel's EXACT Python
// version (pyenv.EnsureVersion, recreating a mismatched venv) since a cp-tag-specific
// wheel only installs into a matching-version venv. This is `hardware bring-up`
// behind the injectable releaseGet/pyenv seams.
func Install() error {
	wheelURL, pythonVersion, err := resolveWheel()
	if err != nil {
		return err
	}
	if _, err := pyenvEnsureVersionFn(pythonVersion); err != nil {
		return err
	}
	if err := pyenvPipInstallFn(wheelURL); err != nil {
		return fmt.Errorf("install omlx wheel into the platform venv: %w", err)
	}
	return nil
}

// InstallGuidance returns ordered manual steps to install omlx into the
// platform-managed venv (~/.ai-platform/venv) that `ai setup` creates. It performs
// NO host mutation — actionable guidance the CLI prints when Detect reports omlx
// missing after a best-effort auto-install attempt.
const ManagedVenvDir = "~/.ai-platform/venv"

func InstallGuidance() []string {
	return []string{
		"omlx requires macOS 15 (Sequoia) or newer on Apple Silicon — it is the sole local-inference backend on this platform.",
		"`ai setup` AUTO-installs it into the platform Python env at " + ManagedVenvDir + ": it resolves the latest omlx release's wheel matching the venv's Python version and pip-installs it.",
		"Install/repair manually with `ai services start omlx` (re-runs the same best-effort install) or `" + ManagedVenvDir + "/bin/pip install <wheel-url>` with a wheel from https://github.com/jundot/omlx/releases.",
		"Models are downloaded/managed entirely through omlx's own admin panel at http://127.0.0.1:" + strconv.Itoa(Port) + "/admin — this platform has no `ai models pull` equivalent.",
		"Verify with `" + ManagedVenvDir + "/bin/omlx --version`; the platform then starts + serves through it automatically.",
	}
}

// LiveModel is one model omlx's live GET /v1/models currently reports.
type LiveModel struct {
	ID string
}

// httpClient is the injectable seam for ListModels' HTTP call. Tests replace it
// with a fake transport; the default has a short timeout since this is a
// local-loopback call.
var httpClient = &http.Client{Timeout: 5 * time.Second}

// ListModels queries the running omlx server's live GET /v1/models and returns the
// model ids it currently reports (whatever is loaded/discovered from StoreDir —
// entirely managed through omlx's own admin panel). Returns an error if the server
// is unreachable or the response is malformed; callers treat that as "nothing to
// sync right now", never a fatal condition.
func ListModels() ([]LiveModel, error) {
	response, err := httpClient.Get(BaseURL() + "/models")
	if err != nil {
		return nil, fmt.Errorf("omlx: GET /v1/models: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("omlx: GET /v1/models: unexpected status %s", response.Status)
	}
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("omlx: parse /v1/models response: %w", err)
	}
	models := make([]LiveModel, 0, len(decoded.Data))
	for _, entry := range decoded.Data {
		if entry.ID != "" {
			models = append(models, LiveModel{ID: entry.ID})
		}
	}
	return models, nil
}

// HealthProbe reports whether the omlx server on the host loopback is answering
// (GET /v1/models → 200). The real probe (DefaultProbe) is a short-timeout HTTP
// GET; tests inject a fake so no network is touched.
type HealthProbe func() bool

// DefaultProbe is the real HealthProbe: a short-timeout GET against the fixed
// omlx port's /v1/models endpoint.
var DefaultProbe HealthProbe = func() bool {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(BaseURL() + "/models")
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// portInUse reports whether ANYTHING is listening on the omlx loopback port — used
// exactly like the old vLLM daemonless-reuse check: `ai` is a short-lived CLI with
// no in-memory handle to a server a PRIOR invocation started, so before spawning a
// new process, check whether the port is already live rather than risk a duplicate
// "Address already in use" crash.
var portInUse = func() bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", Port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Manager owns starting/stopping/health-checking the single shared omlx server.
// Runner/Probe/Sleep are injectable so it is fully unit-tested with fakes — no
// real process or network is touched in tests.
type Manager struct {
	runner       Runner
	probe        HealthProbe
	now          func() time.Time
	sleep        func(time.Duration)
	log          func(string)
	startTimeout time.Duration
	pollInterval time.Duration
}

// Config configures a Manager; zero-value fields default to the real
// implementations (RealRunner, DefaultProbe, time.Now/time.Sleep, a no-op logger,
// DefaultStartTimeout/DefaultPollInterval).
type Config struct {
	Runner       Runner
	Probe        HealthProbe
	Now          func() time.Time
	Sleep        func(time.Duration)
	Log          func(string)
	StartTimeout time.Duration
	PollInterval time.Duration
}

// NewManager builds a Manager from cfg, defaulting unset fields to the real
// implementations.
func NewManager(cfg Config) *Manager {
	runner := cfg.Runner
	if runner == nil {
		runner = RealRunner{}
	}
	probe := cfg.Probe
	if probe == nil {
		probe = DefaultProbe
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	logf := cfg.Log
	if logf == nil {
		logf = func(string) {}
	}
	startTimeout := cfg.StartTimeout
	if startTimeout == 0 {
		startTimeout = DefaultStartTimeout
	}
	pollInterval := cfg.PollInterval
	if pollInterval == 0 {
		pollInterval = DefaultPollInterval
	}
	return &Manager{
		runner: runner, probe: probe, now: now, sleep: sleep, log: logf,
		startTimeout: startTimeout, pollInterval: pollInterval,
	}
}

// startMu serializes concurrent EnsureServed calls WITHIN this process — a
// same-process guard. It does NOT protect across separate `ai` invocations (`ai`
// is daemonless); lock.go's OS-level flock does that.
var startMu sync.Mutex

// EnsureServed makes sure the omlx server is running and healthy, starting it if
// necessary, and returns its base URL once healthy. If a PRIOR invocation's server
// is already up (adopted) or still coming up (waited for), it is never restarted —
// see lock.go for the cross-process race this guards against.
func (manager *Manager) EnsureServed() (string, error) {
	startMu.Lock()
	defer startMu.Unlock()

	if manager.probe() {
		manager.log("omlx: already running and healthy — adopting it")
		return BaseURL(), nil
	}

	lockFile, err := lockServer()
	if err != nil {
		return "", err
	}
	defer unlockServer(lockFile)

	if manager.probe() {
		return BaseURL(), nil // became healthy while we waited for the lock
	}
	modelDir, err := StoreDir()
	if err != nil {
		return "", err
	}
	if portInUse() {
		manager.log("omlx: port is in use; waiting for the existing server to finish loading")
	} else {
		manager.log("omlx: starting the local-inference server")
		if _, startErr := manager.runner.Start(modelDir, Port); startErr != nil {
			return "", fmt.Errorf("omlx: start server: %w", startErr)
		}
	}
	if err := manager.waitHealthy(); err != nil {
		return "", err
	}
	return BaseURL(), nil
}

// waitHealthy polls the probe until healthy or the configured start timeout elapses.
func (manager *Manager) waitHealthy() error {
	deadline := manager.now().Add(manager.startTimeout)
	for {
		if manager.probe() {
			return nil
		}
		if manager.now().After(deadline) {
			return fmt.Errorf("omlx: server did not become healthy within %s", manager.startTimeout)
		}
		manager.sleep(manager.pollInterval)
	}
}

// Stop stops the running omlx server (best-effort — a not-running server is not an
// error).
func (manager *Manager) Stop() error {
	return manager.runner.Stop()
}
