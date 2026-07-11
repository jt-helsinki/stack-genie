package cli

import (
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/spf13/cobra"
)

// Top-level `ai start|stop|restart [name]` are the only microVM lifecycle verbs
// (CLI §4; the surface is flat — there is no `ai workspace start|stop|restart`).
// The target workspace is resolved like every other verb: an explicit [name],
// then --project, then the workspace that owns the current working directory
// (found by walking up to a `.ai-platform` root). The shared RunE backs all
// three verbs and the emitted envelope `command` key stays `workspace.*`.

// cwdProjectName resolves the workspace that owns the current directory by
// walking up to a `.ai-platform` root. When the cwd is not inside any workspace
// it returns an actionable exit-2 error (§18), matching how resolveProjectName
// surfaces the not-in-a-workspace case.
func cwdProjectName() (string, error) {
	name, found, err := currentProjectName()
	if err != nil {
		return "", output.Errorf(output.ExitRuntimeFailure, "resolve current workspace: %s", err)
	}
	if !found {
		return "", output.Errorf(output.ExitInvalidInput,
			"not inside a workspace (no .ai-platform found in this or any parent directory); run `ai create` first, or cd into a workspace")
	}
	return name, nil
}

func newStartCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "start [name]",
		Short:             "Build the image and start the workspace microVM (defaults to the current directory)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE:              workspaceStartRunE(emitter, exit),
	}
}

func newStopCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "stop [name]",
		Short:             "Stop the workspace microVM (state preserved; defaults to the current directory)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE:              workspaceStopRunE(emitter, exit),
	}
}

func newRestartCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "restart [name]",
		Short:             "Restart the workspace microVM (rebuilds + recreates to re-apply config; defaults to the current directory)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE:              workspaceRestartRunE(emitter, exit),
	}
}
