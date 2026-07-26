package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/runtime"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/jt-helsinki/stack-genie/internal/uihosts"
	"github.com/jt-helsinki/stack-genie/internal/uninstall"
	"github.com/spf13/cobra"
)

// preauthorizeHostsSudo refreshes the sudo timestamp on the NORMAL terminal BEFORE the
// bubbletea progress (ui.RunSteps) takes over. The teardown removes the platform's
// /etc/hosts UI-subdomain block via `sudo tee`, but that runs INSIDE the spinner, where
// the terminal is in raw mode owned by bubbletea — so sudo's password prompt never
// receives the user's keystrokes ("does not accept my password"). Validating sudo here
// (plain terminal) caches the credential so the later write runs without prompting.
// Best-effort: only on a TTY, only when a managed block is actually present; a failure
// (or sudoers with no timestamp caching) just falls back to removeHostsBlock printing
// the manual command.
func preauthorizeHostsSudo(em *output.Emitter, interactive bool) {
	if !interactive {
		return
	}
	present, _, err := uihosts.HostsStatus(uihosts.DefaultHostsPath, "")
	if err != nil || !present {
		return
	}
	_, _ = fmt.Fprintln(em.Err, "Removing the platform's /etc/hosts block needs sudo.")
	command := exec.Command("sudo", "-v", "-p", "[ai] enter your login password to update /etc/hosts: ")
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stderr, os.Stderr
	_ = command.Run()
}

// newUninstallCmd builds `ai uninstall` (CLI §2.2): a native, offline teardown —
// the inverse of install + setup. It streams status/progress as each step runs,
// asks per external dependency (msb) whether to remove it too, then the process
// exits when finished (the running binary removes itself; its inode survives
// until exit). It never touches your project directories.
func newUninstallCmd(em *output.Emitter, exit *int) *cobra.Command {
	var purge, removeDeps bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall the platform (binary, PATH/completion entries, containers, state; --purge also removes models)",
		Long: "Remove the platform from this host. Stops the platform containers, removes\n" +
			"the ai binary, the PATH/completion entries, and the platform state under\n" +
			"~/.ai-platform — keeping only the downloaded models (volumes/models); pass\n" +
			"--purge to remove those too. Runs entirely from this binary — no\n" +
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
					_, _ = fmt.Fprintln(em.Err, ui.Muted.Render("Uninstall cancelled — nothing was changed."))
					*exit = em.Success("uninstall", uninstallResult{Aborted: true})
					return nil
				}
				// A plain uninstall already removes ~/.ai-platform EXCEPT the
				// downloaded models; --purge additionally deletes those. This prompt
				// is only about the models. The --purge flag seeds its default (Yes
				// when set, No otherwise), per the §1.8 flags-seed-the-prompt convention.
				purgeAnswer, purgeErr := promptConfirmDefault(
					"Also remove the downloaded models (volumes/models)?",
					"Platform state (config, credentials, caches, overlays) is removed either way. "+
						"Say yes to also delete the downloaded models — otherwise they are kept for a reinstall. "+
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
			// Acquire sudo for the /etc/hosts removal NOW, on the plain terminal, before
			// the progress UI grabs it (the actual write runs inside ui.RunSteps, where a
			// sudo prompt could not read the password).
			preauthorizeHostsSudo(em, interactive)
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
				RemovedState:      report.RemovedState,
				StoppedWorkspaces: report.StoppedWorkspaces,
				RemovedContainers: report.RemovedContainers,
				RemovedImages:     report.RemovedImages,
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
		"also remove the downloaded models (volumes/models); a plain uninstall already removes the rest of ~/.ai-platform (never your project directories)")
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
	)).WithTheme(ui.HuhTheme()).WithWidth(formWidth())
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
	RemovedState      bool     `json:"removed_state,omitempty"`
	StoppedWorkspaces int      `json:"stopped_workspaces,omitempty"`
	RemovedContainers int      `json:"removed_containers,omitempty"`
	RemovedImages     int      `json:"removed_images,omitempty"`
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
		return ui.Muted.Render("Uninstall cancelled — nothing was changed.")
	}
	if len(result.Plan) > 0 {
		lines := []string{ui.Heading.Render("Dry run — would uninstall by:")}
		for _, step := range result.Plan {
			lines = append(lines, "  "+ui.Muted.Render(ui.IconDot)+" "+ui.Value.Render(step))
		}
		if len(result.LeftDeps) > 0 {
			lines = append(lines, "external dependencies detected — you'd be prompted about each: "+ui.Value.Render(strings.Join(result.LeftDeps, ", ")))
		}
		lines = append(lines, ui.Muted.Render("(nothing was changed)"))
		return strings.Join(lines, "\n")
	}

	tail := "Removed platform state (" + ui.Value.Render("~/.ai-platform") + "); kept downloaded models — re-run with --purge to remove them too."
	if result.Purged {
		tail = "Removed all platform state (" + ui.Value.Render("~/.ai-platform") + "), including downloaded models."
	}
	summary := ui.Success.Render(ui.IconOK+" Uninstall complete.") + " " + tail
	if result.StoppedWorkspaces > 0 {
		summary += fmt.Sprintf(" Stopped %d workspace microVM(s) (data preserved).", result.StoppedWorkspaces)
	}
	if result.RemovedImages > 0 {
		summary += fmt.Sprintf(" Removed %d container image(s).", result.RemovedImages)
	}
	if len(result.RemovedDeps) > 0 {
		summary += " Also uninstalled: " + ui.Value.Render(strings.Join(result.RemovedDeps, ", ")) + "."
	}
	if len(result.LeftDeps) > 0 {
		summary += " Left in place: " + ui.Value.Render(strings.Join(result.LeftDeps, ", ")) + " (re-run with --remove-deps to remove)."
	}
	summary += " Your project directories (your source) were left untouched."
	if result.LogPath != "" {
		summary += " Log: " + ui.Value.Render(result.LogPath) + "."
	}
	return summary + " Restart your shell to drop the stale PATH entry."
}
