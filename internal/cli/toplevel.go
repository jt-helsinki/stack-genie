package cli

import (
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/spf13/cobra"
)

// This file builds the canonical FLAT top-level verbs that are unique to the
// flattened surface (`ai destroy`/`ai exec`); the create/list/delete verbs live
// in project.go and the start/stop/restart/shell/agent/attach/sessions verbs live
// in lifecycle.go and workspace.go. The consolidated `ai doctor [name]` lives in
// doctor.go. Each verb takes an OPTIONAL [name] positional resolved by precedence:
// an explicit name, then --project, then the workspace that owns the cwd. The JSON
// envelope `command` keys are the `workspace.*` values.

// newDestroyCmd builds the canonical top-level `ai destroy [name]`: tear down the
// workspace microVM/runtime handle only (the definition, overlay, and host source
// are kept — use `ai delete` to remove the whole workspace).
func newDestroyCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	// Non-destructive (§4.4): keeps the overlay + host source, so no --yes.
	return &cobra.Command{
		Use:               "destroy [name]",
		Short:             "Delete the workspace microVM/runtime handle only (overlay + source kept)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE:              workspaceDestroyRunE(emitter, exit),
	}
}

// newExecCmd builds the canonical top-level `ai exec [name] -- <command>`.
func newExecCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "exec [name] -- <command> [args...]",
		Short:             "Run a command inside the workspace",
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: completeProjectArg,
		RunE:              workspaceExecRunE(emitter, exit),
	}
}
