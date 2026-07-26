package workspace

import (
	"os"
	"regexp"
	"testing"
)

// TestMicrosandboxVersionMatchesSDK guards the CORE invariant behind the platform-managed
// msb: the pinned CLI version MUST equal the embedded SDK FFI version (the sdk/go require
// in go.mod). They share ~/.microsandbox/db; a mismatch reintroduces the "database schema
// is newer than this msb binary" regression on `msb load`. Bumping the SDK without bumping
// (and re-checksumming) microsandboxVersion fails here.
func TestMicrosandboxVersionMatchesSDK(test *testing.T) {
	data, err := os.ReadFile("../../go.mod")
	if err != nil {
		test.Fatalf("read go.mod: %v", err)
	}
	matches := regexp.MustCompile(`github\.com/superradcompany/microsandbox/sdk/go\s+(v[0-9]+\.[0-9]+\.[0-9]+)`).FindStringSubmatch(string(data))
	if matches == nil {
		test.Fatal("could not find the superradcompany/microsandbox/sdk/go require in go.mod")
	}
	if matches[1] != microsandboxVersion {
		test.Errorf("microsandboxVersion = %q, but go.mod pins sdk/go %q — bump microsandboxVersion (and its per-arch checksums) to match", microsandboxVersion, matches[1])
	}
}

// TestMsbAssetsCoverSupportedHosts asserts the pinned asset map covers exactly the
// platform's supported hosts, each with a name and a 64-hex sha256 (so no host silently
// falls back to an unverified/PATH binary).
func TestMsbAssetsCoverSupportedHosts(test *testing.T) {
	wantHosts := []string{"darwin/arm64", "linux/arm64", "linux/amd64"}
	if len(msbAssets) != len(wantHosts) {
		test.Errorf("msbAssets has %d entries, want %d (%v)", len(msbAssets), len(wantHosts), wantHosts)
	}
	hexRe := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, host := range wantHosts {
		asset, ok := msbAssets[host]
		if !ok {
			test.Errorf("msbAssets missing supported host %q", host)
			continue
		}
		if asset.Name == "" {
			test.Errorf("%s: empty asset name", host)
		}
		if !hexRe.MatchString(asset.SHA256) {
			test.Errorf("%s: sha256 %q is not 64 hex chars", host, asset.SHA256)
		}
	}
}
