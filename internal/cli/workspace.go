package cli

import (
	"errors"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/stack-genie/internal/config"
	"github.com/jt-helsinki/stack-genie/internal/output"
	"github.com/jt-helsinki/stack-genie/internal/ui"
	"github.com/jt-helsinki/stack-genie/internal/workspace"
	"github.com/spf13/cobra"
)

// sessionsResult is the typed payload of `ai sessions`.
type sessionsResult struct {
	Project  string              `json:"project"`
	Sessions []workspace.Session `json:"sessions"`
}

// sessionKillResult is the typed payload of `ai sessions kill`.
type sessionKillResult struct {
	Project string `json:"project"`
	Session string `json:"session"`
}

// Human confirms which session was killed.
func (result sessionKillResult) Human() string {
	return ui.Success.Render(ui.IconOK+" killed session ") + ui.Value.Render(result.Session) +
		ui.Muted.Render(" in ") + ui.Value.Render(result.Project)
}

// Human renders the workspace's live nono sessions as a table: ID (short),
// NAME, STATUS, ATTACHED, STARTED — or a friendly hint when there are none.
func (result sessionsResult) Human() string {
	if len(result.Sessions) == 0 {
		return ui.Muted.Render("No sessions yet — start one with ") +
			ui.Primary.Render("`ai agent <cli>`") + ui.Muted.Render(" or ") +
			ui.Primary.Render("`ai shell`") + ui.Muted.Render(".")
	}
	rows := make([][]string, 0, len(result.Sessions))
	for _, session := range result.Sessions {
		name := session.Name
		if name == "" {
			name = "—"
		}
		attached := ui.Muted.Render("no")
		if session.Attachment == "attached" {
			attached = ui.Success.Render("yes")
		}
		rows = append(rows, []string{
			ui.Value.Render(shortSessionID(session.SessionID)),
			ui.Value.Render(name),
			styleWorkspaceStatus(session.Status),
			attached,
			ui.Muted.Render(session.Started),
		})
	}
	return ui.Table([]string{"ID", "NAME", "STATUS", "ATTACHED", "STARTED"}, rows)
}

// shortSessionID renders a nono session id's first 8 characters (enough to
// disambiguate in a table; the full id is still in the JSON payload).
func shortSessionID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// secondArg returns args[1] or "" — the optional trailing <project> positional
// for commands whose first positional is a non-project value (the agent CLI).
func secondArg(args []string) string {
	if len(args) > 1 {
		return args[1]
	}
	return ""
}

// mapWorkspaceErr maps workspace/session errors to exit codes (§18).
func mapWorkspaceErr(err error) error {
	var platformErr *output.Error
	if errors.As(err, &platformErr) {
		return platformErr
	}
	switch {
	case errors.Is(err, workspace.ErrUnknownProject),
		errors.Is(err, workspace.ErrUnknownAgentCLI),
		errors.Is(err, workspace.ErrNoActiveSession):
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	case errors.Is(err, workspace.ErrNonoMissing):
		return output.Errorf(output.ExitMissingDep, "%s", err)
	default:
		return output.Errorf(output.ExitRuntimeFailure, "%s", err)
	}
}

// runningSessionByName returns the SessionID of a RUNNING session named name for
// the project, or "" if none (found=false).
func runningSessionByName(sessions []workspace.Session, name string) (string, bool) {
	for _, session := range sessions {
		if session.Name == name && session.Status == "running" {
			return session.SessionID, true
		}
	}
	return "", false
}

// attachOrLaunchAgent reattaches to cli's already-running session for project
// if one exists, else launches a fresh detached one (Manager.Agent) and
// attaches to it. This is the shared "start or reattach" flow behind `ai
// agent`/`ai start`/`ai restart`.
func attachOrLaunchAgent(manager workspace.Manager, project, cli string) error {
	if sessions, err := manager.ListSessions(project); err == nil {
		if sessionID, found := runningSessionByName(sessions, cli); found {
			return manager.Attach(sessionID)
		}
	}
	handle, err := manager.Agent(project, cli)
	if err != nil {
		return err
	}
	return manager.Attach(handle.ID)
}

// defaultAgentCLI resolves the project's default agent CLI (config.yaml
// agent.default_tool, falling back to the first installed CLI). Used by `ai
// start`/`ai restart`, which launch whichever CLI the workspace was created
// with as its default.
func defaultAgentCLI(root string) (string, error) {
	projectConfig, err := config.Load(root)
	if err != nil {
		return "", err
	}
	if projectConfig.Agent.DefaultTool != "" {
		return projectConfig.Agent.DefaultTool, nil
	}
	if len(projectConfig.Agent.Tools) > 0 {
		return projectConfig.Agent.Tools[0], nil
	}
	return "", output.Errorf(output.ExitRuntimeFailure, "workspace has no agent CLI configured")
}

// workspaceShellRunE is the RunE for `ai shell [name]`: opens a fresh
// interactive nono shell sandboxed to the workspace's project directory.
func workspaceShellRunE(emitter *output.Emitter, exit *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
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
		if err := workspace.RealManager(nowRFC3339).Shell(name); err != nil {
			*exit = emitter.Failure("workspace.shell", mapWorkspaceErr(err))
			return nil
		}
		*exit = output.ExitOK
		return nil
	}
}

// newShellCmd builds the canonical top-level `ai shell [name]` (defaults to
// the current directory's workspace).
func newShellCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "shell [name]",
		Short:             "Open a sandboxed interactive shell in the workspace (defaults to the current directory)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE:              workspaceShellRunE(emitter, exit),
	}
}

// workspaceAgentRunE is the RunE for `ai agent <cli> [name]`: the agent CLI is
// the first positional, the optional trailing [name] resolves the workspace
// (else --project, else cwd). Reattaches to the CLI's already-running session
// if one exists, else launches a fresh one.
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
		manager := workspace.RealManager(nowRFC3339)
		if err := attachOrLaunchAgent(manager, name, args[0]); err != nil {
			*exit = emitter.Failure("workspace.agent", mapWorkspaceErr(err))
			return nil
		}
		*exit = output.ExitOK
		return nil
	}
}

// newAgentCmd builds the canonical top-level `ai agent <cli> [name]` (defaults
// to the current directory's workspace).
func newAgentCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:               "agent <cli> [name]",
		Short:             "Start or reattach an agent CLI's sandboxed session (defaults to the current directory)",
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeAgentArg,
		RunE:              workspaceAgentRunE(emitter, exit),
	}
}

// attachNoSessionsResult is the `ai attach` payload when the workspace has no
// sessions to attach to — it points the user at `ai shell`/`ai agent` to
// create one.
type attachNoSessionsResult struct {
	Project string `json:"project"`
}

// Human tells the user there is nothing to attach to and how to make one.
func (result attachNoSessionsResult) Human() string {
	return ui.Muted.Render("No sessions in workspace ") + ui.Value.Render(result.Project) +
		ui.Muted.Render(" — run ") + ui.Primary.Render("`ai agent <cli>`") + ui.Muted.Render(" to create one.")
}

// workspaceAttachRunE is the RunE for `ai attach [session] [name]`. With an
// explicit [session] (an id or name/prefix nono can resolve) it attaches
// directly; with none it LISTS the workspace's running sessions and lets the
// user pick one — and if there are none, it says so and points at `ai agent`.
// The optional trailing [name] resolves the workspace (else --project, else
// cwd).
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
		manager := workspace.RealManager(nowRFC3339)
		// An explicit session id/name attaches directly (no picker).
		if session := firstArg(args); session != "" {
			if err := manager.Attach(session); err != nil {
				*exit = emitter.Failure("workspace.attach", mapWorkspaceErr(err))
				return nil
			}
			*exit = output.ExitOK
			return nil
		}
		// No session given: list the workspace's running sessions and let the user choose.
		sessions, err := manager.ListSessions(name)
		if err != nil {
			*exit = emitter.Failure("workspace.attach", mapWorkspaceErr(err))
			return nil
		}
		running := make([]workspace.Session, 0, len(sessions))
		for _, session := range sessions {
			if session.Status == "running" {
				running = append(running, session)
			}
		}
		if len(running) == 0 {
			// Nothing to attach to — attach never CREATES; that is `ai agent`/`ai shell`'s job.
			*exit = emitter.Success("workspace.attach", attachNoSessionsResult{Project: name})
			return nil
		}
		sessionID, err := promptChoice("Attach to session",
			"Pick a running workspace session.", sessionOptions(running), running[0].SessionID)
		if err != nil {
			*exit = emitter.Failure("workspace.attach", err)
			return nil
		}
		if err := manager.Attach(sessionID); err != nil {
			*exit = emitter.Failure("workspace.attach", mapWorkspaceErr(err))
			return nil
		}
		*exit = output.ExitOK
		return nil
	}
}

// sessionOptions renders the running-session list as labelled select options
// (name + short id) for the attach picker, valued by SessionID.
func sessionOptions(sessions []workspace.Session) []huh.Option[string] {
	options := make([]huh.Option[string], 0, len(sessions))
	for _, session := range sessions {
		label := session.Name
		if label == "" {
			label = "(shell)"
		}
		label += " [" + shortSessionID(session.SessionID) + "]"
		options = append(options, huh.NewOption(label, session.SessionID))
	}
	return options
}

// newAttachCmd builds the canonical top-level `ai attach [session] [name]`
// (defaults to the current directory's workspace).
func newAttachCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "attach [session] [name]",
		Short: "Attach to a workspace session — lists running ones to choose from (defaults to the current directory)",
		Args:  cobra.MaximumNArgs(2),
		RunE:  workspaceAttachRunE(emitter, exit),
	}
}

// listSessions is the shared body of `ai sessions`: a normal (non-interactive)
// command that lists the project's live nono sessions, rendering a table by
// default and the JSON envelope under --json.
func listSessions(emitter *output.Emitter, exit *int, name string) {
	sessions, err := workspace.RealManager(nowRFC3339).ListSessions(name)
	if err != nil {
		*exit = emitter.Failure("workspace.sessions", mapWorkspaceErr(err))
		return
	}
	*exit = emitter.Success("workspace.sessions", sessionsResult{Project: name, Sessions: sessions})
}

// workspaceSessionsRunE is the RunE for `ai sessions [name]`: list the
// workspace's live nono sessions.
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

// newSessionsCmd builds the canonical top-level `ai sessions [name]` (defaults
// to the current directory's workspace), with a `kill` subcommand to stop a
// session.
func newSessionsCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "sessions [name]",
		Short:             "List the workspace's live nono sessions (defaults to the current directory)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE:              workspaceSessionsRunE(emitter, exit),
	}
	cmd.AddCommand(newSessionsKillCmd(emitter, exit))
	return cmd
}

// newSessionsKillCmd builds `ai sessions kill <session> [name]` — stop a
// workspace session by id or name.
func newSessionsKillCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kill <session> [name]",
		Short: "Stop a workspace session by id or name (defaults to the current directory's workspace)",
		Args:  cobra.RangeArgs(1, 2),
		RunE:  workspaceSessionsKillRunE(emitter, exit),
	}
	cmd.Flags().Bool("force", false, "force-kill (SIGKILL) instead of a graceful stop")
	return cmd
}

// workspaceSessionsKillRunE is the RunE for `ai sessions kill <session> [name]`.
func workspaceSessionsKillRunE(emitter *output.Emitter, exit *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		session := args[0]
		name, err := resolveProjectName(cmd, secondArg(args))
		if err != nil {
			*exit = emitter.Failure("workspace.sessions.kill", err)
			return nil
		}
		force, _ := cmd.Flags().GetBool("force")
		if err := workspace.RealManager(nowRFC3339).Stop(session, force); err != nil {
			*exit = emitter.Failure("workspace.sessions.kill", mapWorkspaceErr(err))
			return nil
		}
		*exit = emitter.Success("workspace.sessions.kill", sessionKillResult{Project: name, Session: session})
		return nil
	}
}

// workspaceStartRunE is the RunE for `ai start [name]`: launches (or
// reattaches to) the workspace's default agent CLI session. There is no
// persistent microVM to boot anymore — this is sugar over `ai agent
// <default-cli>`.
func workspaceStartRunE(emitter *output.Emitter, exit *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		name, err := resolveProjectName(cmd, firstArg(args))
		if err != nil {
			*exit = emitter.Failure("workspace.start", err)
			return nil
		}
		if !interactive(emitter) {
			*exit = emitter.Failure("workspace.start", output.Errorf(output.ExitInvalidInput,
				"ai start is interactive and needs a terminal (not available with --json or when piped)"))
			return nil
		}
		root, err := resolveProjectRoot(name)
		if err != nil {
			*exit = emitter.Failure("workspace.start", output.Errorf(output.ExitInvalidInput, "unknown workspace %q", name))
			return nil
		}
		cli, err := defaultAgentCLI(root)
		if err != nil {
			*exit = emitter.Failure("workspace.start", mapWorkspaceErr(err))
			return nil
		}
		if err := attachOrLaunchAgent(workspace.RealManager(nowRFC3339), name, cli); err != nil {
			*exit = emitter.Failure("workspace.start", mapWorkspaceErr(err))
			return nil
		}
		*exit = output.ExitOK
		return nil
	}
}

// workspaceStopRunE is the RunE for `ai stop [name]`: stops every live session
// for the workspace.
func workspaceStopRunE(emitter *output.Emitter, exit *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		name, err := resolveProjectName(cmd, firstArg(args))
		if err != nil {
			*exit = emitter.Failure("workspace.stop", err)
			return nil
		}
		if err := workspace.RealManager(nowRFC3339).StopAll(name); err != nil {
			*exit = emitter.Failure("workspace.stop", mapWorkspaceErr(err))
			return nil
		}
		*exit = emitter.Success("workspace.stop", map[string]any{"project": name})
		return nil
	}
}

// workspaceRestartRunE is the RunE for `ai restart [name]`: stops every live
// session, then launches the default agent CLI fresh.
func workspaceRestartRunE(emitter *output.Emitter, exit *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		name, err := resolveProjectName(cmd, firstArg(args))
		if err != nil {
			*exit = emitter.Failure("workspace.restart", err)
			return nil
		}
		if !interactive(emitter) {
			*exit = emitter.Failure("workspace.restart", output.Errorf(output.ExitInvalidInput,
				"ai restart is interactive and needs a terminal (not available with --json or when piped)"))
			return nil
		}
		root, err := resolveProjectRoot(name)
		if err != nil {
			*exit = emitter.Failure("workspace.restart", output.Errorf(output.ExitInvalidInput, "unknown workspace %q", name))
			return nil
		}
		cli, err := defaultAgentCLI(root)
		if err != nil {
			*exit = emitter.Failure("workspace.restart", mapWorkspaceErr(err))
			return nil
		}
		manager := workspace.RealManager(nowRFC3339)
		if err := manager.StopAll(name); err != nil {
			*exit = emitter.Failure("workspace.restart", mapWorkspaceErr(err))
			return nil
		}
		if err := attachOrLaunchAgent(manager, name, cli); err != nil {
			*exit = emitter.Failure("workspace.restart", mapWorkspaceErr(err))
			return nil
		}
		*exit = output.ExitOK
		return nil
	}
}
