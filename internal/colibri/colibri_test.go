package colibri

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func swapReleaseGet(test *testing.T) {
	test.Helper()
	original := releaseGet
	test.Cleanup(func() { releaseGet = original })
}

func swapArchiveFetch(test *testing.T) {
	test.Helper()
	original := archiveFetch
	test.Cleanup(func() { archiveFetch = original })
}

func swapChecksumFetch(test *testing.T) {
	test.Helper()
	original := checksumFetch
	test.Cleanup(func() { checksumFetch = original })
}

func TestArchiveSuffixFor(test *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"darwin", "arm64", "macos-arm64"},
		{"linux", "amd64", "linux-x86_64"},
		{"darwin", "amd64", ""},  // Intel Mac: colibri publishes no such asset
		{"windows", "amd64", ""}, // not a target OS for this platform
		{"linux", "arm64", ""},   // no published asset for this combination
	}
	for _, testCase := range cases {
		if got := archiveSuffixFor(testCase.goos, testCase.goarch); got != testCase.want {
			test.Errorf("archiveSuffixFor(%q,%q) = %q, want %q", testCase.goos, testCase.goarch, got, testCase.want)
		}
	}
}

// resolveAsset picks the release asset matching this host's platform/arch suffix,
// verified against the REAL v2.0.0 release asset names.
func TestResolveAssetPicksMatchingPlatform(test *testing.T) {
	swapReleaseGet(test)
	releaseGet = func(string) ([]byte, error) {
		return []byte(`{"tag_name":"v2.0.0","assets":[
			{"name":"colibri-v2.0.0-linux-x86_64.tar.gz","browser_download_url":"https://example/linux.tar.gz"},
			{"name":"colibri-v2.0.0-macos-arm64.tar.gz","browser_download_url":"https://example/macos.tar.gz"},
			{"name":"colibri-v2.0.0-windows-x86_64.zip","browser_download_url":"https://example/windows.zip"},
			{"name":"SHA256SUMS.txt","browser_download_url":"https://example/sums.txt"}
		]}`), nil
	}
	asset, err := resolveAsset("darwin", "arm64")
	if err != nil {
		test.Fatalf("resolveAsset: %v", err)
	}
	if asset.DownloadURL != "https://example/macos.tar.gz" {
		test.Errorf("resolveAsset(darwin,arm64).DownloadURL = %q, want the macos asset", asset.DownloadURL)
	}
	if asset.Filename != "colibri-v2.0.0-macos-arm64.tar.gz" {
		test.Errorf("resolveAsset(darwin,arm64).Filename = %q, want the macos asset's bare name", asset.Filename)
	}
	if asset.ChecksumsURL != "https://example/sums.txt" {
		test.Errorf("resolveAsset(darwin,arm64).ChecksumsURL = %q, want the release's SHA256SUMS.txt URL", asset.ChecksumsURL)
	}
}

func TestResolveAssetUnsupportedPlatform(test *testing.T) {
	if _, err := resolveAsset("windows", "amd64"); err == nil {
		test.Fatal("resolveAsset must error for an unsupported platform without ever calling releaseGet")
	}
}

func TestResolveAssetNoMatch(test *testing.T) {
	swapReleaseGet(test)
	releaseGet = func(string) ([]byte, error) {
		return []byte(`{"tag_name":"v2.0.0","assets":[{"name":"colibri-v2.0.0-linux-x86_64.tar.gz","browser_download_url":"https://example/linux.tar.gz"}]}`), nil
	}
	if _, err := resolveAsset("darwin", "arm64"); err == nil {
		test.Fatal("resolveAsset must error when no asset matches the platform suffix")
	}
}

func TestResolveAssetNetworkError(test *testing.T) {
	swapReleaseGet(test)
	releaseGet = func(string) ([]byte, error) { return nil, errors.New("network down") }
	if _, err := resolveAsset("darwin", "arm64"); err == nil {
		test.Fatal("resolveAsset must surface the network error")
	}
}

// buildTestTarGz builds an in-memory .tar.gz with one directory and one
// executable file, for extractTarGz round-trip testing.
func buildTestTarGz(test *testing.T) []byte {
	test.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	content := []byte("#!/usr/bin/env python3\nprint('hi')\n")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "coli", Mode: 0o755, Size: int64(len(content))}); err != nil {
		test.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		test.Fatalf("Write: %v", err)
	}
	if err := tarWriter.WriteHeader(&tar.Header{Name: "web/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		test.Fatalf("WriteHeader dir: %v", err)
	}
	if err := tarWriter.Close(); err != nil {
		test.Fatalf("tar Close: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		test.Fatalf("gzip Close: %v", err)
	}
	return buffer.Bytes()
}

func TestExtractTarGz(test *testing.T) {
	dir := test.TempDir()
	if err := extractTarGz(buildTestTarGz(test), dir); err != nil {
		test.Fatalf("extractTarGz: %v", err)
	}
	coliPath := filepath.Join(dir, "coli")
	info, err := os.Stat(coliPath)
	if err != nil {
		test.Fatalf("stat coli: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		test.Errorf("coli must be executable, got mode %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(dir, "web")); err != nil {
		test.Errorf("web/ directory must be extracted: %v", err)
	}
}

func TestExtractTarGzRejectsPathTraversal(test *testing.T) {
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	_ = tarWriter.WriteHeader(&tar.Header{Name: "../../evil", Mode: 0o644, Size: 0})
	_ = tarWriter.Close()
	_ = gzipWriter.Close()

	if err := extractTarGz(buffer.Bytes(), test.TempDir()); err == nil {
		test.Fatal("extractTarGz must reject an entry that escapes the target directory")
	}
}

// Install resolves the asset, downloads it, and extracts it into StoreDir.
// TestVerifyChecksumMatches/Mismatch/NoEntry/FetchError are a regression suite
// for a real supply-chain finding: Install() downloaded and extracted colibri's
// release archive with ZERO integrity verification, even though colibri's own
// release publishes a SHA256SUMS.txt and its own `coli setup` verifies against
// it by default. verifyChecksum closes that gap.
func TestVerifyChecksumMatches(test *testing.T) {
	swapChecksumFetch(test)
	archiveBytes := []byte("pretend archive contents")
	sum := sha256.Sum256(archiveBytes)
	checksumFetch = func(string) ([]byte, error) {
		return []byte(hex.EncodeToString(sum[:]) + "  colibri-v2.0.0-macos-arm64.tar.gz\n" +
			"deadbeef  colibri-v2.0.0-linux-x86_64.tar.gz\n"), nil
	}
	if err := verifyChecksum(archiveBytes, "colibri-v2.0.0-macos-arm64.tar.gz", "https://example/sums.txt"); err != nil {
		test.Fatalf("verifyChecksum must succeed on a matching digest: %v", err)
	}
}

func TestVerifyChecksumMismatch(test *testing.T) {
	swapChecksumFetch(test)
	checksumFetch = func(string) ([]byte, error) {
		return []byte("0000000000000000000000000000000000000000000000000000000000000000  colibri-v2.0.0-macos-arm64.tar.gz\n"), nil
	}
	if err := verifyChecksum([]byte("tampered or corrupted bytes"), "colibri-v2.0.0-macos-arm64.tar.gz", "https://example/sums.txt"); err == nil {
		test.Fatal("verifyChecksum must fail on a digest mismatch (possible corrupted or tampered download)")
	}
}

func TestVerifyChecksumNoEntryForFile(test *testing.T) {
	swapChecksumFetch(test)
	checksumFetch = func(string) ([]byte, error) {
		return []byte("deadbeef  colibri-v2.0.0-linux-x86_64.tar.gz\n"), nil
	}
	if err := verifyChecksum([]byte("anything"), "colibri-v2.0.0-macos-arm64.tar.gz", "https://example/sums.txt"); err == nil {
		test.Fatal("verifyChecksum must fail CLOSED when SHA256SUMS.txt has no entry for this file, not silently skip")
	}
}

func TestVerifyChecksumFetchError(test *testing.T) {
	swapChecksumFetch(test)
	checksumFetch = func(string) ([]byte, error) { return nil, errors.New("network down") }
	if err := verifyChecksum([]byte("anything"), "colibri-v2.0.0-macos-arm64.tar.gz", "https://example/sums.txt"); err == nil {
		test.Fatal("verifyChecksum must surface a SHA256SUMS.txt fetch error, not silently skip")
	}
}

// TestInstallFailsOnChecksumMismatch pins the end-to-end wiring: when the
// release publishes a SHA256SUMS.txt and the downloaded bytes don't match it,
// Install must fail WITHOUT ever extracting the archive onto disk.
func TestInstallFailsOnChecksumMismatch(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	swapReleaseGet(test)
	swapArchiveFetch(test)
	swapChecksumFetch(test)
	suffix := archiveSuffixFor(runtime.GOOS, runtime.GOARCH)
	if suffix == "" {
		test.Skip("unsupported platform for this test environment")
	}
	filename := "colibri-v2.0.0-" + suffix + ".tar.gz"
	releaseGet = func(string) ([]byte, error) {
		return []byte(`{"tag_name":"v2.0.0","assets":[
			{"name":"` + filename + `","browser_download_url":"https://example/archive.tar.gz"},
			{"name":"SHA256SUMS.txt","browser_download_url":"https://example/sums.txt"}
		]}`), nil
	}
	archiveFetch = func(string) ([]byte, error) { return buildTestTarGz(test), nil }
	checksumFetch = func(string) ([]byte, error) {
		return []byte("0000000000000000000000000000000000000000000000000000000000000000  " + filename + "\n"), nil
	}

	if err := Install(); err == nil {
		test.Fatal("Install must fail when the downloaded archive does not match the published checksum")
	}
	storeDir, err := StoreDir()
	if err != nil {
		test.Fatalf("StoreDir: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(storeDir, "coli")); statErr == nil {
		test.Error("Install must NOT extract the archive when checksum verification fails")
	}
}

func TestInstallDownloadsAndExtracts(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	swapReleaseGet(test)
	swapArchiveFetch(test)
	// Install() itself calls runtime.GOOS/GOARCH (not injectable — it must reflect
	// the REAL host), so this test matches against whatever platform it actually
	// runs on, skipping on one colibri has no published asset for (e.g. CI on an
	// unsupported arch) rather than asserting a false negative.
	suffix := archiveSuffixFor(runtime.GOOS, runtime.GOARCH)
	if suffix == "" {
		test.Skip("unsupported platform for this test environment")
	}
	releaseGet = func(string) ([]byte, error) {
		return []byte(`{"tag_name":"v2.0.0","assets":[{"name":"colibri-v2.0.0-` + suffix + `.tar.gz","browser_download_url":"https://example/archive.tar.gz"}]}`), nil
	}
	archiveFetch = func(string) ([]byte, error) { return buildTestTarGz(test), nil }

	if err := Install(); err != nil {
		test.Fatalf("Install: %v", err)
	}
	storeDir, err := StoreDir()
	if err != nil {
		test.Fatalf("StoreDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(storeDir, "coli")); err != nil {
		test.Errorf("Install must leave coli in StoreDir: %v", err)
	}
}

func TestDetect(test *testing.T) {
	original := lookPath
	test.Cleanup(func() { lookPath = original })
	lookPath = func(string) (string, error) { return "/usr/local/bin/coli", nil }
	if !Detect() {
		test.Error("Detect = false, want true")
	}
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if Detect() {
		test.Error("Detect = true, want false")
	}
}

func TestPythonAvailable(test *testing.T) {
	original := pythonLookPath
	test.Cleanup(func() { pythonLookPath = original })
	pythonLookPath = func(string) (string, error) { return "/usr/bin/python3", nil }
	if !PythonAvailable() {
		test.Error("PythonAvailable = false, want true")
	}
	pythonLookPath = func(string) (string, error) { return "", errors.New("not found") }
	if PythonAvailable() {
		test.Error("PythonAvailable = true, want false")
	}
}

// APIKey is env-only (colibri's own key requirement is opt-in; this platform's
// policy of always setting one lives in internal/setup, not here).
func TestAPIKeyReadsEnvOnly(test *testing.T) {
	test.Setenv("COLI_API_KEY", "")
	_ = os.Unsetenv("COLI_API_KEY")
	if got := APIKey(); got != "" {
		test.Errorf("APIKey() = %q, want empty with no env set", got)
	}
	test.Setenv("COLI_API_KEY", "coli-test-key")
	if got := APIKey(); got != "coli-test-key" {
		test.Errorf("APIKey() = %q, want %q", got, "coli-test-key")
	}
}

func TestStatusBestEffort(test *testing.T) {
	origLookPath, origStatus := lookPath, statusCommand
	test.Cleanup(func() { lookPath, statusCommand = origLookPath, origStatus })
	lookPath = func(string) (string, error) { return "/usr/local/bin/coli", nil }

	statusCommand = func(string) ([]byte, error) { return []byte(`{"configured": true}`), nil }
	if !Status().Configured {
		test.Error("Status().Configured = false, want true")
	}
	statusCommand = func(string) ([]byte, error) { return []byte(`{"configured": false}`), nil }
	if Status().Configured {
		test.Error("Status().Configured = true, want false")
	}
	statusCommand = func(string) ([]byte, error) { return nil, errors.New("coli not found") }
	if Status().Configured {
		test.Error("Status().Configured must be false (not panic/error) when the command fails")
	}
	statusCommand = func(string) ([]byte, error) { return []byte("not json"), nil }
	if Status().Configured {
		test.Error("Status().Configured must be false (not panic/error) on malformed output")
	}
}
