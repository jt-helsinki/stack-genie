package cli

import (
	goruntime "runtime"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
	"github.com/spf13/cobra"
)

// Top-level `ai start|stop|restart` are cwd-resolved shortcuts for the matching
// `ai workspace start|stop|restart` commands (CLI §4). Unlike the workspace
// subcommands they take NO `[project]` argument: the target workspace is always
// the one that owns the current working directory, found by walking up parent
// directories to a workspace root (a directory containing `.ai-platform`). The
// behavior and emitted envelope are identical to the workspace subcommands, so
// they reuse the same Manager methods and the same `workspace.*` command names.

// cwdProjectName resolves the workspace that owns the current directory by
// walking up to a `.ai-platform` root. When the cwd is not inside any workspace
// it returns an actionable exit-2 error (§18), matching how resolveProjectName
// surfaces the not-in-a-project case.
func cwdProjectName() (string, error) {
	name, found, err := currentProjectName()
	if err != nil {
		return "", output.Errorf(output.ExitRuntimeFailure, "resolve current workspace: %s", err)
	}
	if !found {
		return "", output.Errorf(output.ExitInvalidInput,
			"not inside a workspace (no .ai-platform found in this or any parent directory); run `ai project create` first or cd into a project")
	}
	return name, nil
}

func newStartCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the current directory's workspace microVM (shortcut for `ai workspace start`)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			name, err := cwdProjectName()
			if err != nil {
				*exit = emitter.Failure("workspace.start", err)
				return nil
			}
			handle, err := workspace.RealManager(goruntime.GOOS, nowRFC3339).Start(name)
			if err != nil {
				*exit = emitter.Failure("workspace.start", mapWorkspaceErr(err))
				return nil
			}
			*exit = emitter.Success("workspace.start", handle)
			return nil
		},
	}
}

func newStopCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the current directory's workspace microVM (shortcut for `ai workspace stop`)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			name, err := cwdProjectName()
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

func newRestartCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the current directory's workspace microVM (shortcut for `ai workspace restart`)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			name, err := cwdProjectName()
			if err != nil {
				*exit = emitter.Failure("workspace.restart", err)
				return nil
			}
			handle, err := workspace.RealManager(goruntime.GOOS, nowRFC3339).Restart(name)
			if err != nil {
				*exit = emitter.Failure("workspace.restart", mapWorkspaceErr(err))
				return nil
			}
			*exit = emitter.Success("workspace.restart", handle)
			return nil
		},
	}
}
