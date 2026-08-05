package vllm

import (
	"fmt"
	"runtime"

	"github.com/jt-helsinki/stack-genie/internal/pyenv"
)

// pyenv seams (package vars) so Install is unit-tested without a real Python toolchain
// or network. They default to the real pyenv functions.
var (
	pyenvEnsureFn        = pyenv.Ensure
	pyenvEnsureVersionFn = pyenv.EnsureVersion
	pyenvPipInstallFn    = pyenv.PipInstall
)

// InstallSpecs returns the default pip requirement spec(s) to install vLLM INTO the
// platform-managed host venv for a given GOOS:
//
//   - darwin (Apple Silicon): NONE — the MLX/Metal install is resolved DYNAMICALLY at
//     runtime by Install (the latest vllm-metal wheel + its matching vLLM core wheel),
//     not from a static spec. An empty result makes `ai models install-vllm` (no --spec)
//     fall through to Install's darwin resolver path.
//   - everything else (Linux/CUDA): the plain `vllm` package (manylinux wheels exist;
//     Hugging Face safetensors on CUDA).
func InstallSpecs(goos string) []string {
	if goos == "darwin" {
		return nil
	}
	return []string{"vllm"}
}

// Install performs the one-shot vLLM install into the platform-managed host venv
// (~/.ai-platform/venv). It has three branches:
//
//   - explicit specs (`ai models install-vllm --spec …`): ensure the venv at the newest
//     Python (pyenv.Ensure) and pip-install the specs VERBATIM — the resolver is skipped.
//     This is the escape hatch for pinning an exact wheel.
//   - darwin, no specs: MLX/Metal. Resolve the latest vllm-metal wheel (→ its cp-tag
//     Python version + metal wheel URL) and the matching vLLM core wheel, pin the venv to
//     that EXACT Python version (pyenv.EnsureVersion, recreating a mismatched venv), then
//     pip-install the core wheel FIRST and the metal wheel ON TOP (the plugin overrides
//     the device to Metal). Any resolver error is returned (setup swallows it best-effort).
//   - non-darwin, no specs: ensure the venv (newest Python) and pip-install `vllm`
//     (Linux manylinux wheels exist).
//
// The real install is a large, OS-specific network download (the Metal wheels on Apple
// Silicon), so this is only ever invoked deliberately (`ai models install-vllm`), never
// as part of `ai setup`. A missing host Python toolchain surfaces as pyenv's actionable
// error. This is `hardware bring-up` behind the injectable pyenv + metalReleaseGet seams.
func Install(specs ...string) error {
	if len(specs) > 0 {
		if _, err := pyenvEnsureFn(); err != nil {
			return err
		}
		if err := pyenvPipInstallFn(specs...); err != nil {
			return fmt.Errorf("install vLLM into the platform venv: %w", err)
		}
		return nil
	}
	if runtime.GOOS == "darwin" {
		return installMetal()
	}
	if _, err := pyenvEnsureFn(); err != nil {
		return err
	}
	if err := pyenvPipInstallFn("vllm"); err != nil {
		return fmt.Errorf("install vLLM into the platform venv: %w", err)
	}
	return nil
}

// installMetal resolves and installs the Apple-Silicon MLX/Metal vLLM stack: the latest
// vllm-metal wheel (whose cp tag sets the venv's Python version) plus the matching vLLM
// core wheel, core FIRST then metal on top. See metal.go for the resolvers.
func installMetal() error {
	release, err := resolveMetalWheel()
	if err != nil {
		return err
	}
	coreURL, err := resolveCoreWheel(release.PythonVersion)
	if err != nil {
		return err
	}
	if _, err := pyenvEnsureVersionFn(release.PythonVersion); err != nil {
		return err
	}
	if err := pyenvPipInstallFn(coreURL); err != nil {
		return fmt.Errorf("install vLLM core wheel into the platform venv: %w", err)
	}
	if err := pyenvPipInstallFn(release.MetalWheelURL); err != nil {
		return fmt.Errorf("install vllm-metal wheel into the platform venv: %w", err)
	}
	return nil
}
