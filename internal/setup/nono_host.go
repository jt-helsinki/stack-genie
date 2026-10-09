package setup

// nono_host.go wires the HOST-NATIVE nono CLI (https://nono.sh) into `ai setup`.
// nono is the kernel-capability sandboxing tool (Landlock on Linux, Seatbelt on
// macOS) that replaces Microsandbox microVMs as the workspace runtime — it has
// no container/service-tier presence at all, so unlike every service this
// package otherwise reconciles there is nothing to health-probe or
// docker-inspect: "installed" just means the `nono` binary is on PATH.
//
// Unlike Microsandbox (Apple-Silicon-only HVF or Linux KVM), nono has no
// virtualization-capability gate — any host the binary runs on can use it.

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"

	"github.com/jt-helsinki/stack-genie/internal/nono"
)

// nonoDetectFn is the injectable seam for the nono install probe, so unit tests
// exercise EnsureNonoInstalled without a real binary on PATH.
var nonoDetectFn = func() nono.Info { return nono.Detect(execProber{}) }

// execProber adapts os/exec.LookPath to nono.Prober.
type execProber struct{}

func (execProber) LookPath(file string) (string, error) { return exec.LookPath(file) }

// installNonoFn is the injectable seam for the one-shot nono install. A package
// var so tests exercise EnsureNonoInstalled without a real network install.
var installNonoFn = installNono

// EnsureNonoInstalled installs the `nono` CLI when it is not already present, so
// workspace management works after a plain `ai setup`. Detect-gated (a present
// install is left untouched) and STRICTLY best-effort: any failure is reported
// via progress and swallowed — it NEVER fails `ai setup`. A manual re-run of
// `ai setup` retries the same install.
func EnsureNonoInstalled(progress func(string)) {
	if nonoDetectFn().Installed {
		progress("  • nono present on PATH")
		return
	}
	progress("  • installing nono (workspace sandboxing CLI)…")
	if err := installNonoFn(); err != nil {
		progress("  • nono install skipped (" + err.Error() + ") — install manually: https://nono.sh/docs/cli/getting_started/installation")
		return
	}
	if nonoDetectFn().Installed {
		progress("  • nono installed")
		return
	}
	progress("  • nono install ran but the binary was not detected on PATH — check `nono --version`")
}

// installNono installs the `nono` CLI: `brew install nono` when Homebrew is
// available (covers macOS and Linuxbrew identically), else nono's own official
// curl installer (https://nono.sh/install.sh — verified against the live
// script, which downloads the platform release from GitHub and checksum-
// verifies it). Both are the exact commands nono's own installation docs give.
func installNono() error {
	if _, err := exec.LookPath("brew"); err == nil {
		return runShell("brew", "install", "nono")
	}
	return curlPipeShell("https://nono.sh/install.sh")
}

// runShell runs name with args, discarding output on success and returning a
// combined-output error otherwise (mirrors the rest of this package's
// host-install helpers, e.g. pyenv's installers).
func runShell(name string, args ...string) error {
	command := exec.Command(name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return errors.New(string(output))
	}
	return nil
}

// curlPipeShell runs `curl -fsSL <url> | sh` as two real connected processes
// (no shell interpolation of url — it is always a fixed literal passed by this
// package's own callers, never derived from external input) for installers
// distributed as a pipe-to-shell one-liner, matching this project's existing
// convention (e.g. Ponytail's/Caveman's curl installers).
func curlPipeShell(url string) error {
	curl := exec.Command("curl", "-fsSL", url)
	shell := exec.Command("sh")

	pipe, err := curl.StdoutPipe()
	if err != nil {
		return err
	}
	shell.Stdin = pipe
	var shellOutput bytes.Buffer
	shell.Stdout = &shellOutput
	shell.Stderr = &shellOutput

	if err := shell.Start(); err != nil {
		return err
	}
	if err := curl.Run(); err != nil {
		_ = shell.Wait()
		return fmt.Errorf("curl %s: %w", url, err)
	}
	if err := shell.Wait(); err != nil {
		return errors.New(shellOutput.String())
	}
	return nil
}
