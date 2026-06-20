// Package uninstall removes the platform's footprint — the inverse of install +
// setup — natively from the running binary, with no network or external script.
// It stops the platform containers, strips the managed shell-rc lines, removes
// the completion scripts and the binary, and (with Purge) the platform state.
// It NEVER touches ~/projects (the user's source).
package uninstall

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
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
	Purge bool // also remove ~/.ai-platform and ~/.clawpatrol

	// BinaryPath is the ai binary to remove (normally os.Executable()). The
	// caller injects it so tests need not delete the test binary; empty skips it.
	BinaryPath string

	// RemoveDeps are the external dependencies (msb, clawpatrol) the user opted
	// to also uninstall. The caller decides this (per-dependency prompt or flag);
	// Run just removes their on-disk artifacts.
	RemoveDeps []ExternalDep
}

// Report describes what was removed.
type Report struct {
	RemovedContainers int      `json:"removed_containers"`
	CleanedRC         []string `json:"cleaned_rc"`
	RemovedBinary     string   `json:"removed_binary,omitempty"`
	RemovedDeps       []string `json:"removed_deps,omitempty"`
	Purged            bool     `json:"purged"`
	LogPath           string   `json:"log_path,omitempty"` // ~/ai-uninstall.log
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
// their removal targets resolved against the environment (HOME, MSB_HOME,
// CLAWPATROL_PREFIX). msb installs to $MSB_HOME (default ~/.microsandbox) with
// ~/.local/bin symlinks; clawpatrol installs its binary to $CLAWPATROL_PREFIX
// (default ~/.local/bin), keeps state in ~/.clawpatrol, and on macOS installs a
// system-extension app bundle.
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
	clawPrefix := os.Getenv("CLAWPATROL_PREFIX")
	if clawPrefix == "" {
		clawPrefix = localBin
	}
	clawState, _ := paths.ClawPatrolDir()

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
		{
			Name:   "clawpatrol",
			Binary: "clawpatrol",
			Targets: []string{
				filepath.Join(clawPrefix, "clawpatrol"),
				clawState,
				"/Applications/Clawpatrol.app",
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

	report.RemovedContainers = removeContainers(prober, record)

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

	// External dependencies the user opted to also uninstall (msb, clawpatrol).
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

	if options.Purge {
		if platformDir, err := paths.PlatformDir(); err == nil {
			_ = os.RemoveAll(platformDir)
		}
		if clawDir, err := paths.ClawPatrolDir(); err == nil {
			_ = os.RemoveAll(clawDir)
		}
		report.Purged = true
		record("Purged ~/.ai-platform and ~/.clawpatrol (your projects were left untouched)")
	} else {
		record("Left ~/.ai-platform and ~/.clawpatrol in place — re-run with --purge to remove them")
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
		"stop and remove platform containers (aip-*)",
		"remove the ai binary",
		"remove the shell completion scripts",
		"strip the managed PATH/completion lines from the shell rc files",
	}
	if purge {
		steps = append(steps, "remove platform state (~/.ai-platform and ~/.clawpatrol)")
	} else {
		steps = append(steps, "keep platform state (~/.ai-platform, ~/.clawpatrol) — pass --purge to remove")
	}
	steps = append(steps, "ask, per external dependency (msb, clawpatrol), whether to uninstall it too")
	steps = append(steps, "leave ~/projects untouched")
	return steps
}

func emit(progress Progress, line string) {
	if progress != nil {
		progress(line)
	}
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
		filepath.Join(configHome(home), "fish", "config.fish"),
	}
}

func completionFiles() []string {
	home, err := paths.Home()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".zsh", "completions", "_ai"),
		filepath.Join(configHome(home), "fish", "completions", "ai.fish"),
		filepath.Join(dataHome(home), "bash-completion", "completions", "ai"),
		filepath.Join(configHome(home), "powershell", "ai.completion.ps1"),
	}
}

func configHome(home string) string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".config")
}

func dataHome(home string) string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".local", "share")
}
