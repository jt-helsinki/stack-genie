package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/uninstall"
	"github.com/spf13/cobra"
)

// newUninstallCmd builds `ai uninstall` (CLI §2.2): a native, offline teardown —
// the inverse of install + setup. It streams status/progress as each step runs,
// asks per external dependency (msb, clawpatrol) whether to remove it too, then
// the process exits when finished (the running binary removes itself; its inode
// survives until exit). It never touches ~/projects.
func newUninstallCmd(em *output.Emitter, exit *int) *cobra.Command {
	var purge, removeDeps bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall the platform (binary, PATH/completion entries, containers; --purge also removes state)",
		Long: "Remove the platform from this host. Stops the platform containers, removes\n" +
			"the ai binary and the PATH/completion entries, and (with --purge) the\n" +
			"platform state under ~/.ai-platform and ~/.clawpatrol. Runs entirely from\n" +
			"this binary — no network or external script. On a terminal it asks, per\n" +
			"external dependency (msb, clawpatrol), whether to uninstall it too. Never\n" +
			"touches ~/projects.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			prober := runtime.RealProber()

			if dryRun, _ := cmd.Flags().GetBool("dry-run"); dryRun {
				*exit = em.Success("uninstall", uninstallResult{
					Purged:   purge,
					Plan:     uninstall.Plan(purge),
					LeftDeps: presentDepNames(prober),
				})
				return nil
			}

			// Confirm before doing anything destructive. On a terminal we ask;
			// --yes skips the prompt. Without a terminal and without --yes there
			// is no way to confirm (e.g. --json / automation), so require --yes.
			interactive := !em.JSON && term.IsTerminal(os.Stdin.Fd())
			confirmed, _ := cmd.Flags().GetBool("yes") // global --yes (§20)
			if !confirmed {
				if !interactive {
					*exit = em.Failure("uninstall", output.Errorf(output.ExitInvalidInput,
						"destructive: pass --yes to confirm (no terminal available to prompt)"))
					return nil
				}
				if !confirmUninstall(purge) {
					_, _ = fmt.Fprintln(em.Err, "Uninstall cancelled — nothing was changed.")
					*exit = em.Success("uninstall", uninstallResult{Aborted: true})
					return nil
				}
			}

			// Decide which external dependencies to also remove: --remove-deps
			// takes all detected ones non-interactively; otherwise ask per
			// dependency on a real terminal. With neither, they are left in place.
			var toRemove []uninstall.ExternalDep
			var leftDeps []string
			for _, dep := range uninstall.ExternalDeps() {
				if !dep.Present(prober) {
					continue
				}
				switch {
				case removeDeps:
					toRemove = append(toRemove, dep)
				case interactive && confirmRemoveDep(dep):
					toRemove = append(toRemove, dep)
				default:
					leftDeps = append(leftDeps, dep.Name)
				}
			}

			binaryPath, _ := os.Executable()
			var progress uninstall.Progress
			if !em.JSON {
				_, _ = fmt.Fprintln(em.Err, "Uninstalling the AI Development Platform…")
				progress = func(line string) { _, _ = fmt.Fprintln(em.Err, line) }
			}

			report, err := uninstall.Run(
				uninstall.Options{Purge: purge, BinaryPath: binaryPath, RemoveDeps: toRemove},
				prober,
				progress,
			)
			if err != nil {
				*exit = em.Failure("uninstall", output.Errorf(output.ExitRuntimeFailure, "uninstall: %s", err))
				return nil
			}

			*exit = em.Success("uninstall", uninstallResult{
				Purged:            report.Purged,
				RemovedContainers: report.RemovedContainers,
				CleanedRC:         report.CleanedRC,
				RemovedBinary:     report.RemovedBinary,
				RemovedDeps:       report.RemovedDeps,
				LeftDeps:          leftDeps,
				LogPath:           report.LogPath,
			})
			return nil
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false,
		"also remove ~/.ai-platform and ~/.clawpatrol (never ~/projects)")
	cmd.Flags().BoolVar(&removeDeps, "remove-deps", false,
		"also uninstall the external dependencies (msb, clawpatrol) without prompting")
	return cmd
}

// confirmUninstall asks for top-level confirmation before the teardown,
// summarizing what will be removed. Defaults to no.
func confirmUninstall(purge bool) bool {
	description := "Removes the ai binary, PATH/completion entries, and platform containers."
	if purge {
		description += " Also removes platform state (~/.ai-platform, ~/.clawpatrol)."
	}
	description += " Never touches ~/projects."
	var yes bool
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Uninstall the AI Development Platform?").
			Description(description).
			Affirmative("Yes, uninstall").
			Negative("Cancel").
			Value(&yes),
	))
	if err := form.Run(); err != nil {
		return false
	}
	return yes
}

// confirmRemoveDep asks whether to also uninstall one external dependency,
// showing exactly what would be removed. Defaults to no.
func confirmRemoveDep(dep uninstall.ExternalDep) bool {
	var yes bool
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Also uninstall " + dep.Name + "?").
			Description("Removes: " + strings.Join(dep.Targets, ", ")).
			Affirmative("Yes, remove it").
			Negative("No, keep it").
			Value(&yes),
	))
	if err := form.Run(); err != nil {
		return false
	}
	return yes
}

// presentDepNames lists the external dependencies currently detected on the host
// (for the --dry-run plan, so the user knows what they'd be asked about).
func presentDepNames(prober runtime.Prober) []string {
	var names []string
	for _, dep := range uninstall.ExternalDeps() {
		if dep.Present(prober) {
			names = append(names, dep.Name)
		}
	}
	return names
}

// uninstallResult is the `ai uninstall` payload.
type uninstallResult struct {
	Purged            bool     `json:"purged"`
	RemovedContainers int      `json:"removed_containers,omitempty"`
	CleanedRC         []string `json:"cleaned_rc,omitempty"`
	RemovedBinary     string   `json:"removed_binary,omitempty"`
	RemovedDeps       []string `json:"removed_deps,omitempty"`
	LeftDeps          []string `json:"left_deps,omitempty"`
	LogPath           string   `json:"log_path,omitempty"`
	Aborted           bool     `json:"aborted,omitempty"` // user declined the confirmation
	Plan              []string `json:"plan,omitempty"`    // set only for --dry-run
}

// Human renders a clear completion (or plan) summary for non-JSON output.
func (result uninstallResult) Human() string {
	if result.Aborted {
		return "Uninstall cancelled — nothing was changed."
	}
	if len(result.Plan) > 0 {
		lines := []string{"Dry run — would uninstall by:"}
		for _, step := range result.Plan {
			lines = append(lines, "  • "+step)
		}
		if len(result.LeftDeps) > 0 {
			lines = append(lines, "External dependencies detected (you'd be asked about each): "+strings.Join(result.LeftDeps, ", "))
		}
		lines = append(lines, "(nothing was changed)")
		return strings.Join(lines, "\n")
	}

	tail := "Platform state was kept — re-run with --purge to remove ~/.ai-platform and ~/.clawpatrol."
	if result.Purged {
		tail = "Removed platform state (~/.ai-platform, ~/.clawpatrol)."
	}
	summary := "Uninstall complete. " + tail
	if len(result.RemovedDeps) > 0 {
		summary += " Also uninstalled: " + strings.Join(result.RemovedDeps, ", ") + "."
	}
	if len(result.LeftDeps) > 0 {
		summary += " Left in place: " + strings.Join(result.LeftDeps, ", ") + " (re-run with --remove-deps to remove)."
	}
	summary += " Your projects under ~/projects were left untouched."
	if result.LogPath != "" {
		summary += " Log: " + result.LogPath + "."
	}
	return summary + " Restart your shell to drop the stale PATH entry."
}
