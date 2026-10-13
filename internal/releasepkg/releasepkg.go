// Package releasepkg installs a host-native CLI from a GitHub release archive,
// FAIL-CLOSED: the archive is extracted only after its SHA-256 matches the entry in
// the same release's published checksums file. A missing/unfetchable checksums file
// or a missing entry is a verification failure, never a skip — those are exactly the
// states where something is already wrong, and the extracted binary is later
// executed on the host.
//
// internal/colibri carries its own older copy of this logic (with a degrade-to-skip
// when no checksums file is published); folding it into this package is a follow-up.
package releasepkg

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Source describes one release to install.
type Source struct {
	// LatestAPI is the GitHub releases/latest API URL.
	LatestAPI string
	// ArchiveName returns the exact release-asset filename for a platform, or ""
	// when the platform is unsupported.
	ArchiveName func(goos, goarch string) string
	// ChecksumsName is the release asset holding "<sha256>  <filename>" lines.
	ChecksumsName string
	// DestDir is where the archive is extracted (created if absent).
	DestDir string
	// Executables are archive-relative paths re-chmod'd 0755 after extraction.
	Executables []string
}

// Get is the injectable HTTP seam (tests replace it so no network is touched).
var Get = func(url string) ([]byte, error) {
	response, err := http.Get(url) //nolint:gosec // fixed GitHub release hosts, caller-supplied constants
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %s", url, response.Status)
	}
	return io.ReadAll(response.Body)
}

type release struct {
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Install downloads, verifies, and extracts the latest release for goos/goarch.
func Install(source Source, goos, goarch string) error {
	archiveName := source.ArchiveName(goos, goarch)
	if archiveName == "" {
		return fmt.Errorf("no published release for %s/%s", goos, goarch)
	}
	body, err := Get(source.LatestAPI)
	if err != nil {
		return fmt.Errorf("resolve latest release: %w", err)
	}
	var latest release
	if err := json.Unmarshal(body, &latest); err != nil {
		return fmt.Errorf("parse release JSON: %w", err)
	}
	var archiveURL, checksumsURL string
	for _, asset := range latest.Assets {
		switch asset.Name {
		case archiveName:
			archiveURL = asset.URL
		case source.ChecksumsName:
			checksumsURL = asset.URL
		}
	}
	if archiveURL == "" {
		return fmt.Errorf("latest release has no asset %q", archiveName)
	}
	if checksumsURL == "" {
		return fmt.Errorf("latest release publishes no %q — refusing to install an unverifiable archive", source.ChecksumsName)
	}
	archive, err := Get(archiveURL)
	if err != nil {
		return fmt.Errorf("download %s: %w", archiveName, err)
	}
	sums, err := Get(checksumsURL)
	if err != nil {
		return fmt.Errorf("download %s: %w", source.ChecksumsName, err)
	}
	if err := verify(archive, archiveName, sums); err != nil {
		return err
	}
	if err := extractTarGz(archive, source.DestDir); err != nil {
		return fmt.Errorf("extract %s into %s: %w", archiveName, source.DestDir, err)
	}
	for _, name := range source.Executables {
		_ = os.Chmod(filepath.Join(source.DestDir, name), 0o755) //nolint:gosec // intentionally executable
	}
	return nil
}

func verify(archive []byte, filename string, sums []byte) error {
	var expected string
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == filename {
			expected = fields[0]
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("checksums file has no entry for %s", filename)
	}
	sum := sha256.Sum256(archive)
	if actual := hex.EncodeToString(sum[:]); !strings.EqualFold(actual, expected) {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s (possible corrupted or tampered download)", filename, actual, expected)
	}
	return nil
}

// extractTarGz extracts into dir, preserving mode bits and rejecting any entry whose
// name resolves outside dir (path traversal).
func extractTarGz(archive []byte, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	gzipReader, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return err
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(gzipReader)
	root := filepath.Clean(dir)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(root, header.Name) //nolint:gosec // validated below
		if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
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
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode)&0o777) //nolint:gosec // mode clamped to permission bits
			if err != nil {
				return err
			}
			if _, err := io.Copy(file, tarReader); err != nil { //nolint:gosec // bounded by the release asset size
				_ = file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
		}
	}
}
