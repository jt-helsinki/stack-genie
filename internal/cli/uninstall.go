package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/jt-helsinki/ideal-robot/internal/uninstall"
	"github.com/spf13/cobra"
)

// newUninstallCmd builds `ai uninstall` (CLI §2.2): a native, offline teardown —
// the inverse of install + setup. It streams status/progress as each step runs,
// asks per external dependency (msb) whether to remove it too, then the process
// exits when finished (the running binary removes itself; its inode survives
// until exit). It never touches your project directories.
func newUninstallCmd(em *output.Emitter, exit *int) *cobra.Command {
	var purge, removeDeps bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall the platform (binary, PATH/completion entries, containers; --purge also removes state)",
		Long: "Remove the platform from this host. Stops the platform containers, removes\n" +
			"the ai binary and the PATH/completion entries, and (with --purge) the\n" +
			"platform state under ~/.ai-platform. Runs entirely from this binary — no\n" +
			"network or external script. On a terminal it asks, per external dependency\n" +
			"(msb), whether to uninstall it too. Never touches your project directories\n" +
			"(your source lives wherever you created it).",
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
				confirmedPrompt, promptErr := confirmUninstall()
				if promptErr != nil {
					*exit = em.Failure("uninstall", promptErr)
					return nil
				}
				if !confirmedPrompt {
					_, _ = fmt.Fprintln(em.Err, "Uninstall cancelled — nothing was changed.")
					*exit = em.Success("uninstall", uninstallResult{Aborted: true})
					return nil
				}
				// Whether to also remove platform state (~/.ai-platform) is an
				// interactive input; the --purge flag seeds its default (Yes when set,
				// No otherwise), per the §1.8 flags-seed-the-prompt convention.
				purgeAnswer, purgeErr := promptConfirmDefault(
					"Also remove platform state (~/.ai-platform)?",
					"Deletes downloaded models, config, credentials and runtime state. "+
						"Your project directories are left untouched.",
					purge,
				)
				if purgeErr != nil {
					*exit = em.Failure("uninstall", purgeErr)
					return nil
				}
				purge = purgeAnswer
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
			runOpts := uninstall.Options{Purge: purge, BinaryPath: binaryPath, RemoveDeps: toRemove}
			var report uninstall.Report
			var err error
			if ui.Enabled(em) {
				err = ui.RunSteps(em.Err, "Uninstalling the AI Development Platform", func(emit func(step string)) error {
					var runErr error
					report, runErr = uninstall.Run(runOpts, prober, uninstall.Progress(emit))
					return runErr
				})
			} else {
				var progress uninstall.Progress
				if !em.JSON {
					_, _ = fmt.Fprintln(em.Err, "Uninstalling the AI Development Platform…")
					progress = func(line string) { _, _ = fmt.Fprintln(em.Err, line) }
				}
				report, err = uninstall.Run(runOpts, prober, progress)
			}
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
		"also remove ~/.ai-platform; on a terminal this is prompted and --purge seeds the default to yes (never your project directories)")
	cmd.Flags().BoolVar(&removeDeps, "remove-deps", false,
		"also uninstall the external dependencies (msb) without prompting")
	return cmd
}

// confirmUninstall asks for top-level confirmation before the teardown. Defaults
// to no; a user abort is treated as a decline. Whether to also remove platform
// state is a separate prompt (the purge question), so it is not summarized here.
func confirmUninstall() (bool, error) {
	return promptConfirm("Uninstall the AI Development Platform?",
		"Removes the ai binary, PATH/completion entries, and platform containers. "+
			"Never touches your project directories (your source).")
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
			lines = append(lines, "external dependencies detected — you'd be prompted about each: "+strings.Join(result.LeftDeps, ", "))
		}
		lines = append(lines, "(nothing was changed)")
		return strings.Join(lines, "\n")
	}

	tail := "Platform state was kept — re-run with --purge to remove ~/.ai-platform."
	if result.Purged {
		tail = "Removed platform state (~/.ai-platform)."
	}
	summary := "Uninstall complete. " + tail
	if len(result.RemovedDeps) > 0 {
		summary += " Also uninstalled: " + strings.Join(result.RemovedDeps, ", ") + "."
	}
	if len(result.LeftDeps) > 0 {
		summary += " Left in place: " + strings.Join(result.LeftDeps, ", ") + " (re-run with --remove-deps to remove)."
	}
	summary += " Your project directories (your source) were left untouched."
	if result.LogPath != "" {
		summary += " Log: " + result.LogPath + "."
	}
	return summary + " Restart your shell to drop the stale PATH entry."
}
