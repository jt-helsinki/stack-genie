// Package uninstall removes the platform's footprint — the inverse of install +
// setup — natively from the running binary, with no network or external script.
// It stops the platform containers, strips the managed shell-rc lines, removes
// the completion scripts and the binary, and (with Purge) the platform state.
// It NEVER touches your project directories (your source, created anywhere).
package uninstall

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jt-helsinki/stack-genie/internal/paths"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
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

	// RemoveDeps are the external dependencies (msb) the user opted to also
	// uninstall. The caller decides this (per-dependency prompt or flag); Run just
	// removes their on-disk artifacts.
	RemoveDeps []ExternalDep
}

// Report describes what was removed.
type Report struct {
	StoppedWorkspaces int      `json:"stopped_workspaces"`
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
	Name    string   // human label, e.g. "msb (Microsandbox)"
	Binary  string   // command name used to detect it on PATH
	Targets []string // files/dirs removed when the user opts in
}

// ExternalDeps returns the external dependencies `ai setup` may install, with
// their removal targets resolved against the environment (HOME, MSB_HOME). msb
// installs to $MSB_HOME (default ~/.microsandbox) with ~/.local/bin symlinks.
func ExternalDeps() []ExternalDep {
	home, err := paths.Home()
	if err != nil {
		return nil
	}
	localBin := filepath.Join(home, ".local", "bin")

	msbHome := os.Getenv("MSB_HOME")
	if msbHome == "" {
		msbHome = filepath.Join(home, ".microsandbox")
	}

	return []ExternalDep{
		{
			Name:   "msb (Microsandbox)",
			Binary: "msb",
			Targets: []string{
				msbHome,
				filepath.Join(localBin, "msb"),
				filepath.Join(localBin, "microsandbox"),
			},
		},
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

	// Stop any running workspace microVMs FIRST — before removeContainers/removeImages
	// and before a possible --remove-deps msb uninstall (so msb is still present to run
	// `stop`). This only halts the VM runtime instances; workspace DATA (project source
	// + /persist overlays are host bind mounts) is deliberately left untouched.
	report.StoppedWorkspaces = stopWorkspaces(prober, record)
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

	// External dependencies the user opted to also uninstall (msb).
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
		} else if removePlatformStateKeepModels(platformDir) {
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
		"stop and remove platform containers (aip-*)",
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
	steps = append(steps, "ask, per external dependency (msb), whether to uninstall it too")
	steps = append(steps, "leave your project directories untouched")
	return steps
}

func emit(progress Progress, line string) {
	if progress != nil {
		progress(line)
	}
}

// msbBinary resolves the Microsandbox CLI WITHOUT importing internal/workspace
// (which would risk an import cycle): the platform-managed pinned binary under
// ~/.ai-platform/bin/msb if it exists, else bare "msb" on PATH.
func msbBinary() string {
	if binDir, err := paths.BinDir(); err == nil {
		candidate := filepath.Join(binDir, "msb")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "msb"
}

// parseMsbWorkspaceNames extracts the aip-* sandbox names from `msb list` output:
// the first whitespace-delimited field of each line, skipping a "NAME" header row
// and any non-aip- entry (workspace VMs are named aip-<project>).
func parseMsbWorkspaceNames(listOutput string) []string {
	var names []string
	for _, line := range strings.Split(listOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if name == "NAME" {
			continue // header row
		}
		if strings.HasPrefix(name, "aip-") {
			names = append(names, name)
		}
	}
	return names
}

// stopWorkspaces stops (but never DELETES) any running workspace microVMs so an
// uninstall leaves no VM instances running, while PRESERVING all workspace DATA:
// project source and the /persist overlays are host bind mounts, untouched here —
// this only halts the runtime instance via `msb stop`, never `msb delete` and never
// a RemoveAll of any overlay/project dir. Workspace VMs are named aip-<project>; it
// parses `msb list`'s NAME column for aip-* entries and `msb stop -f <name>`s each.
// Best-effort: a missing msb, an unreadable list, or a stop error is not fatal. It
// must run EARLY in Run (before msb might be --remove-deps uninstalled). Returns how
// many it stopped.
func stopWorkspaces(prober runtime.Prober, record func(string)) int {
	msb := msbBinary()
	if msb == "msb" {
		// Only a bare name resolved — if it is not on PATH there is nothing to do.
		if _, err := prober.LookPath("msb"); err != nil {
			return 0
		}
	}
	out, err := prober.Run(msb, "list")
	if err != nil {
		return 0
	}
	stopped := 0
	for _, name := range parseMsbWorkspaceNames(string(out)) {
		if _, err := prober.Run(msb, "stop", "-f", name); err == nil {
			stopped++
		}
	}
	if stopped > 0 {
		record(fmt.Sprintf("Stopped %d workspace microVM(s) (data preserved)", stopped))
	}
	return stopped
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
// native microsandbox runtime). Sorted for a deterministic removal order/output.
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
// (~/.ai-platform/volumes/models). Kept verbatim in sync with
// setup.ollamaModelsVolume; a plain (non-purge) uninstall preserves it so a
// reinstall need not re-download tens of GB of models.
const modelsVolumeSubdir = "models"

// removePlatformStateKeepModels removes every entry under ~/.ai-platform EXCEPT
// the downloaded model store (volumes/models). It is what makes a plain
// uninstall a real uninstall — config, credentials, caches, overlays, logs and
// the legacy resource pools are all gone, while the one expensive-to-refetch
// piece survives. Best-effort; returns true if anything was removed.
func removePlatformStateKeepModels(platformDir string) bool {
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
	entries, err := os.ReadDir(volumesDir)
	if err != nil {
		return false
	}
	removedAny := false
	for _, entry := range entries {
		if entry.Name() == modelsVolumeSubdir {
			continue
		}
		if os.RemoveAll(filepath.Join(volumesDir, entry.Name())) == nil {
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
