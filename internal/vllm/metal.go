package vllm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// metal.go resolves, at RUNTIME, the Apple-Silicon MLX/Metal vLLM install: the
// community vllm-metal plugin wheel (which overrides vLLM's device to Metal/MLX) plus
// the matching vLLM CORE macOS wheel it layers on top. Both are GitHub release assets —
// PyPI ships no macOS vLLM wheel — and the metal release is a fast-moving dev-tagged
// snapshot, so its URL + cp tag (which dictates the venv's Python version) MUST be
// resolved live, never hardcoded. All network access goes through the injectable
// metalReleaseGet seam so this is unit-tested with fakes and is `hardware bring-up`
// (best-effort; a resolver failure is swallowed by `ai setup`).

// metalLatestReleaseAPI is the GitHub API endpoint for the newest vllm-metal release.
const metalLatestReleaseAPI = "https://api.github.com/repos/vllm-project/vllm-metal/releases/latest"

// vllmReleasesAPI lists vLLM CORE releases, queried only when the metal wheel's cp tag
// is something other than the confirmed cp312 pair (see resolveCoreWheel).
const vllmReleasesAPI = "https://api.github.com/repos/vllm-project/vllm/releases"

// coreWheelCP312 is the CONFIRMED-good vLLM CORE macOS (arm64) wheel that pairs with a
// cp312 metal wheel — upstream's install.sh installs this core wheel FIRST, then the
// metal wheel on top. The `+` in the local version is URL-encoded (`%2B`) so pip
// fetches the asset verbatim. Other cp tags are resolved best-effort at runtime.
const coreWheelCP312 = "https://github.com/vllm-project/vllm/releases/download/v0.26.0/vllm-0.26.0%2Bcpu-cp312-cp312-macosx_11_0_arm64.whl"

// metalRelease is the resolved vllm-metal install: the metal wheel URL and the CPython
// "major.minor" its cp tag implies (the platform venv MUST be created at that version).
type metalRelease struct {
	MetalWheelURL string
	PythonVersion string
}

// metalReleaseGet is the injectable HTTP seam for querying the GitHub releases API.
// Tests replace it so no network is touched.
var metalReleaseGet = func(url string) ([]byte, error) {
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

// ghAsset / ghRelease decode only the GitHub release fields the resolver needs.
type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type ghRelease struct {
	Assets []ghAsset `json:"assets"`
}

// cpTagPattern matches a cpython wheel tag (cp312, cp313, cp39, …).
var cpTagPattern = regexp.MustCompile(`cp3(\d+)`)

// pythonVersionFromWheel maps a wheel filename's cp tag to a "major.minor" version:
// cp312 -> "3.12", cp313 -> "3.13", cp39 -> "3.9". Reports false when no cp tag is present.
func pythonVersionFromWheel(name string) (string, bool) {
	match := cpTagPattern.FindStringSubmatch(name)
	if match == nil {
		return "", false
	}
	return "3." + match[1], true
}

// resolveMetalWheel queries the latest vllm-metal release and returns the first `.whl`
// asset's download URL plus the Python version its cp tag implies. Any failure (network,
// malformed JSON, no wheel asset, unrecognizable cp tag) is returned as an error for the
// caller to treat best-effort.
func resolveMetalWheel() (metalRelease, error) {
	body, err := metalReleaseGet(metalLatestReleaseAPI)
	if err != nil {
		return metalRelease{}, fmt.Errorf("resolve vllm-metal latest release: %w", err)
	}
	var release ghRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return metalRelease{}, fmt.Errorf("parse vllm-metal release JSON: %w", err)
	}
	for _, asset := range release.Assets {
		if !strings.HasSuffix(asset.Name, ".whl") {
			continue
		}
		version, ok := pythonVersionFromWheel(asset.Name)
		if !ok {
			return metalRelease{}, fmt.Errorf("vllm-metal wheel %q has no recognizable cp tag", asset.Name)
		}
		return metalRelease{MetalWheelURL: asset.BrowserDownloadURL, PythonVersion: version}, nil
	}
	return metalRelease{}, fmt.Errorf("vllm-metal latest release has no .whl asset")
}

// resolveCoreWheel returns the vLLM CORE macOS arm64 wheel URL matching the metal wheel's
// Python version. The cp312 -> v0.26.0 pairing is the confirmed-good pin; ANY other
// version is resolved best-effort by scanning vLLM's releases for a `cp<NN>…macosx…arm64.whl`
// asset, returning an error when none is found.
func resolveCoreWheel(pythonVersion string) (string, error) {
	if pythonVersion == "3.12" {
		return coreWheelCP312, nil
	}
	cpTag := "cp3" + strings.TrimPrefix(pythonVersion, "3.")
	body, err := metalReleaseGet(vllmReleasesAPI)
	if err != nil {
		return "", fmt.Errorf("resolve vLLM core wheel for Python %s: %w", pythonVersion, err)
	}
	var releases []ghRelease
	if err := json.Unmarshal(body, &releases); err != nil {
		return "", fmt.Errorf("parse vLLM releases JSON: %w", err)
	}
	for _, release := range releases {
		for _, asset := range release.Assets {
			name := asset.Name
			if strings.HasSuffix(name, ".whl") && strings.Contains(name, cpTag) &&
				strings.Contains(name, "macosx") && strings.Contains(name, "arm64") {
				return asset.BrowserDownloadURL, nil
			}
		}
	}
	return "", fmt.Errorf("no vLLM core macOS arm64 wheel found for Python %s", pythonVersion)
}
