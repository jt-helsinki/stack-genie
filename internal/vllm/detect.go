package vllm

import (
	"os"
	"os/exec"
	"runtime"

	"github.com/jt-helsinki/stack-genie/internal/pyenv"
)

// Format names the on-disk model weight format vLLM expects on a given host.
const (
	// FormatMLX is Apple Silicon (darwin): mlx-community/* weights served through the
	// vLLM-Metal plugin.
	FormatMLX = "mlx"
	// FormatSafetensors is Linux/CUDA: Hugging Face safetensors weights.
	FormatSafetensors = "safetensors"
)

// lookPath is the injectable seam for locating the vllm binary (defaults to
// exec.LookPath); overridable in tests without touching the host.
var lookPath = exec.LookPath

// BinaryPath resolves the `vllm` executable the platform runs: the platform-managed
// venv's vllm (~/.ai-platform/venv/bin/vllm, pyenv.BinPath) when that file exists,
// else the bare name "vllm" for a PATH lookup. This is how the platform "uses the
// managed Python environment" for vLLM — once vLLM is installed into the venv, both
// Detect and RealRunner.Start pick it up automatically, with the PATH install kept as
// a fallback. An unresolvable venv path degrades to "vllm".
func BinaryPath() string {
	venvBinary, err := pyenv.BinPath("vllm")
	if err != nil {
		return "vllm"
	}
	if _, statErr := os.Stat(venvBinary); statErr == nil {
		return venvBinary
	}
	return "vllm"
}

// ModelFormat returns the weight format vLLM uses on the given GOOS: MLX on darwin,
// safetensors elsewhere (Linux/CUDA). It is the single source of truth callers use
// to pick a default model repo (mlx-community/* vs a plain HF repo).
func ModelFormat(goos string) string {
	if goos == "darwin" {
		return FormatMLX
	}
	return FormatSafetensors
}

// Detect reports whether a usable vLLM install is present and, if so, its kind
// ("mlx" on darwin, "safetensors"/CUDA elsewhere — mirroring ModelFormat). It only
// checks for the `vllm` binary on PATH (a read-only probe).
//
// hardware bring-up: a COMPLETE detection must also verify the platform-specific
// runtime — on darwin that the vLLM-Metal plugin is importable, on Linux that a
// CUDA device + driver are present. Those deeper checks require the real toolchain
// on a provisioned host and are NOT wired here; this returns based on the binary
// alone. When wired, extend this to run `vllm --version` / a plugin import probe.
func Detect() (installed bool, kind string) {
	if _, err := lookPath(BinaryPath()); err != nil {
		return false, ""
	}
	return true, ModelFormat(runtime.GOOS)
}

// ManagedVenvDir is the documented path of the platform-managed host Python venv
// (paths.VenvDir). It is used verbatim in the install guidance so the steps point the
// user at the venv `ai setup` already created, rather than telling them to make and
// manage their own. Kept as a literal (guidance is a pure per-OS function) matching
// the resolved ~/.ai-platform/venv.
const ManagedVenvDir = "~/.ai-platform/venv"

// InstallGuidance returns the ordered, per-OS manual steps to install vLLM INTO the
// platform-managed venv (~/.ai-platform/venv) that `ai setup` creates. It performs NO
// host mutation — it is the actionable guidance the CLI prints when Detect reports
// vLLM missing. Once vLLM is installed
// into that venv, the platform resolves + serves it automatically (see BinaryPath).
// The actual install is `hardware bring-up`.
func InstallGuidance(goos string) []string {
	switch goos {
	case "darwin":
		return []string{
			"vLLM on Apple Silicon uses the Metal/MLX backend (the community vllm-metal plugin) and requires macOS Sonoma (14) or newer on native arm64 Python (PyPI ships no macOS wheel, so `pip install vllm` alone will NOT work).",
			"`ai setup` AUTO-installs it into the platform Python env at " + ManagedVenvDir + ": it resolves the LATEST vllm-metal release wheel (plus the matching vLLM core wheel) from GitHub and pip-installs both — the venv's Python version is DERIVED from the wheel's cp tag (3.12 today, auto-following to cp313).",
			"Install/repair manually with `ai models install-vllm` (or override the wheel with `ai models install-vllm --spec <wheel-url>`).",
			"It serves `mlx-community/<model>` weights on the Metal GPU — no `--dtype` needed (this is the MLX backend, not a CPU build).",
			"Verify with `" + ManagedVenvDir + "/bin/vllm --version`; the platform then discovers + serves models from that venv automatically.",
		}
	default:
		return []string{
			"vLLM on Linux requires an NVIDIA GPU with a working CUDA driver, and Python 3.9+.",
			"`ai setup` already created the platform Python env at " + ManagedVenvDir + " — install vLLM INTO it (no separate venv to make or activate).",
			"Install CUDA vLLM into it: `" + ManagedVenvDir + "/bin/pip install vllm` (or `uv pip install --python " + ManagedVenvDir + "/bin/python vllm`).",
			"Use Hugging Face safetensors weights; the platform sets HF_HOME to its model store at serve time.",
			"Verify with `" + ManagedVenvDir + "/bin/vllm --version`; the platform then discovers + serves models from that venv automatically.",
		}
	}
}
