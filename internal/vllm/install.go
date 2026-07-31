package vllm

import (
	"fmt"
	"runtime"

	"github.com/jt-helsinki/stack-genie/internal/pyenv"
)

// InstallSpecs returns the default pip requirement spec(s) to install vLLM INTO the
// platform-managed host venv for a given GOOS. It mirrors ModelFormat's split:
//
//   - darwin (Apple Silicon): the vLLM-Metal plugin, which pulls in the Metal-backed
//     vLLM build serving mlx-community/* weights. Installed from its upstream repo via
//     pip's standard git form. This exact spec should be confirmed against a real
//     Apple Silicon host (hardware bring-up); callers can override it with --spec.
//   - everything else (Linux/CUDA): the plain `vllm` package (Hugging Face safetensors
//     on CUDA).
func InstallSpecs(goos string) []string {
	if goos == "darwin" {
		return []string{"git+https://github.com/vllm-project/vllm-metal.git"}
	}
	return []string{"vllm"}
}

// Install performs the one-shot vLLM install into the platform-managed host venv
// (~/.ai-platform/venv): it ensures the venv exists (pyenv.Ensure — prefers uv, falls
// back to python3 -m venv) and then pip-installs the given specs into it. With no
// specs it uses the per-OS default from InstallSpecs(runtime.GOOS). After a successful
// install BinaryPath/Detect resolve the venv's vllm automatically.
//
// The real install is a large, OS-specific network download (the Metal plugin on
// Apple Silicon), so this is only ever invoked deliberately (`ai models install-vllm`),
// never as part of `ai setup`. A missing host Python toolchain surfaces as pyenv's
// actionable error.
func Install(specs ...string) error {
	if len(specs) == 0 {
		specs = InstallSpecs(runtime.GOOS)
	}
	if _, err := pyenv.Ensure(); err != nil {
		return err
	}
	if err := pyenv.PipInstall(specs...); err != nil {
		return fmt.Errorf("install vLLM into the platform venv: %w", err)
	}
	return nil
}
