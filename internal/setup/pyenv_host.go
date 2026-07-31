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

import "github.com/jt-helsinki/stack-genie/internal/pyenv"

// ensurePlatformVenvFn is the injectable seam for creating the platform venv, so unit
// tests exercise the reconcile without a real Python toolchain. It defaults to
// pyenv.Ensure (created?, error).
var ensurePlatformVenvFn = pyenv.Ensure

// ensurePlatformVenv creates the platform-managed host Python venv if absent,
// streaming a short progress line. It is best-effort: any failure (no uv/python3,
// offline uv, etc.) is reported via progress and swallowed — it NEVER fails the
// reconcile, since the venv only matters to opt-in host Python backends (vLLM).
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
