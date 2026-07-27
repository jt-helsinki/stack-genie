package vllm

import (
	"os/exec"
	"runtime"
)

// Format names the on-disk model weight format vLLM expects on a given host. vLLM
// does not use GGUF and shares no store with Ollama.
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
	if _, err := lookPath("vllm"); err != nil {
		return false, ""
	}
	return true, ModelFormat(runtime.GOOS)
}

// InstallGuidance returns the ordered, per-OS manual steps to install vLLM for the
// given GOOS. It performs NO host mutation — it is the actionable guidance the CLI
// prints when Detect reports vLLM missing (mirroring the host-Ollama bring-up
// guidance pattern). The actual install is `hardware bring-up`.
func InstallGuidance(goos string) []string {
	switch goos {
	case "darwin":
		return []string{
			"vLLM on Apple Silicon requires macOS Sonoma (14) or newer.",
			"Create a Python 3.11+ environment (e.g. `uv venv` or `python3 -m venv .venv && source .venv/bin/activate`).",
			"Install the vLLM Metal plugin from its repository (github.com/vllm-project/vllm-metal), which pulls in a Metal-backed vLLM build.",
			"Use MLX weights only — pull models from the `mlx-community` org on Hugging Face (e.g. `mlx-community/<model>`).",
			"Verify with `vllm --version`; models are served with `vllm serve mlx-community/<model> --port <port>`.",
		}
	default:
		return []string{
			"vLLM on Linux requires an NVIDIA GPU with a working CUDA driver.",
			"Create a Python 3.9+ environment (e.g. `uv venv` or `python3 -m venv .venv && source .venv/bin/activate`).",
			"Install CUDA vLLM: `pip install vllm` (or `uv pip install vllm`).",
			"Use Hugging Face safetensors weights (the default HF repo format); set HF_HOME to the platform store.",
			"Verify with `vllm --version`; models are served with `vllm serve <hf-repo> --port <port>`.",
		}
	}
}
