package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"github.com/spf13/cobra"
)

// logServices is the set accepted by `ai logs --service` (CLI §13.1).
var logServices = []string{"microsandbox", "litellm", "clawpatrol", "headroom", "ollama"}

// tailLines is how many trailing lines `--tail` keeps per source.
const tailLines = 200

// newLogsCmd builds `ai logs` (CLI §13.1): show platform, host-service, and
// workspace logs. It reads the log files the platform writes under
// ~/.ai-platform/logs (and a project's run/ dir with --workspace). The live
// capture that produces service/microVM logs is wired during hardware bring-up;
// host-side this surfaces whatever is already on disk.
func newLogsCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var workspaceName, service string
	var tail bool
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show platform, service, and workspace logs",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if service != "" && !slices.Contains(logServices, service) {
				*exit = emitter.Failure("logs",
					output.Errorf(output.ExitInvalidInput, "unknown service %q (one of %v)", service, logServices))
				return nil
			}

			sources, err := logSources(workspaceName, service)
			if err != nil {
				*exit = emitter.Failure("logs", output.Errorf(output.ExitInvalidInput, "%s", err))
				return nil
			}

			collected := []map[string]any{}
			for _, source := range sources {
				lines, err := readLogLines(source, tail)
				if err != nil {
					*exit = emitter.Failure("logs", output.Errorf(output.ExitRuntimeFailure, "read %s: %s", source, err))
					return nil
				}
				collected = append(collected, map[string]any{"source": source, "lines": lines})
			}

			*exit = emitter.Success("logs", map[string]any{
				"logs": collected,
				"note": "live service/microVM log capture is wired during hardware bring-up; this shows logs already on disk",
			})
			return nil
		},
	}
	cmd.Flags().StringVar(&workspaceName, "workspace", "", "scope to a project's workspace run/ logs")
	cmd.Flags().StringVar(&service, "service", "", "scope to a host service: "+strings.Join(logServices, "|"))
	cmd.Flags().BoolVar(&tail, "tail", false, "show only the most recent lines per source")
	return cmd
}

// logSources resolves the *.log files to read: the platform logs dir (filtered by
// service when given), plus the project run/ dir when --workspace is set. Missing
// directories yield no sources (not an error).
func logSources(workspaceName, service string) ([]string, error) {
	var roots []string

	logsDir, err := paths.LogsDir()
	if err != nil {
		return nil, err
	}
	roots = append(roots, logsDir)

	if workspaceName != "" {
		root, err := resolveProjectRoot(workspaceName)
		if err != nil {
			return nil, err
		}
		roots = append(roots, filepath.Join(root, ".ai-platform", "run"))
	}

	var sources []string
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
				continue
			}
			if service != "" && !strings.Contains(entry.Name(), service) {
				continue
			}
			sources = append(sources, filepath.Join(root, entry.Name()))
		}
	}
	return sources, nil
}

func readLogLines(path string, tail bool) ([]string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []string{}, nil
	}
	if tail && len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	return lines, nil
}
