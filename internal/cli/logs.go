package cli

import (
	goruntime "runtime"
	"slices"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/logs"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/setup"
	"github.com/spf13/cobra"
)

// logsResult is the `ai logs` payload (JSON) with a readable human rendering.
type logsResult struct {
	Logs []logSource `json:"logs"`
	Note string      `json:"note"`
}

type logSource struct {
	Source string   `json:"source"`
	Lines  []string `json:"lines"`
}

// Human renders the logs readably: a clear "no logs" message when empty, else
// each source under a header.
func (result logsResult) Human() string {
	var builder strings.Builder
	if len(result.Logs) == 0 {
		builder.WriteString("No logs on disk yet.")
		if result.Note != "" {
			builder.WriteString("\n(" + result.Note + ")")
		}
		return builder.String()
	}
	for index, source := range result.Logs {
		if index > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString("── " + source.Source + " ──\n")
		for _, line := range source.Lines {
			builder.WriteString(line + "\n")
		}
	}
	return strings.TrimRight(builder.String(), "\n")
}

// logServices is the host services accepted by `ai logs --service` (CLI §13.1).
// The list lives in internal/logs (the single source of truth shared with the
// TUI Logs view); `ai logs` validates against it and offers it as completions.
var logServices = logs.Services()

// newLogsCmd builds `ai logs` (CLI §13.1): show platform, host-service, and
// workspace logs. It reads the log files the platform writes under
// ~/.ai-platform/logs (and a project's run/ dir with --workspace). Those service
// logs are written by setup's CaptureServiceLogs snapshot (run by `ai setup` /
// `ai services status`); continuous follow (`logs -f`) is a later enhancement, so
// this surfaces whatever snapshot is already on disk.
func newLogsCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var workspaceName, service string
	var tail, follow bool
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show platform, service, and workspace logs",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Default the workspace scope to the current directory's project (if
			// any) when --workspace is not given (arch §3).
			if workspaceName == "" {
				if name, found, _ := currentProjectName(); found {
					workspaceName = name
				}
			}
			if service != "" && !slices.Contains(logServices, service) {
				*exit = emitter.Failure("logs",
					output.Errorf(output.ExitInvalidInput, "unknown service %q (one of %v)", service, logServices))
				return nil
			}
			// --follow streams a service's logs live to stdout until Ctrl-C. It is
			// human-only (no single-envelope JSON) and needs a --service.
			if follow {
				if emitter.JSON {
					*exit = emitter.Failure("logs", output.Errorf(output.ExitInvalidInput,
						"--follow streams continuously and is not available with --json"))
					return nil
				}
				if service == "" {
					*exit = emitter.Failure("logs", output.Errorf(output.ExitInvalidInput,
						"--follow needs a --service to stream (e.g. ai logs --service litellm --follow)"))
					return nil
				}
				deps := setup.RealDeps(goruntime.GOOS, goruntime.GOARCH, nowRFC3339)
				if err := setup.FollowServiceLogs(deps, service, emitter.Out); err != nil {
					*exit = emitter.Failure("logs", err)
					return nil
				}
				*exit = output.ExitOK
				return nil
			}

			sources, err := logs.Sources(workspaceName, service)
			if err != nil {
				*exit = emitter.Failure("logs", output.Errorf(output.ExitInvalidInput, "%s", err))
				return nil
			}

			result := logsResult{
				Logs: []logSource{},
				Note: "service logs are snapshotted by `ai setup` / `ai services status`; this shows the latest snapshot on disk — use `--service <name> --follow` to stream live",
			}
			tailCount := 0
			if tail {
				tailCount = logs.TailLines
			}
			for _, source := range sources {
				lines, err := logs.Tail(source, tailCount)
				if err != nil {
					*exit = emitter.Failure("logs", output.Errorf(output.ExitRuntimeFailure, "read %s: %s", source, err))
					return nil
				}
				result.Logs = append(result.Logs, logSource{Source: source, Lines: lines})
			}

			*exit = emitter.Success("logs", result)
			return nil
		},
	}
	cmd.Flags().StringVar(&workspaceName, "workspace", "", "scope to a project's workspace run/ logs")
	cmd.Flags().StringVar(&service, "service", "", "scope to a host service: "+strings.Join(logServices, "|"))
	cmd.Flags().BoolVar(&tail, "tail", false, "show only the most recent lines per source")
	cmd.Flags().BoolVar(&follow, "follow", false, "stream a --service's logs live until Ctrl-C (human-only)")
	_ = cmd.RegisterFlagCompletionFunc("service", fixedValues(logServices...))
	_ = cmd.RegisterFlagCompletionFunc("workspace", completeProjectNames)
	return cmd
}
