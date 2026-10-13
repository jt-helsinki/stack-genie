package setup

// multica_host.go is the HOST half of the Multica service: install the verified
// `multica` CLI, point it at the self-hosted server, write the nono-sandboxing agent
// shims, and start the daemon with a shim-first PATH (internal/multica). Like colibri
// it is best-effort end to end — none of it ever fails `ai setup` or `ai services
// start multica` — because the one unavoidable manual step is the user's own browser
// login (`multica login`), which this platform cannot automate.
//
// hardware bring-up: see internal/multica's package doc.

import (
	"os"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/multica"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
)

// Injectable seams so tests never install, shell out to multica, or touch the keychain.
var (
	multicaDetectFn  = multica.Detect
	multicaInstallFn = multica.Install
	multicaRunFn     = multica.RealRunner
	multicaMintKeyFn = func() error {
		return workspace.RealManager(func() string { return time.Now().UTC().Format(time.RFC3339) }).EnsureMulticaKey()
	}
)

// multicaShimmedBinaries maps every platform agent CLI Multica can run to the binary
// name it detects on PATH. Multica documents no gemini runtime, so gemini is omitted —
// a shim for it would never be called.
func multicaShimmedBinaries() map[string]string {
	binaries := map[string]string{}
	for _, cli := range workspace.AgentCLINames() {
		if cli == "gemini" {
			continue
		}
		if binary, ok := workspace.AgentBinary(cli); ok {
			binaries[cli] = binary
		}
	}
	return binaries
}

// ensureMulticaHost installs + configures the host CLI and (re)writes the shims,
// reporting progress and swallowing every failure.
func ensureMulticaHost(progress func(string), domain string) {
	appURL := multicaPublicURL(domain)
	if !multicaDetectFn() {
		progress("  • installing the multica CLI (SHA-256 verified)…")
		if err := multicaInstallFn(); err != nil {
			progress("  • multica CLI install skipped (" + err.Error() + ")")
			for _, line := range multica.InstallGuidance(appURL) {
				progress("      " + line)
			}
			return
		}
	}
	if err := multica.Configure(multicaRunFn, appURL); err != nil {
		progress("  • multica configure skipped (" + err.Error() + ")")
		return
	}
	if err := writeMulticaShims(); err != nil {
		progress("  • multica agent shims skipped (" + err.Error() + ")")
		return
	}
	startMulticaDaemonHost(progress, appURL)
}

func writeMulticaShims() error {
	shimDir, err := multica.ShimDir()
	if err != nil {
		return err
	}
	aiBinary, err := os.Executable()
	if err != nil {
		return err
	}
	return multica.WriteShims(shimDir, aiBinary, multicaShimmedBinaries())
}

// startMulticaDaemonHost starts the daemon once the user is signed in. Not signed in is
// the expected first-run state: print the one manual step and stop.
func startMulticaDaemonHost(progress func(string), appURL string) {
	if !multicaDetectFn() {
		return
	}
	if !multica.LoggedIn(multicaRunFn) {
		progress("  • multica daemon not started — sign in first:")
		for _, line := range multica.InstallGuidance(appURL)[1:] {
			progress("      " + line)
		}
		return
	}
	if err := writeMulticaShims(); err != nil {
		progress("  • multica agent shims failed (" + err.Error() + ") — daemon not started")
		return
	}
	// One shared scoped key for every task (a per-task mint would revoke running tasks').
	if err := multicaMintKeyFn(); err != nil {
		progress("  • multica gateway key not minted (" + err.Error() + ") — daemon not started")
		return
	}
	shimDir, err := multica.ShimDir()
	if err != nil {
		progress("  • multica daemon not started (" + err.Error() + ")")
		return
	}
	// Stop first so a daemon left over from `multica setup` (which has no shim PATH) is replaced.
	multica.StopDaemon(multicaRunFn)
	if err := multica.StartDaemon(multicaRunFn, shimDir); err != nil {
		progress("  • " + err.Error())
		return
	}
	progress("  • multica daemon started — agents run inside nono via the gateway")
}

// stopMulticaDaemonHost stops the host daemon, best-effort.
func stopMulticaDaemonHost() {
	if multicaDetectFn() {
		multica.StopDaemon(multicaRunFn)
	}
}

// multicaDaemonDetail is the one-line host-daemon state shown on the multica service.
func multicaDaemonDetail() string {
	switch {
	case !multicaDetectFn():
		return "CLI not installed — run `ai setup`"
	case !multica.LoggedIn(multicaRunFn):
		return "daemon needs sign-in — run `multica login`, then `ai services start multica`"
	case !multica.DaemonRunning(multicaRunFn):
		return "daemon not running — `ai services start multica`"
	}
	return "daemon running (agents sandboxed in nono)"
}
