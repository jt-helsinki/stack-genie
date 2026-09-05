package setup

// pyenv_host.go wires the platform-managed HOST Python venv (~/.ai-platform/venv,
// internal/pyenv) into the service reconcile. The venv is the single home for
// platform-wide host Python tooling — the omlx local-inference backend, the SOLE
// local-inference runtime on this platform, which is then resolved from it
// (omlx.BinaryPath).
//
// Creation is best-effort and OPTIONAL: `ai setup` must never fail because a host
// lacks uv/python3. The mutation (a `uv venv` / `python3 -m venv` under
// ~/.ai-platform) is self-contained and removed by `ai uninstall --purge`.

import (
	"github.com/jt-helsinki/stack-genie/internal/omlx"
	"github.com/jt-helsinki/stack-genie/internal/pyenv"
)

// ensurePlatformVenvFn is the injectable seam for creating the platform venv, so unit
// tests exercise the reconcile without a real Python toolchain. It defaults to
// pyenv.Ensure (created?, error).
var ensurePlatformVenvFn = pyenv.Ensure

// ensurePlatformVenv creates the platform-managed host Python venv if absent, at the
// NEWEST available Python (pyenv.Ensure is version-agnostic), streaming a short
// progress line. The subsequent omlx install (ensureOmlxInstalled → omlx.Install)
// re-pins the venv to the EXACT Python version the resolved omlx wheel requires via
// pyenv.EnsureVersion (recreating this venv if the versions differ), so this
// create-then-maybe-recreate sequence is intentional and coherent. It is
// best-effort: any failure (no uv/python3, offline uv, etc.) is reported via
// progress and swallowed — it NEVER fails the reconcile.
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

// installOmlxFn is the injectable seam for the one-shot omlx install into the
// platform venv (omlx.Install). A package var so tests exercise
// ensureOmlxInstalled without a real (large) network install.
var installOmlxFn = omlx.Install

// ensureOmlxInstalled installs omlx into the platform-managed host venv when it is
// not already present, so the local-inference backend works after a plain
// `ai setup`. It is Detect-gated (a present install is left untouched — installs
// once) and STRICTLY best-effort: the real pip download is large, so any failure
// is reported via progress and swallowed — it NEVER fails `ai setup`. A manual
// re-run of `ai services start omlx` retries the same install.
func ensureOmlxInstalled(progress func(string)) {
	if omlxDetectFn() {
		progress("  • omlx present in the platform venv")
		return
	}
	progress("  • installing omlx into the platform venv (this can take a while)…")
	if err := installOmlxFn(); err != nil {
		progress("  • omlx install skipped (" + err.Error() + ") — install later with `ai services start omlx`")
		return
	}
	if omlxDetectFn() {
		progress("  • omlx installed in the platform venv")
		return
	}
	progress("  • omlx install ran but the binary was not detected — check `" +
		omlx.ManagedVenvDir + "/bin/omlx --version`")
}
