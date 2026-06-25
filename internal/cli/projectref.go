package cli

import (
	"os"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/state"
	"github.com/spf13/cobra"
)

// currentProjectName returns the project that owns the current working directory,
// found by walking up until a directory with .ai-platform/project.yaml is reached
// (arch §3). found is false when the CWD is not inside any project — that is not
// an error here, so callers can decide whether it is required.
func currentProjectName() (name string, found bool, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false, err
	}
	root, ok := state.FindProjectRoot(cwd)
	if !ok {
		return "", false, nil
	}
	projectState, err := state.OpenStore(root).LoadProject()
	if err != nil {
		return "", false, err
	}
	return projectState.Name, true, nil
}

// resolveProjectName determines the project a command targets. Precedence:
// an explicit positional name, then the --project flag, then the project that
// owns the current working directory (bubbling up). When none resolve it returns
// an exit-2 error. This is what lets `ai start` (no args, inside a project) work
// while `ai start other` still targets a named project.
func resolveProjectName(cmd *cobra.Command, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if flag, _ := cmd.Flags().GetString("project"); flag != "" {
		return flag, nil
	}
	name, found, err := currentProjectName()
	if err != nil {
		return "", output.Errorf(output.ExitRuntimeFailure, "resolve current workspace: %s", err)
	}
	if !found {
		return "", output.Errorf(output.ExitInvalidInput,
			"no workspace specified and the current directory is not inside one; pass a workspace name, use --project, or cd into a workspace")
	}
	return name, nil
}

// firstArg returns args[0] or "" — the optional leading <project> positional.
func firstArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}
