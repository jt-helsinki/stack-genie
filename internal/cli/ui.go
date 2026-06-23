package cli

import (
	"os"

	"github.com/charmbracelet/x/term"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/tui"
	"github.com/spf13/cobra"
)

// newUICmd builds `ai ui`: the full-screen, K9s-style management UI. It detects
// the project at/above the cwd (the default scope) and lets the user manage the
// service tier and, when a project is selected, its workspace. The UI is
// interactive-only — it needs a real terminal and has no JSON envelope — so it is
// rejected under --json or when stdin/stderr is not a TTY (exit 2).
func newUICmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "ui",
		Short: "Open the full-screen management UI (K9s-style)",
		Long: "Open the full-screen, K9s-style management UI. It shows the host service\n" +
			"tier and container status, and — when run inside (or after selecting) a\n" +
			"project — that project's workspace. Use the menu (`:`) to switch views or\n" +
			"exit; `q` quits. Interactive only (a terminal is required; not available\n" +
			"with --json).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if emitter.JSON || !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stderr.Fd()) {
				*exit = emitter.Failure("ui", output.Errorf(output.ExitInvalidInput,
					"ai ui is interactive and needs a terminal (not available with --json or when piped)"))
				return nil
			}
			cwd, err := os.Getwd()
			if err != nil {
				*exit = emitter.Failure("ui", output.Errorf(output.ExitRuntimeFailure,
					"cannot determine the current directory: %s", err))
				return nil
			}
			if err := tui.Run(cwd); err != nil {
				*exit = emitter.Failure("ui", output.Errorf(output.ExitRuntimeFailure, "ui: %s", err))
				return nil
			}
			// Interactive session: no stdout envelope on a clean exit.
			*exit = output.ExitOK
			return nil
		},
	}
}
