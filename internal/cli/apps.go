package cli

import (
	"errors"
	"fmt"
	goruntime "runtime"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/apps"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/ui"
	"github.com/jt-helsinki/ideal-robot/internal/workspace"
	"github.com/spf13/cobra"
)

// appsResult is the typed payload of `ai apps list`. The `apps` field keeps the
// JSON shape stable while Human() renders a table.
type appsResult struct {
	Project string        `json:"project"`
	Apps    []apps.Status `json:"apps"`
}

// Human renders the apps as a table APP / STATUS / URL.
func (result appsResult) Human() string {
	if len(result.Apps) == 0 {
		return "No apps available."
	}
	rows := make([][]string, 0, len(result.Apps))
	for _, status := range result.Apps {
		rows = append(rows, []string{status.Name, appStatusLabel(status), orDash(status.URL)})
	}
	return ui.Table([]string{"APP", "STATUS", "URL"}, rows)
}

// appStatusLabel renders one app's lifecycle state for the table.
func appStatusLabel(status apps.Status) string {
	switch {
	case !status.Installed:
		return "not installed"
	case status.Running:
		return "running"
	default:
		return "installed (stopped)"
	}
}

// appActionResult is the typed payload of the mutating `ai apps` subcommands.
type appActionResult struct {
	Project         string `json:"project"`
	App             string `json:"app"`
	Action          string `json:"action"`
	Port            int    `json:"port,omitempty"`
	RestartRequired bool   `json:"restart_required,omitempty"`
}

// Human renders a one-line confirmation, noting when a restart is needed to apply
// the change to the published-port set (adding/removing an app).
func (result appActionResult) Human() string {
	line := fmt.Sprintf("%s app %q in workspace %q", result.Action, result.App, result.Project)
	if result.Port > 0 {
		line += fmt.Sprintf(" (host port %d)", result.Port)
	}
	if result.RestartRequired {
		line += "\nRestart the workspace to apply the published port change: `ai restart " + result.Project + "`"
	}
	return line
}

// mapAppsErr maps app lifecycle errors to exit codes (§18).
func mapAppsErr(err error) error {
	var platformErr *output.Error
	if errors.As(err, &platformErr) {
		return platformErr
	}
	switch {
	case errors.Is(err, apps.ErrUnknownApp), errors.Is(err, apps.ErrNotInstalled), errors.Is(err, apps.ErrAlreadyInstalled),
		errors.Is(err, workspace.ErrUnknownProject):
		return output.Errorf(output.ExitInvalidInput, "%s", err)
	case errors.Is(err, apps.ErrWorkspaceNotRunning):
		return output.Errorf(output.ExitMissingDep, "%s", err)
	default:
		return output.Errorf(output.ExitRuntimeFailure, "%s", err)
	}
}

// newAppsCmd builds `ai apps <list|add|remove|update|start|stop|restart> [app] [name]`.
// list takes no app; the mutating verbs take an app key. The optional trailing
// [name] resolves the workspace like the other verbs (explicit → --project → cwd).
func newAppsCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apps <list|add|remove|update|start|stop|restart> [app] [name]",
		Short: "Manage the AI applications running inside the workspace (Open WebUI, AnythingLLM)",
		Args:  cobra.MinimumNArgs(1),
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return []string{"list", "add", "remove", "update", "start", "stop", "restart"}, cobra.ShellCompDirectiveNoFileComp
			}
			if len(args) == 1 {
				return apps.Keys(), cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			action := args[0]
			if action == "list" {
				name, err := resolveProjectName(cmd, secondArg(args))
				if err != nil {
					*exit = emitter.Failure("apps.list", err)
					return nil
				}
				runAppsList(emitter, exit, name)
				return nil
			}
			if !isAppAction(action) {
				*exit = emitter.Failure("apps", output.Errorf(output.ExitInvalidInput,
					"unknown apps action %q (one of: list, add, remove, update, start, stop, restart)", action))
				return nil
			}
			if len(args) < 2 {
				*exit = emitter.Failure("apps."+action, output.Errorf(output.ExitInvalidInput,
					"usage: ai apps %s <app> [name]  (app one of: %s)", action, strings.Join(apps.Keys(), ", ")))
				return nil
			}
			app := args[1]
			if err := apps.Validate(app); err != nil {
				*exit = emitter.Failure("apps."+action, output.Errorf(output.ExitInvalidInput, "%s", err))
				return nil
			}
			// The workspace name is the optional THIRD positional.
			name, err := resolveProjectName(cmd, thirdArg(args))
			if err != nil {
				*exit = emitter.Failure("apps."+action, err)
				return nil
			}
			runAppsAction(emitter, exit, action, app, name)
			return nil
		},
	}
	return cmd
}

// isAppAction reports whether action is a mutating apps verb.
func isAppAction(action string) bool {
	switch action {
	case "add", "remove", "update", "start", "stop", "restart":
		return true
	}
	return false
}

// thirdArg returns args[2] or "" — the optional trailing [name] for the mutating
// apps verbs (whose first two positionals are the action + the app).
func thirdArg(args []string) string {
	if len(args) > 2 {
		return args[2]
	}
	return ""
}

// runAppsList lists the workspace's apps with their status + URL.
func runAppsList(emitter *output.Emitter, exit *int, name string) {
	manager, err := workspace.RealManager(goruntime.GOOS, nowRFC3339).AppManagerFor(name)
	if err != nil {
		*exit = emitter.Failure("apps.list", mapAppsErr(err))
		return
	}
	statuses, err := manager.List()
	if err != nil {
		*exit = emitter.Failure("apps.list", mapAppsErr(err))
		return
	}
	*exit = emitter.Success("apps.list", appsResult{Project: name, Apps: statuses})
}

// runAppsAction performs a mutating apps verb and emits its result.
func runAppsAction(emitter *output.Emitter, exit *int, action, app, name string) {
	command := "apps." + action
	manager, err := workspace.RealManager(goruntime.GOOS, nowRFC3339).AppManagerFor(name)
	if err != nil {
		*exit = emitter.Failure(command, mapAppsErr(err))
		return
	}
	result := appActionResult{Project: name, App: app, Action: action}
	switch action {
	case "add":
		port, restart, addErr := manager.Install(app)
		if addErr != nil {
			*exit = emitter.Failure(command, mapAppsErr(addErr))
			return
		}
		result.Port = port
		result.RestartRequired = restart
	case "remove":
		restart, removeErr := manager.Remove(app)
		if removeErr != nil {
			*exit = emitter.Failure(command, mapAppsErr(removeErr))
			return
		}
		result.RestartRequired = restart
	case "update":
		if updateErr := manager.Update(app); updateErr != nil {
			*exit = emitter.Failure(command, mapAppsErr(updateErr))
			return
		}
	case "start":
		if startErr := manager.Start(app); startErr != nil {
			*exit = emitter.Failure(command, mapAppsErr(startErr))
			return
		}
	case "stop":
		if stopErr := manager.Stop(app); stopErr != nil {
			*exit = emitter.Failure(command, mapAppsErr(stopErr))
			return
		}
	case "restart":
		if restartErr := manager.Restart(app); restartErr != nil {
			*exit = emitter.Failure(command, mapAppsErr(restartErr))
			return
		}
	}
	*exit = emitter.Success(command, result)
}
