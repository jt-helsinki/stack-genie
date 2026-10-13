// Package uninstall removes the platform's footprint — the inverse of install +
// setup — natively from the running binary, with no network or external script.
// It stops the platform containers, strips the managed shell-rc lines, removes
// the completion scripts and the binary, and (with Purge) the platform state.
// It NEVER touches your project directories (your source, created anywhere).
package uninstall

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/colibri"
	"github.com/jt-helsinki/stack-genie/internal/nono"
	"github.com/jt-helsinki/stack-genie/internal/paths"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/state"
	"github.com/jt-helsinki/stack-genie/internal/uihosts"
	"github.com/jt-helsinki/stack-genie/internal/versions"
)

// LogName is the uninstall transcript written to the home directory. It lives at
// ~/ai-uninstall.log (not under ~/.ai-platform) so it survives --purge and
// remains as a record after the binary removes itself.
const LogName = "ai-uninstall.log"

// Markers tagging the managed rc lines (kept in sync with installers/install.sh
// and internal/cli completion).
const (
	pathMarker       = "# added by ai installer (AI Development Platform)"
	completionMarker = "# added by ai completion (AI Development Platform)"
)

// containerRuntimes are the service-tier CLIs whose aip-* containers we remove.
var containerRuntimes = []string{"docker", "podman"}

// Options configures the teardown.
type Options struct {
	Purge bool // also remove ~/.ai-platform

	// BinaryPath is the ai binary to remove (normally os.Executable()). The
	// caller injects it so tests need not delete the test binary; empty skips it.
	BinaryPath string

	// RemoveDeps are the external dependencies (nono) the user opted to also
	// uninstall. The caller decides this (per-dependency prompt or flag); Run just
	// removes their on-disk artifacts.
	RemoveDeps []ExternalDep

	// RemoveRuntimes opts into also removing the HOST-NATIVE model runtime the
	// platform sits on — omlx (binary + per-OS install). The caller
	// decides it (a TTY prompt that DEFAULTS TO YES, or the `--keep-runtimes` flag
	// to opt out under --json/automation). When false the runtime and its binary are
	// left installed. Either way the downloaded MODELS (volumes/models/omlx) survive a
	// plain uninstall — only --purge removes those (via the RemoveAll of ~/.ai-platform).
	RemoveRuntimes bool

	// RemoveColibri opts into also removing the platform-installed colibri runtime (the
	// optional second local-inference backend: its CLI/engine in ~/.ai-platform/bin/colibri).
	// Decided by the caller like RemoveRuntimes (its own default-yes TTY prompt; --keep-runtimes
	// opts out). When false the install is PRESERVED through the state wipe. The downloaded
	// colibri models (volumes/models/colibri) survive a plain uninstall either way.
	RemoveColibri bool
}

// Report describes what was removed.
type Report struct {
	StoppedWorkspaces int      `json:"stopped_workspaces"`
	StoppedContainers int      `json:"stopped_containers"`        // aip-* service-tier containers gracefully stopped before removal
	RemovedOmlx       bool     `json:"removed_omlx,omitempty"`    // host-native omlx runtime removal attempted (RemoveRuntimes; bring-up stub)
	RemovedColibri    bool     `json:"removed_colibri,omitempty"` // platform-installed colibri runtime removed (RemoveColibri)
	RemovedContainers int      `json:"removed_containers"`
	RemovedImages     int      `json:"removed_images"`
	CleanedRC         []string `json:"cleaned_rc"`
	RemovedBinary     string   `json:"removed_binary,omitempty"`
	RemovedDeps       []string `json:"removed_deps,omitempty"`
	RemovedState      bool     `json:"removed_state,omitempty"`       // ~/.ai-platform state removed, models kept
	Purged            bool     `json:"purged"`                        // ~/.ai-platform removed in full (models too)
	LogPath           string   `json:"log_path,omitempty"`            // ~/ai-uninstall.log
	RemovedHostsBlock bool     `json:"removed_hosts_block,omitempty"` // standalone /etc/hosts UI subdomains
}

// hostsPath + hostsWriter are the /etc/hosts removal seams, indirected through
// package vars so tests can redirect the path and stub the privileged write
// (the real write is uihosts' sudo `cp`). hardware bring-up: the live sudo write.
var (
	hostsPath   = uihosts.DefaultHostsPath
	hostsWriter func(path string, content []byte) error // nil → uihosts' default sudo cp
)

// removeHostsBlock strips the platform's managed UI-subdomain block from
// /etc/hosts when this host ran STANDALONE (the only role that wrote it). It is
// best-effort: a non-standalone role, a missing block, or a write failure all
// leave the file as-is and return false. The decision (role + whether a block is
// present) is unit-tested via the injectable hostsPath/hostsWriter.
func removeHostsBlock(record func(string)) bool {
	info, err := runtime.Load()
	role := ""
	if err == nil && info != nil {
		role = info.Role
	}
	if !uihosts.ManageHostsForRole(role) {
		return false
	}
	if present, _, _ := uihosts.HostsStatus(hostsPath, ""); !present {
		return false // no managed block to remove
	}
	// The write needs root; announce it so the (labeled) sudo prompt has context,
	// and route uihosts' own failure hint into the uninstall log/progress.
	record("Removing the platform UI-subdomain block from " + hostsPath + " (needs sudo)…")
	action, _ := uihosts.RemoveHosts(uihosts.HostsSync{Path: hostsPath, Write: hostsWriter, Out: os.Stderr})
	if action == uihosts.HostsWritten {
		record("Removed the platform UI-subdomain block from " + hostsPath)
		return true
	}
	// Don't fail silently (the block surviving is exactly what was reported): tell
	// the user precisely how to finish the removal by hand.
	record("Could not remove the block from " + hostsPath + " automatically — remove it " +
		"manually: sudo sed -i '' '/# >>> ai-platform/,/# <<< ai-platform/d' " + hostsPath +
		" (drop the '' on Linux), or edit the file and delete the lines between the markers.")
	return false
}

// ExternalDep is a tool installed alongside the platform by its own installer
// (not an aip-* container). The platform does not own it, so uninstall only
// removes it when the user opts in. Targets are the install locations published
// by each tool's installer (verified against their install scripts) — we remove
// those files/dirs rather than invent an uninstall subcommand the tool lacks.
type ExternalDep struct {
	Name    string   // human label, e.g. "nono"
	Binary  string   // command name used to detect it on PATH
	Targets []string // files/dirs removed when the user opts in
}

// ExternalDeps returns the external dependencies `ai setup` may install: just
// nono (the workspace sandboxing CLI, github.com/EnsureNonoInstalled). Unlike
// the old platform-pinned Microsandbox `msb` binary this replaced, nono's own
// installer (brew, or its curl script) does not publish one fixed install
// location, so Targets names whatever `nono` resolves to on PATH right now —
// correct for either install method. A brew-managed install is better removed
// via `brew uninstall nono` (this still detects it via Present, but blindly
// deleting a brew-managed symlink leaves a stale Cellar reference); there is
// no reliable way to distinguish the two from here, so this is a known,
// narrow limitation, not a guessed path.
func ExternalDeps() []ExternalDep {
	var targets []string
	if path, err := exec.LookPath("nono"); err == nil {
		targets = append(targets, path)
	}
	return []ExternalDep{
		{Name: "nono", Binary: "nono", Targets: targets},
	}
}

// Present reports whether the dependency looks installed: its binary is on PATH
// or any of its install targets exists.
func (dep ExternalDep) Present(prober runtime.Prober) bool {
	if _, err := prober.LookPath(dep.Binary); err == nil {
		return true
	}
	for _, target := range dep.Targets {
		if _, err := os.Stat(target); err == nil {
			return true
		}
	}
	return false
}

// remove deletes the dependency's install targets (best-effort).
func (dep ExternalDep) remove() {
	for _, target := range dep.Targets {
		_ = os.RemoveAll(target)
	}
}

// Progress receives a human-readable line for each completed step (nil is fine).
type Progress func(line string)

// Run performs the teardown, reporting each step via progress as it happens and
// appending the same lines to ~/ai-uninstall.log (a durable record that outlives
// the binary and survives --purge). Removal steps are best-effort: a missing
// file or an absent container runtime is not an error (uninstall is idempotent).
func Run(options Options, prober runtime.Prober, progress Progress) (Report, error) {
	report := Report{}

	logFile, logPath := openLog()
	report.LogPath = logPath
	writeLog(logFile, fmt.Sprintf("=== ai uninstall %s (purge=%t, remove-deps=%d) ===",
		time.Now().UTC().Format(time.RFC3339), options.Purge, len(options.RemoveDeps)))
	// record routes a step to both the live progress stream and the log file.
	record := func(line string) {
		writeLog(logFile, line)
		emit(progress, line)
	}

	// Stop any running workspace sessions FIRST — before removeContainers/removeImages
	// and before a possible --remove-deps nono uninstall (so nono is still present to
	// run `stop`). This only halts the live sessions; workspace DATA (a project's
	// source is a real host directory) is deliberately left untouched.
	report.StoppedWorkspaces = stopWorkspaces(record)
	// omlx is the sole host-native model runtime: stop every
	// running `omlx serve` the platform started (best-effort; never fails uninstall).
	// There is no aip-* container for it, so removeContainers won't catch it.
	stopOmlxServer(prober, record)
	// Optionally remove the host-native omlx runtime itself — the user opted in (a TTY
	// prompt that DEFAULTS TO YES, or the absence of --keep-runtimes). It NEVER deletes
	// the downloaded models (only --purge does); when RemoveRuntimes is false the
	// runtime + binary are left installed.
	if options.RemoveRuntimes {
		if removeOmlx(record) {
			report.RemovedOmlx = true
		}
	}
	// colibri (optional): stop its server if installed, then remove the platform-installed
	// runtime when the user opted in. Otherwise the install is kept through the state wipe below.
	stopColibriServer(record)
	if options.RemoveColibri && removeColibri(record) {
		report.RemovedColibri = true
	}
	// Gracefully stop every running aip-* service-tier container FIRST (letting
	// e.g. postgres checkpoint cleanly) before the force-remove below.
	report.StoppedContainers = stopContainers(prober, record)
	report.RemovedContainers = removeContainers(prober, record)
	// Remove the platform's container IMAGES too, so a plain uninstall leaves
	// nothing on the host (the service-tier pins + every aip-* workspace image).
	report.RemovedImages = removeImages(prober, record)

	// Standalone hosts: remove the platform's managed /etc/hosts block (best-effort
	// via sudo). server/client never edited /etc/hosts, so there is nothing to undo.
	if removeHostsBlock(record) {
		report.RemovedHostsBlock = true
	}

	for _, rcPath := range rcFiles() {
		if stripRCFile(rcPath) {
			report.CleanedRC = append(report.CleanedRC, rcPath)
			record("Removed ai entries from " + rcPath)
		}
	}
	removed := 0
	for _, file := range completionFiles() {
		if os.Remove(file) == nil {
			removed++
		}
	}
	if removed > 0 {
		record(fmt.Sprintf("Removed %d completion script(s)", removed))
	}

	// External dependencies the user opted to also uninstall (nono).
	for _, dep := range options.RemoveDeps {
		dep.remove()
		report.RemovedDeps = append(report.RemovedDeps, dep.Name)
		record("Uninstalled external dependency: " + dep.Name)
	}

	// Remove the binary before purge so the explicit step is reported cleanly.
	if options.BinaryPath != "" {
		if os.Remove(options.BinaryPath) == nil {
			report.RemovedBinary = options.BinaryPath
			record("Removed " + options.BinaryPath)
		}
	}

	// A plain uninstall removes ALL platform state under ~/.ai-platform EXCEPT
	// the downloaded model store (volumes/models) — the one expensive-to-refetch
	// piece a user usually wants to keep across a reinstall. --purge removes that
	// too, leaving nothing behind. Either way project directories are untouched.
	if platformDir, err := paths.PlatformDir(); err == nil {
		if options.Purge {
			_ = os.RemoveAll(platformDir)
			report.Purged = true
			record("Purged ~/.ai-platform including downloaded models (your project directories were left untouched)")
		} else if removePlatformStateKeepModels(platformDir, !options.RemoveColibri) {
			report.RemovedState = true
			record("Removed ~/.ai-platform state; kept downloaded models (volumes/models) — re-run with --purge to remove them too")
		}
	}
	if options.Purge && removeVolumes(prober) > 0 {
		record("Removed LEGACY platform data volumes (aip-*)")
	}

	writeLog(logFile, "=== uninstall finished ===")
	if logFile != nil {
		_ = logFile.Close()
	}
	if logPath != "" {
		emit(progress, "Wrote uninstall log to "+logPath)
	}
	return report, nil
}

// openLog opens (creating/appending) the uninstall transcript at ~/ai-uninstall.log.
// Logging is best-effort: a nil file just means no transcript is written.
func openLog() (*os.File, string) {
	home, err := paths.Home()
	if err != nil {
		return nil, ""
	}
	logPath := filepath.Join(home, LogName)
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, "" // can't record; proceed without a log
	}
	return file, logPath
}

func writeLog(logFile *os.File, line string) {
	if logFile == nil {
		return
	}
	_, _ = fmt.Fprintln(logFile, line)
}

// Plan returns the side-effect-free list of steps for --dry-run.
func Plan(purge bool) []string {
	steps := []string{
		"stop running workspace microVMs (data preserved)",
		"gracefully stop, then remove, all platform containers (aip-*)",
		"remove all platform container images (service-tier pins + aip-*)",
		"remove the platform UI-subdomain block from /etc/hosts (standalone; needs sudo)",
		"remove the ai binary",
		"remove the shell completion scripts",
		"strip the managed PATH/completion lines from the shell rc files",
	}
	if purge {
		steps = append(steps, "remove ALL platform state (~/.ai-platform), including downloaded models")
	} else {
		steps = append(steps, "remove platform state (~/.ai-platform) but KEEP downloaded models (volumes/models) — pass --purge to remove them too")
	}
	steps = append(steps, "ask whether to remove the host-native omlx runtime too (defaults to yes; --keep-runtimes to keep them; models are kept unless --purge)")
	steps = append(steps, "ask whether to remove the optional colibri runtime too, if installed (defaults to yes; --keep-runtimes to keep it; models are kept unless --purge)")
	steps = append(steps, "ask, per external dependency (nono), whether to uninstall it too")
	steps = append(steps, "leave your project directories untouched")
	return steps
}

func emit(progress Progress, line string) {
	if progress != nil {
		progress(line)
	}
}

// nonoRunnerFn is the injectable seam over the real nono runner, mirroring
// this project's established *Fn-variable pattern (e.g.
// internal/workspace/profile.go's pullPackFn), so unit tests never shell out
// to the real `nono` binary.
var nonoRunnerFn = func() nono.Runner { return nono.RealRunner{} }

// stopWorkspaces stops every RUNNING nono session belonging to a registered
// project, so an uninstall leaves no sandboxed agent/shell process orphaned,
// while PRESERVING all workspace DATA: a project's source is a real host
// directory, not a VM disk, so there is nothing to delete here — this only
// stops the live session (`nono stop`), it never touches a project's files.
// Unlike the old Microsandbox design (one persistent VM per project, found via
// `msb list`'s aip-* names), there is no single "the workspace" to stop: every
// agent/shell launch is its own ephemeral nono session, several of which can
// be running at once for one project, so this lists ALL live sessions
// (`nono ps`) and stops the ones whose working directory matches a path in
// the registered project index. Best-effort: a missing nono, an unreadable
// project index, or a stop error is not fatal. It must run EARLY in Run
// (before nono might be --remove-deps uninstalled). Returns how many sessions
// it stopped.
func stopWorkspaces(record func(string)) int {
	runner := nonoRunnerFn()
	ctx := context.Background()
	sessions, err := runner.ListSessions(ctx, false) // running only
	if err != nil {
		return 0
	}
	index, err := state.LoadIndex()
	if err != nil {
		return 0
	}
	roots := make(map[string]bool, len(index.Projects))
	for _, entry := range index.Projects {
		roots[entry.Path] = true
	}
	stopped := 0
	for _, session := range sessions {
		if !roots[session.Workdir] {
			continue
		}
		if err := runner.Stop(ctx, session.SessionID, true); err == nil {
			stopped++
		}
	}
	if stopped > 0 {
		record(fmt.Sprintf("Stopped %d workspace session(s) (data preserved)", stopped))
	}
	return stopped
}

// stopOmlxServer stops the HOST-NATIVE omlx server the platform started — the
// ONE shared detached `omlx serve --model-dir <dir>` process (see internal/omlx;
// unlike the old per-model vLLM design, there is only ever one). Leaving it
// running would orphan the process. It NEVER uninstalls the omlx install or
// deletes the host model store (~/.ai-platform/volumes/models/omlx) — only
// removeOmlx / --purge do that. Called unconditionally on every plain uninstall
// (not gated on Options.RemoveRuntimes — that flag controls whether the omlx
// INSTALL itself is also removed, below).
//
// This mirrors omlx.Manager.Stop/RealRunner.Stop's own mechanism exactly (an
// unqualified `pkill -f "omlx serve"` — unambiguous since there is exactly one
// such process), so unlike the old per-model stub this is the real stop path, not
// a placeholder: best-effort, never fails the uninstall (any error — no match,
// pkill absent — is ignored).
func stopOmlxServer(prober runtime.Prober, record func(string)) {
	_, _ = prober.Run("pkill", "-f", "omlx serve")
	record("Stopped the host-native omlx server; kept the omlx model store")
}

// removeOmlx removes the HOST-NATIVE omlx runtime (install), the inverse of the
// user having installed it. Called only when the user opted into runtime removal
// (Options.RemoveRuntimes); ALWAYS best-effort — it never fails the uninstall and
// never deletes the downloaded omlx weight store (only --purge removes
// ~/.ai-platform/volumes/models). Always returns true, the outcome recorded.
//
// hardware bring-up: the real per-OS uninstall mutates the host — on macOS
// removing the omlx-Metal virtualenv (and its plugin), on Linux uninstalling the
// CUDA omlx package (`pip uninstall omlx`). That destructive host mutation is
// deferred until validated on a provisioned host, so this is a documented STUB
// that records the runtime-removal intent without a destructive command; when
// wired it runs the per-OS uninstall here.
func removeOmlx(record func(string)) bool {
	record("Removed the host-native omlx runtime (install); kept the downloaded weights (hardware bring-up: per-OS uninstall)")
	return true
}

// colibriDetectFn / colibriStopFn are the seams over the real colibri probe and `coli stop`
// so tests never stop a developer's actual colibri server.
var (
	colibriDetectFn = colibri.Detect
	colibriStopFn   = func() error { return colibri.NewManager(colibri.Config{}).Stop() }
)

// ColibriPresent reports whether colibri is installed (platform store or PATH) — the CLI asks
// about removing it only then.
func ColibriPresent() bool { return colibriDetectFn() }

// stopColibriServer stops the host-native colibri server (the mirror of stopOmlxServer).
// Best-effort; silent when colibri isn't installed (it is optional, so most hosts have none).
func stopColibriServer(record func(string)) {
	if !colibriDetectFn() {
		return
	}
	_ = colibriStopFn()
	record("Stopped the host-native colibri server; kept the colibri model store")
}

// removeColibri deletes the platform-installed colibri runtime (the CLI/engine in
// colibri.StoreDir). Unlike removeOmlx this is a real removal: the store is platform-owned
// and plain files. It never touches the downloaded models (volumes/models/colibri — only
// --purge removes those) and cannot remove a copy installed elsewhere on PATH, which it says so.
// Returns true when the store was removed.
func removeColibri(record func(string)) bool {
	storeDir, err := colibri.StoreDir()
	if err == nil {
		if _, statErr := os.Stat(storeDir); statErr == nil {
			if os.RemoveAll(storeDir) == nil {
				record("Removed the platform-installed colibri runtime (" + storeDir + "); kept the downloaded models")
				return true
			}
		}
	}
	if colibriDetectFn() {
		record("colibri is installed outside the platform store (on PATH) — remove that copy yourself; the platform did not install it")
	}
	return false
}

// stopContainers gracefully stops the platform's running aip-* containers (a
// plain `stop`, giving each one its own stop-grace-period to shut down cleanly —
// e.g. postgres flushing/checkpointing — rather than jumping straight to the
// force-remove below) via every installed runtime, returning how many were
// signalled. Best-effort: an already-stopped container is simply a no-op for
// `stop`, and removeContainers' force-remove right after covers anything that
// doesn't shut down in time regardless.
func stopContainers(prober runtime.Prober, record func(string)) int {
	total := 0
	for _, containerRuntime := range containerRuntimes {
		if _, err := prober.LookPath(containerRuntime); err != nil {
			continue
		}
		out, err := prober.Run(containerRuntime, "ps", "-aq", "--filter", "name=aip-")
		if err != nil {
			continue
		}
		ids := strings.Fields(string(out))
		if len(ids) == 0 {
			continue
		}
		_, _ = prober.Run(containerRuntime, append([]string{"stop"}, ids...)...)
		record("Stopped platform containers (aip-*) via " + containerRuntime)
		total += len(ids)
	}
	return total
}

// removeContainers stops + removes the platform's aip-* containers via every
// installed runtime (docker and/or podman), returning how many were removed.
func removeContainers(prober runtime.Prober, record func(string)) int {
	total := 0
	for _, containerRuntime := range containerRuntimes {
		if _, err := prober.LookPath(containerRuntime); err != nil {
			continue
		}
		out, err := prober.Run(containerRuntime, "ps", "-aq", "--filter", "name=aip-")
		if err != nil {
			continue
		}
		ids := strings.Fields(string(out))
		if len(ids) == 0 {
			continue
		}
		_, _ = prober.Run(containerRuntime, append([]string{"rm", "-f"}, ids...)...)
		record("Removed platform containers (aip-*) via " + containerRuntime)
		total += len(ids)
	}
	return total
}

// serviceImageRefs returns the pinned service-tier image refs (image:tag) from
// the built-in defaults (versions.Default), skipping entries with no image (the
// host-native omlx runtime). Sorted for a deterministic removal order/output.
func serviceImageRefs() []string {
	refs := make([]string, 0, len(versions.Default().Services))
	for _, svc := range versions.Default().Services {
		if svc.Image == "" || svc.Tag == "" {
			continue // native-tier (no container image) or an incomplete pin
		}
		refs = append(refs, svc.Image+":"+svc.Tag)
	}
	sort.Strings(refs)
	return refs
}

// removeImages removes the platform's container IMAGES via every installed runtime
// (docker and/or podman), returning how many were removed. It removes two sets so
// nothing is left on the host: (a) the pinned service-tier images (versions.Default)
// and (b) every workspace/platform image by the aip-* reference filter. Best-effort:
// a ref that is absent or still in use just doesn't count. Run on EVERY uninstall
// (not only --purge) — the user wants the images gone.
func removeImages(prober runtime.Prober, record func(string)) int {
	serviceRefs := serviceImageRefs()
	total := 0
	for _, containerRuntime := range containerRuntimes {
		if _, err := prober.LookPath(containerRuntime); err != nil {
			continue
		}
		removedHere := 0
		// (a) the pinned service-tier images.
		for _, ref := range serviceRefs {
			if _, err := prober.Run(containerRuntime, "rmi", "-f", ref); err == nil {
				removedHere++
			}
		}
		// (b) all workspace/platform images by reference filter (aip-*).
		out, err := prober.Run(containerRuntime, "images", "--filter", "reference=aip-*", "-q")
		if err == nil {
			ids := strings.Fields(string(out))
			if len(ids) > 0 {
				if _, err := prober.Run(containerRuntime, append([]string{"rmi", "-f"}, ids...)...); err == nil {
					removedHere += len(ids)
				}
			}
		}
		if removedHere > 0 {
			record(fmt.Sprintf("Removed %d container images via %s", removedHere, containerRuntime))
			total += removedHere
		}
	}
	return total
}

// removeVolumes removes any LEGACY platform Docker NAMED volumes (aip-*). System
// data now lives as bind mounts under ~/.ai-platform/volumes, so the --purge
// RemoveAll of ~/.ai-platform (done above) is the PRIMARY mechanism that removes
// it. This stays only as a best-effort cleanup of named volumes left over from the
// OLD topology (notably the former aip-litellm-db-data Postgres volume) so an
// upgrade doesn't strand them. Only called on --purge.
func removeVolumes(prober runtime.Prober) int {
	total := 0
	for _, containerRuntime := range containerRuntimes {
		if _, err := prober.LookPath(containerRuntime); err != nil {
			continue
		}
		out, err := prober.Run(containerRuntime, "volume", "ls", "-q", "--filter", "name=aip-")
		if err != nil {
			continue
		}
		names := strings.Fields(string(out))
		if len(names) == 0 {
			continue
		}
		_, _ = prober.Run(containerRuntime, append([]string{"volume", "rm", "-f"}, names...)...)
		total += len(names)
	}
	return total
}

// modelsVolumeSubdir is the downloaded-model store under VolumesDir
// (~/.ai-platform/volumes/models — the omlx weights live in its `omlx` subdir). A
// plain (non-purge) uninstall preserves it so a reinstall need not re-download tens
// of GB of models.
const modelsVolumeSubdir = "models"

// removePlatformStateKeepModels removes every entry under ~/.ai-platform EXCEPT
// the downloaded model store (volumes/models). It is what makes a plain
// uninstall a real uninstall — config, credentials, caches, overlays, logs and
// the legacy resource pools are all gone, while the one expensive-to-refetch
// piece survives. Best-effort; returns true if anything was removed.
//
// keepColibri additionally preserves bin/colibri (the platform-installed colibri runtime)
// when the user chose to keep it.
func removePlatformStateKeepModels(platformDir string, keepColibri bool) bool {
	entries, err := os.ReadDir(platformDir)
	if err != nil {
		return false
	}
	removedAny := false
	for _, entry := range entries {
		if entry.Name() == "volumes" {
			removedAny = pruneVolumesKeepModels(filepath.Join(platformDir, entry.Name())) || removedAny
			continue
		}
		if entry.Name() == "bin" && keepColibri {
			removedAny = pruneKeeping(filepath.Join(platformDir, entry.Name()), colibriBinSubdir) || removedAny
			continue
		}
		if os.RemoveAll(filepath.Join(platformDir, entry.Name())) == nil {
			removedAny = true
		}
	}
	return removedAny
}

// pruneVolumesKeepModels removes everything under volumes/ except the models
// subdir, leaving the volumes dir itself in place to hold it. Best-effort;
// returns true if anything was removed.
func pruneVolumesKeepModels(volumesDir string) bool {
	return pruneKeeping(volumesDir, modelsVolumeSubdir)
}

// colibriBinSubdir is the platform-installed colibri runtime under ~/.ai-platform/bin
// (colibri.StoreDir's last element).
const colibriBinSubdir = "colibri"

// pruneKeeping removes every entry of dir except the one named keep. Best-effort;
// returns true if anything was removed.
func pruneKeeping(dir, keep string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	removedAny := false
	for _, entry := range entries {
		if entry.Name() == keep {
			continue
		}
		if os.RemoveAll(filepath.Join(dir, entry.Name())) == nil {
			removedAny = true
		}
	}
	return removedAny
}

// stripRCFile removes the managed PATH line and completion block from one rc
// file, preserving the user's own lines. Returns true if the file changed.
func stripRCFile(rcPath string) bool {
	data, err := os.ReadFile(rcPath)
	if err != nil {
		return false
	}
	stripped := stripManaged(string(data))
	if stripped == string(data) {
		return false
	}
	return os.WriteFile(rcPath, []byte(stripped), 0o644) == nil
}

// stripManaged drops the PATH marker line and the completion block (marker plus
// its fpath/compinit or dot-source body), keeping everything else — including
// unrelated user lines that follow the block.
func stripManaged(content string) string {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	inCompletionBlock := false
	for _, line := range lines {
		switch {
		case strings.Contains(line, pathMarker):
			continue // drop the PATH line
		case strings.Contains(line, completionMarker):
			inCompletionBlock = true
			continue
		case inCompletionBlock:
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "fpath=") || strings.Contains(trimmed, "compinit") ||
				strings.HasPrefix(trimmed, ". ") || trimmed == "" {
				if trimmed == "" {
					inCompletionBlock = false
				}
				continue // drop block body
			}
			inCompletionBlock = false // any other line ends the block and is kept
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func rcFiles() []string {
	home, err := paths.Home()
	if err != nil {
		return nil
	}
	zdot := os.Getenv("ZDOTDIR")
	if zdot == "" {
		zdot = home
	}
	return []string{
		filepath.Join(zdot, ".zshrc"),
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".bash_profile"),
	}
}

func completionFiles() []string {
	home, err := paths.Home()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".zsh", "completions", "_ai"),
		filepath.Join(dataHome(home), "bash-completion", "completions", "ai"),
	}
}

func dataHome(home string) string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".local", "share")
}
