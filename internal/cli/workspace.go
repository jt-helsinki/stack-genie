package cli

import (
	"errors"
	"fmt"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
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

// defaultShellSession is the session name pre-filled in the `ai shell` new-session
// prompt — the conventional shell session (matching the platform default), so the
// common "just give me a shell" case is one Enter away.
const defaultShellSession = "shell"

// newSessionSentinel is the non-typeable select value standing for "create a new
// session" in the `ai shell` picker (it can't collide with a real session name —
// session names are validated to letters/digits/'-'/'_').
const newSessionSentinel = "\x00new"

// runAttach opens (creating it if absent) the named tmux session in the workspace
// and reports the outcome under commandKey. The interactive gate is the caller's
// (shell/attach are TTY-only); a clean inner-shell exit leaves no stdout envelope.
func runAttach(emitter *output.Emitter, exit *int, commandKey, name, session string) {
	if err := workspace.RealManager(goruntime.GOOS, nowRFC3339).Attach(name, session); err != nil {
		*exit = emitter.Failure(commandKey, mapWorkspaceErr(err))
		return
	}
	*exit = output.ExitOK
}

// sessionNames projects the session list to its names (for the pickers).
func sessionNames(sessions []workspace.Session) []string {
	names := make([]string, 0, len(sessions))
	for _, session := range sessions {
		names = append(names, session.Name)
	}
	return names
}

// validateSessionName enforces a tmux-safe session name (tmux forbids '.'/':' and
// whitespace), so a newly-created session name can't break the tmux invocation.
func validateSessionName(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("a session name is required")
	}
	for _, char := range trimmed {
		switch {
		case char == '-', char == '_',
			char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		default:
			return fmt.Errorf("use only letters, digits, '-' or '_'")
		}
	}
	return nil
}

// promptShellSession is the `ai shell` picker: in ONE form, choose an existing
// session to attach to OR create a new one (typing its name, shown only when "new"
// is selected). Returns the chosen session name. With no existing sessions it opens
// straight on the new-session name (pre-filled with the default), so first use is a
// single Enter.
func promptShellSession(existing []string) (string, error) {
	choice := newSessionSentinel
	newName := defaultShellSession
	if len(existing) > 0 {
		choice = existing[0] // pre-select the first existing session to attach
	}

	options := make([]huh.Option[string], 0, len(existing)+1)
	for _, name := range existing {
		options = append(options, huh.NewOption("attach: "+name, name))
	}
	options = append(options, huh.NewOption("＋ new session…", newSessionSentinel))

	selectGroup := huh.NewGroup(
		huh.NewSelect[string]().
			Title("Workspace session").
			Description("Attach to an existing session, or create a new one.").
			Options(options...).
			Value(&choice),
	)
	// The name input only appears (and only validates) when "new session" is chosen.
	nameGroup := huh.NewGroup(
		huh.NewInput().
			Title("New session name").
			Description("Letters, digits, '-' or '_'.").
			Value(&newName).
			Validate(validateSessionName),
	).WithHideFunc(func() bool { return choice != newSessionSentinel })

	if err := runForm(selectGroup, nameGroup); err != nil {
		return "", err
	}
	if choice != newSessionSentinel {
		return choice, nil
	}
	return strings.TrimSpace(newName), nil
}

// workspaceShellRunE is the RunE for `ai shell [name]`: resolve the workspace, then
// offer to ATTACH to an existing session or CREATE a new one (an explicit
// `--session` skips the picker and attaches/creates that one directly).
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
		// An explicit --session attaches/creates that session directly (scriptable,
		// and skips the picker).
		if session, _ := cmd.Flags().GetString("session"); session != "" {
			if validateErr := validateSessionName(session); validateErr != nil {
				*exit = emitter.Failure("workspace.shell", output.Errorf(output.ExitInvalidInput, "%s", validateErr))
				return nil
			}
			runAttach(emitter, exit, "workspace.shell", name, session)
			return nil
		}
		// Otherwise offer attach-or-create over the workspace's current sessions.
		sessions, err := workspace.RealManager(goruntime.GOOS, nowRFC3339).ListSessions(name)
		if err != nil {
			*exit = emitter.Failure("workspace.shell", mapWorkspaceErr(err))
			return nil
		}
		session, err := promptShellSession(sessionNames(sessions))
		if err != nil {
			*exit = emitter.Failure("workspace.shell", err)
			return nil
		}
		runAttach(emitter, exit, "workspace.shell", name, session)
		return nil
	}
}

// newShellCmd builds the canonical top-level `ai shell [name]` (defaults to the
// current directory's workspace).
func newShellCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "shell [name]",
		Short:             "Open a workspace session: attach to one or create a new one (defaults to the current directory)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE:              workspaceShellRunE(emitter, exit),
	}
	cmd.Flags().String("session", "", "attach to (or create) this session directly, skipping the picker")
	return cmd
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

// attachNoSessionsResult is the `ai attach` payload when the workspace has no
// sessions to attach to — it points the user at `ai shell` to create one.
type attachNoSessionsResult struct {
	Project string `json:"project"`
}

// Human tells the user there is nothing to attach to and how to make one.
func (result attachNoSessionsResult) Human() string {
	return ui.Muted.Render("No sessions in workspace ") + ui.Value.Render(result.Project) +
		ui.Muted.Render(" — run ") + ui.Primary.Render("`ai shell`") + ui.Muted.Render(" to create one.")
}

// workspaceAttachRunE is the RunE for `ai attach [session] [name]`. With an explicit
// [session] it attaches directly; with none it LISTS the workspace's sessions and
// lets the user pick one — and if there are none, it says so and points at
// `ai shell`. The optional trailing [name] resolves the workspace (else --project,
// else cwd).
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
		// An explicit session name attaches directly (no picker).
		if session := firstArg(args); session != "" {
			runAttach(emitter, exit, "workspace.attach", name, session)
			return nil
		}
		// No session given: list the workspace's sessions and let the user choose.
		sessions, err := workspace.RealManager(goruntime.GOOS, nowRFC3339).ListSessions(name)
		if err != nil {
			*exit = emitter.Failure("workspace.attach", mapWorkspaceErr(err))
			return nil
		}
		if len(sessions) == 0 {
			// Nothing to attach to — attach never CREATES; that is `ai shell`'s job.
			*exit = emitter.Success("workspace.attach", attachNoSessionsResult{Project: name})
			return nil
		}
		session, err := promptChoice("Attach to session",
			"Pick a running workspace session.", sessionOptions(sessions), sessions[0].Name)
		if err != nil {
			*exit = emitter.Failure("workspace.attach", err)
			return nil
		}
		runAttach(emitter, exit, "workspace.attach", name, session)
		return nil
	}
}

// sessionOptions renders the session list as labelled select options (name + an
// "attached"/idle hint) for the attach picker.
func sessionOptions(sessions []workspace.Session) []huh.Option[string] {
	options := make([]huh.Option[string], 0, len(sessions))
	for _, session := range sessions {
		label := session.Name
		if session.Attached {
			label += " (attached)"
		} else if idle := sessionIdle(session.Activity); idle != "-" {
			label += " (idle " + idle + ")"
		}
		options = append(options, huh.NewOption(label, session.Name))
	}
	return options
}

// newAttachCmd builds the canonical top-level `ai attach [session] [name]`
// (defaults to the current directory's workspace).
func newAttachCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "attach [session] [name]",
		Short: "Attach to a workspace session — lists them to choose from (defaults to the current directory)",
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
// the current directory's workspace), with a `kill` subcommand to delete a session.
func newSessionsCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "sessions [name]",
		Short:             "List the workspace's persistent sessions (defaults to the current directory)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProjectArg,
		RunE:              workspaceSessionsRunE(emitter, exit),
	}
	cmd.AddCommand(newSessionsKillCmd(emitter, exit))
	return cmd
}

// newSessionsKillCmd builds `ai sessions kill <session> [name]` — delete (kill) a
// workspace tmux session by name.
func newSessionsKillCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	return &cobra.Command{
		Use:   "kill <session> [name]",
		Short: "Kill a workspace session by name (defaults to the current directory's workspace)",
		Args:  cobra.RangeArgs(1, 2),
		RunE:  workspaceSessionsKillRunE(emitter, exit),
	}
}

// workspaceSessionsKillRunE is the RunE for `ai sessions kill <session> [name]`.
func workspaceSessionsKillRunE(emitter *output.Emitter, exit *int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		session := args[0]
		nameArg := ""
		if len(args) > 1 {
			nameArg = args[1]
		}
		name, err := resolveProjectName(cmd, nameArg)
		if err != nil {
			*exit = emitter.Failure("workspace.sessions.kill", err)
			return nil
		}
		if err := workspace.RealManager(goruntime.GOOS, nowRFC3339).KillSession(name, session); err != nil {
			*exit = emitter.Failure("workspace.sessions.kill", mapWorkspaceErr(err))
			return nil
		}
		*exit = emitter.Success("workspace.sessions.kill", sessionKillResult{Project: name, Session: session})
		return nil
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
	case errors.Is(err, workspace.ErrWorkspaceStale), errors.Is(err, workspace.ErrWorkspaceUnresponsive):
		// A stale or overloaded microVM is a runtime condition the user fixes with
		// `ai restart` (the sentinels carry that nudge) — exit 4.
		return output.Errorf(output.ExitRuntimeFailure, "%s", err)
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
	// The image BUILD + `msb load` + microVM create stream their OWN native progress
	// (docker BuildKit layers, msb load/pull bars) to the terminal — a bubbletea
	// spinner cannot share the screen with them (it garbles INTO the build output, as
	// observed). So we DON'T spin: print a one-line header (on a TTY) and let the
	// native progress show, exactly like `ai setup`'s pre-pull. Under --json/no-TTY no
	// header is printed and the envelope path is unchanged.
	if ui.Enabled(emitter) {
		_, _ = fmt.Fprintln(emitter.Err, "Starting workspace "+name+" (building image + booting microVM)…")
	}
	return manager.Start(name)
}

// restartWorkspace restarts the existing workspace microVM (rebuild + recreate to
// re-apply the network/published-port set, via Manager.Restart → Start). Like
// startWorkspace it animates a spinner on a TTY and runs directly otherwise.
func restartWorkspace(emitter *output.Emitter, name string) (*state.Workspace, error) {
	manager := workspace.RealManager(goruntime.GOOS, nowRFC3339)
	// As with start: the rebuild + image-load + recreate stream native progress, so no
	// spinner (it would garble into the build output). Header on a TTY, then stream.
	if ui.Enabled(emitter) {
		_, _ = fmt.Fprintln(emitter.Err, "Restarting workspace "+name+" (rebuilding image + booting microVM)…")
	}
	return manager.Restart(name)
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
