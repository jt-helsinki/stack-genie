package vllm

import (
	"errors"
	"testing"
)

func TestPythonVersionFromWheel(test *testing.T) {
	cases := []struct {
		name   string
		want   string
		wantOK bool
	}{
		{"vllm_metal-0.3.0.dev1-cp312-cp312-macosx_11_0_arm64.whl", "3.12", true},
		{"vllm_metal-0.4.0-cp313-cp313-macosx_11_0_arm64.whl", "3.13", true},
		{"vllm_metal-cp39-cp39-macosx_11_0_arm64.whl", "3.9", true},
		{"vllm_metal-nowheeltag.tar.gz", "", false},
	}
	for _, testCase := range cases {
		got, ok := pythonVersionFromWheel(testCase.name)
		if ok != testCase.wantOK || got != testCase.want {
			test.Errorf("pythonVersionFromWheel(%q) = (%q,%v), want (%q,%v)",
				testCase.name, got, ok, testCase.want, testCase.wantOK)
		}
	}
}

// resolveMetalWheel picks the first `.whl` asset and derives its Python version.
func TestResolveMetalWheelPicksWheel(test *testing.T) {
	swapMetalGet(test)
	metalReleaseGet = func(string) ([]byte, error) {
		return []byte(`{"assets":[
			{"name":"source.tar.gz","browser_download_url":"https://example/src.tgz"},
			{"name":"vllm_metal-0.3.0.dev1-cp313-cp313-macosx_11_0_arm64.whl","browser_download_url":"https://example/metal.whl"}
		]}`), nil
	}
	release, err := resolveMetalWheel()
	if err != nil {
		test.Fatalf("resolveMetalWheel: %v", err)
	}
	if release.PythonVersion != "3.13" {
		test.Errorf("PythonVersion = %q, want 3.13", release.PythonVersion)
	}
	if release.MetalWheelURL != "https://example/metal.whl" {
		test.Errorf("MetalWheelURL = %q, want the metal wheel URL", release.MetalWheelURL)
	}
}

// A cp312 wheel resolves to Python 3.12.
func TestResolveMetalWheelCP312(test *testing.T) {
	swapMetalGet(test)
	metalReleaseGet = func(string) ([]byte, error) {
		return []byte(`{"assets":[{"name":"vllm_metal-cp312-cp312-macosx_11_0_arm64.whl","browser_download_url":"https://example/m.whl"}]}`), nil
	}
	release, err := resolveMetalWheel()
	if err != nil {
		test.Fatalf("resolveMetalWheel: %v", err)
	}
	if release.PythonVersion != "3.12" {
		test.Errorf("PythonVersion = %q, want 3.12", release.PythonVersion)
	}
}

// An HTTP-seam error is surfaced as an error.
func TestResolveMetalWheelError(test *testing.T) {
	swapMetalGet(test)
	metalReleaseGet = func(string) ([]byte, error) { return nil, errors.New("boom") }
	if _, err := resolveMetalWheel(); err == nil {
		test.Fatal("resolveMetalWheel must error when the HTTP seam fails")
	}
}

// A release with no wheel asset is an error.
func TestResolveMetalWheelNoWheel(test *testing.T) {
	swapMetalGet(test)
	metalReleaseGet = func(string) ([]byte, error) {
		return []byte(`{"assets":[{"name":"notes.txt","browser_download_url":"https://example/n.txt"}]}`), nil
	}
	if _, err := resolveMetalWheel(); err == nil {
		test.Fatal("resolveMetalWheel must error with no .whl asset")
	}
}

// resolveCoreWheel returns the confirmed pin for 3.12 without touching the network.
func TestResolveCoreWheelCP312Pin(test *testing.T) {
	swapMetalGet(test)
	metalReleaseGet = func(string) ([]byte, error) {
		test.Fatal("the cp312 core wheel is a static pin — no network")
		return nil, nil
	}
	url, err := resolveCoreWheel("3.12")
	if err != nil {
		test.Fatalf("resolveCoreWheel(3.12): %v", err)
	}
	if url != coreWheelCP312 {
		test.Errorf("resolveCoreWheel(3.12) = %q, want the pinned core wheel", url)
	}
}

// resolveCoreWheel best-effort scans vLLM releases for a matching macOS arm64 wheel.
func TestResolveCoreWheelOtherVersion(test *testing.T) {
	swapMetalGet(test)
	metalReleaseGet = func(string) ([]byte, error) {
		return []byte(`[{"assets":[
			{"name":"vllm-0.27.0+cpu-cp313-cp313-manylinux1_x86_64.whl","browser_download_url":"https://example/linux.whl"},
			{"name":"vllm-0.27.0+cpu-cp313-cp313-macosx_11_0_arm64.whl","browser_download_url":"https://example/mac313.whl"}
		]}]`), nil
	}
	url, err := resolveCoreWheel("3.13")
	if err != nil {
		test.Fatalf("resolveCoreWheel(3.13): %v", err)
	}
	if url != "https://example/mac313.whl" {
		test.Errorf("resolveCoreWheel(3.13) = %q, want the macOS arm64 wheel", url)
	}
}

// resolveCoreWheel errors when no matching macOS arm64 wheel exists.
func TestResolveCoreWheelNoMatch(test *testing.T) {
	swapMetalGet(test)
	metalReleaseGet = func(string) ([]byte, error) {
		return []byte(`[{"assets":[{"name":"vllm-0.27.0-cp313-cp313-manylinux1_x86_64.whl","browser_download_url":"https://example/linux.whl"}]}]`), nil
	}
	if _, err := resolveCoreWheel("3.13"); err == nil {
		test.Fatal("resolveCoreWheel must error when no macOS arm64 wheel matches")
	}
}
