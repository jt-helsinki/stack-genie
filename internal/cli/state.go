package cli

import (
	"os"

	"github.com/jt-helsinki/ideal-robot/internal/config"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/state"
	"github.com/spf13/cobra"
)

// newStateCmd builds `ai state` and its subcommands (CLI spec §14). The shared
// emitter renders results; *exit carries the chosen exit code back to Execute.
func newStateCmd(em *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Inspect and repair platform state",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help() // `ai state` with no subcommand prints help (§17.0)
		},
	}
	cmd.AddCommand(newStateShowCmd(em, exit), newStateRepairCmd(em, exit))
	return cmd
}

func newStateShowCmd(em *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show current platform state (projects, runtime handles, effective config)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				*exit = em.Failure("state.show", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			snap, err := state.Show(cwd)
			if err != nil {
				*exit = em.Failure("state.show", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			projectRoot := ""
			if snap.Project != nil {
				projectRoot = snap.Project.Root
			}
			cfg, err := config.Load(projectRoot)
			if err != nil {
				*exit = em.Failure("state.show", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			*exit = em.Success("state.show", map[string]any{
				"projects": snap.Projects,
				"project":  snap.Project,
				"config":   cfg,
			})
			return nil
		},
	}
}

func newStateRepairCmd(em *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "repair",
		Short: "Rebuild host-local state from the filesystem",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			rep, err := state.Repair()
			if err != nil {
				*exit = em.Failure("state.repair", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			*exit = em.Success("state.repair", rep)
			return nil
		},
	}
}
