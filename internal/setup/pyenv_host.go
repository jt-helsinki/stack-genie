package setup

// pyenv_host.go wires the platform-managed HOST Python venv (~/.ai-platform/venv,
// internal/pyenv) into the service reconcile. The venv is the single home for
// platform-wide host Python tooling — most importantly the vLLM local-inference
// backend, which the platform then resolves from it (vllm.BinaryPath).
//
// Creation is best-effort and OPTIONAL: `ai setup` must never fail because a host
// lacks uv/python3 (the venv is only needed by opt-in Python backends). The
// mutation (a `uv venv` / `python3 -m venv` under ~/.ai-platform) is self-contained
// and removed by `ai uninstall --purge`.

import (
	"github.com/jt-helsinki/stack-genie/internal/pyenv"
	"github.com/jt-helsinki/stack-genie/internal/vllm"
)

// ensurePlatformVenvFn is the injectable seam for creating the platform venv, so unit
// tests exercise the reconcile without a real Python toolchain. It defaults to
// pyenv.Ensure (created?, error).
var ensurePlatformVenvFn = pyenv.Ensure

// ensurePlatformVenv creates the platform-managed host Python venv if absent, at the
// NEWEST available Python (pyenv.Ensure is version-agnostic), streaming a short
// progress line. On darwin the subsequent vLLM install (ensureVLLMInstalled →
// vllm.Install) re-pins the venv to the EXACT Python version the resolved vllm-metal
// wheel requires via pyenv.EnsureVersion (recreating this venv if the versions differ),
// so this create-then-maybe-recreate sequence is intentional and coherent. It is
// best-effort: any failure (no uv/python3, offline uv, etc.) is reported via progress
// and swallowed — it NEVER fails the reconcile, since the venv only matters to opt-in
// host Python backends (vLLM).
func ensurePlatformVenv(progress func(string)) {
	created, err := ensurePlatformVenvFn()
	if err != nil {
		progress("  • platform Python env: skipped (" + err.Error() + ")")
		return
	}
	if created {
		progress("  • platform Python env created at ~/.ai-platform/venv")
		return
	}
	progress("  • platform Python env present at ~/.ai-platform/venv")
}

// installVLLMFn is the injectable seam for the one-shot vLLM install into the platform
// venv (vllm.Install). A package var so tests exercise ensureVLLMInstalled without a
// real (large) network install.
var installVLLMFn = vllm.Install

// ensureVLLMInstalled installs vLLM into the platform-managed host venv when it is not
// already present, so `ai models pull --runtime vllm` works after a plain `ai setup`.
// It is Detect-gated (a present install is left untouched — installs once) and STRICTLY
// best-effort: the real pip download is large and OS-specific, so any failure is
// reported via progress and swallowed — it NEVER fails `ai setup`. The per-OS spec
// (vllm.InstallSpecs) is used: the vLLM-Metal plugin on Apple Silicon, plain `vllm`
// on Linux. A manual `ai models install-vllm --spec …` still overrides the spec.
func ensureVLLMInstalled(progress func(string)) {
	if installed, _ := vllmDetect(); installed {
		progress("  • vLLM present in the platform venv")
		return
	}
	progress("  • installing vLLM into the platform venv (this can take a while)…")
	if err := installVLLMFn(); err != nil {
		progress("  • vLLM install skipped (" + err.Error() + ") — install later with `ai models install-vllm`")
		return
	}
	if installed, kind := vllmDetect(); installed {
		progress("  • vLLM installed in the platform venv (" + kind + " weights)")
		return
	}
	progress("  • vLLM install ran but the binary was not detected — check `" +
		vllm.ManagedVenvDir + "/bin/vllm --version`")
}
