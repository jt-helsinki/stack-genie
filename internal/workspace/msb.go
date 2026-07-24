package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/paths"
)

// microsandboxVersion pins the Microsandbox `msb` CLI to the SAME release as the
// embedded SDK FFI (the github.com/superradcompany/microsandbox/sdk/go pin in go.mod).
// WHY THIS EXISTS: the CLI (used by the Builder for `msb load`) and the FFI (used for
// the microVM lifecycle) share ~/.microsandbox/db. If their DB-migration sets diverge,
// the FFI migrates the DB to a schema the CLI cannot read and `msb load` fails with
// "database schema is newer than this msb binary". That is exactly what happens when the
// user installs `msb` from a DIFFERENT source than the SDK (e.g. upstream
// install.microsandbox.dev vs the superradcompany fork the SDK is built from) — same
// version number, different migrations. To keep them in lockstep the platform DOWNLOADS
// the matching fork CLI itself (sha256-verified) and NEVER uses the user's PATH `msb`.
//
// MUST match the go.mod sdk/go pin — asserted by TestMicrosandboxVersionMatchesSDK, so
// bumping the SDK forces bumping (and re-checksumming) this.
const microsandboxVersion = "v0.6.6"

// microsandboxRepo is the fork the SDK + FFI come from; its releases publish the matching
// msb-<os>-<arch> CLI binaries.
const microsandboxRepo = "superradcompany/microsandbox"

// msbAsset is a pinned release asset (name + sha256) for microsandboxVersion.
type msbAsset struct {
	Name   string
	SHA256 string
}

// msbAssets maps GOOS/GOARCH to the pinned release asset for microsandboxVersion. Only the
// platform's supported hosts are listed (macOS Apple Silicon, Linux arm64/amd64); an
// unlisted host errors rather than fetch an unverified binary. Update the checksums when
// bumping microsandboxVersion (from the release's msb-* assets).
var msbAssets = map[string]msbAsset{
	"darwin/arm64": {"msb-darwin-aarch64", "3cf88c09b96cd5331938771dd33854d6f2c98c91c93aa14d5a45c213960cfb58"},
	"linux/arm64":  {"msb-linux-aarch64", "76ee23899f1e50d504d5af9500a96e471b75ec9680cb816b86514fff1d3ef57f"},
	"linux/amd64":  {"msb-linux-x86_64", "75d72e02b758229ee95f7f9d4e8893f0410c53ee379fdf6e076e49fc8080b975"},
}

var (
	msbOnce sync.Once
	msbPath string
	msbErr  error
)

// msbBinaryFn resolves the msb CLI path for the lifecycle call sites. It defaults to the
// real (downloading, sha256-verifying) MsbBinary but is a var so unit tests can stub it
// without touching the network or the host filesystem.
var msbBinaryFn = MsbBinary

// MsbBinary returns the path to the platform-managed `msb` CLI, installing it into
// ~/.ai-platform/bin/msb (pinned version, sha256-verified) on first use if it is absent
// or the wrong build. The result is cached for the process and is used for every `msb`
// invocation so the CLI always matches the embedded SDK FFI (no version skew). A download
// failure is returned so the caller can surface it.
func MsbBinary() (string, error) {
	msbOnce.Do(func() { msbPath, msbErr = ensureMsb() })
	return msbPath, msbErr
}

// msbBinOrDefault returns the managed msb path, falling back to bare "msb" (PATH) when the
// managed one cannot be resolved — for best-effort/non-critical call sites (logs, the CLI
// backend's exec) that should still attempt rather than hard-fail. The critical
// image-load path calls MsbBinary directly so it fails loudly on skew.
func msbBinOrDefault() string {
	if path, err := msbBinaryFn(); err == nil {
		return path
	}
	return "msb"
}

func ensureMsb() (string, error) {
	key := runtime.GOOS + "/" + runtime.GOARCH
	asset, ok := msbAssets[key]
	if !ok {
		return "", fmt.Errorf("no pinned msb binary for %s (supported: darwin/arm64, linux/arm64, linux/amd64)", key)
	}
	binDir, err := paths.BinDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	target := filepath.Join(binDir, "msb")
	// Already the pinned binary? Verify by content hash (a version string can't tell the
	// fork build from the upstream one — both report the same number).
	if sum, err := fileSHA256(target); err == nil && sum == asset.SHA256 {
		return target, nil
	}
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", microsandboxRepo, microsandboxVersion, asset.Name)
	if err := downloadVerified(url, asset.SHA256, target); err != nil {
		return "", fmt.Errorf("install pinned msb %s: %w", microsandboxVersion, err)
	}
	return target, nil
}

// fileSHA256 returns the hex sha256 of a file (error if it does not exist).
func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// downloadVerified fetches url, verifies its sha256 == wantSHA, and installs it atomically
// at target with 0755. A checksum mismatch (or non-200) aborts without touching target.
func downloadVerified(url, wantSHA, target string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, response.StatusCode)
	}
	tempFile, err := os.CreateTemp(filepath.Dir(target), ".msb-download-*")
	if err != nil {
		return err
	}
	tempName := tempFile.Name()
	defer func() { _ = os.Remove(tempName) }()
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tempFile, hasher), response.Body); err != nil {
		_ = tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != wantSHA {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", url, got, wantSHA)
	}
	if err := os.Chmod(tempName, 0o755); err != nil {
		return err
	}
	return os.Rename(tempName, target)
}
