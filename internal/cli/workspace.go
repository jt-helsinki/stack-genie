package cli

import (
	"errors"
	goruntime "runtime"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/state"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
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
		newWorkspaceRestartCmd(emitter, exit),
		newWorkspaceDestroyCmd(emitter, exit),
		newWorkspaceExecCmd(emitter, exit),
		newWorkspaceDoctorCmd(emitter, exit),
	)
	return cmd
}

// newWorkspaceDoctorCmd builds `ai workspace doctor <project>` (CLI §12.1, AT
// §11.1): report the service-tier rootless/privileged posture and the workspace
// virtualization. There is no rooted or non-microVM fallback (§6.1/§6.2), so a
// rootless or virtualization shortfall exits 4; a missing runtime exits 3.
func newWorkspaceDoctorCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "doctor [project]",
		Short:             "Diagnose the project's workspace runtime and virtualization",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveProjectName(cmd, firstArg(args))
			if err != nil {
				*exit = emitter.Failure("workspace.doctor", err)
				return nil
			}
			if _, err := resolveProjectRoot(name); err != nil {
				*exit = emitter.Failure("workspace.doctor", output.Errorf(output.ExitInvalidInput, "%s", err))
				return nil
			}
			info, err := runtime.Detect(goruntime.GOOS, goruntime.GOARCH, runtime.RealProber(), nowRFC3339())
			if err != nil {
				// Missing container runtime or Microsandbox → exit 3 (§18).
				*exit = emitter.Failure("workspace.doctor", output.Errorf(output.ExitMissingDep, "%s", err))
				return nil
			}
			data := map[string]any{
				"runtime": map[string]any{
					"detected":   info.Detected,
					"rootless":   info.Rootless,
					"privileged": false, // the platform never runs privileged containers (§6.1)
				},
				"workspace": map[string]any{
					"kind":           "microvm", // workspaces are always microVMs (§6.2)
					"virtualization": info.Microsandbox.Virtualization,
					"available":      info.Microsandbox.Available,
				},
			}
			if err := runtime.Verify(info); err != nil {
				// No rooted / non-microVM fallback (§6.1, §6.2) → exit 4.
				*exit = emitter.Failure("workspace.doctor", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			*exit = emitter.Success("workspace.doctor", data)
			return nil
		},
	}
}

// mapWorkspaceErr maps lifecycle errors to exit codes (§18).
func mapWorkspaceErr(err error) error {
	switch {
	case errors.Is(err, workspace.ErrUnknownProject), errors.Is(err, workspace.ErrNotStarted):
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	case errors.Is(err, workspace.ErrContainerRuntimeMissing), errors.Is(err, workspace.ErrMsbMissing):
		return output.Errorf(output.ExitMissingDep, "%s", err)
	default:
		return output.Errorf(output.ExitRuntimeFailure, "%s", err)
	}
}

// startWorkspace builds the image and boots the project's workspace microVM.
// Both steps are slow, so on a TTY (not --json/--plain) it animates a spinner on
// stderr while the work runs; under --json/automation/no-TTY it runs the Manager
// directly with no spinner (the envelope path is unchanged). Shared by `ai
// workspace start`, the `ai start` cwd shortcut, and project attach.
func startWorkspace(emitter *output.Emitter, name string) (*state.Workspace, error) {
	manager := workspace.RealManager(goruntime.GOOS, nowRFC3339)
	if !ui.Enabled(emitter) {
		return manager.Start(name)
	}
	var handle *state.Workspace
	err := ui.RunWithSpinner(emitter.Err, "starting workspace "+name, func() error {
		var workErr error
		handle, workErr = manager.Start(name)
		return workErr
	})
	return handle, err
}

// restartWorkspace restarts the existing workspace microVM (no rebuild). Like
// startWorkspace it animates a spinner on a TTY and runs directly otherwise.
func restartWorkspace(emitter *output.Emitter, name string) (*state.Workspace, error) {
	manager := workspace.RealManager(goruntime.GOOS, nowRFC3339)
	if !ui.Enabled(emitter) {
		return manager.Restart(name)
	}
	var handle *state.Workspace
	err := ui.RunWithSpinner(emitter.Err, "restarting workspace "+name, func() error {
		var workErr error
		handle, workErr = manager.Restart(name)
		return workErr
	})
	return handle, err
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
		Use:               "start [project]",
		Short:             "Build the image and start the project's workspace microVM",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveProjectName(cmd, firstArg(args))
			if err != nil {
				*exit = emitter.Failure("workspace.start", err)
				return nil
			}
			handle, err := startWorkspace(emitter, name)
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
		Use:               "stop [project]",
		Short:             "Stop the workspace microVM (state preserved)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveProjectName(cmd, firstArg(args))
			if err != nil {
				*exit = emitter.Failure("workspace.stop", err)
				return nil
			}
			if err := workspace.RealManager(goruntime.GOOS, nowRFC3339).Stop(name); err != nil {
				*exit = emitter.Failure("workspace.stop", mapWorkspaceErr(err))
				return nil
			}
			*exit = emitter.Success("workspace.stop", map[string]any{"project": name})
			return nil
		},
	}
}

func newWorkspaceRestartCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "restart [project]",
		Short:             "Restart the existing workspace microVM (no rebuild, state preserved)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveProjectName(cmd, firstArg(args))
			if err != nil {
				*exit = emitter.Failure("workspace.restart", err)
				return nil
			}
			handle, err := restartWorkspace(emitter, name)
			if err != nil {
				*exit = emitter.Failure("workspace.restart", mapWorkspaceErr(err))
				return nil
			}
			*exit = emitter.Success("workspace.restart", handle)
			return nil
		},
	}
}

func newWorkspaceDestroyCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	// Non-destructive (§4.4): keeps the overlay + host source, so no --yes.
	return &cobra.Command{
		Use:               "destroy [project]",
		Short:             "Delete the microVM/runtime handle only (overlay + source kept)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveProjectName(cmd, firstArg(args))
			if err != nil {
				*exit = emitter.Failure("workspace.destroy", err)
				return nil
			}
			if err := workspace.RealManager(goruntime.GOOS, nowRFC3339).Destroy(name); err != nil {
				*exit = emitter.Failure("workspace.destroy", mapWorkspaceErr(err))
				return nil
			}
			*exit = emitter.Success("workspace.destroy", map[string]any{"project": name})
			return nil
		},
	}
}

func newWorkspaceExecCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "exec [project] -- <command> [args...]",
		Short:             "Run a command inside the project workspace",
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: completeProjectArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Args before `--` are the optional [project]; args after are the
			// command. `--` is required so the command is unambiguous.
			dash := cmd.ArgsLenAtDash()
			if dash < 0 || dash > 1 || dash >= len(args) {
				*exit = emitter.Failure("workspace.exec",
					output.Errorf(output.ExitInvalidInput, "usage: ai workspace exec [project] -- <command> [args...]"))
				return nil
			}
			name, err := resolveProjectName(cmd, firstArg(args[:dash]))
			if err != nil {
				*exit = emitter.Failure("workspace.exec", err)
				return nil
			}
			argv := args[dash:]
			result, err := workspace.RealManager(goruntime.GOOS, nowRFC3339).Exec(name, argv)
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
