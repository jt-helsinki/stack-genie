package omlx

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func swapReleaseGet(test *testing.T) {
	test.Helper()
	original := releaseGet
	test.Cleanup(func() { releaseGet = original })
}

func swapPyenv(test *testing.T) {
	test.Helper()
	ensureVersion, pip := pyenvEnsureVersionFn, pyenvPipInstallFn
	test.Cleanup(func() { pyenvEnsureVersionFn, pyenvPipInstallFn = ensureVersion, pip })
}

// resolveWheel picks the NEWEST cp-tagged wheel asset from the latest release.
func TestResolveWheelPicksNewestPythonVersion(test *testing.T) {
	swapReleaseGet(test)
	releaseGet = func(string) ([]byte, error) {
		return []byte(`{"tag_name":"v0.6.4","assets":[
			{"name":"omlx-0.6.4-cp311-cp311-macosx_15_0_universal2.whl","browser_download_url":"https://example/cp311.whl"},
			{"name":"omlx-0.6.4-cp312-cp312-macosx_15_0_universal2.whl","browser_download_url":"https://example/cp312.whl"},
			{"name":"omlx-0.6.4-cp313-cp313-macosx_15_0_universal2.whl","browser_download_url":"https://example/cp313.whl"},
			{"name":"oMLX-0.6.4-macos15-sequoia.dmg","browser_download_url":"https://example/app.dmg"}
		]}`), nil
	}
	wheelURL, pythonVersion, err := resolveWheel()
	if err != nil {
		test.Fatalf("resolveWheel: %v", err)
	}
	if pythonVersion != "3.13" || wheelURL != "https://example/cp313.whl" {
		test.Errorf("resolveWheel = (%q,%q), want (https://example/cp313.whl, 3.13)", wheelURL, pythonVersion)
	}
}

func TestResolveWheelNoMatch(test *testing.T) {
	swapReleaseGet(test)
	releaseGet = func(string) ([]byte, error) {
		return []byte(`{"tag_name":"v0.6.4","assets":[{"name":"oMLX-0.6.4-macos15-sequoia.dmg","browser_download_url":"https://example/app.dmg"}]}`), nil
	}
	if _, _, err := resolveWheel(); err == nil {
		test.Fatal("resolveWheel must error when no .whl asset has a recognizable cp tag")
	}
}

func TestResolveWheelNetworkError(test *testing.T) {
	swapReleaseGet(test)
	releaseGet = func(string) ([]byte, error) { return nil, errors.New("network down") }
	if _, _, err := resolveWheel(); err == nil {
		test.Fatal("resolveWheel must surface the network error")
	}
}

// Install pins the venv to the resolved wheel's Python version then pip-installs it.
func TestInstallPinsVenvAndInstallsWheel(test *testing.T) {
	swapReleaseGet(test)
	swapPyenv(test)
	releaseGet = func(string) ([]byte, error) {
		return []byte(`{"tag_name":"v0.6.4","assets":[{"name":"omlx-0.6.4-cp312-cp312-macosx_15_0_universal2.whl","browser_download_url":"https://example/cp312.whl"}]}`), nil
	}
	var pinned string
	var installed []string
	pyenvEnsureVersionFn = func(version string) (bool, error) { pinned = version; return true, nil }
	pyenvPipInstallFn = func(specs ...string) error { installed = append(installed, specs...); return nil }

	if err := Install(); err != nil {
		test.Fatalf("Install: %v", err)
	}
	if pinned != "3.12" {
		test.Errorf("EnsureVersion pinned %q, want 3.12", pinned)
	}
	if len(installed) != 1 || installed[0] != "https://example/cp312.whl" {
		test.Errorf("installed = %v, want the resolved wheel URL", installed)
	}
}

func TestInstallResolverError(test *testing.T) {
	swapReleaseGet(test)
	swapPyenv(test)
	releaseGet = func(string) ([]byte, error) { return nil, errors.New("network down") }
	pyenvEnsureVersionFn = func(string) (bool, error) {
		test.Fatal("EnsureVersion must not run when the resolver fails")
		return false, nil
	}
	if err := Install(); err == nil {
		test.Fatal("Install must return the resolver error")
	}
}

// fakeTransport returns a canned response regardless of the request URL, so
// ListModels can be tested without binding a real port.
type fakeTransport struct {
	status  int
	body    string
	err     error
	capture *http.Request // set to receive the request that was sent, for header assertions
}

func (transport fakeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.capture != nil {
		*transport.capture = *request
	}
	if transport.err != nil {
		return nil, transport.err
	}
	return &http.Response{
		StatusCode: transport.status,
		Body:       io.NopCloser(strings.NewReader(transport.body)),
		Header:     make(http.Header),
	}, nil
}

// ListModels parses the live GET /v1/models response into LiveModel ids.
func TestListModels(test *testing.T) {
	original := httpClient
	test.Cleanup(func() { httpClient = original })
	httpClient = &http.Client{Transport: fakeTransport{
		status: http.StatusOK,
		body:   `{"data":[{"id":"Qwen3-Coder-Next-8bit"},{"id":"bge-m3"},{"id":""}]}`,
	}}

	models, err := ListModels()
	if err != nil {
		test.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 || models[0].ID != "Qwen3-Coder-Next-8bit" || models[1].ID != "bge-m3" {
		test.Errorf("ListModels = %+v, want [Qwen3-Coder-Next-8bit bge-m3] (blank id dropped)", models)
	}
}

// swapSettingsFile points omlxSettingsFile at a fixture path for the duration of
// the test.
func swapSettingsFile(test *testing.T, path string) {
	test.Helper()
	original := omlxSettingsFile
	omlxSettingsFile = func() string { return path }
	test.Cleanup(func() { omlxSettingsFile = original })
}

// APIKey prefers OMLX_API_KEY over the settings file, matching omlx's own
// GlobalSettings.load precedence.
func TestAPIKeyPrefersEnvOverSettingsFile(test *testing.T) {
	test.Setenv("OMLX_API_KEY", "env-key")
	path := filepath.Join(test.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"auth":{"api_key":"file-key"}}`), 0o600); err != nil {
		test.Fatal(err)
	}
	swapSettingsFile(test, path)
	if key := APIKey(); key != "env-key" {
		test.Errorf("APIKey() = %q, want the env var to win", key)
	}
}

// APIKey falls back to the persisted auth.api_key in omlx's own settings.json.
func TestAPIKeyReadsSettingsFile(test *testing.T) {
	test.Setenv("OMLX_API_KEY", "")
	path := filepath.Join(test.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"auth":{"api_key":"file-key","skip_api_key_verification":false}}`), 0o600); err != nil {
		test.Fatal(err)
	}
	swapSettingsFile(test, path)
	if key := APIKey(); key != "file-key" {
		test.Errorf("APIKey() = %q, want file-key", key)
	}
}

// APIKey returns "" (never an error) when omlx has no key configured — the
// common, unremarkable case (auth is opt-in on omlx's side).
// defaultSettingsFile falls back to BasePathDir (the platform's own managed
// ~/.ai-platform/omlx) when OMLX_BASE_PATH is unset in this process's env —
// matching where RealRunner.Start always points a platform-started server.
func TestDefaultSettingsFileUsesBasePathDir(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	test.Setenv("OMLX_BASE_PATH", "")
	basePath, err := BasePathDir()
	if err != nil {
		test.Fatalf("BasePathDir: %v", err)
	}
	want := filepath.Join(basePath, "settings.json")
	// BasePathDir is only preferred once it actually HAS a settings.json — write
	// one so this test exercises the "server already migrated" case.
	if err := os.WriteFile(want, []byte(`{}`), 0o600); err != nil {
		test.Fatal(err)
	}
	if got := defaultSettingsFile(); got != want {
		test.Errorf("defaultSettingsFile() = %q, want %q", got, want)
	}
}

// defaultSettingsFile falls back to the legacy ~/.omlx when the platform's own
// managed base path has no settings.json yet — a server started BEFORE
// RealRunner.Start began setting OMLX_BASE_PATH (or one started manually, outside
// the platform) still reads/writes omlx's own default ~/.omlx, and a key
// configured there must still be found rather than silently reading empty.
func TestDefaultSettingsFileFallsBackToLegacyHome(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("OMLX_BASE_PATH", "")
	// BasePathDir exists (created on use) but has NO settings.json.
	if _, err := BasePathDir(); err != nil {
		test.Fatalf("BasePathDir: %v", err)
	}
	want := filepath.Join(home, ".omlx", "settings.json")
	if got := defaultSettingsFile(); got != want {
		test.Errorf("defaultSettingsFile() = %q, want the legacy path %q", got, want)
	}
}

// An explicit OMLX_BASE_PATH in this process's env (matching omlx's own
// precedence) wins over the platform's managed base path.
func TestDefaultSettingsFilePrefersExplicitEnv(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	custom := test.TempDir()
	test.Setenv("OMLX_BASE_PATH", custom)
	want := filepath.Join(custom, "settings.json")
	if got := defaultSettingsFile(); got != want {
		test.Errorf("defaultSettingsFile() = %q, want %q", got, want)
	}
}

// BasePathDir and PagedSSDCacheDir resolve under ~/.ai-platform and create
// themselves on use.
func TestBasePathDirAndPagedSSDCacheDir(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	basePath, err := BasePathDir()
	if err != nil {
		test.Fatalf("BasePathDir: %v", err)
	}
	if want := filepath.Join(home, ".ai-platform", "omlx"); basePath != want {
		test.Errorf("BasePathDir() = %q, want %q", basePath, want)
	}
	if info, statErr := os.Stat(basePath); statErr != nil || !info.IsDir() {
		test.Errorf("BasePathDir() should create the directory: %v", statErr)
	}

	cacheDir, err := PagedSSDCacheDir()
	if err != nil {
		test.Fatalf("PagedSSDCacheDir: %v", err)
	}
	if want := filepath.Join(home, ".ai-platform", "cache", "omlx"); cacheDir != want {
		test.Errorf("PagedSSDCacheDir() = %q, want %q", cacheDir, want)
	}
	if info, statErr := os.Stat(cacheDir); statErr != nil || !info.IsDir() {
		test.Errorf("PagedSSDCacheDir() should create the directory: %v", statErr)
	}
}

func TestAPIKeyAbsentWhenUnconfigured(test *testing.T) {
	test.Setenv("OMLX_API_KEY", "")
	swapSettingsFile(test, filepath.Join(test.TempDir(), "does-not-exist.json"))
	if key := APIKey(); key != "" {
		test.Errorf("APIKey() = %q, want empty when unconfigured", key)
	}
}

// ListModels and DefaultProbe attach a Bearer Authorization header when omlx has
// an API key configured, so a key-protected server authenticates instead of 401ing.
func TestListModelsAndProbeSendAuthorizationHeader(test *testing.T) {
	test.Setenv("OMLX_API_KEY", "sekret")
	test.Cleanup(func() { test.Setenv("OMLX_API_KEY", "") })

	var captured http.Request
	originalClient := httpClient
	httpClient = &http.Client{Transport: fakeTransport{
		status: http.StatusOK, body: `{"data":[]}`, capture: &captured,
	}}
	test.Cleanup(func() { httpClient = originalClient })
	if _, err := ListModels(); err != nil {
		test.Fatalf("ListModels: %v", err)
	}
	if got := captured.Header.Get("Authorization"); got != "Bearer sekret" {
		test.Errorf("ListModels Authorization header = %q, want %q", got, "Bearer sekret")
	}

	originalDefault := http.DefaultTransport
	http.DefaultTransport = fakeTransport{status: http.StatusOK, body: `{"data":[]}`, capture: &captured}
	test.Cleanup(func() { http.DefaultTransport = originalDefault })
	captured = http.Request{}
	if !DefaultProbe() {
		test.Fatal("DefaultProbe should report healthy")
	}
	if got := captured.Header.Get("Authorization"); got != "Bearer sekret" {
		test.Errorf("DefaultProbe Authorization header = %q, want %q", got, "Bearer sekret")
	}
}

func TestListModelsUnreachable(test *testing.T) {
	original := httpClient
	test.Cleanup(func() { httpClient = original })
	httpClient = &http.Client{Transport: fakeTransport{err: errors.New("connection refused")}}
	if _, err := ListModels(); err == nil {
		test.Fatal("ListModels must error when the server is unreachable")
	}
}

// EnsureServed starts the server when the port is free and healthy after start.
func TestEnsureServedLazyStart(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	healthy := false
	fake := &FakeRunner{}
	runner := &healthAfterStartRunner{FakeRunner: fake, onStart: func() { healthy = true }}
	manager := NewManager(Config{
		Runner: runner,
		Probe:  func() bool { return healthy },
		Sleep:  func(time.Duration) {},
	})
	originalPortInUse := portInUse
	portInUse = func() bool { return false }
	test.Cleanup(func() { portInUse = originalPortInUse })

	baseURL, err := manager.EnsureServed()
	if err != nil {
		test.Fatalf("EnsureServed: %v", err)
	}
	if baseURL != BaseURL() {
		test.Errorf("EnsureServed = %q, want %q", baseURL, BaseURL())
	}
	if fake.StartCount() != 1 {
		test.Fatalf("StartCount = %d, want 1", fake.StartCount())
	}
}

type healthAfterStartRunner struct {
	*FakeRunner
	onStart func()
}

func (runner *healthAfterStartRunner) Start(modelDir string, port int) (ServerHandle, error) {
	handle, err := runner.FakeRunner.Start(modelDir, port)
	if err == nil {
		runner.onStart()
	}
	return handle, err
}

// EnsureServed adopts an already-healthy server without starting a new one.
func TestEnsureServedAdoptsHealthy(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	runner := &FakeRunner{}
	manager := NewManager(Config{Runner: runner, Probe: func() bool { return true }})

	if _, err := manager.EnsureServed(); err != nil {
		test.Fatalf("EnsureServed: %v", err)
	}
	if runner.StartCount() != 0 {
		test.Errorf("StartCount = %d, want 0 (already healthy — must adopt, not start)", runner.StartCount())
	}
}

// EnsureServed times out (rather than hangs forever) when the server never
// becomes healthy — driven by a fake clock that jumps past StartTimeout on the
// first sleep, so the test completes instantly regardless of the real timeout.
func TestEnsureServedHealthTimeout(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	clock := time.Now()
	runner := &FakeRunner{}
	manager := NewManager(Config{
		Runner:       runner,
		Probe:        func() bool { return false },
		Now:          func() time.Time { return clock },
		Sleep:        func(time.Duration) { clock = clock.Add(time.Hour) },
		StartTimeout: time.Minute,
	})
	originalPortInUse := portInUse
	portInUse = func() bool { return false }
	test.Cleanup(func() { portInUse = originalPortInUse })

	if _, err := manager.EnsureServed(); err == nil {
		test.Fatal("EnsureServed must time out when the probe never reports healthy")
	}
}

// EnsureServed's cross-process lock serializes two Managers (standing in for two
// separate `ai` invocations) racing to start the server for the same port — proves
// only one of them actually spawns a process (see lock.go).
func TestEnsureServedSerializesAcrossManagers(test *testing.T) {
	test.Setenv("HOME", test.TempDir())

	var boundMu sync.Mutex
	bound := false
	probe := func() bool {
		boundMu.Lock()
		defer boundMu.Unlock()
		return bound
	}
	originalPortInUse := portInUse
	portInUse = probe
	test.Cleanup(func() { portInUse = originalPortInUse })

	setBound := func() {
		boundMu.Lock()
		bound = true
		boundMu.Unlock()
	}
	runnerA := &raceRunner{FakeRunner: &FakeRunner{}, bindDelay: 30 * time.Millisecond, onBound: setBound}
	runnerB := &raceRunner{FakeRunner: &FakeRunner{}, bindDelay: 30 * time.Millisecond, onBound: setBound}

	managerA := NewManager(Config{Runner: runnerA, Probe: probe, Sleep: func(time.Duration) {}})
	managerB := NewManager(Config{Runner: runnerB, Probe: probe, Sleep: func(time.Duration) {}})

	var wait sync.WaitGroup
	errs := make([]error, 2)
	wait.Add(2)
	go func() { defer wait.Done(); _, errs[0] = managerA.EnsureServed() }()
	go func() { defer wait.Done(); _, errs[1] = managerB.EnsureServed() }()
	wait.Wait()

	for index, err := range errs {
		if err != nil {
			test.Fatalf("EnsureServed[%d]: %v", index, err)
		}
	}
	total := runnerA.StartCount() + runnerB.StartCount()
	if total != 1 {
		test.Fatalf("total Runner.Start calls = %d, want 1 (the losing manager must adopt, not double-start)", total)
	}
}

type raceRunner struct {
	*FakeRunner
	bindDelay time.Duration
	onBound   func()
}

func (runner *raceRunner) Start(modelDir string, port int) (ServerHandle, error) {
	handle, err := runner.FakeRunner.Start(modelDir, port)
	if err == nil {
		time.Sleep(runner.bindDelay)
		runner.onBound()
	}
	return handle, err
}

func TestDetect(test *testing.T) {
	original := lookPath
	test.Cleanup(func() { lookPath = original })
	lookPath = func(string) (string, error) { return "/usr/local/bin/omlx", nil }
	if !Detect() {
		test.Error("Detect = false, want true")
	}
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if Detect() {
		test.Error("Detect = true, want false")
	}
}
