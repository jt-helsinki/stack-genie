package cli

import (
	"errors"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/state"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
	"github.com/spf13/cobra"
)

// newWorkspaceCmd builds `ai workspace` and its subcommands (CLI §4).
func newWorkspaceCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workspace",
		Short: "Manage the project workspace microVM",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		newWorkspaceListCmd(emitter, exit),
		newWorkspaceStartCmd(emitter, exit),
		newWorkspaceStopCmd(emitter, exit),
		newWorkspaceDestroyCmd(emitter, exit),
		newWorkspaceExecCmd(emitter, exit),
	)
	return cmd
}

// mapWorkspaceErr maps lifecycle errors to exit codes (§18).
func mapWorkspaceErr(err error) error {
	switch {
	case errors.Is(err, workspace.ErrUnknownProject):
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	case errors.Is(err, workspace.ErrContainerRuntimeMissing), errors.Is(err, workspace.ErrMsbMissing):
		return output.Errorf(output.ExitMissingDep, "%s", err)
	default:
		return output.Errorf(output.ExitRuntimeFailure, "%s", err)
	}
}

func newWorkspaceListCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List workspaces across all projects",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			index, err := state.LoadIndex()
			if err != nil {
				*exit = emitter.Failure("workspace.list", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			workspaces := []state.Workspace{}
			for _, entry := range index.Projects {
				perProject, err := state.OpenStore(entry.Path).ListWorkspaces()
				if err != nil {
					*exit = emitter.Failure("workspace.list", output.Errorf(output.ExitRuntimeFailure, "%s", err))
					return nil
				}
				workspaces = append(workspaces, perProject...)
			}
			*exit = emitter.Success("workspace.list", map[string]any{"workspaces": workspaces})
			return nil
		},
	}
}

func newWorkspaceStartCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "start <project>",
		Short: "Build the image and start the project's workspace microVM",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			handle, err := workspace.RealManager(nowRFC3339).Start(args[0])
			if err != nil {
				*exit = emitter.Failure("workspace.start", mapWorkspaceErr(err))
				return nil
			}
			*exit = emitter.Success("workspace.start", handle)
			return nil
		},
	}
}

func newWorkspaceStopCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "stop <project>",
		Short: "Stop the workspace microVM (state preserved)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := workspace.RealManager(nowRFC3339).Stop(args[0]); err != nil {
				*exit = emitter.Failure("workspace.stop", mapWorkspaceErr(err))
				return nil
			}
			*exit = emitter.Success("workspace.stop", map[string]any{"project": args[0]})
			return nil
		},
	}
}

func newWorkspaceDestroyCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	// Non-destructive (§4.4): keeps the overlay + host source, so no --yes.
	return &cobra.Command{
		Use:   "destroy <project>",
		Short: "Delete the microVM/runtime handle only (overlay + source kept)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := workspace.RealManager(nowRFC3339).Destroy(args[0]); err != nil {
				*exit = emitter.Failure("workspace.destroy", mapWorkspaceErr(err))
				return nil
			}
			*exit = emitter.Success("workspace.destroy", map[string]any{"project": args[0]})
			return nil
		},
	}
}

func newWorkspaceExecCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "exec <project> -- <command> [args...]",
		Short: "Run a command inside the project workspace",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			if dash < 1 || dash >= len(args) {
				*exit = emitter.Failure("workspace.exec",
					output.Errorf(output.ExitInvalidInput, "usage: ai workspace exec <project> -- <command> [args...]"))
				return nil
			}
			project, argv := args[0], args[dash:]
			result, err := workspace.RealManager(nowRFC3339).Exec(project, argv)
			if err != nil {
				// Platform failure (microVM down, unknown project, …) — §4.5.
				*exit = emitter.Failure("workspace.exec", mapWorkspaceErr(err))
				return nil
			}
			// Inner command ran: `ai` exits 0; the inner exit code rides in data
			// (§4.5), so a non-zero inner exit does not make `ai` exit non-zero.
			*exit = emitter.Success("workspace.exec", result)
			return nil
		},
	}
}
