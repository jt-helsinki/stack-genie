package cli

import (
	"errors"
	"fmt"
	goruntime "runtime"
	"strconv"
	"time"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/runtime"
	"github.com/jt-helsinki/ideal-robot/internal/state"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
	"github.com/spf13/cobra"
)

// workspacesResult is the typed payload of `ai workspace list`. The `workspaces`
// field keeps the JSON envelope shape stable while Human() renders a table.
type workspacesResult struct {
	Workspaces []state.Workspace `json:"workspaces"`
}

// Human renders the workspaces as a table PROJECT, ID, STATUS, CREATED,
// LAST-STARTED, or a friendly hint when there are none.
func (result workspacesResult) Human() string {
	if len(result.Workspaces) == 0 {
		return "No workspaces yet — start one with `ai workspace start`."
	}
	rows := make([][]string, 0, len(result.Workspaces))
	for _, entry := range result.Workspaces {
		rows = append(rows, []string{
			entry.Project,
			entry.ID,
			string(entry.Status),
			orDash(entry.Created),
			orDash(entry.LastStarted),
		})
	}
	return ui.Table([]string{"PROJECT", "ID", "STATUS", "CREATED", "LAST-STARTED"}, rows)
}

// sessionsResult is the typed payload of `ai sessions` / `ai workspace sessions`.
type sessionsResult struct {
	Project  string              `json:"project"`
	Sessions []workspace.Session `json:"sessions"`
}

// Human renders the workspace sessions as a table NAME, ATTACHED, IDLE, or a
// friendly hint when there are none.
func (result sessionsResult) Human() string {
	if len(result.Sessions) == 0 {
		return "No sessions yet — start one with `ai agent <cli>` or `ai shell`."
	}
	rows := make([][]string, 0, len(result.Sessions))
	for _, session := range result.Sessions {
		attached := "no"
		if session.Attached {
			attached = "yes"
		}
		rows = append(rows, []string{session.Name, attached, sessionIdle(session.Activity)})
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
		newWorkspaceShellCmd(emitter, exit),
		newWorkspaceAgentCmd(emitter, exit),
		newWorkspaceAttachCmd(emitter, exit),
		newWorkspaceSessionsCmd(emitter, exit),
		newWorkspaceDoctorCmd(emitter, exit),
	)
	return cmd
}

// openWorkspaceShell is the shared body of `ai shell` / `ai workspace shell`: an
// interactive login shell inside the project's running workspace microVM (a real
// PTY via msb exec -t). It is interactive-only — it owns the terminal and emits
// no JSON envelope — so it is rejected under --json / a non-TTY (exit 2). On a
// clean exit it leaves no stdout envelope (like `ai ui`).
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

// newWorkspaceShellCmd builds `ai workspace shell [project]`.
func newWorkspaceShellCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "shell [project]",
		Short:             "Open an interactive shell inside the project workspace",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve the project before the TTY gate only when a name might come
			// from the arg/flag; the gate itself doesn't need it, but a bad project
			// should still report cleanly, so gate first (interactive is the cheap
			// check) then resolve.
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
		},
	}
}

// newShellCmd builds the top-level `ai shell` (shortcut for the current
// directory's project — `ai workspace shell`).
func newShellCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "shell",
		Short: "Open an interactive shell in the current directory's workspace (shortcut for `ai workspace shell`)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !interactive(emitter) {
				*exit = emitter.Failure("workspace.shell", output.Errorf(output.ExitInvalidInput,
					"ai shell is interactive and needs a terminal (not available with --json or when piped)"))
				return nil
			}
			name, err := cwdProjectName()
			if err != nil {
				*exit = emitter.Failure("workspace.shell", err)
				return nil
			}
			openWorkspaceShell(emitter, exit, name)
			return nil
		},
	}
}

// openWorkspaceAgent is the shared body of `ai agent <cli>` / `ai workspace agent
// <cli> [project]`: start (or reattach to) a per-CLI tmux session running the
// agent CLI inside the project's running workspace microVM. Like the shell it is
// interactive-only — it owns the terminal and emits no JSON envelope — so it is
// rejected under --json / a non-TTY (exit 2). On a clean exit it leaves no stdout
// envelope.
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

// newWorkspaceAgentCmd builds `ai workspace agent <cli> [project]`.
func newWorkspaceAgentCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "agent <cli> [project]",
		Short:             "Start or reattach an agent CLI session inside the project workspace",
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeAgentArg,
		RunE: func(cmd *cobra.Command, args []string) error {
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
		},
	}
}

// newAgentCmd builds the top-level `ai agent <cli>` (shortcut for the current
// directory's project — `ai workspace agent`).
func newAgentCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "agent <cli>",
		Short:             "Start an agent CLI session in the current directory's workspace (shortcut for `ai workspace agent`)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeAgentArg,
		RunE: func(_ *cobra.Command, args []string) error {
			if !interactive(emitter) {
				*exit = emitter.Failure("workspace.agent", output.Errorf(output.ExitInvalidInput,
					"ai agent is interactive and needs a terminal (not available with --json or when piped)"))
				return nil
			}
			name, err := cwdProjectName()
			if err != nil {
				*exit = emitter.Failure("workspace.agent", err)
				return nil
			}
			openWorkspaceAgent(emitter, exit, name, args[0])
			return nil
		},
	}
}

// openWorkspaceAttach is the shared body of `ai attach [session]` / `ai workspace
// attach [session] [project]`: attach to (creating it if needed) a persistent
// tmux session inside the project's running workspace microVM, defaulting to the
// "shell" session. Interactive-only like the shell (exit 2 under --json/no-TTY).
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

// newWorkspaceAttachCmd builds `ai workspace attach [session] [project]`.
func newWorkspaceAttachCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "attach [session] [project]",
		Short: "Attach to a workspace session (default: shell)",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
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
		},
	}
}

// newAttachCmd builds the top-level `ai attach [session]` (shortcut for the
// current directory's project — `ai workspace attach`).
func newAttachCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "attach [session]",
		Short: "Attach to a session in the current directory's workspace (shortcut for `ai workspace attach`)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if !interactive(emitter) {
				*exit = emitter.Failure("workspace.attach", output.Errorf(output.ExitInvalidInput,
					"ai attach is interactive and needs a terminal (not available with --json or when piped)"))
				return nil
			}
			name, err := cwdProjectName()
			if err != nil {
				*exit = emitter.Failure("workspace.attach", err)
				return nil
			}
			openWorkspaceAttach(emitter, exit, name, firstArg(args))
			return nil
		},
	}
}

// listSessions is the shared body of `ai sessions` / `ai workspace sessions`: a
// normal (non-interactive) command that lists the project's workspace tmux
// sessions, rendering a table by default and the JSON envelope under --json.
func listSessions(emitter *output.Emitter, exit *int, name string) {
	sessions, err := workspace.RealManager(goruntime.GOOS, nowRFC3339).ListSessions(name)
	if err != nil {
		*exit = emitter.Failure("workspace.sessions", mapWorkspaceErr(err))
		return
	}
	*exit = emitter.Success("workspace.sessions", sessionsResult{Project: name, Sessions: sessions})
}

// newWorkspaceSessionsCmd builds `ai workspace sessions [project]`.
func newWorkspaceSessionsCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "sessions [project]",
		Short:             "List the persistent sessions in the project workspace",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveProjectName(cmd, firstArg(args))
			if err != nil {
				*exit = emitter.Failure("workspace.sessions", err)
				return nil
			}
			listSessions(emitter, exit, name)
			return nil
		},
	}
}

// newSessionsCmd builds the top-level `ai sessions` (shortcut for the current
// directory's project — `ai workspace sessions`).
func newSessionsCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "sessions",
		Short: "List the current directory's workspace sessions (shortcut for `ai workspace sessions`)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			name, err := cwdProjectName()
			if err != nil {
				*exit = emitter.Failure("workspace.sessions", err)
				return nil
			}
			listSessions(emitter, exit, name)
			return nil
		},
	}
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
			*exit = emitter.Success("workspace.list", workspacesResult{Workspaces: workspaces})
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
			// On a terminal (and not --json) confirm before destroying the
			// runtime handle; declining cancels with a success envelope (exit 0).
			// Under --json / no TTY the behavior is unchanged: it proceeds directly
			// (this command has never required --yes), so callers are not broken.
			// --yes bypasses the dialog.
			if yes, _ := cmd.Flags().GetBool("yes"); !yes && interactive(emitter) {
				ok, promptErr := promptConfirm(
					fmt.Sprintf("Destroy the workspace for %q?", name),
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
