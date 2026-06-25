// Package logs resolves and reads the platform's on-disk log files — the single
// source of truth shared by `ai logs` (internal/cli) and the TUI Logs view
// (internal/tui/views), which cannot share code through internal/cli (cli
// imports tui → cycle). It depends only on low-level packages (paths, project,
// os) so both callers can import it.
//
// The live capture that writes these files is the snapshot in
// setup.realServices.CaptureServiceLogs (run by `ai setup` and `ai services
// status`), which is the SOLE container-runtime touch-point — this package stays
// a pure file reader (no docker dependency). Continuous follow (`logs -f`) is a
// later enhancement. A missing logs dir yields an empty result, not an error.
package logs

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/jt-helsinki/ideal-robot/internal/paths"
	"github.com/jt-helsinki/ideal-robot/internal/project"
	"github.com/jt-helsinki/ideal-robot/internal/services"
)

// TailLines is how many trailing lines a tail keeps per source.
const TailLines = 200

// Services returns the host services accepted by `ai logs --service` (CLI §13.1):
// the microVM runtime (microsandbox) plus every container in the service tier —
// ollama, presidio, litellm, headroom, proxy, open-webui, dns, and Odysseus's
// containers (odysseus + chromadb / searxng / ntfy). Logs are per-CONTAINER, so
// this is finer-grained than `ai services` (which acts on whole logical
// services). A service value selects a log source by substring match on the
// *.log file names under ~/.ai-platform/logs (see Sources); those files are
// written by setup.realServices.CaptureServiceLogs (a point-in-time snapshot run
// by `ai setup` / `ai services status`), so for services with nothing on disk yet
// this surfaces an empty result, not an error.
//
// The scope list is derived from the internal/services registry (the single
// source of truth for the platform's service topology), so it cannot drift from
// the console endpoints / version pins / setup reconcile. It returns a fresh copy
// each call, so callers cannot mutate the backing slice.
func Services() []string {
	return services.LogScopes()
}

// Sources resolves the *.log files to read: the platform logs dir (filtered by
// service when given), plus the project run/ dir when workspaceName is set.
// Missing directories yield no sources (not an error). The service filter is a
// substring match on the file name.
func Sources(workspaceName, service string) ([]string, error) {
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

// Tail reads the file at path and returns its lines, keeping only the last
// `lines` lines when the file is longer (lines <= 0 keeps everything). An empty
// file yields an empty slice.
func Tail(path string, lines int) ([]string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	split := strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
	if len(split) == 1 && split[0] == "" {
		return []string{}, nil
	}
	if lines > 0 && len(split) > lines {
		split = split[len(split)-lines:]
	}
	return split, nil
}

// resolveProjectRoot maps a project name to its root via the global index.
func resolveProjectRoot(name string) (string, error) {
	root, exists, err := project.Path(name)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", project.ErrUnknownProject
	}
	return root, nil
}
