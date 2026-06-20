package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/spf13/cobra"
)

// installScriptURL is the canonical installer. `ai uninstall` fetches it and
// pipes it to bash with --uninstall, mirroring the curl|bash install path
// (CLI §1.5/§1.6) so there is a single source of truth for the teardown logic.
// Override for forks/mirrors with AIP_INSTALL_SCRIPT_URL.
const installScriptURL = "https://raw.githubusercontent.com/jt-helsinki/ideal-robot/main/installers/install.sh"

// newUninstallCmd builds `ai uninstall` (CLI §2.2): the inverse of the curl|bash
// install. It streams the installer's progress to the user, then the process
// exits when the teardown finishes (the running binary removes itself).
func newUninstallCmd(em *output.Emitter, exit *int) *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall the platform (curl | bash of the installer with --uninstall)",
		Long: "Remove the platform the same way it was installed: fetch the installer and\n" +
			"run it with --uninstall. It stops the platform containers, removes the ai\n" +
			"binary and the PATH/completion entries, and (with --purge) the platform\n" +
			"state under ~/.ai-platform and ~/.clawpatrol. It never touches ~/projects.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			scriptURL := installScriptURL
			if override := os.Getenv("AIP_INSTALL_SCRIPT_URL"); override != "" {
				scriptURL = override
			}
			pipeline := "curl -fsSL " + scriptURL + " | bash -s -- --uninstall"
			if purge {
				pipeline += " --purge"
			}

			if dryRun, _ := cmd.Flags().GetBool("dry-run"); dryRun {
				*exit = em.Success("uninstall", uninstallResult{Purged: purge, Script: scriptURL, Planned: pipeline})
				return nil
			}

			confirmed, _ := cmd.Flags().GetBool("yes") // global --yes (§20)
			if !confirmed {
				*exit = em.Failure("uninstall", output.Errorf(output.ExitInvalidInput,
					"destructive: removes the ai binary, PATH/completion entries, and platform containers — pass --yes to confirm (never touches ~/projects)"))
				return nil
			}

			// Stream the installer's progress live: it reports each step
			// (containers, binary, rc files, purge) on stderr as it runs.
			_, _ = fmt.Fprintln(em.Err, "Uninstalling the AI Development Platform…")
			command := exec.Command("bash", "-c", pipeline) // #nosec G204 — fixed pipeline, scriptURL is operator-controlled
			command.Stdout = em.Err
			command.Stderr = em.Err
			command.Stdin = os.Stdin
			if err := command.Run(); err != nil {
				*exit = em.Failure("uninstall", output.Errorf(output.ExitRuntimeFailure,
					"uninstall failed (is curl/bash available and the network reachable?): %s", err))
				return nil
			}

			*exit = em.Success("uninstall", uninstallResult{Purged: purge, Script: scriptURL})
			return nil
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false,
		"also remove ~/.ai-platform and ~/.clawpatrol (never ~/projects)")
	return cmd
}

// uninstallResult is the `ai uninstall` payload.
type uninstallResult struct {
	Purged  bool   `json:"purged"`
	Script  string `json:"script"`
	Planned string `json:"planned,omitempty"` // set only for --dry-run
}

// Human renders a clear completion (or plan) line for non-JSON output.
func (result uninstallResult) Human() string {
	if result.Planned != "" {
		return "Dry run — would uninstall by running:\n  " + result.Planned + "\n(nothing was changed)"
	}
	if result.Purged {
		return "Uninstall complete. Removed the binary, PATH/completion entries, containers, and platform state (~/.ai-platform, ~/.clawpatrol). Your projects under ~/projects were left untouched. Restart your shell to drop the stale PATH entry."
	}
	return "Uninstall complete. Removed the binary, PATH/completion entries, and containers. Platform state was kept — re-run with --purge to remove ~/.ai-platform and ~/.clawpatrol. Your projects were left untouched. Restart your shell to drop the stale PATH entry."
}
