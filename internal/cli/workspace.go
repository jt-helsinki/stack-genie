package cli

import (
	"errors"
	"fmt"
	goruntime "runtime"
	"strconv"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/state"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
	"github.com/spf13/cobra"
)

// sessionsResult is the typed payload of `ai sessions`.
type sessionsResult struct {
	Project  string              `json:"project"`
	Sessions []workspace.Session `json:"sessions"`
}

// Human renders the workspace sessions as a table NAME, ATTACHED, IDLE, or a
// friendly hint when there are none.
func (result sessionsResult) Human() string {
	if len(result.Sessions) == 0 {
		return ui.Muted.Render("No sessions yet — start one with ") +
			ui.Primary.Render("`ai agent <cli>`") + ui.Muted.Render(" or ") +
			ui.Primary.Render("`ai shell`") + ui.Muted.Render(".")
	}
	rows := make([][]string, 0, len(result.Sessions))
	for _, session := range result.Sessions {
		attached := ui.Muted.Render("no")
		if session.Attached {
			attached = ui.Success.Render("yes")
		}
		rows = append(rows, []string{ui.Value.Render(session.Name), attached, ui.Value.Render(sessionIdle(session.Activity))})
	}
	return ui.Table([]string{"NAME", "ATTACHED", "IDLE"}, rows)
}

// sessionIdle renders how long a session has been idle from tmux's raw
// last-activity epoch string (relative to now). A blank/unparseable value renders
// as a dash so the table stays readable.
func sessionIdle(activity string) string {
	if activity == "" {
		return "-"
	}
	epoch, err := strconv.ParseInt(activity, 10, 64)
	if err != nil {
		return "-"
	}
	idle := time.Since(time.Unix(epoch, 0))
	if idle < 0 {
		idle = 0
	}
	switch {
	case idle < time.Minute:
		return fmt.Sprintf("%ds", int(idle.Seconds()))
	case idle < time.Hour:
		return fmt.Sprintf("%dm", int(idle.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(idle.Hours()))
	}
}

// secondArg returns args[1] or "" — the optional trailing <project> positional
// for commands whose first positional is a non-project value (the agent CLI).
func secondArg(args []string) string {
	if len(args) > 1 {
		return args[1]
	}
	return ""
}

// openWorkspaceShell is the shared body of `ai shell`: an interactive login shell
// inside the project's running workspace microVM (a real PTY via msb exec -t). It
// is interactive-only — it owns the terminal and emits no JSON envelope — so it is
// rejected under --json / a non-TTY (exit 2). On a clean exit it leaves no stdout
// envelope (like `ai ui`).
func openWorkspaceShell(emitter *output.Emitter, exit *int, name string) {
	if !interactive(emitter) {
		*exit = emitter.Failure("workspace.shell", output.Errorf(output.ExitInvalidInput,
			"ai shell is interactive and needs a terminal (not available with --json or when piped)"))
		return
	}
	if err := workspace.RealManager(goruntime.GOOS, nowRFC3339).Shell(name); err != nil {
		*exit = emitter.Failure("workspace.shell", mapWorkspaceErr(err))
		return
	}
	*exit = output.ExitOK
}

// workspaceShellRunE is the RunE for `ai shell [name]`: resolve the workspace
// (explicit name, --project, or cwd) and open an interactive login shell inside it.
func workspaceShellRunE(emitter *output.Emitter, exit *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		// Gate the TTY first (the cheap check); a bad name still reports cleanly
		// after, since resolution runs only once a terminal is present.
		if !interactive(emitter) {
			*exit = emitter.Failure("workspace.shell", output.Errorf(output.ExitInvalidInput,
				"ai shell is interactive and needs a terminal (not available with --json or when piped)"))
			return nil
		}
		name, err := resolveProjectName(cmd, firstArg(args))
		if err != nil {
			*exit = emitter.Failure("workspace.shell", err)
			return nil
		}
		openWorkspaceShell(emitter, exit, name)
		return nil
	}
}

// newShellCmd builds the canonical top-level `ai shell [name]` (defaults to the
// current directory's workspace).
func newShellCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "shell [name]",
		Short:             "Open an interactive shell inside the workspace (defaults to the current directory)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE:              workspaceShellRunE(emitter, exit),
	}
}

// openWorkspaceAgent is the shared body of `ai agent <cli> [name]`: start (or
// reattach to) a per-CLI tmux session running the agent CLI inside the project's
// running workspace microVM. Like the shell it is interactive-only — it owns the
// terminal and emits no JSON envelope — so it is rejected under --json / a non-TTY
// (exit 2). On a clean exit it leaves no stdout envelope.
func openWorkspaceAgent(emitter *output.Emitter, exit *int, name, cli string) {
	if !interactive(emitter) {
		*exit = emitter.Failure("workspace.agent", output.Errorf(output.ExitInvalidInput,
			"ai agent is interactive and needs a terminal (not available with --json or when piped)"))
		return
	}
	if err := workspace.RealManager(goruntime.GOOS, nowRFC3339).Agent(name, cli); err != nil {
		*exit = emitter.Failure("workspace.agent", mapWorkspaceErr(err))
		return
	}
	*exit = output.ExitOK
}

// workspaceAgentRunE is the RunE for `ai agent <cli> [name]`: the agent CLI is the
// first positional, the optional trailing [name] resolves the workspace (else
// --project, else cwd).
func workspaceAgentRunE(emitter *output.Emitter, exit *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if !interactive(emitter) {
			*exit = emitter.Failure("workspace.agent", output.Errorf(output.ExitInvalidInput,
				"ai agent is interactive and needs a terminal (not available with --json or when piped)"))
			return nil
		}
		name, err := resolveProjectName(cmd, secondArg(args))
		if err != nil {
			*exit = emitter.Failure("workspace.agent", err)
			return nil
		}
		openWorkspaceAgent(emitter, exit, name, args[0])
		return nil
	}
}

// newAgentCmd builds the canonical top-level `ai agent <cli> [name]` (defaults to
// the current directory's workspace).
func newAgentCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "agent <cli> [name]",
		Short:             "Start or reattach an agent CLI session inside the workspace (defaults to the current directory)",
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeAgentArg,
		RunE:              workspaceAgentRunE(emitter, exit),
	}
}

// openWorkspaceAttach is the shared body of `ai attach [session] [name]`: attach
// to (creating it if needed) a persistent tmux session inside the project's
// running workspace microVM, defaulting to the "shell" session. Interactive-only
// like the shell (exit 2 under --json/no-TTY).
func openWorkspaceAttach(emitter *output.Emitter, exit *int, name, session string) {
	if !interactive(emitter) {
		*exit = emitter.Failure("workspace.attach", output.Errorf(output.ExitInvalidInput,
			"ai attach is interactive and needs a terminal (not available with --json or when piped)"))
		return
	}
	if err := workspace.RealManager(goruntime.GOOS, nowRFC3339).Attach(name, session); err != nil {
		*exit = emitter.Failure("workspace.attach", mapWorkspaceErr(err))
		return
	}
	*exit = output.ExitOK
}

// workspaceAttachRunE is the RunE for `ai attach [session] [name]`: the optional
// [session] is the first positional (default "shell"), the optional trailing
// [name] resolves the workspace (else --project, else cwd).
func workspaceAttachRunE(emitter *output.Emitter, exit *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if !interactive(emitter) {
			*exit = emitter.Failure("workspace.attach", output.Errorf(output.ExitInvalidInput,
				"ai attach is interactive and needs a terminal (not available with --json or when piped)"))
			return nil
		}
		name, err := resolveProjectName(cmd, secondArg(args))
		if err != nil {
			*exit = emitter.Failure("workspace.attach", err)
			return nil
		}
		openWorkspaceAttach(emitter, exit, name, firstArg(args))
		return nil
	}
}

// newAttachCmd builds the canonical top-level `ai attach [session] [name]`
// (defaults to the current directory's workspace).
func newAttachCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "attach [session] [name]",
		Short: "Attach to a workspace session (default: shell; defaults to the current directory)",
		Args:  cobra.MaximumNArgs(2),
		RunE:  workspaceAttachRunE(emitter, exit),
	}
}

// listSessions is the shared body of `ai sessions`: a normal (non-interactive)
// command that lists the project's workspace tmux sessions, rendering a table by
// default and the JSON envelope under --json.
func listSessions(emitter *output.Emitter, exit *int, name string) {
	sessions, err := workspace.RealManager(goruntime.GOOS, nowRFC3339).ListSessions(name)
	if err != nil {
		*exit = emitter.Failure("workspace.sessions", mapWorkspaceErr(err))
		return
	}
	*exit = emitter.Success("workspace.sessions", sessionsResult{Project: name, Sessions: sessions})
}

// workspaceSessionsRunE is the RunE for `ai sessions [name]`: list the
// workspace's persistent tmux sessions.
func workspaceSessionsRunE(emitter *output.Emitter, exit *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		name, err := resolveProjectName(cmd, firstArg(args))
		if err != nil {
			*exit = emitter.Failure("workspace.sessions", err)
			return nil
		}
		listSessions(emitter, exit, name)
		return nil
	}
}

// newSessionsCmd builds the canonical top-level `ai sessions [name]` (defaults to
// the current directory's workspace).
func newSessionsCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "sessions [name]",
		Short:             "List the workspace's persistent sessions (defaults to the current directory)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE:              workspaceSessionsRunE(emitter, exit),
	}
}

// mapWorkspaceErr maps lifecycle errors to exit codes (§18).
func mapWorkspaceErr(err error) error {
	var platformErr *output.Error
	if errors.As(err, &platformErr) {
		return platformErr
	}
	switch {
	case errors.Is(err, workspace.ErrUnknownProject), errors.Is(err, workspace.ErrNotStarted), errors.Is(err, workspace.ErrUnknownAgentCLI):
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	case errors.Is(err, workspace.ErrContainerRuntimeMissing), errors.Is(err, workspace.ErrMsbMissing), errors.Is(err, workspace.ErrTmuxMissing):
		return output.Errorf(output.ExitMissingDep, "%s", err)
	default:
		return output.Errorf(output.ExitRuntimeFailure, "%s", err)
	}
}

// startWorkspace builds the image and boots the project's workspace microVM.
// Both steps are slow, so on a TTY (not --json/--plain) it animates a spinner on
// stderr while the work runs; under --json/automation/no-TTY it runs the Manager
// directly with no spinner (the envelope path is unchanged). Shared by `ai start`,
// the `ai start` cwd shortcut, and project attach.
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

// restartWorkspace restarts the existing workspace microVM (rebuild + recreate to
// re-apply the network/published-port set, via Manager.Restart → Start). Like
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

// workspaceStartRunE is the RunE for `ai start [name]`.
func workspaceStartRunE(emitter *output.Emitter, exit *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
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
	}
}

// workspaceStopRunE is the RunE for `ai stop [name]`.
func workspaceStopRunE(emitter *output.Emitter, exit *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
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
	}
}

// workspaceRestartRunE is the RunE for `ai restart [name]`.
func workspaceRestartRunE(emitter *output.Emitter, exit *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
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
	}
}

// workspaceDestroyRunE is the RunE for `ai destroy [name]`. Non-destructive
// (§4.4): keeps the overlay + host source, so no --yes is required; on a TTY it
// confirms first.
func workspaceDestroyRunE(emitter *output.Emitter, exit *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		name, err := resolveProjectName(cmd, firstArg(args))
		if err != nil {
			*exit = emitter.Failure("workspace.destroy", err)
			return nil
		}
		// On a terminal (and not --json) confirm before destroying the
		// runtime handle; declining cancels with a success envelope (exit 0).
		// Under --json / no TTY the behavior is unchanged: it proceeds directly
		// (this command has never required --yes), so callers are not broken.
		// --yes bypasses the dialog.
		if yes, _ := cmd.Flags().GetBool("yes"); !yes && interactive(emitter) {
			ok, promptErr := promptConfirm(
				fmt.Sprintf("Destroy the workspace %q?", name),
				"Removes the microVM/runtime handle only — the overlay and your source are kept.")
			if promptErr != nil {
				*exit = emitter.Failure("workspace.destroy", promptErr)
				return nil
			}
			if !ok {
				*exit = emitter.Success("workspace.destroy", map[string]any{"cancelled": true})
				return nil
			}
		}
		if err := workspace.RealManager(goruntime.GOOS, nowRFC3339).Destroy(name); err != nil {
			*exit = emitter.Failure("workspace.destroy", mapWorkspaceErr(err))
			return nil
		}
		*exit = emitter.Success("workspace.destroy", map[string]any{"project": name})
		return nil
	}
}

// workspaceExecRunE is the RunE for `ai exec [name] -- <command>`.
func workspaceExecRunE(emitter *output.Emitter, exit *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		// Args before `--` are the optional [name]; args after are the command.
		// `--` is required so the command is unambiguous.
		dash := cmd.ArgsLenAtDash()
		if dash < 0 || dash > 1 || dash >= len(args) {
			*exit = emitter.Failure("workspace.exec",
				output.Errorf(output.ExitInvalidInput, "usage: ai exec [name] -- <command> [args...]"))
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
			// Platform failure (microVM down, unknown workspace, …) — §4.5.
			*exit = emitter.Failure("workspace.exec", mapWorkspaceErr(err))
			return nil
		}
		// Inner command ran: `ai` exits 0; the inner exit code rides in data
		// (§4.5), so a non-zero inner exit does not make `ai` exit non-zero.
		*exit = emitter.Success("workspace.exec", result)
		return nil
	}
}
