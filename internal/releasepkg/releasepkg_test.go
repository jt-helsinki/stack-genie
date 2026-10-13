package releasepkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func buildArchive(test *testing.T, entries map[string]string) []byte {
	test.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, content := range entries {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			test.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			test.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		test.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		test.Fatal(err)
	}
	return buffer.Bytes()
}

// serve fakes the three HTTP fetches: the release JSON, the archive, the checksums.
func serve(test *testing.T, archive []byte, sums string, assets []string) {
	test.Helper()
	original := Get
	test.Cleanup(func() { Get = original })
	Get = func(url string) ([]byte, error) {
		switch {
		case strings.HasSuffix(url, "/latest"):
			var parts []string
			for _, name := range assets {
				parts = append(parts, fmt.Sprintf(`{"name":%q,"browser_download_url":"https://dl/%s"}`, name, name))
			}
			return []byte(`{"assets":[` + strings.Join(parts, ",") + `]}`), nil
		case strings.HasSuffix(url, "/tool.tar.gz"):
			return archive, nil
		case strings.HasSuffix(url, "/sums.txt"):
			return []byte(sums), nil
		}
		return nil, fmt.Errorf("unexpected GET %s", url)
	}
}

func source(dir string) Source {
	return Source{
		LatestAPI:     "https://api/latest",
		ArchiveName:   func(string, string) string { return "tool.tar.gz" },
		ChecksumsName: "sums.txt",
		DestDir:       dir,
		Executables:   []string{"tool"},
	}
}

func checksum(archive []byte) string {
	sum := sha256.Sum256(archive)
	return hex.EncodeToString(sum[:]) + "  tool.tar.gz\n"
}

func TestInstallVerifiesAndExtracts(test *testing.T) {
	archive := buildArchive(test, map[string]string{"tool": "#!/bin/sh\n"})
	serve(test, archive, checksum(archive), []string{"tool.tar.gz", "sums.txt"})
	dir := filepath.Join(test.TempDir(), "bin")
	if err := Install(source(dir), "linux", "amd64"); err != nil {
		test.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "tool"))
	if err != nil || info.Mode()&0o111 == 0 {
		test.Fatalf("tool not extracted executable: %v %v", info, err)
	}
}

// Every verification gap must FAIL the install and extract nothing.
func TestInstallFailsClosed(test *testing.T) {
	archive := buildArchive(test, map[string]string{"tool": "x"})
	cases := map[string]struct {
		sums   string
		assets []string
	}{
		"checksum mismatch":     {sums: strings.Repeat("0", 64) + "  tool.tar.gz\n", assets: []string{"tool.tar.gz", "sums.txt"}},
		"no entry for archive":  {sums: strings.Repeat("0", 64) + "  other.tar.gz\n", assets: []string{"tool.tar.gz", "sums.txt"}},
		"no checksums asset":    {sums: checksum(archive), assets: []string{"tool.tar.gz"}},
		"no archive for the OS": {sums: checksum(archive), assets: []string{"sums.txt"}},
	}
	for name, testCase := range cases {
		test.Run(name, func(test *testing.T) {
			serve(test, archive, testCase.sums, testCase.assets)
			dir := filepath.Join(test.TempDir(), "bin")
			if err := Install(source(dir), "linux", "amd64"); err == nil {
				test.Fatal("expected a verification failure")
			}
			if _, err := os.Stat(filepath.Join(dir, "tool")); err == nil {
				test.Fatal("nothing may be extracted when verification fails")
			}
		})
	}
}

func TestInstallRejectsPathTraversal(test *testing.T) {
	archive := buildArchive(test, map[string]string{"../escape": "x"})
	serve(test, archive, checksum(archive), []string{"tool.tar.gz", "sums.txt"})
	parent := test.TempDir()
	if err := Install(source(filepath.Join(parent, "bin")), "linux", "amd64"); err == nil {
		test.Fatal("expected a traversal rejection")
	}
	if _, err := os.Stat(filepath.Join(parent, "escape")); err == nil {
		test.Fatal("traversal entry was written outside the destination")
	}
}

func TestInstallUnsupportedPlatform(test *testing.T) {
	src := source(test.TempDir())
	src.ArchiveName = func(string, string) string { return "" }
	if err := Install(src, "plan9", "mips"); err == nil {
		test.Fatal("expected an unsupported-platform error")
	}
}
